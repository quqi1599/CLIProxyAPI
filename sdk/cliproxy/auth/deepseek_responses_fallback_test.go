package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestDeepSeekResponsesFallbackSkipsSameEndpointCredentials(t *testing.T) {
	for _, marker := range []string{"deepseek_responses_route_unsupported", "deepseek_responses_unsupported_tools"} {
		for _, stream := range []bool{false, true} {
			name := marker + "/execute"
			if stream {
				name = marker + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				m.SetRetryConfig(0, 0, 5)
				failure := &Error{HTTPStatus: http.StatusBadRequest, Code: "request_feature_unsupported", Message: "request_feature_unsupported: " + marker}
				exec := &authFallbackExecutor{id: "claude", executeErrors: map[string]error{"aa-primary": failure}, streamFirstErrors: map[string]error{"aa-primary": failure}}
				m.RegisterExecutor(exec)
				model := "deepseek-flash"
				auths := []*Auth{
					{ID: "aa-primary", Provider: "claude", Attributes: map[string]string{"base_url": "https://unsupported.example/v1", "priority": "10"}},
					{ID: "ab-same-endpoint", Provider: "claude", Attributes: map[string]string{"base_url": "https://unsupported.example/v1", "priority": "10"}},
					{ID: "ba-compatible", Provider: "claude", Attributes: map[string]string{"base_url": "https://compatible.example/v1", "priority": "0"}},
				}
				reg := registry.GetGlobalRegistry()
				for _, a := range auths {
					reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
					if _, err := m.Register(context.Background(), a); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					for _, a := range auths {
						reg.UnregisterClient(a.ID)
					}
				})
				req := cliproxyexecutor.Request{Model: model}
				var calls []string
				if stream {
					r, err := m.ExecuteStream(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range r.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
					calls = exec.StreamCalls()
				} else {
					_, err := m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
					if err != nil {
						t.Fatal(err)
					}
					calls = exec.ExecuteCalls()
				}
				if !reflect.DeepEqual(calls, []string{"aa-primary", "ba-compatible"}) {
					t.Fatalf("calls=%v", calls)
				}
			})
		}
	}
}

func TestDeepSeekResponsesFallbackDoesNotRelaxOtherBadRequests(t *testing.T) {
	for _, message := range []string{"invalid_request_error: invalid tool result", "request_feature_unsupported: deepseek_responses_state", "invalid_request_error: missing reasoning_content"} {
		if isDeepSeekCompatibilityFallbackError(&Error{HTTPStatus: 400, Message: message}) {
			t.Fatal(message)
		}
	}
}
