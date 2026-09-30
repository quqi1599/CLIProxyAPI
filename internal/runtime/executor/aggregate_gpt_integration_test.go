package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAggregateSonnetToGPTManagerProtocolRoundTrip(t *testing.T) {
	const alias, upstream = "claude-sonnet-4-6", "gpt-6-luna"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			requests := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- body
				if r.URL.Path != "/responses" {
					t.Errorf("upstream path = %q, want /responses", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []string{
					`{"type":"response.created","response":{"id":"resp_aggregate","model":"gpt-6-luna"}}`,
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_next","call_id":"call_next","name":"lookup","arguments":""}}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_next","delta":"{\"city\":\"Shanghai\"}"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_next","call_id":"call_next","name":"lookup","arguments":"{\"city\":\"Shanghai\"}"}}`,
					`{"type":"response.completed","response":{"id":"resp_aggregate","model":"gpt-6-luna","status":"completed","output":[{"type":"function_call","id":"fc_next","call_id":"call_next","name":"lookup","arguments":"{\"city\":\"Shanghai\"}"}],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}}`,
				} {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
			}))
			defer server.Close()
			cfg := &config.Config{
				SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
				CodexKey:  []config.CodexKey{{APIKey: "test-key", BaseURL: server.URL, Models: []config.CodexModel{{Name: upstream, Alias: alias}}}},
			}
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewCodexExecutor(cfg))
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": server.URL, "api_key": "test-key"}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: alias, UserDefined: true}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			payload := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"output_config":{"effort":"low"},"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"messages":[{"role":"user","content":"check weather"}]}`)
			original := bytes.Clone(payload)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: stream}
			req := cliproxyexecutor.Request{Model: alias, Payload: payload}
			if !stream {
				response, err := manager.Execute(context.Background(), []string{"codex"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for path, want := range map[string]string{"model": alias, "content.0.type": "tool_use", "content.0.id": "call_next", "content.0.name": "lookup", "content.0.input.city": "Shanghai", "stop_reason": "tool_use"} {
					if got := gjson.GetBytes(response.Payload, path).String(); got != want {
						t.Fatalf("%s = %q, want %q: %s", path, got, want, response.Payload)
					}
				}
				if gjson.GetBytes(response.Payload, "usage.output_tokens").Int() != 3 {
					t.Fatalf("missing usage: %s", response.Payload)
				}
			} else {
				result, err := manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				defer result.Close()
				var output strings.Builder
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					output.Write(chunk.Payload)
				}
				started, tools, usage, stopped := 0, 0, 0, 0
				for _, line := range strings.Split(output.String(), "\n") {
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					switch event.Get("type").String() {
					case "message_start":
						started++
						if event.Get("message.model").String() != alias {
							t.Fatalf("alias lost: %s", event.Raw)
						}
					case "content_block_start":
						if event.Get("content_block.type").String() == "tool_use" {
							tools++
							if event.Get("content_block.id").String() != "call_next" || event.Get("content_block.name").String() != "lookup" {
								t.Fatalf("tool identity lost: %s", event.Raw)
							}
						}
					case "message_delta":
						if event.Get("delta.stop_reason").String() != "tool_use" || event.Get("usage.output_tokens").Int() != 3 {
							t.Fatalf("terminal metadata lost: %s", event.Raw)
						}
						usage++
					case "message_stop":
						stopped++
					}
				}
				if started != 1 || tools != 1 || usage != 1 || stopped != 1 {
					t.Fatalf("invalid lifecycle starts=%d tools=%d usage=%d stops=%d: %s", started, tools, usage, stopped, output.String())
				}
			}
			body := <-requests
			if gjson.GetBytes(body, "model").String() != upstream || gjson.GetBytes(body, "reasoning.effort").String() != "low" {
				t.Fatalf("wrong upstream model or effort: %s", body)
			}
			for _, field := range []string{"messages", "thinking", "output_config", "cache_control"} {
				if gjson.GetBytes(body, field).Exists() {
					t.Fatalf("Claude field %s leaked upstream", field)
				}
			}
			if !bytes.Equal(payload, original) {
				t.Fatal("aggregate routing mutated the original request")
			}
		})
	}
}
