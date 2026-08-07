package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerGPTChannelGateDoesNotConsumeCredentialRetryBudget(t *testing.T) {
	const model = "gpt-5.5"

	tests := []struct {
		name   string
		invoke func(*testing.T, context.Context, *Manager) []byte
		calls  func(*authFallbackExecutor) []string
	}{
		{
			name: "execute",
			invoke: func(t *testing.T, ctx context.Context, manager *Manager) []byte {
				t.Helper()
				resp, errExecute := manager.executeMixedOnce(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}, 1)
				if errExecute != nil {
					t.Fatalf("executeMixedOnce() error = %v", errExecute)
				}
				return resp.Payload
			},
			calls: func(executor *authFallbackExecutor) []string {
				return executor.ExecuteCalls()
			},
		},
		{
			name: "count",
			invoke: func(t *testing.T, ctx context.Context, manager *Manager) []byte {
				t.Helper()
				resp, errExecute := manager.executeCountMixedOnce(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}, 1)
				if errExecute != nil {
					t.Fatalf("executeCountMixedOnce() error = %v", errExecute)
				}
				return resp.Payload
			},
			calls: func(executor *authFallbackExecutor) []string {
				return executor.CountCalls()
			},
		},
		{
			name: "stream",
			invoke: func(t *testing.T, ctx context.Context, manager *Manager) []byte {
				t.Helper()
				result, errExecute := manager.executeStreamMixedOnce(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}, 1)
				if errExecute != nil {
					t.Fatalf("executeStreamMixedOnce() error = %v", errExecute)
				}
				var payload []byte
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error = %v", chunk.Err)
					}
					payload = append(payload, chunk.Payload...)
				}
				return payload
			},
			calls: func(executor *authFallbackExecutor) []string {
				return executor.StreamCalls()
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			manager, executor, healthyAuthID := newGPTChannelGateBudgetTestManager(t, model)
			ctx, _ := ensureRequestAttemptTrace(context.Background())

			payload := test.invoke(t, ctx, manager)
			if got := string(payload); got != healthyAuthID {
				t.Fatalf("payload = %q, want healthy auth %q", got, healthyAuthID)
			}
			calls := test.calls(executor)
			if len(calls) != 1 || calls[0] != healthyAuthID {
				t.Fatalf("executor calls = %v, want [%s]", calls, healthyAuthID)
			}
		})
	}
}

func newGPTChannelGateBudgetTestManager(t *testing.T, model string) (*Manager, *authFallbackExecutor, string) {
	t.Helper()

	prefix := uuid.NewString()
	busyA := gptChannelBreakerTestAuth(prefix+"-aa-busy", "https://gate-busy-a.example/v1")
	busyB := gptChannelBreakerTestAuth(prefix+"-ab-busy", "https://gate-busy-b.example/v1")
	healthy := gptChannelBreakerTestAuth(prefix+"-ac-healthy", "https://gate-healthy.example/v1")

	selector := &SequentialFillSelector{current: map[string]string{
		"codex:" + model: busyA.ID,
	}}
	manager := NewManager(nil, selector, nil)
	executor := &authFallbackExecutor{id: "codex"}
	manager.RegisterExecutor(executor)

	auths := []*Auth{busyA, busyB, healthy}
	modelRegistry := registry.GetGlobalRegistry()
	for _, auth := range auths {
		modelRegistry.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %s: %v", auth.ID, errRegister)
		}
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			modelRegistry.UnregisterClient(auth.ID)
		}
	})

	now := time.Now()
	manager.mu.Lock()
	for _, auth := range auths[:2] {
		manager.gptChannelBreakers[gptChannelBreakerKey(auth, model)] = &codexChannelBreakerState{
			Health:          HealthState{BreakerState: HealthBreakerHalfOpen},
			ProbeRequestID:  auth.ID + "-other-request",
			ProbeLeaseUntil: now.Add(time.Minute),
		}
	}
	manager.mu.Unlock()

	return manager, executor, healthy.ID
}
