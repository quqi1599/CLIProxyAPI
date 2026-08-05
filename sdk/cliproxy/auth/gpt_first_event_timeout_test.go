package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type firstEventTimeoutExecutor struct {
	id string

	mu    sync.Mutex
	calls []string
}

func (e *firstEventTimeoutExecutor) Identifier() string { return e.id }

func (e *firstEventTimeoutExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *firstEventTimeoutExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	e.mu.Unlock()

	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	switch auth.ID {
	case "aa-stalled-primary":
		go func() {
			<-ctx.Done()
			close(ch)
		}()
	case "aa-slow-tail":
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
		go func() {
			select {
			case <-time.After(60 * time.Millisecond):
				ch <- cliproxyexecutor.StreamChunk{Payload: []byte("second")}
				close(ch)
			case <-ctx.Done():
				close(ch)
			}
		}()
	default:
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("backup")}
		close(ch)
	}
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *firstEventTimeoutExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *firstEventTimeoutExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *firstEventTimeoutExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *firstEventTimeoutExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func TestGPTFirstEventTimeoutFailsOverToNextChannel(t *testing.T) {
	const (
		model    = "gpt-5.6-sol"
		provider = "gpt-first-event-timeout"
	)
	executor := &firstEventTimeoutExecutor{id: provider}
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(10, 30*time.Second, 10)
	manager.SetGPTFirstEventTimeout(20 * time.Millisecond)
	manager.RegisterExecutor(executor)
	auths := []*Auth{
		openAICompatChannelBreakerAuth("aa-stalled-primary", provider, "https://stalled.example/v1", 10),
		openAICompatChannelBreakerAuth("ba-healthy-backup", provider, "https://healthy.example/v1", 1),
	}
	registerGPTChannelFailoverAuths(t, manager, provider, model, auths)

	startedAt := time.Now()
	result, err := manager.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "backup" {
		t.Fatalf("payload = %q, want backup", payload)
	}
	if got, want := executor.Calls(), []string{"aa-stalled-primary", "ba-healthy-backup"}; !stringSlicesEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("failover took %v, want less than 500ms", elapsed)
	}
}

func TestGPTFirstEventTimeoutStopsAfterFirstDeliverableEvent(t *testing.T) {
	const (
		model    = "gpt-5.6-sol"
		provider = "gpt-first-event-slow-tail"
	)
	executor := &firstEventTimeoutExecutor{id: provider}
	manager := NewManager(nil, nil, nil)
	manager.SetGPTFirstEventTimeout(10 * time.Millisecond)
	manager.RegisterExecutor(executor)
	auth := openAICompatChannelBreakerAuth("aa-slow-tail", provider, "https://slow-tail.example/v1", 10)
	registerGPTChannelFailoverAuths(t, manager, provider, model, []*Auth{auth})

	result, err := manager.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "firstsecond" {
		t.Fatalf("payload = %q, want firstsecond", payload)
	}
}

func TestFirstEventTimeoutDisabledForNonGPTModels(t *testing.T) {
	const (
		model    = "claude-sonnet-4-8"
		provider = "non-gpt-first-event"
	)
	executor := &firstEventTimeoutExecutor{id: provider}
	manager := NewManager(nil, nil, nil)
	manager.SetGPTFirstEventTimeout(10 * time.Millisecond)
	manager.RegisterExecutor(executor)
	auth := openAICompatChannelBreakerAuth("aa-stalled-primary", provider, "https://non-gpt.example/v1", 10)
	registerGPTChannelFailoverAuths(t, manager, provider, model, []*Auth{auth})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := manager.ExecuteStream(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil || err != context.DeadlineExceeded {
		t.Fatalf("error = %v, want caller deadline", err)
	}
}
