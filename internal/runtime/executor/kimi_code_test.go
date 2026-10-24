package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type kimiCodeTestExecutor interface {
	Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
}

func TestKimiCodeNativeResponsesWire(t *testing.T) {
	for _, dedicated := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("dedicated=%t/stream=%t", dedicated, stream), func(t *testing.T) {
				var gotBody []byte
				var gotPath, gotDevice, gotAuth string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotBody, _ = io.ReadAll(r.Body)
					gotPath, gotDevice, gotAuth = r.URL.Path, r.Header.Get("X-Msh-Device-Id"), r.Header.Get("Authorization")
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-kimi\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"resp-kimi","object":"response","status":"completed","model":"k3.1-pro","output":[]}`)
					}
				}))
				defer server.Close()
				auth := &cliproxyauth.Auth{Provider: "kimi", Attributes: map[string]string{"base_url": server.URL + "/coding/v1", "api_key": "test", "compat_kind": "kimi"}, Metadata: map[string]any{"device_id": "test-device", "access_token": "oauth-test"}}
				var executor kimiCodeTestExecutor = NewOpenAICompatExecutor("kimi", &config.Config{})
				if dedicated {
					executor = NewKimiExecutor(&config.Config{})
				}
				payload := []byte(`{"model":"k3.1-pro","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}}],"reasoning":{"effort":"xhigh","summary":"auto"}}`)
				req := cliproxyexecutor.Request{Model: "k3.1-pro", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				if stream {
					result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					var output strings.Builder
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output.Write(chunk.Payload)
					}
					if !strings.Contains(output.String(), "response.completed") || !strings.Contains(output.String(), "response.output_text.delta") {
						t.Fatalf("SSE lost: %s", output.String())
					}
				} else {
					resp, err := executor.Execute(context.Background(), auth, req, opts)
					if err != nil || gjson.GetBytes(resp.Payload, "object").String() != "response" {
						t.Fatalf("response=%s, err=%v", resp.Payload, err)
					}
				}
				if gotPath != "/coding/v1/responses" {
					t.Fatalf("not native: %s", gotPath)
				}
				if gjson.GetBytes(gotBody, "reasoning.effort").String() != "max" {
					t.Fatalf("effort lost: %s", gotBody)
				}
				for _, path := range []string{"input", "tools", "reasoning.summary"} {
					if gjson.GetBytes(gotBody, path).Raw != gjson.GetBytes(payload, path).Raw {
						t.Fatalf("%s altered: %s", path, gotBody)
					}
				}
				if dedicated && (gotDevice != "test-device" || gotAuth != "Bearer oauth-test") {
					t.Fatalf("OAuth identity not retained: device=%q", gotDevice)
				}
				if auth.Attributes["base_url"] != server.URL+"/coding/v1" || auth.Attributes["api_key"] != "test" || auth.Attributes["header:X-Msh-Device-Id"] != "" {
					t.Fatal("stored auth mutated")
				}
			})
		}
	}
}

func TestKimiCodeChatWire(t *testing.T) {
	for _, dedicated := range []bool{false, true} {
		for _, model := range []string{"k3.1-pro", "k3.1-pro-highspeed", "k3", "k3-256k", "kimi-for-coding", "kimi-for-coding-highspeed"} {
			t.Run(fmt.Sprintf("%s/dedicated=%t", model, dedicated), func(t *testing.T) {
				var gotBody []byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotBody, _ = io.ReadAll(r.Body)
					if r.URL.Path != "/coding/v1/chat/completions" {
						t.Errorf("path=%s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"chatcmpl-kimi","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()
				var executor kimiCodeTestExecutor = NewOpenAICompatExecutor("kimi", &config.Config{})
				if dedicated {
					executor = NewKimiExecutor(&config.Config{})
				}
				auth := &cliproxyauth.Auth{Provider: "kimi", Attributes: map[string]string{"base_url": server.URL + "/coding/v1", "api_key": "test", "compat_kind": "kimi"}}
				effort := `,"reasoning_effort":"low"`
				if model == "kimi-for-coding-highspeed" {
					effort = ""
				}
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]` + effort + `}`)}
				_, err := executor.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(gotBody, "model").String() != model {
					t.Fatalf("model rewritten: %s", gotBody)
				}
				if effort != "" && gjson.GetBytes(gotBody, "reasoning_effort").String() != "low" {
					t.Fatalf("effort lost: %s", gotBody)
				}
			})
		}
	}
}

func TestKimiCodeRejectsBeforeUpstream(t *testing.T) {
	for _, kind := range []string{"compat-chat", "compat-responses", "dedicated-chat", "dedicated-responses", "claude"} {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/override=%t", kind, override), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
				defer server.Close()
				cfg := &config.Config{}
				format, path := "openai", "reasoning_effort"
				if strings.HasSuffix(kind, "responses") {
					format, path = "openai-response", "reasoning.effort"
				}
				if kind == "claude" {
					format, path = "claude", "thinking.type"
				}
				body := `{"model":"k3.1-pro","messages":[{"role":"user","content":"hello"}],"input":"hello","thinking":{"type":"disabled"}}`
				if override {
					body = `{"model":"k3.1-pro","messages":[{"role":"user","content":"hello"}],"input":"hello"}`
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "k3.1-pro"}}, Params: map[string]any{path: "disabled"}}}
				}
				var executor kimiCodeTestExecutor = NewOpenAICompatExecutor("kimi", cfg)
				if strings.HasPrefix(kind, "dedicated") {
					executor = NewKimiExecutor(cfg)
				}
				if kind == "claude" {
					executor = NewClaudeExecutor(cfg)
				}
				auth := &cliproxyauth.Auth{Provider: "kimi", Attributes: map[string]string{"base_url": server.URL + "/coding/v1", "api_key": "test", "compat_kind": "kimi"}}
				req := cliproxyexecutor.Request{Model: "k3.1-pro", Payload: []byte(body)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format)}
				_, err := executor.Execute(context.Background(), auth, req, opts)
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 {
					t.Fatalf("expected 400, got %v", err)
				}
				_, err = executor.ExecuteStream(context.Background(), auth, req, opts)
				status, ok = err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 {
					t.Fatalf("stream expected 400, got %v", err)
				}
				if calls.Load() != 0 {
					t.Fatalf("invalid request reached upstream %d times", calls.Load())
				}
			})
		}
	}
}

func TestKimiCodeNativeMessagesWire(t *testing.T) {
	for _, source := range []string{"claude", "openai"} {
		t.Run(source, func(t *testing.T) {
			var wire []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire, _ = io.ReadAll(r.Body)
				if r.URL.Path != "/coding/v1/messages" {
					t.Errorf("path=%s", r.URL.Path)
				}
				if gjson.GetBytes(wire, "stream").Bool() {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-kimi\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"k3.1-pro\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg-kimi","type":"message","role":"assistant","model":"k3.1-pro","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()
			payload := `{"model":"k3.1-pro","max_tokens":1024,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"max"}}`
			if source == "openai" {
				payload = `{"model":"k3.1-pro","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"max"}`
			}
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": server.URL + "/coding", "api_key": "test", "compat_kind": "kimi"}}
			_, err := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "k3.1-pro", Payload: []byte(payload)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source)})
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(wire, "output_config.effort").String() != "max" || gjson.GetBytes(wire, "thinking.type").String() != "adaptive" || gjson.GetBytes(wire, "thinking.budget_tokens").Exists() {
				t.Fatalf("lost original effort: %s", wire)
			}
		})
	}
}
