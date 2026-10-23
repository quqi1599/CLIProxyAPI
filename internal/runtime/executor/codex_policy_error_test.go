package executor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexCyberPolicyAcrossTransports(t *testing.T) {
	const model = "gpt-6.1-sol"
	for _, envelopeType := range []string{"invalid_request", "server_error"} {
		for _, tc := range []struct {
			name, transport string
			stream, output  bool
			outer           int
		}{
			{"http", "http", false, false, 400},
			{"http_stream", "http", true, false, 400},
			{"sse", "sse", true, false, 200},
			{"sse_after_output", "sse", true, true, 200},
			{"websocket", "ws", true, false, 101},
			{"websocket_after_output", "ws", true, true, 101},
		} {
			t.Run(envelopeType+"/"+tc.name, func(t *testing.T) {
				var primaryCalls, backupCalls atomic.Int32
				body := fmt.Sprintf(`{"error":{"code":"cyber_policy","type":%q,"message":"synthetic policy refusal"}}`, envelopeType)
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					if tc.transport == "http" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(body))
						return
					}
					events := []string{`{"type":"response.created","response":{"id":"resp_policy","model":"gpt-6.1-sol"}}`}
					if tc.output {
						events = append(events, `{"type":"response.output_text.delta","item_id":"msg_policy","output_index":0,"content_index":0,"delta":"fixture output"}`)
					}
					events = append(events, fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_policy","status":"failed","error":{"code":"cyber_policy","type":%q,"message":"synthetic policy refusal"}}}`, envelopeType))
					writeCodexLengthFixture(t, w, r, tc.transport, events)
				}))
				t.Cleanup(primary.Close)
				backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					backupCalls.Add(1)
					w.WriteHeader(http.StatusBadGateway)
				}))
				t.Cleanup(backup.Close)
				manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
				manager.SetRetryConfig(3, time.Millisecond, 3)
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
				var exec cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
				if tc.transport == "ws" {
					exec = NewCodexWebsocketsExecutor(cfg)
				}
				manager.RegisterExecutor(exec)
				for i, url := range []string{primary.URL, backup.URL} {
					auth := &cliproxyauth.Auth{ID: fmt.Sprintf("policy-%s-%s-%d", envelopeType, tc.name, i), Provider: "codex", Status: cliproxyauth.StatusActive,
						Attributes: map[string]string{"base_url": url, "api_key": "fixture", "priority": fmt.Sprint(10 - i)}}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					if _, err := manager.Register(t.Context(), auth); err != nil {
						t.Fatal(err)
					}
				}
				request := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"gpt-6.1-sol","input":"synthetic fixture"}`)}
				options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), ResponseFormat: sdktranslator.FromString("openai-response"), Stream: tc.stream}
				var terminalErr error
				if tc.stream {
					result, err := manager.ExecuteStream(t.Context(), []string{"codex"}, request, options)
					terminalErr = err
					if result != nil {
						defer result.Close()
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								terminalErr = chunk.Err
							}
						}
					}
				} else {
					_, terminalErr = manager.Execute(t.Context(), []string{"codex"}, request, options)
				}
				f := failurecontract.Classify(terminalErr)
				if f == nil || f.Kind != failurecontract.ContentSafetyBlocked || f.Scope != failurecontract.ScopeRequest || f.HTTPStatus != http.StatusBadRequest || f.OuterStatus != tc.outer || f.SemanticCode != "cyber_policy" || f.Retryable || f.OutputCommitted != tc.output {
					t.Fatalf("policy refusal misclassified: %+v", f)
				}
				if primaryCalls.Load() != 1 || backupCalls.Load() != 0 {
					t.Fatalf("policy refusal replayed: primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
				}
				for _, auth := range manager.List() {
					if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Health.ConsecutiveFailures != 0 {
						t.Fatalf("request refusal affected provider availability: id=%s unavailable=%t retry=%v health=%+v", auth.ID, auth.Unavailable, auth.NextRetryAfter, auth.Health)
					}
					for _, state := range auth.ModelStates {
						if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Health.FailureCount != 0 {
							t.Fatalf("request refusal affected model availability: %+v", state)
						}
					}
				}
			})
		}
	}
}

func TestCodexCyberPolicyRequiresStructuredIdentifier(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"server_error","code":"upstream_error","message":"cyber_policy"}}`,
		`{"error":{"type":"server_error","code":"cyber_policy_unknown","message":"fixture"}}`,
	} {
		f, _ := canonicalCodexFailure(codexFailureInput{outerStatus: 400, body: []byte(body)})
		if f.Kind != failurecontract.ProviderUnavailable || !f.Retryable {
			t.Fatalf("unrecognized error changed classification: %+v", f)
		}
	}
}
