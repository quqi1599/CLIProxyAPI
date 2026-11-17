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
	"github.com/tidwall/sjson"
)

func TestKimiCodeForcedToolRequestPlans(t *testing.T) {
	for _, format := range []string{"openai", "openai-response", "claude"} {
		for _, stream := range []bool{false, true} {
			for _, override := range []bool{false, true} {
				for _, named := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t/override=%t/named=%t", format, stream, override, named), func(t *testing.T) {
						tools := `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}}},{"type":"function","function":{"name":"delete","parameters":{"type":"object","properties":{}}}}]`
						choice := `"required"`
						if named {
							choice = `{"type":"function","function":{"name":"lookup"}}`
						}
						if format == "claude" {
							tools = `[{"name":"lookup","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}},{"name":"delete","input_schema":{"type":"object","properties":{}}}]`
							choice = `{"type":"any"}`
							if named {
								choice = `{"type":"tool","name":"lookup"}`
							}
						} else if format == "openai-response" {
							tools = `[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}},{"type":"function","name":"delete","parameters":{"type":"object","properties":{}}}]`
							if named {
								choice = `{"type":"function","name":"lookup"}`
							}
						}
						body := []byte(`{"model":"k3.1-pro","messages":[{"role":"user","content":"hello"}],"input":"hello","tools":` + tools + `,"tool_choice":` + choice + `,"reasoning_effort":"high"}`)
						if format == "claude" {
							body, _ = sjson.SetBytes(body, "output_config.effort", "high")
						}
						if format == "openai-response" {
							body, _ = sjson.SetBytes(body, "reasoning.effort", "high")
						}
						req := cliproxyexecutor.Request{Model: "k3.1-pro", Payload: body}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format), OriginalRequest: body, Stream: stream}
						auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.kimi.com/coding", "api_key": "test", "compat_kind": "kimi"}}
						cfg := &config.Config{DisableClaudeCloakMode: true}
						if override {
							tc := map[string]any{"type": "any"}
							if named {
								tc = map[string]any{"type": "tool", "name": "lookup"}
							}
							cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "k3.1-pro"}}, Params: map[string]any{"tool_choice": tc}}}
							req.Payload, _ = sjson.DeleteBytes(body, "tool_choice")
						}
						claudePlan, err := NewClaudeExecutor(cfg).prepareClaudeRequest(context.Background(), auth, req, opts, "k3.1-pro", stream)
						if err != nil {
							t.Fatal(err)
						}
						assertKimiForcedToolPlan(t, claudePlan.bodyForUpstream, "claude", named)
						cfg = &config.Config{}
						if override {
							tc := any("required")
							if named {
								tc = map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}
								if format == "openai-response" {
									tc = map[string]any{"type": "function", "name": "lookup"}
								}
							}
							cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "k3.1-pro"}}, Params: map[string]any{"tool_choice": tc}}}
						}
						openaiPlan, err := NewOpenAICompatExecutor("kimi", cfg).prepareOpenAICompatRequest(context.Background(), auth, req, opts, "https://api.kimi.com/coding/v1", "k3.1-pro", openAICompatProfileForKind("kimi"), stream)
						if err != nil {
							t.Fatal(err)
						}
						target := "openai"
						if format == "openai-response" {
							target = format
						}
						assertKimiForcedToolPlan(t, openaiPlan.body, target, named)
					})
				}
			}
		}
	}
}

func assertKimiForcedToolPlan(t *testing.T, body []byte, format string, named bool) {
	t.Helper()
	path, want, effort, namePath := "tool_choice", "required", "reasoning_effort", "tools.0.function.name"
	if format == "claude" {
		path, want, effort, namePath = "tool_choice.type", "any", "output_config.effort", "tools.0.name"
	}
	if format == "openai-response" {
		effort, namePath = "reasoning.effort", "tools.0.name"
	}
	count := int64(2)
	if named {
		count = 1
	}
	if gjson.GetBytes(body, path).String() != want || gjson.GetBytes(body, effort).String() != "high" || gjson.GetBytes(body, "tools.#").Int() != count || gjson.GetBytes(body, namePath).String() != "lookup" {
		t.Fatalf("lost tool/thinking constraint: %s", body)
	}
	if gjson.GetBytes(body, "thinking.type").String() == "disabled" {
		t.Fatalf("thinking disabled: %s", body)
	}
}

func TestKimiCodeDedicatedNamedToolSanitizer(t *testing.T) {
	body := []byte(`{"model":"k3.1-pro","reasoning_effort":"high","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"delete"}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
	out, err := sanitizeKimiOpenAICompatibleRequestBodyForModel(body, "k3.1-pro", "")
	if err != nil {
		t.Fatal(err)
	}
	assertKimiForcedToolPlan(t, out, "openai", true)
}

func TestKimiCodeEmptyToolsForcedChoiceRequestPlans(t *testing.T) {
	for _, format := range []string{"openai", "openai-response", "claude"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				body := []byte(`{"model":"k3.1-pro","messages":[{"role":"user","content":"hello"}],"input":"hello","tools":[],"tool_choice":"required","reasoning_effort":"high"}`)
				if format == "claude" {
					body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"any"}`))
					body, _ = sjson.SetBytes(body, "output_config.effort", "high")
				}
				if format == "openai-response" {
					body, _ = sjson.SetBytes(body, "reasoning.effort", "high")
				}
				req := cliproxyexecutor.Request{Model: "k3.1-pro", Payload: body}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format), OriginalRequest: body, Stream: stream}
				auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.kimi.com/coding", "api_key": "test", "compat_kind": "kimi"}}
				claudePlan, err := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}).prepareClaudeRequest(context.Background(), auth, req, opts, "k3.1-pro", stream)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(claudePlan.bodyForUpstream, "tool_choice").Exists() || gjson.GetBytes(claudePlan.bodyForUpstream, "output_config.effort").String() != "high" {
					t.Fatalf("invalid Messages plan: %s", claudePlan.bodyForUpstream)
				}
				plan, err := NewOpenAICompatExecutor("kimi", &config.Config{}).prepareOpenAICompatRequest(context.Background(), auth, req, opts, "https://api.kimi.com/coding/v1", "k3.1-pro", openAICompatProfileForKind("kimi"), stream)
				if err != nil {
					t.Fatal(err)
				}
				field := "reasoning_effort"
				if format == "openai-response" {
					field = "reasoning.effort"
				}
				if gjson.GetBytes(plan.body, "tool_choice").Exists() || gjson.GetBytes(plan.body, field).String() != "high" {
					t.Fatalf("invalid OpenAI plan: %s", plan.body)
				}
			})
		}
	}
}
