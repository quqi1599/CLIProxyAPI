package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDispatchPreservesSequenceAndTerminal(t *testing.T) {
	s := newDispatchTestSession()
	const requestID = "sequence"
	req := newPendingRequest(requestID)
	s.pending.Store(requestID, req)

	for i := 0; i < pendingMailboxSize; i++ {
		s.dispatch(Message{
			ID:      requestID,
			Type:    MessageTypeStreamChunk,
			Payload: map[string]any{"sequence": i},
		})
	}
	s.dispatch(Message{ID: requestID, Type: MessageTypeStreamEnd})

	for i := 0; i < pendingMailboxSize; i++ {
		msg := receiveMessage(t, req.ch)
		if msg.Type != MessageTypeStreamChunk {
			t.Fatalf("message %d type = %q, want %q", i, msg.Type, MessageTypeStreamChunk)
		}
		if got, ok := msg.Payload["sequence"].(int); !ok || got != i {
			t.Fatalf("message %d sequence = %#v, want %d", i, msg.Payload["sequence"], i)
		}
	}
	terminal := receiveMessage(t, req.ch)
	if terminal.Type != MessageTypeStreamEnd {
		t.Fatalf("terminal type = %q, want %q", terminal.Type, MessageTypeStreamEnd)
	}
	requireChannelClosed(t, req.ch)
	if _, ok := s.pending.Load(requestID); ok {
		t.Fatal("terminal message left request in pending map")
	}
}

func TestDispatchOverflowTerminatesRequestWithError(t *testing.T) {
	s := newDispatchTestSession()
	const requestID = "overflow"
	req := newPendingRequest(requestID)
	s.pending.Store(requestID, req)

	for i := 0; i < pendingMailboxSize+2; i++ {
		s.dispatch(Message{
			ID:      requestID,
			Type:    MessageTypeStreamChunk,
			Payload: map[string]any{"sequence": i},
		})
	}
	if _, ok := s.pending.Load(requestID); ok {
		t.Fatal("overflowed request remained in pending map")
	}

	messages := receiveUntilClosed(t, req.ch)
	if len(messages) < pendingMailboxSize+1 || len(messages) > pendingMailboxSize+2 {
		t.Fatalf("received %d messages after overflow, want %d or %d", len(messages), pendingMailboxSize+1, pendingMailboxSize+2)
	}
	for i, msg := range messages[:len(messages)-1] {
		if msg.Type != MessageTypeStreamChunk {
			t.Fatalf("message %d type = %q, want %q", i, msg.Type, MessageTypeStreamChunk)
		}
		if got, ok := msg.Payload["sequence"].(int); !ok || got != i {
			t.Fatalf("message %d sequence = %#v, want %d", i, msg.Payload["sequence"], i)
		}
	}
	terminal := messages[len(messages)-1]
	if terminal.Type != MessageTypeError {
		t.Fatalf("overflow terminal type = %q, want %q", terminal.Type, MessageTypeError)
	}
	if got, _ := terminal.Payload["error"].(string); !strings.Contains(got, errMailboxOverflow.Error()) {
		t.Fatalf("overflow error = %q, want it to contain %q", got, errMailboxOverflow.Error())
	}
}

func TestSlowRequestDoesNotBlockOtherRequests(t *testing.T) {
	s := newDispatchTestSession()
	slow := newPendingRequest("slow")
	fast := newPendingRequest("fast")
	s.pending.Store("slow", slow)
	s.pending.Store("fast", fast)

	for i := 0; i < pendingMailboxSize+2; i++ {
		s.dispatch(Message{ID: "slow", Type: MessageTypeStreamChunk, Payload: map[string]any{"sequence": i}})
	}
	s.dispatch(Message{ID: "fast", Type: MessageTypeHTTPResp, Payload: map[string]any{"body": "ok"}})

	msg := receiveMessage(t, fast.ch)
	if msg.Type != MessageTypeHTTPResp {
		t.Fatalf("fast request type = %q, want %q", msg.Type, MessageTypeHTTPResp)
	}
	if got, _ := msg.Payload["body"].(string); got != "ok" {
		t.Fatalf("fast request body = %q, want ok", got)
	}
	requireChannelClosed(t, fast.ch)

	slowMessages := receiveUntilClosed(t, slow.ch)
	if terminal := slowMessages[len(slowMessages)-1]; terminal.Type != MessageTypeError {
		t.Fatalf("slow request terminal type = %q, want %q", terminal.Type, MessageTypeError)
	}
}

func TestCleanupRacingRequestRegistrationLeavesNoPendingRequests(t *testing.T) {
	mgr, s, client := newConnectedTestSession(t, "cleanup-race")
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for {
			var msg Message
			if err := client.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()

	before, err := s.request(context.Background(), Message{ID: "before-cleanup", Type: MessageTypeHTTPReq})
	if err != nil {
		t.Fatalf("register request before cleanup: %v", err)
	}

	const requestCount = 32
	type requestResult struct {
		id  string
		ch  <-chan Message
		err error
	}
	results := make(chan requestResult, requestCount)
	start := make(chan struct{})
	var requests sync.WaitGroup
	requests.Add(requestCount)
	for i := 0; i < requestCount; i++ {
		go func(i int) {
			defer requests.Done()
			<-start
			id := fmt.Sprintf("racing-%d", i)
			ch, errRequest := s.request(context.Background(), Message{ID: id, Type: MessageTypeHTTPReq})
			results <- requestResult{id: id, ch: ch, err: errRequest}
		}(i)
	}

	cleanupCause := errors.New("test cleanup")
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		<-start
		runtime.Gosched()
		s.cleanup(cleanupCause)
	}()
	close(start)
	requests.Wait()
	<-cleanupDone
	close(results)

	assertCleanupError(t, before, cleanupCause)
	for result := range results {
		if result.err != nil {
			if result.ch != nil {
				t.Fatalf("request %s returned both channel and error %v", result.id, result.err)
			}
			continue
		}
		assertCleanupError(t, result.ch, cleanupCause)
	}
	if count := pendingRequestCount(s); count != 0 {
		t.Fatalf("pending request count after cleanup = %d, want 0", count)
	}
	if ch, errAfter := s.request(context.Background(), Message{ID: "after-cleanup", Type: MessageTypeHTTPReq}); !errors.Is(errAfter, errClosed) || ch != nil {
		t.Fatalf("request after cleanup = (%v, %v), want (nil, %v)", ch, errAfter, errClosed)
	}
	if count := pendingRequestCount(s); count != 0 {
		t.Fatalf("pending request count after rejected registration = %d, want 0", count)
	}

	if errStop := mgr.Stop(context.Background()); errStop != nil {
		t.Fatalf("stop manager: %v", errStop)
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("client reader did not stop after cleanup")
	}
}

func newDispatchTestSession() *session {
	return &session{
		manager:  NewManager(Options{}),
		provider: "test",
		closed:   make(chan struct{}),
	}
}

func newConnectedTestSession(t *testing.T, provider string) (*Manager, *session, *websocket.Conn) {
	t.Helper()
	connected := make(chan struct{})
	var connectedOnce sync.Once
	mgr := NewManager(Options{
		ProviderFactory: func(*http.Request) (string, error) {
			return provider, nil
		},
		OnConnected: func(string) {
			connectedOnce.Do(func() {
				close(connected)
			})
		},
	})
	server := httptest.NewServer(mgr.Handler())
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + mgr.Path()
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = mgr.Stop(context.Background())
		server.Close()
	})
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket session")
	}
	s := mgr.session(provider)
	if s == nil {
		t.Fatal("connected websocket session is missing")
	}
	return mgr, s, client
}

func receiveMessage(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if !ok {
			t.Fatal("message channel closed early")
		}
		return msg
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
		return Message{}
	}
}

func receiveUntilClosed(t *testing.T, ch <-chan Message) []Message {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	var messages []Message
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return messages
			}
			messages = append(messages, msg)
		case <-timer.C:
			t.Fatal("timed out waiting for message channel to close")
			return nil
		}
	}
}

func requireChannelClosed(t *testing.T, ch <-chan Message) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("message channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message channel to close")
	}
}

func assertCleanupError(t *testing.T, ch <-chan Message, cause error) {
	t.Helper()
	messages := receiveUntilClosed(t, ch)
	if len(messages) != 1 {
		t.Fatalf("cleanup delivered %d messages, want 1", len(messages))
	}
	msg := messages[0]
	if msg.Type != MessageTypeError {
		t.Fatalf("cleanup message type = %q, want %q", msg.Type, MessageTypeError)
	}
	if got, _ := msg.Payload["error"].(string); got != cause.Error() {
		t.Fatalf("cleanup error = %q, want %q", got, cause.Error())
	}
}

func pendingRequestCount(s *session) int {
	count := 0
	s.pending.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
