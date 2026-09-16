package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestContextLimitFallbackSkipsDuplicateCapacity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, larger := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/larger=%v", stream, larger), func(t *testing.T) {
				m := NewManager(nil, &RoundRobinSelector{}, nil)
				m.SetRetryConfig(10, 0, 5)
				exec := &authFallbackExecutor{id: "claude", executeErrors: map[string]error{}, streamFirstErrors: map[string]error{}}
				m.RegisterExecutor(exec)
				const model = "claude-sonnet-4-6"
				reg := registry.GetGlobalRegistry()
				for i := 0; i < 32; i++ {
					if i == 31 && !larger {
						continue
					}
					id := fmt.Sprintf("capacity-%02d", i)
					group := "a"
					if i >= 17 {
						group = "b"
					}
					if i == 31 {
						group = "c"
					}
					a := &Auth{ID: id, Provider: "claude", Attributes: map[string]string{"base_url": "https://" + group + ".example/v1"}}
					reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { reg.UnregisterClient(id) })
					if _, err := m.Register(context.Background(), a); err != nil {
						t.Fatal(err)
					}
					if i < 31 {
						err := &Error{HTTPStatus: http.StatusBadRequest, Message: requestScopedContextLimitMessage}
						exec.executeErrors[id] = err
						exec.streamFirstErrors[id] = err
					}
				}
				var err error
				var calls []string
				if stream {
					var result *cliproxyexecutor.StreamResult
					result, err = m.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					}
					calls = exec.StreamCalls()
				} else {
					_, err = m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
					calls = exec.ExecuteCalls()
				}
				want := 2
				if larger {
					want = 3
				}
				if (err == nil) != larger || len(calls) != want {
					t.Fatalf("calls=%v err=%v, want %d distinct routes, success=%v", calls, err, want, larger)
				}
			})
		}
	}
}

func TestContextLimitFallbackHasHardAttemptBound(t *testing.T) {
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	err := &Error{HTTPStatus: 400, Message: requestScopedContextLimitMessage}
	for i := 0; i < contextLimitFallbackMaxAttempts; i++ {
		trace.nextAttempt("")
	}
	if !credentialRetryLimitReached(ctx, 1, 50, "claude-sonnet-4-6", cliproxyexecutor.Options{}, err) {
		t.Fatal("context fallback exceeded its request-wide attempt limit")
	}
}

func TestTotalAttemptBudgetSpansOuterRounds(t *testing.T) {
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.configureTotalAttemptBudget(10, 5)
	m := NewManager(nil, nil, nil)
	// Each round can otherwise reset its local set and try six credentials.
	calls := 0
	runner := managerAttemptRunner[cliproxyexecutor.Response]{manager: m,
		runOnce: func(ctx context.Context, _ []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, configured int) (cliproxyexecutor.Response, error) {
			for local := 0; !credentialRetryLimitReached(ctx, local, configured, req.Model, opts, nil); local++ {
				trace.nextAttempt("")
				calls++
			}
			return cliproxyexecutor.Response{}, &Error{HTTPStatus: 500, Retryable: true, Message: "upstream unavailable"}
		}}
	// Exercise two fresh round-local credential sets against the same trace.
	for i := 0; i < 2; i++ {
		_, _ = runner.runOnce(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: "MiniMax-M3"}, cliproxyexecutor.Options{}, 5)
	}
	if calls != 11 || !trace.totalAttemptBudgetExhausted() {
		t.Fatalf("calls=%d, want 11", calls)
	}
	trace.configureTotalAttemptBudget(0, 5)
	if trace.summary().MaxAttempts != 6 {
		t.Fatal("configured fallback pool was removed")
	}
	trace.configureTotalAttemptBudget(0, 0)
	if trace.totalAttemptBudgetExhausted() {
		t.Fatal("unbounded credential mode changed")
	}
}
