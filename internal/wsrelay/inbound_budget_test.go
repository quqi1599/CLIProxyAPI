package wsrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestInboundBudgetBounds128ConcurrentFrames(t *testing.T) {
	const workers = 128
	budget := newInboundBudget(128, 64)
	start := make(chan struct{})
	readGate := make(chan struct{})
	results := make(chan error, workers)
	var workersGroup sync.WaitGroup
	workersGroup.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer workersGroup.Done()
			<-start
			payload := []byte(fmt.Sprintf(`{"id":"%03d","type":"ping"}`, i))
			msg, err := decodeInboundMessage(context.Background(), budget, &gatedReader{
				reader: bytes.NewReader(payload),
				gate:   readGate,
			})
			msg.Release()
			results <- err
		}(i)
	}
	close(start)
	waitForInboundBudgetPeak(t, budget, 128)
	close(readGate)
	workersGroup.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("decode concurrent frame: %v", err)
		}
	}
	used, peak := budget.snapshot()
	if used != 0 {
		t.Fatalf("inbound bytes after concurrent decode = %d, want 0", used)
	}
	if peak > 128 {
		t.Fatalf("peak inbound bytes = %d, limit = 128", peak)
	}
}

func TestInboundBudgetBlockedReserveWakesOnCancellation(t *testing.T) {
	budget := newInboundBudget(8, 8)
	owner := budget.newFrameLease()
	if err := owner.reserve(context.Background(), 8); err != nil {
		t.Fatalf("fill inbound budget: %v", err)
	}
	waiter := budget.newFrameLease()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- waiter.reserve(ctx, 1)
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked reserve error = %v, want context canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked inbound reserve did not wake after cancellation")
	}
	waiter.release()
	owner.release()
	if used, _ := budget.snapshot(); used != 0 {
		t.Fatalf("inbound bytes after canceled reserve = %d, want 0", used)
	}
}

func TestDecodeInboundMessageReleasesBudgetOnEveryExit(t *testing.T) {
	exactFrame := `{"id":"exact","type":"ping"}`
	exactFrame += strings.Repeat(" ", 32-len(exactFrame))
	tests := []struct {
		name    string
		payload string
		wantID  string
		wantErr error
	}{
		{name: "normal", payload: `{"id":"ok","type":"ping"}   `, wantID: "ok"},
		{name: "exact frame limit", payload: exactFrame, wantID: "exact"},
		{name: "malformed", payload: `{`, wantErr: errInboundMalformedJSON},
		{name: "multiple values", payload: `{"id":"one"}{"id":"two"}`, wantErr: errInboundMultipleJSON},
		{name: "trailing data", payload: `{"id":"one"}!`, wantErr: errInboundTrailingData},
		{name: "oversize", payload: strings.Repeat(" ", 33), wantErr: errInboundFrameTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := newInboundBudget(64, 32)
			msg, err := decodeInboundMessage(context.Background(), budget, zeroLengthBlindReader{Reader: strings.NewReader(test.payload)})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("decode error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && (msg.ID != test.wantID || msg.Type != MessageTypePing) {
				t.Fatalf("decoded message = %#v, want id %q and type ping", msg, test.wantID)
			}
			msg.Release()
			if used, peak := budget.snapshot(); used != 0 || peak > 64 {
				t.Fatalf("budget after decode = (used=%d, peak=%d), want used 0 and peak <= 64", used, peak)
			}
		})
	}
}

func TestPendingMailboxRetainsAndReleasesInboundBudget(t *testing.T) {
	budget := newInboundBudget(1024, 128)
	request := newPendingRequest("slow")
	first, err := decodeInboundMessage(context.Background(), budget, strings.NewReader(`{"id":"slow","type":"stream_chunk","payload":{"data":"first"}}`))
	if err != nil {
		t.Fatalf("decode first queued message: %v", err)
	}
	if terminated := request.enqueue(first); terminated {
		t.Fatal("first queued message terminated request")
	}
	waitForPendingMailboxLength(t, request, 0)

	for i := 0; i < pendingMailboxSize; i++ {
		payload := fmt.Sprintf(`{"id":"slow","type":"stream_chunk","payload":{"data":"%08d"}}`, i)
		msg, errDecode := decodeInboundMessage(context.Background(), budget, strings.NewReader(payload))
		if errDecode != nil {
			t.Fatalf("decode queued message %d: %v", i, errDecode)
		}
		if terminated := request.enqueue(msg); terminated {
			t.Fatalf("queued message %d terminated request before overflow", i)
		}
	}
	if got := len(request.mailbox); got != pendingMailboxSize {
		t.Fatalf("pending mailbox length = %d, want %d", got, pendingMailboxSize)
	}
	usedBeforeOverflow, peak := budget.snapshot()
	if usedBeforeOverflow == 0 || peak > 1024 {
		t.Fatalf("queued decoded messages budget = (used=%d, peak=%d), want retained bytes within limit", usedBeforeOverflow, peak)
	}

	overflow, err := decodeInboundMessage(context.Background(), budget, strings.NewReader(`{"id":"slow","type":"stream_chunk","payload":{"data":"overflow"}}`))
	if err != nil {
		t.Fatalf("decode overflow message: %v", err)
	}
	if terminated := request.enqueue(overflow); !terminated {
		t.Fatal("overflow message did not terminate request")
	}
	usedAfterOverflow, _ := budget.snapshot()
	if usedAfterOverflow != usedBeforeOverflow {
		t.Fatalf("overflow message retained budget: before=%d after=%d", usedBeforeOverflow, usedAfterOverflow)
	}

	request.abandon()
	select {
	case <-request.done:
	case <-time.After(3 * time.Second):
		t.Fatal("pending request did not release queued messages")
	}
	if used, _ := budget.snapshot(); used != 0 {
		t.Fatalf("inbound bytes after pending request abandon = %d, want 0", used)
	}
}

func TestPendingTerminalCancellationReleasesInboundBudget(t *testing.T) {
	budget := newInboundBudget(256, 128)
	request := newPendingRequest("terminal-cancel")
	terminal, err := decodeInboundMessage(context.Background(), budget, strings.NewReader(`{"id":"terminal-cancel","type":"http_response","payload":{"body":"ok"}}`))
	if err != nil {
		t.Fatalf("decode terminal message: %v", err)
	}
	if terminated := request.enqueue(terminal); !terminated {
		t.Fatal("terminal message did not terminate request")
	}
	request.abandon()
	select {
	case <-request.done:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal request did not stop after cancellation")
	}
	if used, _ := budget.snapshot(); used != 0 {
		t.Fatalf("inbound bytes after terminal cancellation = %d, want 0", used)
	}
}

func TestManagerSendRetainsInboundBudgetUntilConsumerRelease(t *testing.T) {
	const provider = "send-lease"
	mgr, session, client := newConnectedTestSession(t, provider)
	respCh, err := mgr.Send(context.Background(), provider, Message{ID: "send-lease-id", Type: MessageTypeHTTPReq})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	request := readRelayRequest(t, client)
	writeRelayMessage(t, client, Message{ID: request.ID, Type: MessageTypeHTTPResp, Payload: map[string]any{"status": float64(201), "body": "ok"}})

	msg := receiveMessage(t, respCh)
	if used, _ := mgr.inbound.snapshot(); used == 0 {
		t.Fatal("inbound budget released when Manager.Send consumer received message")
	}
	resp, err := decodeResponse(msg.Payload)
	if err != nil {
		msg.Release()
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != 201 || string(resp.Body) != "ok" {
		msg.Release()
		t.Fatalf("decoded response = %#v, want status 201 and body ok", resp)
	}
	if used, _ := mgr.inbound.snapshot(); used == 0 {
		msg.Release()
		t.Fatal("inbound budget released before consumer finished processing Payload")
	}
	msg.Release()
	waitForInboundBudgetUsed(t, mgr.inbound, false)
	requireChannelClosed(t, respCh)
	waitForNoPendingRequests(t, session)
}

func TestStreamInboundBudgetFollowsBlockedConsumer(t *testing.T) {
	for _, test := range []struct {
		name         string
		cancelBefore bool
	}{
		{name: "handoff releases", cancelBefore: false},
		{name: "cancellation releases", cancelBefore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := "stream-lease-" + strings.ReplaceAll(test.name, " ", "-")
			mgr, session, client := newConnectedTestSession(t, provider)
			ctx, cancel := context.WithCancel(context.Background())
			events, errStream := mgr.Stream(ctx, provider, &HTTPRequest{Method: "POST"})
			if errStream != nil {
				cancel()
				t.Fatalf("Stream() error = %v", errStream)
			}
			request := readRelayRequest(t, client)
			payload := strings.Repeat("x", 1024)
			writeRelayMessage(t, client, Message{ID: request.ID, Type: MessageTypeStreamChunk, Payload: map[string]any{"data": payload}})
			waitForInboundBudgetUsed(t, mgr.inbound, true)

			if test.cancelBefore {
				cancel()
				requireEventChannelClosed(t, events)
				waitForInboundBudgetUsed(t, mgr.inbound, false)
				waitForNoPendingRequests(t, session)
				return
			}

			event := receiveRelayEvent(t, events)
			if event.Type != MessageTypeStreamChunk || string(event.Payload) != payload {
				cancel()
				t.Fatalf("stream event = %#v, want copied chunk payload", event)
			}
			waitForInboundBudgetUsed(t, mgr.inbound, false)
			cancel()
			requireEventChannelClosed(t, events)
			waitForNoPendingRequests(t, session)
		})
	}
}

func waitForPendingMailboxLength(t *testing.T, request *pendingRequest, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(request.mailbox) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending mailbox length = %d, want %d", len(request.mailbox), want)
}

func waitForInboundBudgetUsed(t *testing.T, budget *inboundBudget, wantUsed bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		used, _ := budget.snapshot()
		if (used > 0) == wantUsed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	used, _ := budget.snapshot()
	t.Fatalf("inbound budget used = %d, wantUsed = %t", used, wantUsed)
}

type zeroLengthBlindReader struct {
	*strings.Reader
}

func (r zeroLengthBlindReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.Reader.Read(p)
}

func TestSessionAcceptsControlTextAndBinaryFramesWithinBudget(t *testing.T) {
	mgr, _, client := newConnectedTestSession(t, "frame-semantics")
	if err := client.WriteControl(websocket.PingMessage, []byte("control"), time.Now().Add(3*time.Second)); err != nil {
		t.Fatalf("write control ping: %v", err)
	}
	writeSessionJSONFrame(t, client, websocket.TextMessage, Message{ID: "text", Type: MessageTypePing})
	if pong := readSessionMessage(t, client); pong.ID != "text" || pong.Type != MessageTypePong {
		t.Fatalf("text pong = %#v", pong)
	}
	writeSessionJSONFrame(t, client, websocket.BinaryMessage, Message{ID: "binary", Type: MessageTypePing})
	if pong := readSessionMessage(t, client); pong.ID != "binary" || pong.Type != MessageTypePong {
		t.Fatalf("binary pong = %#v", pong)
	}
	if err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("stop manager: %v", err)
	}
	if used, _ := mgr.inbound.snapshot(); used != 0 {
		t.Fatalf("inbound bytes after manager stop = %d, want 0", used)
	}
}

type gatedReader struct {
	reader *bytes.Reader
	gate   <-chan struct{}
}

func (r *gatedReader) Read(p []byte) (int, error) {
	<-r.gate
	return r.reader.Read(p)
}

func waitForInboundBudgetPeak(t *testing.T, budget *inboundBudget, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, peak := budget.snapshot()
		if peak >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, peak := budget.snapshot()
	t.Fatalf("inbound budget peak = %d, want at least %d", peak, want)
}

func writeSessionJSONFrame(t *testing.T, client *websocket.Conn, messageType int, msg Message) {
	t.Helper()
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("encode websocket message: %v", err)
	}
	if err := client.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set websocket write deadline: %v", err)
	}
	if err := client.WriteMessage(messageType, payload); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
}

func readSessionMessage(t *testing.T, client *websocket.Conn) Message {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	var msg Message
	if err := client.ReadJSON(&msg); err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	return msg
}
