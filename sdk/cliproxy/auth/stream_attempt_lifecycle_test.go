package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const streamLifecycleTestModel = "stream-lifecycle-model"

type streamLifecycleExecutor struct {
	executeStream func(context.Context) (*cliproxyexecutor.StreamResult, error)
}

func (e *streamLifecycleExecutor) Identifier() string { return "codex" }

func (e *streamLifecycleExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *streamLifecycleExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return e.executeStream(ctx)
}

func (e *streamLifecycleExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *streamLifecycleExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *streamLifecycleExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func executeStreamLifecycleAttempt(t *testing.T, manager *Manager, ctx context.Context, executor ProviderExecutor) (*cliproxyexecutor.StreamResult, error) {
	t.Helper()
	return manager.executeStreamWithModelPool(
		ctx,
		executor,
		&Auth{ID: "stream-lifecycle-auth", Provider: "codex", Status: StatusActive},
		"codex",
		cliproxyexecutor.Request{Model: streamLifecycleTestModel},
		cliproxyexecutor.Options{},
		streamLifecycleTestModel,
		[]string{streamLifecycleTestModel},
		false,
	)
}

func streamLifecycleModelLoad(manager *Manager) int {
	key := codexModelLoadKey("codex", streamLifecycleTestModel)
	manager.codexModelLoadMu.Lock()
	defer manager.codexModelLoadMu.Unlock()
	return manager.codexModelLoads[key]
}

func waitForStreamLifecycleSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func drainStreamLifecycleResult(t *testing.T, result *cliproxyexecutor.StreamResult) []cliproxyexecutor.StreamChunk {
	t.Helper()
	var chunks []cliproxyexecutor.StreamChunk
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				return chunks
			}
			chunks = append(chunks, chunk)
		case <-timer.C:
			t.Fatal("timed out waiting for stream to close")
		}
	}
}

func TestStreamAttemptLifecycleCloseCancelsNilCancelProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	providerContextDone := make(chan struct{})
	executor := &streamLifecycleExecutor{executeStream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
		go func() {
			<-ctx.Done()
			close(providerContextDone)
			close(chunks)
		}()
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}}

	result, errExecute := executeStreamLifecycleAttempt(t, manager, context.Background(), executor)
	if errExecute != nil {
		t.Fatalf("execute stream: %v", errExecute)
	}
	if load := streamLifecycleModelLoad(manager); load != 1 {
		t.Fatalf("model load before close = %d, want 1", load)
	}

	result.Close()
	result.Close()
	waitForStreamLifecycleSignal(t, providerContextDone, "provider attempt context was not canceled")
	drainStreamLifecycleResult(t, result)
	if load := streamLifecycleModelLoad(manager); load != 0 {
		t.Fatalf("model load after close = %d, want 0", load)
	}
}

func TestStreamAttemptLifecycleClientCancelClosesProviderOnce(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	providerContextDone := make(chan struct{})
	var closeCalls atomic.Int32
	executor := &streamLifecycleExecutor{executeStream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
		go func() {
			<-ctx.Done()
			close(providerContextDone)
		}()
		return &cliproxyexecutor.StreamResult{
			Chunks: chunks,
			Cancel: func() {
				closeCalls.Add(1)
			},
		}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	result, errExecute := executeStreamLifecycleAttempt(t, manager, ctx, executor)
	if errExecute != nil {
		t.Fatalf("execute stream: %v", errExecute)
	}
	cancel()
	drainStreamLifecycleResult(t, result)
	waitForStreamLifecycleSignal(t, providerContextDone, "provider attempt context was not canceled")
	result.Close()
	result.Close()
	if calls := closeCalls.Load(); calls != 1 {
		t.Fatalf("provider close calls = %d, want 1", calls)
	}
	if load := streamLifecycleModelLoad(manager); load != 0 {
		t.Fatalf("model load after client cancel = %d, want 0", load)
	}
}

func TestStreamAttemptLifecycleTerminalPathsCleanup(t *testing.T) {
	t.Run("execute error", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		providerContextDone := make(chan struct{})
		executeErr := errors.New("execute stream failed")
		var closeCalls atomic.Int32
		executor := &streamLifecycleExecutor{executeStream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
			go func() {
				<-ctx.Done()
				close(providerContextDone)
			}()
			return &cliproxyexecutor.StreamResult{Cancel: func() { closeCalls.Add(1) }}, executeErr
		}}

		result, errExecute := executeStreamLifecycleAttempt(t, manager, context.Background(), executor)
		if result != nil {
			t.Fatal("failed execution unexpectedly returned a result")
		}
		if !errors.Is(errExecute, executeErr) {
			t.Fatalf("execute error = %v, want %v", errExecute, executeErr)
		}
		waitForStreamLifecycleSignal(t, providerContextDone, "failed execution context was not canceled")
		if calls := closeCalls.Load(); calls != 1 {
			t.Fatalf("provider close calls = %d, want 1", calls)
		}
		if load := streamLifecycleModelLoad(manager); load != 0 {
			t.Fatalf("model load after execution error = %d, want 0", load)
		}
	})

	t.Run("completed", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		providerContextDone := make(chan struct{})
		var closeCalls atomic.Int32
		executor := &streamLifecycleExecutor{executeStream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("complete")}
			close(chunks)
			go func() {
				<-ctx.Done()
				close(providerContextDone)
			}()
			return &cliproxyexecutor.StreamResult{Chunks: chunks, Cancel: func() { closeCalls.Add(1) }}, nil
		}}

		result, errExecute := executeStreamLifecycleAttempt(t, manager, context.Background(), executor)
		if errExecute != nil {
			t.Fatalf("execute stream: %v", errExecute)
		}
		chunks := drainStreamLifecycleResult(t, result)
		if len(chunks) != 1 || string(chunks[0].Payload) != "complete" {
			t.Fatalf("completed chunks = %#v, want one complete payload", chunks)
		}
		waitForStreamLifecycleSignal(t, providerContextDone, "completed attempt context was not canceled")
		if calls := closeCalls.Load(); calls != 1 {
			t.Fatalf("provider close calls = %d, want 1", calls)
		}
		if load := streamLifecycleModelLoad(manager); load != 0 {
			t.Fatalf("model load after completion = %d, want 0", load)
		}
	})

	t.Run("error", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		providerContextDone := make(chan struct{})
		streamErr := errors.New("stream failed")
		var closeCalls atomic.Int32
		executor := &streamLifecycleExecutor{executeStream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 2)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
			chunks <- cliproxyexecutor.StreamChunk{Err: streamErr}
			close(chunks)
			go func() {
				<-ctx.Done()
				close(providerContextDone)
			}()
			return &cliproxyexecutor.StreamResult{Chunks: chunks, Cancel: func() { closeCalls.Add(1) }}, nil
		}}

		result, errExecute := executeStreamLifecycleAttempt(t, manager, context.Background(), executor)
		if errExecute != nil {
			t.Fatalf("execute stream: %v", errExecute)
		}
		chunks := drainStreamLifecycleResult(t, result)
		if len(chunks) != 2 || !errors.Is(chunks[1].Err, streamErr) {
			t.Fatalf("terminal chunks = %#v, want payload followed by stream error", chunks)
		}
		waitForStreamLifecycleSignal(t, providerContextDone, "failed attempt context was not canceled")
		if calls := closeCalls.Load(); calls != 1 {
			t.Fatalf("provider close calls = %d, want 1", calls)
		}
		if load := streamLifecycleModelLoad(manager); load != 0 {
			t.Fatalf("model load after stream error = %d, want 0", load)
		}
	})
}

func TestStreamAttemptLifecycleEmptyStreamClosesProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	providerContextDone := make(chan struct{})
	var closeCalls atomic.Int32
	executor := &streamLifecycleExecutor{executeStream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
		chunks := make(chan cliproxyexecutor.StreamChunk)
		close(chunks)
		go func() {
			<-ctx.Done()
			close(providerContextDone)
		}()
		return &cliproxyexecutor.StreamResult{Chunks: chunks, Cancel: func() { closeCalls.Add(1) }}, nil
	}}

	result, errExecute := executeStreamLifecycleAttempt(t, manager, context.Background(), executor)
	if result != nil {
		t.Fatal("empty stream unexpectedly returned a result")
	}
	if errExecute == nil || !strings.Contains(errExecute.Error(), "empty_stream") {
		t.Fatalf("empty stream error = %v, want empty_stream", errExecute)
	}
	waitForStreamLifecycleSignal(t, providerContextDone, "empty attempt context was not canceled")
	if calls := closeCalls.Load(); calls != 1 {
		t.Fatalf("provider close calls = %d, want 1", calls)
	}
	if load := streamLifecycleModelLoad(manager); load != 0 {
		t.Fatalf("model load after empty stream = %d, want 0", load)
	}
}

func TestStreamAttemptLifecycleConcurrentCloseReleasesEachSlotOnce(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	closeCalls := [2]atomic.Int32{}
	var attempts atomic.Int32
	executor := &streamLifecycleExecutor{executeStream: func(context.Context) (*cliproxyexecutor.StreamResult, error) {
		attempt := int(attempts.Add(1)) - 1
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
		return &cliproxyexecutor.StreamResult{
			Chunks: chunks,
			Cancel: func() {
				closeCalls[attempt].Add(1)
			},
		}, nil
	}}

	first, errFirst := executeStreamLifecycleAttempt(t, manager, context.Background(), executor)
	if errFirst != nil {
		t.Fatalf("execute first stream: %v", errFirst)
	}
	second, errSecond := executeStreamLifecycleAttempt(t, manager, context.Background(), executor)
	if errSecond != nil {
		t.Fatalf("execute second stream: %v", errSecond)
	}
	if load := streamLifecycleModelLoad(manager); load != 2 {
		t.Fatalf("model load before closes = %d, want 2", load)
	}

	done := make(chan struct{})
	go func() {
		first.Close()
		first.Close()
		close(done)
	}()
	first.Close()
	waitForStreamLifecycleSignal(t, done, "concurrent first-stream closes did not return")
	drainStreamLifecycleResult(t, first)
	if calls := closeCalls[0].Load(); calls != 1 {
		t.Fatalf("first provider close calls = %d, want 1", calls)
	}
	if load := streamLifecycleModelLoad(manager); load != 1 {
		t.Fatalf("model load after first close = %d, want 1", load)
	}

	second.Close()
	second.Close()
	drainStreamLifecycleResult(t, second)
	if calls := closeCalls[1].Load(); calls != 1 {
		t.Fatalf("second provider close calls = %d, want 1", calls)
	}
	if load := streamLifecycleModelLoad(manager); load != 0 {
		t.Fatalf("model load after both closes = %d, want 0", load)
	}
}
