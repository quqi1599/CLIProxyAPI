package wsrelay

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestRelayLimitFailuresUseCanonicalContract(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		message  string
		sentinel error
		trigger  func() error
	}{
		{
			name:     "response body",
			code:     wsrelayResponseBodyTooLargeCode,
			message:  "upstream websocket response body exceeded the configured limit",
			sentinel: errHTTPResponseBodyTooLarge,
			trigger: func() error {
				_, err := decodeResponseWithLimit(map[string]any{"body": "secret"}, 1)
				return err
			},
		},
		{
			name:     "stream chunk",
			code:     wsrelayStreamChunkTooLargeCode,
			message:  "upstream websocket stream chunk exceeded the configured limit",
			sentinel: errStreamChunkTooLarge,
			trigger: func() error {
				_, err := decodeChunkWithLimit(map[string]any{"data": "secret"}, 1)
				return err
			},
		},
		{
			name:     "stream response",
			code:     wsrelayStreamResponseTooLargeCode,
			message:  "upstream websocket stream response exceeded the configured limit",
			sentinel: errStreamResponseTooLarge,
			trigger: func() error {
				return (&responseByteBudget{limit: 1}).add(2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.trigger()
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("error = %v, want sentinel %v", err, tt.sentinel)
			}
			typed, ok := failurecontract.As(err)
			if !ok {
				t.Fatalf("error = %T, want typed failure", err)
			}
			if typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider || typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != tt.code {
				t.Fatalf("failure = %#v, want upstream protocol/provider/502/%q", typed, tt.code)
			}
			if typed.Retryable {
				t.Fatal("relay size-limit failure must not be retryable")
			}
			if err.Error() != tt.message {
				t.Fatalf("public message = %q, want %q", err.Error(), tt.message)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("relay size-limit failure exposed payload")
			}
		})
	}
}

func TestDecodeRelayPayloadBoundaries(t *testing.T) {
	exact := strings.Repeat("x", 4)
	resp, err := decodeResponseWithLimit(map[string]any{"status": float64(http.StatusCreated), "body": exact}, len(exact))
	if err != nil {
		t.Fatalf("decode exact-limit response: %v", err)
	}
	if resp.Status != http.StatusCreated || string(resp.Body) != exact {
		t.Fatalf("decoded response = %#v, want status 201 and exact-limit body", resp)
	}

	secret := exact + "sensitive-response-body"
	_, err = decodeResponseWithLimit(map[string]any{"body": secret}, len(exact))
	if !errors.Is(err, errHTTPResponseBodyTooLarge) {
		t.Fatalf("decode limit+1 response error = %v, want %v", err, errHTTPResponseBodyTooLarge)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("size-limit error exposed response content")
	}

	chunk, err := decodeChunkWithLimit(map[string]any{"data": exact}, len(exact))
	if err != nil {
		t.Fatalf("decode exact-limit chunk: %v", err)
	}
	if string(chunk) != exact {
		t.Fatalf("decoded chunk = %q, want %q", chunk, exact)
	}
	_, err = decodeChunkWithLimit(map[string]any{"data": exact + "x"}, len(exact))
	if !errors.Is(err, errStreamChunkTooLarge) {
		t.Fatalf("decode limit+1 chunk error = %v, want %v", err, errStreamChunkTooLarge)
	}
}

func TestNonStreamResponseBudgetAcceptsBoundaryAndRejectsNextChunk(t *testing.T) {
	budget := responseByteBudget{limit: 5}
	for _, size := range []int{2, 3} {
		if err := budget.add(size); err != nil {
			t.Fatalf("add %d-byte chunk at boundary: %v", size, err)
		}
	}
	if budget.total != budget.limit {
		t.Fatalf("budget total = %d, want %d", budget.total, budget.limit)
	}
	if err := budget.add(1); !errors.Is(err, errStreamResponseTooLarge) {
		t.Fatalf("add limit+1 chunk error = %v, want %v", err, errStreamResponseTooLarge)
	}
	if budget.total != budget.limit {
		t.Fatalf("rejected chunk changed budget total to %d", budget.total)
	}
}

func TestRelayChunkLimitClearsPendingRequest(t *testing.T) {
	oversized := strings.Repeat("x", maxStreamChunkBytes+1)

	t.Run("non-stream", func(t *testing.T) {
		mgr, session, client := newConnectedTestSession(t, "nonstream-limit")
		result := make(chan nonStreamResult, 1)
		go func() {
			resp, err := mgr.NonStream(context.Background(), "nonstream-limit", &HTTPRequest{Method: http.MethodPost})
			result <- nonStreamResult{resp: resp, err: err}
		}()

		request := readRelayRequest(t, client)
		writeRelayMessage(t, client, Message{ID: request.ID, Type: MessageTypeStreamChunk, Payload: map[string]any{"data": oversized}})
		got := receiveNonStreamResult(t, result)
		if !errors.Is(got.err, errStreamChunkTooLarge) {
			t.Fatalf("NonStream() limit+1 error = %v, want %v", got.err, errStreamChunkTooLarge)
		}
		if got.resp != nil {
			t.Fatalf("NonStream() limit+1 response = %#v, want nil", got.resp)
		}
		waitForNoPendingRequests(t, session)
	})

	t.Run("stream", func(t *testing.T) {
		mgr, session, client := newConnectedTestSession(t, "stream-limit")
		events, err := mgr.Stream(context.Background(), "stream-limit", &HTTPRequest{Method: http.MethodPost})
		if err != nil {
			t.Fatalf("Stream() error = %v", err)
		}
		request := readRelayRequest(t, client)
		writeRelayMessage(t, client, Message{ID: request.ID, Type: MessageTypeStreamChunk, Payload: map[string]any{"data": oversized}})

		event := receiveRelayEvent(t, events)
		if event.Type != MessageTypeError || !errors.Is(event.Err, errStreamChunkTooLarge) {
			t.Fatalf("stream limit+1 event = %#v, want stable size-limit error", event)
		}
		requireEventChannelClosed(t, events)
		waitForNoPendingRequests(t, session)
	})
}

func TestStreamCancellationClearsPendingRequest(t *testing.T) {
	mgr, session, client := newConnectedTestSession(t, "stream-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	events, err := mgr.Stream(ctx, "stream-cancel", &HTTPRequest{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	_ = readRelayRequest(t, client)
	cancel()
	requireEventChannelClosed(t, events)
	waitForNoPendingRequests(t, session)
}

func TestNonStreamCancellationPreservesContextError(t *testing.T) {
	mgr, session, client := newConnectedTestSession(t, "nonstream-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan nonStreamResult, 1)
	go func() {
		resp, err := mgr.NonStream(ctx, "nonstream-cancel", &HTTPRequest{Method: http.MethodPost})
		result <- nonStreamResult{resp: resp, err: err}
	}()
	_ = readRelayRequest(t, client)
	cancel()
	got := receiveNonStreamResult(t, result)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("NonStream() cancellation error = %v, want context canceled", got.err)
	}
	waitForNoPendingRequests(t, session)
}

func TestNonStreamConnectionCloseClearsPendingRequest(t *testing.T) {
	mgr, session, client := newConnectedTestSession(t, "nonstream-close")
	result := make(chan nonStreamResult, 1)
	go func() {
		resp, err := mgr.NonStream(context.Background(), "nonstream-close", &HTTPRequest{Method: http.MethodPost})
		result <- nonStreamResult{resp: resp, err: err}
	}()
	_ = readRelayRequest(t, client)
	if err := client.Close(); err != nil {
		t.Fatalf("close relay client: %v", err)
	}
	got := receiveNonStreamResult(t, result)
	if got.err == nil {
		t.Fatal("NonStream() connection close error = nil")
	}
	waitForNoPendingRequests(t, session)
}

type nonStreamResult struct {
	resp *HTTPResponse
	err  error
}

func readRelayRequest(t *testing.T, client *websocket.Conn) Message {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set relay client read deadline: %v", err)
	}
	var msg Message
	if err := client.ReadJSON(&msg); err != nil {
		t.Fatalf("read relay request: %v", err)
	}
	return msg
}

func writeRelayMessage(t *testing.T, client *websocket.Conn, msg Message) {
	t.Helper()
	if err := client.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set relay client write deadline: %v", err)
	}
	if err := client.WriteJSON(msg); err != nil {
		t.Fatalf("write relay message: %v", err)
	}
}

func receiveNonStreamResult(t *testing.T, results <-chan nonStreamResult) nonStreamResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for non-stream response")
		return nonStreamResult{}
	}
}

func receiveRelayEvent(t *testing.T, events <-chan StreamEvent) StreamEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("relay event channel closed early")
		}
		return event
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for relay event")
		return StreamEvent{}
	}
}

func requireEventChannelClosed(t *testing.T, events <-chan StreamEvent) {
	t.Helper()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("relay event channel remained open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for relay event channel to close")
	}
}

func waitForNoPendingRequests(t *testing.T, session *session) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pendingRequestCount(session) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending request count = %d, want 0", pendingRequestCount(session))
}
