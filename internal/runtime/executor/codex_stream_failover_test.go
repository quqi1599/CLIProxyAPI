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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexFastAPIStreamFailureFailoverBoundary(t *testing.T) {
	const model = "gpt-6-astra"
	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "before_output"},
		{name: "after_text", output: `{"type":"response.output_text.delta","item_id":"msg_partial","output_index":0,"content_index":0,"delta":"partial text"}`},
		{name: "after_tool", output: `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_partial","type":"function_call","call_id":"call_partial","name":"write_file","arguments":""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var primaryCalls, backupCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bad\",\"model\":\"gpt-6-astra\"}}\n\n")
				if tc.output != "" {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.output)
					if tc.name == "after_tool" {
						_, _ = fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_partial\",\"output_index\":0,\"delta\":\"{}\"}\n\n")
					}
				}
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_bad\",\"status\":\"failed\",\"error\":{\"type\":\"fastapi_error\",\"message\":\"upstream generation failed\"}}}\n\n")
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				backupCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_backup\",\"output_index\":0,\"content_index\":0,\"delta\":\"backup answer\"}\n\n")
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_backup\",\"model\":\"gpt-6-astra\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_backup\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"backup answer\"}]}]}}\n\n")
			}))
			defer backup.Close()

			manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
			manager.SetRetryConfig(3, time.Second, 3)
			manager.RegisterExecutor(NewCodexExecutor(&config.Config{}))
			primaryID := "fastapi-primary-" + tc.name
			for i, route := range []struct{ id, url string }{{primaryID, primary.URL}, {"fastapi-backup-" + tc.name, backup.URL}} {
				auth := &cliproxyauth.Auth{
					ID: route.id, Provider: "codex", Status: cliproxyauth.StatusActive,
					Attributes: map[string]string{"base_url": route.url, "api_key": "test", "priority": fmt.Sprint(10 - i)},
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, errExecute := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{
				Model: model, Payload: []byte(`{"model":"gpt-6-astra","input":"hello"}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
			if errExecute != nil {
				t.Fatalf("ExecuteStream: %v", errExecute)
			}
			defer result.Close()
			var output strings.Builder
			var terminalErr error
			for chunk := range result.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					terminalErr = chunk.Err
				}
			}
			if primaryCalls.Load() != 1 {
				t.Fatalf("primary calls = %d, want 1", primaryCalls.Load())
			}
			if tc.output == "" {
				if terminalErr != nil || backupCalls.Load() != 1 || !strings.Contains(output.String(), "backup answer") || strings.Contains(output.String(), "resp_bad") {
					t.Fatalf("pre-output failover: backup=%d error=%v output=%s", backupCalls.Load(), terminalErr, output.String())
				}
			} else {
				failure := failurecontract.Classify(terminalErr)
				if failure == nil || !failure.OutputCommitted || failure.Retryable || failure.SemanticType != "fastapi_error" || failure.HTTPStatus != http.StatusBadGateway {
					t.Fatalf("terminal failure = %+v", failure)
				}
				if backupCalls.Load() != 0 || !strings.Contains(output.String(), "partial") || strings.Contains(output.String(), "backup answer") {
					t.Fatalf("committed stream was replayed or lost: backup=%d output=%s", backupCalls.Load(), output.String())
				}
			}
			failedAuth, _ := manager.GetByID(primaryID)
			state := failedAuth.ModelStates[model]
			if state == nil || !state.Health.Observed || state.Health.ConsecutiveFailures != 1 {
				t.Fatalf("upstream failure missing from route health: %+v", state)
			}
		})
	}
}
