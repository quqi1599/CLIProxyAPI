package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// Exercise the final request plans, including provider policies and payload scrubbing.
func TestThinkingToggleHybridProviderMatrix(t *testing.T) {
	for _, model := range []struct{ kind, name string }{
		{"qwen", "qwen3.8-max"}, {"qwen", "qwen3.8-flash"}, {"qwen", "qwen3.7-plus"},
		{"kimi", "kimi-k2.6"}, {"kimi", "kimi-k2.7"},
		{"zhipu", "glm-4.7"}, {"zhipu", "glm-5.2"},
		{"deepseek", "deepseek-v4-flash"}, {"deepseek", "deepseek-v4-pro"},
		{"doubao", "doubao-seed-2.0-pro"}, {"doubao", "doubao-seed-2.1-pro"},
	} {
		for _, source := range []string{"openai", "openai-response", "claude"} {
			for _, route := range []string{"openai", "claude"} {
				for _, stream := range []bool{false, true} {
					for _, control := range []string{"native", "effort"} {
						t.Run(fmt.Sprintf("%s/%s/%s-to-%s/stream=%t/%s", model.kind, model.name, source, route, stream, control), func(t *testing.T) {
							fields := `"thinking":{"type":"disabled"}`
							if source != "claude" && model.kind == "qwen" {
								fields = `"enable_thinking":false`
							}
							if control == "effort" && source == "openai" {
								fields = `"reasoning_effort":"none"`
							} else if control == "effort" && source == "openai-response" {
								fields = `"reasoning":{"effort":"none"}`
							}
							content := `"messages":[{"role":"user","content":"hi"}],"max_tokens":4096`
							if source == "openai-response" {
								content = `"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"max_output_tokens":4096`
							}
							payload := []byte(fmt.Sprintf(`{"model":%q,%s,%s}`, model.name, content, fields))
							auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://upstream.invalid/v1", "api_key": "test", "compat_kind": model.kind}}
							req := cliproxyexecutor.Request{Model: model.name, Payload: payload}
							opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source), Stream: stream}
							var body []byte
							if route == "claude" {
								plan, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(context.Background(), auth, req, opts, model.name, stream)
								if err != nil {
									t.Fatal(err)
								}
								body = plan.bodyForUpstream
							} else {
								exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
								profile := exec.resolveProfile(auth)
								plan, err := exec.prepareOpenAICompatRequest(context.Background(), auth, req, opts, auth.Attributes["base_url"], model.name, profile, stream)
								if err != nil {
									t.Fatal(err)
								}
								body = plan.body
							}
							off := gjson.GetBytes(body, "thinking.type").String() == "disabled"
							if route == "openai" && model.kind == "qwen" {
								off = gjson.GetBytes(body, "enable_thinking").Type == gjson.False
							} else if route == "openai" && model.kind == "deepseek" && source == "openai-response" {
								off = gjson.GetBytes(body, "reasoning.effort").String() == "none"
							}
							if !off {
								t.Fatalf("disabled intent lost: thinking=%s enable_thinking=%s reasoning=%s effort=%s", gjson.GetBytes(body, "thinking"), gjson.GetBytes(body, "enable_thinking"), gjson.GetBytes(body, "reasoning"), gjson.GetBytes(body, "reasoning_effort"))
							}
							if gjson.GetBytes(body, "thinking.type").String() == "enabled" || gjson.GetBytes(body, "enable_thinking").Type == gjson.True {
								t.Fatalf("conflicting enabled control: %s", body)
							}
							if gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
								t.Fatalf("disabled thinking retains budget: %s", body)
							}
						})
					}
				}
			}
		}
	}
}

func TestDeepSeekResponsesDisabledAfterPayloadConfig(t *testing.T) {
	profile := openAICompatProfileForKind("deepseek")
	body := scrubOpenAICompatPostConfigPayload([]byte(`{"model":"deepseek-v4-pro","input":"hi","reasoning":{"effort":"none"}}`), profile, "deepseek-v4-pro", "https://api.deepseek.com/v1", "responses")
	if gjson.GetBytes(body, "reasoning.effort").String() != "none" || gjson.GetBytes(body, "thinking").Exists() {
		t.Fatalf("post-config disabled control invalid: %s", body)
	}
}
