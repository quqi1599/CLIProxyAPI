package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestCodexCompletedClientCloseKeepsFinalSuccess(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	const model = "gpt-6.1-sol"
	for _, format := range []string{"openai-response", "openai", "claude"} {
		t.Run(format, func(t *testing.T) {
			rid := "completed-close-" + format
			const completed = `data: {"type":"response.completed","response":{"id":"resp_terminal_fixture","object":"response","status":"completed","model":"gpt-6.1-sol","output":[{"id":"msg_terminal_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}],"usage":{"input_tokens":17,"output_tokens":3,"total_tokens":20}}}` + "\n\n"
			body := &codexBlockingBody{closed: make(chan struct{}), prefix: []byte(completed)}
			ctx, cancel := context.WithCancel(logging.WithRequestID(t.Context(), rid))
			defer cancel()
			ctx = cliproxyauth.WithStreamSummaryTracking(ctx)
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", codexRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
			}))
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
			cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewCodexExecutor(cfg))
			auth := &cliproxyauth.Auth{ID: rid, Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"api_key": "fixture"}}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if _, err := manager.Register(ctx, auth); err != nil {
				t.Fatal(err)
			}
			result, err := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"gpt-6.1-sol","input":"OK","stream":true}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FromString(format), Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			defer result.Close()
			done := make(chan error, 1)
			go func() {
				seen := false
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						done <- chunk.Err
						return
					}
					payload := string(chunk.Payload)
					if strings.Contains(payload, `"type":"response.completed"`) || strings.Contains(payload, `"finish_reason":"stop"`) || strings.Contains(payload, `"type":"message_stop"`) {
						seen = true
						cancel()
					}
				}
				if !seen {
					done <- fmt.Errorf("terminal frame missing for %s", format)
					return
				}
				done <- nil
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				result.Close()
				t.Fatal("completed stream did not finish")
			}
			cliproxyauth.MarkStreamSummaryDownstreamDone(ctx)
			found := false
			for _, entry := range hook.AllEntries() {
				if entry.Data["event"] != "stream_execution_summary" || entry.Data["request_id"] != rid {
					continue
				}
				found = true
				if entry.Data["final_success"] != true || entry.Data["client_gone"] != false {
					t.Fatalf("completed client close changed outcome: %v", entry.Data)
				}
			}
			if !found {
				t.Fatal("final summary missing")
			}
		})
	}
}
