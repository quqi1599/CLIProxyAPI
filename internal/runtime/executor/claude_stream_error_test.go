package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestClaudeExecutorStreamErrorIsFailureAcrossProtocols(t *testing.T) {
	t.Run("network", func(t *testing.T) {
		testClaudeStreamFailure(t, "1234", "1234", http.StatusBadGateway, failurecontract.UpstreamProtocolError)
	})
	t.Run("safety", func(t *testing.T) {
		testClaudeStreamFailure(t, "1301", "content_policy_violation", http.StatusBadRequest, failurecontract.ContentSafetyBlocked)
	})
}

func testClaudeStreamFailure(t *testing.T, providerCode, semanticCode string, status int, kind failurecontract.Kind) {
	t.Helper()
	const start = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"glm-5.3-flash\",\"content\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n"
	failureEvent := fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":%q,\"message\":\"private-upstream-content\"}}\n\n", providerCode)
	const reportedUsage = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"input_tokens\":17,\"output_tokens\":3}}\n\n"
	const stop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, format := range []string{"claude", "openai", "openai-response"} {
		for _, prefix := range []string{"", start, start + reportedUsage} {
			t.Run(fmt.Sprintf("%s/started=%t/usage=%t", format, prefix != "", strings.Contains(prefix, "message_delta")), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, prefix+failureEvent+stop)
				}))
				defer server.Close()
				payload := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"OK"}]}`)
				if format == "openai-response" {
					payload = []byte(`{"model":"glm-5.3-flash","stream":true,"input":"OK"}`)
				}
				manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
				manager.SetRetryConfig(3, time.Second, 5)
				manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
				for _, id := range []string{"glm-stream-error-a", "glm-stream-error-b"} {
					registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "glm-5.3-flash"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					_, err := manager.Register(context.Background(), &cliproxyauth.Auth{ID: id, Provider: "claude",
						Attributes: map[string]string{"base_url": server.URL, "api_key": "fixture", "compat_kind": "zhipu"}})
					if err != nil {
						t.Fatal(err)
					}
				}
				plugin := &captureAIStudioUsagePlugin{records: make(chan usage.Record, 16)}
				usage.RegisterNamedPlugin("claude-stream-error-test", plugin)
				requestID := fmt.Sprintf("glm-stream-error-%s-%s-%d", providerCode, format, len(prefix))
				ctx := logging.WithRequestID(context.Background(), requestID)
				result, err := manager.ExecuteStream(ctx, []string{"claude"},
					cliproxyexecutor.Request{Model: "glm-5.3-flash", Payload: payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format), OriginalRequest: payload, Stream: true})
				var output strings.Builder
				var failures []error
				if err != nil {
					failures = append(failures, err)
				} else {
					if result.Cancel != nil {
						defer result.Cancel()
					}
					for chunk := range result.Chunks {
						output.Write(chunk.Payload)
						if chunk.Err != nil {
							failures = append(failures, chunk.Err)
						}
					}
				}
				if len(failures) != 1 {
					t.Fatalf("terminal errors = %d, want exactly one", len(failures))
				}
				failure, ok := failurecontract.As(failures[0])
				if !ok || failure.HTTPStatus != status || failure.OuterStatus != 200 || failure.Retryable || failure.ErrorCode() != semanticCode || failure.ProviderCode != providerCode || failure.Kind != kind || failure.Scope != failurecontract.ScopeRequest {
					t.Fatalf("failure = %#v, want non-retryable request-scoped %s", failure, semanticCode)
				}
				for _, forbidden := range []string{"private-upstream-content", "message_stop", "response.completed", "[DONE]"} {
					if strings.Contains(output.String(), forbidden) || strings.Contains(failures[0].Error(), forbidden) {
						t.Fatalf("error produced private content or success marker %q", forbidden)
					}
				}
				if calls.Load() != 1 {
					t.Fatalf("upstream calls = %d, want 1", calls.Load())
				}
				for _, auth := range manager.List() {
					if auth.Success != 0 {
						t.Fatalf("failed stream restored success statistics: %d", auth.Success)
					}
				}
				deadline := time.NewTimer(2 * time.Second)
				defer deadline.Stop()
				for {
					select {
					case record := <-plugin.records:
						if record.RequestID != requestID {
							continue
						}
						if !record.Failed || record.Fail.SemanticCode != semanticCode {
							t.Fatalf("usage outcome = %+v, want failed %s", record.Fail, semanticCode)
						}
						if strings.Contains(prefix, "message_delta") && (record.Detail.InputTokens != 17 || record.Detail.OutputTokens != 3) {
							t.Fatalf("reported partial usage lost: %+v", record.Detail)
						}
						return
					case <-deadline.C:
						t.Fatal("failed stream usage record missing")
					}
				}
			})
		}
	}
}

func TestClaudeStreamCompletedBeforeClientCloseKeepsSuccessfulUsage(t *testing.T) {
	const complete = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"glm-5.3-flash\",\"content\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":17,\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, complete)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	plugin := &captureAIStudioUsagePlugin{records: make(chan usage.Record, 16)}
	usage.RegisterNamedPlugin("claude-completed-close-test", plugin)
	ctx := logging.WithRequestID(t.Context(), "claude-completed-close-fixture")
	payload := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"OK"}]}`)
	auth := &cliproxyauth.Auth{ID: "completed-close-fixture", Provider: "claude", Attributes: map[string]string{"base_url": server.URL, "api_key": "fixture", "compat_kind": "zhipu"}}
	result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "glm-5.3-flash", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Cancel()
	for chunk := range result.Chunks {
		if strings.Contains(string(chunk.Payload), "message_stop") {
			result.Cancel()
		}
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case record := <-plugin.records:
			if record.RequestID != "claude-completed-close-fixture" {
				continue
			}
			if record.Failed || record.Detail.InputTokens != 17 || record.Detail.OutputTokens != 3 {
				t.Fatalf("completed stream recorded as failed or lost usage: failed=%t detail=%+v", record.Failed, record.Detail)
			}
			return
		case <-deadline.C:
			t.Fatal("completed stream usage record missing")
		}
	}
}
