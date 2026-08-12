package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type opaqueUpstream500Executor struct {
	executeCalls atomic.Int32
	streamCalls  atomic.Int32
}

func (executor *opaqueUpstream500Executor) Identifier() string { return "opaque-upstream-500" }

func (executor *opaqueUpstream500Executor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if executor.executeCalls.Add(1) == 1 {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream request failed"}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (executor *opaqueUpstream500Executor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if executor.streamCalls.Add(1) == 1 {
		return nil, &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream request failed"}
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (executor *opaqueUpstream500Executor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (executor *opaqueUpstream500Executor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (executor *opaqueUpstream500Executor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerRetriesOpaqueUpstream500BeforeOutput(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(context.Context, *Manager, cliproxyexecutor.Request) error
		calls  func(*opaqueUpstream500Executor) int32
	}{
		{
			name: "execute",
			invoke: func(ctx context.Context, manager *Manager, request cliproxyexecutor.Request) error {
				response, err := manager.Execute(ctx, []string{"opaque-upstream-500"}, request, cliproxyexecutor.Options{})
				if err == nil && string(response.Payload) != "ok" {
					t.Fatalf("response payload = %q, want ok", response.Payload)
				}
				return err
			},
			calls: func(executor *opaqueUpstream500Executor) int32 { return executor.executeCalls.Load() },
		},
		{
			name: "stream bootstrap",
			invoke: func(ctx context.Context, manager *Manager, request cliproxyexecutor.Request) error {
				stream, err := manager.ExecuteStream(ctx, []string{"opaque-upstream-500"}, request, cliproxyexecutor.Options{})
				if err != nil {
					return err
				}
				chunk, ok := <-stream.Chunks
				if !ok || chunk.Err != nil || string(chunk.Payload) != "ok" {
					t.Fatalf("stream chunk = %+v, open=%t, want ok payload", chunk, ok)
				}
				return nil
			},
			calls: func(executor *opaqueUpstream500Executor) int32 { return executor.streamCalls.Load() },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const model = "claude-sonnet-4-6"
			const authID = "opaque-upstream-500-auth"
			executor := &opaqueUpstream500Executor{}
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(1, time.Second, 0)
			manager.SetRetryQueueDelay(0)
			manager.RegisterExecutor(executor)

			modelRegistry := registry.GetGlobalRegistry()
			modelRegistry.RegisterClient(authID, executor.Identifier(), []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: executor.Identifier()}); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if errInvoke := test.invoke(ctx, manager, cliproxyexecutor.Request{Model: model}); errInvoke != nil {
				t.Fatalf("operation failed after retry: %v", errInvoke)
			}
			if got := test.calls(executor); got != 2 {
				t.Fatalf("executor calls = %d, want 2", got)
			}
		})
	}
}

func TestNormalizeOpaqueUpstream500FailureBoundaries(t *testing.T) {
	raw := &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream request failed"}
	beforeOutput := normalizeOpaqueUpstream500Failure(raw, failurecontract.StreamPhaseBeforeOutput, false)
	failure, ok := failurecontract.As(beforeOutput)
	if !ok || failure == nil {
		t.Fatalf("before-output error = %T, want canonical failure", beforeOutput)
	}
	if failure.Scope != failurecontract.ScopeProvider || !failure.Retryable || failure.OutputCommitted {
		t.Fatalf("before-output failure = %+v, want retryable provider failure", failure)
	}

	afterOutput := normalizeOpaqueUpstream500Failure(raw, failurecontract.StreamPhaseAfterOutput, true)
	failure, ok = failurecontract.As(afterOutput)
	if !ok || failure == nil || failure.Retryable || !failure.OutputCommitted {
		t.Fatalf("after-output failure = %+v, want committed non-retryable failure", failure)
	}

	coded := &Error{Code: "content_policy_violation", HTTPStatus: http.StatusInternalServerError, Message: "blocked"}
	if got := normalizeOpaqueUpstream500Failure(coded, failurecontract.StreamPhaseBeforeOutput, false); got != coded {
		t.Fatalf("coded upstream 500 was rewritten: %v", got)
	}
}
