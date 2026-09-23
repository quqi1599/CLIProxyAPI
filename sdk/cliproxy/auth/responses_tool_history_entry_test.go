package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNativeResponsesPluginDelegationPreservesFilteredCandidates(t *testing.T) {
	for _, delegate := range []string{pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst} {
		for _, mixed := range []bool{false, true} {
			name := delegate + "/single"
			if mixed {
				name = delegate + "/mixed"
			}
			t.Run(name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.RegisterExecutor(&authFallbackExecutor{id: "codex"})
				unknownID, readyID := t.Name()+"-unknown", t.Name()+"-ready"
				const model = "gpt-5.6-sol"
				registerGPTChannelFailoverAuths(t, manager, "codex", model, []*Auth{
					{ID: unknownID, Provider: "codex", Attributes: map[string]string{"base_url": "https://unknown.example/v1", "priority": "100"}},
					{ID: readyID, Provider: "codex", Attributes: map[string]string{"base_url": "https://verified.example/v1", "native_responses": "true", "priority": "1"}},
				})
				plugin := &fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: delegate}}
				manager.SetPluginScheduler(plugin)
				tried := map[string]struct{}{"previous-attempt": {}}
				var selected *Auth
				var err error
				if mixed {
					selected, _, _, err = manager.pickNextMixed(context.Background(), []string{"codex"}, model, longResponsesToolOptions(), tried)
				} else {
					selected, _, err = manager.pickNext(context.Background(), "codex", model, longResponsesToolOptions(), tried)
				}
				if err != nil || selected == nil || selected.ID != readyID {
					t.Fatalf("selected=%v err=%v, want verified lower-priority candidate", selected, err)
				}
				if len(plugin.requests) != 1 || len(plugin.requests[0].Candidates) != 1 || plugin.requests[0].Candidates[0].ID != readyID {
					t.Fatalf("plugin saw unexpected prefiltered candidates: %#v", plugin.requests)
				}
				if len(tried) != 1 {
					t.Fatalf("delegation mutated caller attempt history: %v", tried)
				}
			})
		}
	}
}

func TestNativeResponsesManagerCountKeepsLocalCountingAvailable(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 5)
	executor := &authFallbackExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	const model = "gpt-5.6-sol"
	id := t.Name() + "-unknown"
	registerGPTChannelFailoverAuths(t, manager, "codex", model, []*Auth{
		{ID: id, Provider: "codex", Attributes: map[string]string{"base_url": "https://unknown.example/v1"}},
	})
	_, err := manager.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, longResponsesToolOptions())
	if err != nil {
		t.Fatalf("local token count incorrectly requires upstream capability: %v", err)
	}
	if calls := executor.CountCalls(); len(calls) != 1 || calls[0] != id {
		t.Fatalf("count calls=%v, want one local count", calls)
	}
	if len(executor.ExecuteCalls()) != 0 || len(executor.StreamCalls()) != 0 {
		t.Fatal("token count dispatched a generation")
	}
}

func TestNativeResponsesSDKUsesRequestPayloadAndIgnoresCountFlag(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, countFlag := range []bool{false, true} {
			name := "execute"
			if stream {
				name = "stream"
			}
			if countFlag {
				name += "/caller-token-count"
			}
			t.Run(name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetRetryConfig(0, 0, 5)
				executor := &authFallbackExecutor{id: "codex"}
				manager.RegisterExecutor(executor)
				const model = "gpt-5.6-sol"
				unknownID, readyID := t.Name()+"-unknown", t.Name()+"-ready"
				registerGPTChannelFailoverAuths(t, manager, "codex", model, []*Auth{
					{ID: unknownID, Provider: "codex", Attributes: map[string]string{"base_url": "https://unknown.example/v1", "priority": "100"}},
					{ID: readyID, Provider: "codex", Attributes: map[string]string{"base_url": "https://verified.example/v1", "native_responses": "true", "priority": "1"}},
				})
				input := make([]map[string]string, 272)
				for i := range input {
					input[i] = map[string]string{"type": "message", "role": "user", "content": "test"}
				}
				for i := 0; i < 12; i++ {
					input[i] = map[string]string{"type": "function_call", "name": "test", "arguments": "{}"}
				}
				body, err := json.Marshal(map[string]any{"model": model, "input": input})
				if err != nil {
					t.Fatal(err)
				}
				opts := cliproxyexecutor.Options{TokenCount: countFlag, Metadata: map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}}
				req := cliproxyexecutor.Request{Model: model, Payload: body}
				var calls []string
				if stream {
					result, err := manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
					calls = executor.StreamCalls()
				} else {
					if _, err := manager.Execute(context.Background(), []string{"codex"}, req, opts); err != nil {
						t.Fatal(err)
					}
					calls = executor.ExecuteCalls()
				}
				if len(calls) != 1 || calls[0] != readyID {
					t.Fatalf("dispatch=%v, want only verified route even without OriginalRequest/shape counters", calls)
				}
			})
		}
	}
}

func TestNativeResponsesHomePinnedWebsocketCapabilityChecked(t *testing.T) {
	for _, declared := range []bool{false, true} {
		name := "unknown"
		if declared {
			name = "verified"
		}
		t.Run(name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			manager.RegisterExecutor(&authFallbackExecutor{id: "codex"})
			auth := &Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": "https://custom.example/v1", "websockets": "true"}}
			if declared {
				auth.Attributes["native_responses"] = "true"
			}
			manager.rememberHomeRuntimeAuth(t.Name(), auth)
			opts := longResponsesToolOptions()
			opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = t.Name()
			opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = auth.ID
			ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
			selected, _, _, err := manager.pickNextMixed(ctx, []string{"codex"}, "gpt-5.6-sol", opts, nil)
			if declared {
				if err != nil || selected == nil || selected.ID != auth.ID {
					t.Fatalf("verified home selection=%v err=%v", selected, err)
				}
			} else if statusCodeFromError(err) != http.StatusBadRequest || errorCodeFromError(err) != "request_feature_unsupported" || selected != nil {
				t.Fatalf("unknown home selection=%v err=%v, want deterministic capability rejection", selected, err)
			}
		})
	}
}
