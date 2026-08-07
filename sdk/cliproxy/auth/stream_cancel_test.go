package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type cancellableStreamExecutor struct {
	cancelled chan struct{}
	calls     atomic.Int32
	once      sync.Once
}

func newCancellableStreamExecutor() *cancellableStreamExecutor {
	return &cancellableStreamExecutor{cancelled: make(chan struct{})}
}

func (e *cancellableStreamExecutor) Identifier() string { return "codex" }

func (e *cancellableStreamExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *cancellableStreamExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
	return &cliproxyexecutor.StreamResult{
		Chunks: ch,
		Cancel: func() {
			e.calls.Add(1)
			e.once.Do(func() { close(e.cancelled) })
		},
	}, nil
}

func (e *cancellableStreamExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *cancellableStreamExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *cancellableStreamExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerExecuteStreamCancelsUpstreamWhenContextEnds(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	executor := newCancellableStreamExecutor()
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "cancel-stream-auth",
		Provider: "codex",
		Status:   StatusActive,
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	ctx, cancel := context.WithCancel(context.Background())
	streamResult, errExecute := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "model"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute stream: %v", errExecute)
	}

	select {
	case chunk := <-streamResult.Chunks:
		if string(chunk.Payload) != "first" {
			t.Fatalf("first payload = %q, want first", string(chunk.Payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first chunk")
	}

	cancel()

	select {
	case <-executor.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected upstream stream cancel callback to run")
	}
	if calls := executor.calls.Load(); calls != 1 {
		t.Fatalf("cancel callback calls = %d, want 1", calls)
	}
}

func TestManagerExecuteStreamCancelReleasesSelectorAndHalfOpenProbe(t *testing.T) {
	const model = "gpt-5.5"

	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	manager := NewManager(nil, selector, nil)
	executor := newCancellableStreamExecutor()
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "cancel-half-open-auth",
		Provider: "codex",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":  "cancel-test-key",
			"base_url": "https://cancel-half-open.example/v1",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	channelKey := gptChannelBreakerKey(auth, model)
	manager.mu.Lock()
	if manager.gptChannelBreakers == nil {
		manager.gptChannelBreakers = make(map[string]*codexChannelBreakerState)
	}
	manager.gptChannelBreakers[channelKey] = &codexChannelBreakerState{
		Health: HealthState{
			Observed:     true,
			Score:        10,
			BreakerState: HealthBreakerOpen,
			OpenUntil:    time.Now().Add(-time.Second),
		},
	}
	manager.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	streamResult, errExecute := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		cancel()
		t.Fatalf("execute stream: %v", errExecute)
	}

	select {
	case chunk := <-streamResult.Chunks:
		if string(chunk.Payload) != "first" {
			cancel()
			t.Fatalf("first payload = %q, want first", string(chunk.Payload))
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for first chunk")
	}

	loadRecordKey := routingChannelBaseKey(auth)
	if inFlight := streamCancelSpreadInflight(selector, model, loadRecordKey); inFlight != 1 {
		cancel()
		t.Fatalf("selector inflight before cancel = %d, want 1", inFlight)
	}
	manager.mu.RLock()
	state := manager.gptChannelBreakers[channelKey]
	breakerState := state.Health.BreakerState
	probeRequestID := state.ProbeRequestID
	manager.mu.RUnlock()
	if breakerState != HealthBreakerHalfOpen || probeRequestID == "" {
		cancel()
		t.Fatalf("breaker before cancel = state:%q probe:%q, want half-open reserved probe", breakerState, probeRequestID)
	}

	cancel()
	select {
	case <-executor.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected upstream stream cancel callback to run")
	}
	select {
	case _, ok := <-streamResult.Chunks:
		if ok {
			t.Fatal("unexpected chunk after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for wrapped stream to close")
	}

	if inFlight := streamCancelSpreadInflight(selector, model, loadRecordKey); inFlight != 0 {
		t.Fatalf("selector inflight after cancel = %d, want 0", inFlight)
	}
	manager.mu.RLock()
	state = manager.gptChannelBreakers[channelKey]
	probeRequestID = state.ProbeRequestID
	probeLeaseUntil := state.ProbeLeaseUntil
	manager.mu.RUnlock()
	if probeRequestID != "" || !probeLeaseUntil.IsZero() {
		t.Fatalf("probe after cancel = request:%q lease:%v, want released", probeRequestID, probeLeaseUntil)
	}
}

func streamCancelSpreadInflight(selector *SpreadSelector, model, recordKey string) int {
	selector.mu.Lock()
	defer selector.mu.Unlock()
	key := "mixed:" + canonicalModelKey(model)
	snapshot := selector.load.snapshot(key, []string{recordKey}, time.Now(), spreadLoadDefaultKeyLimit)
	return snapshot[recordKey].inFlight
}
