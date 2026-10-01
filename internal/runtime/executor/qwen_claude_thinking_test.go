package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestQwenClaudeEffortDoesNotExceedOutputBudget(t *testing.T) {
	for _, model := range []string{"qwen3.8-flash", "qwen3.8-max", "qwen3.8-max-0902"} {
		for _, source := range []string{"openai", "openai-response", "claude"} {
			for _, stream := range []bool{false, true} {
				for _, maxTokens := range []int{0, 128, 4096, 32768} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/max=%d", model, source, stream, maxTokens), func(t *testing.T) {
						var payload string
						switch source {
						case "openai-response":
							payload = fmt.Sprintf(`{"model":%q,"input":"Reply OK.","reasoning":{"effort":"xhigh"}}`, model)
						case "claude":
							payload = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Reply OK."}],"thinking":{"type":"enabled"},"output_config":{"effort":"xhigh"}}`, model)
						default:
							payload = fmt.Sprintf(`{"model":%q,"messages":[{"role":"system","content":"Synthetic compatibility test."},{"role":"user","content":"Reply OK."}],"reasoning_effort":"xhigh"}`, model)
						}
						tools := make([]string, 24)
						for i := range tools {
							name := fmt.Sprintf("lookup_%d", i)
							schema := `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`
							switch source {
							case "claude":
								tools[i] = fmt.Sprintf(`{"name":%q,"input_schema":%s}`, name, schema)
							case "openai-response":
								tools[i] = fmt.Sprintf(`{"type":"function","name":%q,"parameters":%s}`, name, schema)
							default:
								tools[i] = fmt.Sprintf(`{"type":"function","function":{"name":%q,"parameters":%s}}`, name, schema)
							}
						}
						payload = payload[:len(payload)-1] + `,"tools":[` + strings.Join(tools, ",") + `]}`
						if maxTokens > 0 {
							field := "max_tokens"
							if source == "openai-response" {
								field = "max_output_tokens"
							}
							payload = payload[:len(payload)-1] + fmt.Sprintf(`,%q:%d}`, field, maxTokens)
						}
						auth := &cliproxyauth.Auth{Attributes: map[string]string{
							"base_url": "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic",
							"api_key":  "test", "compat_kind": "qwen",
						}}
						plan, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(context.Background(), auth,
							cliproxyexecutor.Request{Model: model, Payload: []byte(payload)},
							cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source), Stream: stream,
								Metadata: map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "workbuddy"}}, model, stream)
						if err != nil {
							t.Fatal(err)
						}
						body := plan.bodyForUpstream
						if got := gjson.GetBytes(body, "thinking.type").String(); got != "enabled" {
							t.Fatalf("thinking type = %q, want enabled", got)
						}
						if got := gjson.GetBytes(body, "output_config.effort").String(); got != "xhigh" {
							t.Fatalf("effort = %q, want xhigh; thinking=%s", got, gjson.GetBytes(body, "thinking"))
						}
						if gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
							t.Fatalf("level request gained a numeric budget: %s", gjson.GetBytes(body, "thinking"))
						}
						if maxTokens > 0 && gjson.GetBytes(body, "max_tokens").Int() != int64(maxTokens) {
							t.Fatalf("client output limit changed: %s", gjson.GetBytes(body, "max_tokens"))
						}
						if got := gjson.GetBytes(body, "tools.#").Int(); got != 24 {
							t.Fatalf("tool definitions lost: %d", got)
						}
					})
				}
			}
		}
	}
}
