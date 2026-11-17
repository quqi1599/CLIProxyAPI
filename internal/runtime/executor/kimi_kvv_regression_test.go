package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// The KVV failures came from losing fields before reaching Kimi. Inspect the
// actual HTTP request, then verify that native response fields and usage survive.
func TestKimiKVVChatWirePreservesContract(t *testing.T) {
	longName := "tool" + strings.Repeat("a", 124)
	body := []byte(`{"model":"kimi-k3.1-pro","messages":[{"role":"system","content":"","tools":[{"type":"function","function":{"name":"dynamic","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}}}]},{"role":"user","content":"Return JSON"}],"tools":[{"type":"function","function":{"name":"` + longName + `","strict":true,"parameters":{"type":"object","properties":{"n":{"const":9007199254740993}},"additionalProperties":false}}}],"tool_choice":"none","response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}}},"thinking":{"type":"enabled","effort":"low"},"reasoning_effort":"max","temperature":1.1,"top_p":0.8,"presence_penalty":0.5,"frequency_penalty":0.5,"max_completion_tokens":1024,"stop":["END"]}`)
	for _, kind := range []string{"compat", "dedicated", "claude"} {
		for _, stream := range []bool{false, true} {
			for _, override := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/override=%t", kind, stream, override), func(t *testing.T) {
					cfg := &config.Config{}
					if override {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "k3.1-pro"}}, Params: map[string]any{"temperature": 2.0}}}
					}
					e, auth := kimiKVVExecutor(kind, cfg)
					calls := 0
					ctx := contextWithGinHeaders(map[string]string{"User-Agent": "kvv-test/1.0"})
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						wire, _ := io.ReadAll(r.Body)
						if r.URL.String() != "https://api.kimi.com/coding/v1/chat/completions" {
							t.Errorf("protocol changed: %s", r.URL)
						}
						for _, path := range []string{"messages", "tools", "tool_choice", "response_format", "temperature", "top_p", "presence_penalty", "frequency_penalty", "max_completion_tokens", "stop"} {
							expected := gjson.GetBytes(body, path).Raw
							if override && path == "temperature" {
								expected = "2"
							}
							if gjson.GetBytes(wire, path).Raw != expected {
								t.Errorf("%s changed: got %s", path, gjson.GetBytes(wire, path).Raw)
							}
						}
						if gjson.GetBytes(wire, "reasoning_effort").String() != "low" {
							t.Errorf("thinking.effort lost precedence: %s", gjson.GetBytes(wire, "reasoning_effort"))
						}
						if r.Header.Get("Authorization") != "Bearer synthetic" || r.Header.Get("X-Test-Route") != "retained" {
							t.Error("credential or configured header changed")
						}
						return kimiKVVResponse(stream), nil
					}))
					out, err := executeKimiKVV(ctx, e, auth, body, sdktranslator.FormatOpenAI, stream)
					if err != nil {
						t.Fatal(err)
					}
					if calls != 1 || !strings.Contains(out, `"reasoning_content":"checked"`) || !strings.Contains(out, `"prompt_tokens":17`) {
						t.Fatalf("native reasoning/usage lost: calls=%d response=%s", calls, out)
					}
					if kind == "claude" && auth.Attributes["base_url"] != "https://api.kimi.com/coding" {
						t.Fatal("stored credential mutated")
					}
				})
			}
		}
	}
}

func TestKimiKVVClaudeRouteNativeResponses(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			e, auth := kimiKVVExecutor("claude", &config.Config{})
			body := []byte(`{"model":"k3.1-pro","input":[{"type":"reasoning","encrypted_content":"opaque","summary":[]},{"type":"message","role":"user","content":"hello"}],"reasoning":{"effort":"low"},"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}}}`)
			calls := 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				wire, _ := io.ReadAll(r.Body)
				if r.URL.String() != "https://api.kimi.com/coding/v1/responses" {
					t.Errorf("wrong native endpoint: %s", r.URL)
				}
				for _, path := range []string{"input", "reasoning", "text"} {
					if gjson.GetBytes(wire, path).Raw != gjson.GetBytes(body, path).Raw {
						t.Errorf("native %s changed: %s", path, gjson.GetBytes(wire, path))
					}
				}
				response := `{"id":"resp_kvv","object":"response","status":"completed","output":[],"usage":{"input_tokens":17,"output_tokens":3,"total_tokens":20}}`
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					response = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			}))
			out, err := executeKimiKVV(ctx, e, auth, body, sdktranslator.FormatOpenAIResponse, stream)
			if err != nil || calls != 1 || !strings.Contains(out, `"input_tokens":17`) {
				t.Fatalf("native Responses lost: calls=%d response=%s err=%v", calls, out, err)
			}
		})
	}
}

func TestKimiKVVInvalidFieldsReachUpstream(t *testing.T) {
	for _, fields := range []string{
		`"messages":[{"role":"system","tools":"invalid"},{"role":"user","content":"hello"}],"tool_choice":"bogus"`,
		`"messages":[{"role":"assistant","tools":[{"type":"bogus"}]}],"response_format":{"type":"json_schema","json_schema":{"name":"bad","strict":"yes","schema":"invalid"}}`,
	} {
		body := []byte(`{"model":"k3.1-pro",` + fields + `}`)
		for _, kind := range []string{"compat", "dedicated", "claude"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%t/%s", kind, stream, fields), func(t *testing.T) {
					e, auth := kimiKVVExecutor(kind, &config.Config{})
					calls := 0
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						wire, _ := io.ReadAll(r.Body)
						for _, path := range []string{"messages", "tool_choice", "response_format"} {
							if gjson.GetBytes(wire, path).Raw != gjson.GetBytes(body, path).Raw {
								t.Errorf("invalid %s silently rewritten: %s", path, gjson.GetBytes(wire, path).Raw)
							}
						}
						return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"invalid response_format"}}`))}, nil
					}))
					_, err := executeKimiKVV(ctx, e, auth, body, sdktranslator.FormatOpenAI, stream)
					status, ok := err.(interface{ StatusCode() int })
					if !ok || status.StatusCode() != 400 || calls != 1 {
						t.Fatalf("rejection not retained: calls=%d error=%v", calls, err)
					}
				})
			}
		}
	}
}

func TestKimiKVVClaudeNativeMessagesNoDefaultCloaking(t *testing.T) {
	longName := "tool" + strings.Repeat("a", 124)
	body := []byte(`{"model":"k3.1-pro","system":"Original system","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"` + longName + `","input_schema":{"type":"object"}}],"max_tokens":1024}`)
	e := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.kimi.com/coding", "api_key": "synthetic", "compat_kind": "kimi"}}
	for _, stream := range []bool{false, true} {
		plan, err := e.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{Model: "k3.1-pro", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body}, "k3.1-pro", stream)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(plan.bodyForUpstream, "system").Raw; got != `"Original system"` {
			t.Errorf("unexpected system prompt: %s", got)
		}
		if gjson.GetBytes(plan.bodyForUpstream, "tools.0.name").String() != longName || gjson.GetBytes(plan.bodyForUpstream, "metadata.user_id").Exists() {
			t.Errorf("native Messages modified: %s", plan.bodyForUpstream)
		}
	}
}

func kimiKVVExecutor(kind string, cfg *config.Config) (kimiCodeTestExecutor, *cliproxyauth.Auth) {
	auth := &cliproxyauth.Auth{ID: "kvv-synthetic", Provider: "kimi", Attributes: map[string]string{"base_url": "https://api.kimi.com/coding/v1", "api_key": "synthetic", "compat_kind": "kimi", "header:X-Test-Route": "retained"}}
	switch kind {
	case "dedicated":
		return NewKimiExecutor(cfg), auth
	case "claude":
		auth.Provider = "claude"
		auth.Attributes["base_url"] = "https://api.kimi.com/coding"
		return NewClaudeExecutor(cfg), auth
	default:
		return NewOpenAICompatExecutor("kimi", cfg), auth
	}
}

func executeKimiKVV(ctx context.Context, e kimiCodeTestExecutor, auth *cliproxyauth.Auth, body []byte, format sdktranslator.Format, stream bool) (string, error) {
	req := cliproxyexecutor.Request{Model: "k3.1-pro", Payload: body}
	opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: body, Stream: stream}
	if !stream {
		result, err := e.Execute(ctx, auth, req, opts)
		return string(result.Payload), err
	}
	result, err := e.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return output.String(), chunk.Err
		}
		output.Write(chunk.Payload)
	}
	return output.String(), nil
}

func kimiKVVResponse(stream bool) *http.Response {
	body := `{"id":"kvv","object":"chat.completion","model":"k3.1-pro","choices":[{"index":0,"message":{"role":"assistant","content":"OK","reasoning_content":"checked"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":3,"total_tokens":20}}`
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
		body = "data: {\"id\":\"kvv\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\",\"reasoning_content\":\"checked\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":3,\"total_tokens\":20}}\n\ndata: [DONE]\n\n"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}
