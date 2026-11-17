package thinking_test

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/codex"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/kimi"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/openai"
	"github.com/tidwall/gjson"
)

func TestKimiCodeThinkingProtocols(t *testing.T) {
	for _, sourceFormat := range []struct{ format, path string }{{"openai", "reasoning_effort"}, {"openai-response", "reasoning.effort"}, {"claude", "output_config.effort"}} {
		for _, target := range []struct{ format, path string }{{"openai", "reasoning_effort"}, {"kimi", "reasoning_effort"}, {"openai-response", "reasoning.effort"}, {"claude", "output_config.effort"}} {
			for _, effort := range []struct{ input, want string }{{"low", "low"}, {"minimum", "low"}, {"light", "low"}, {"minimal", "low"}, {"medium", "high"}, {"high", "high"}, {"xhigh", "max"}, {"ultra", "max"}, {"max", "max"}} {
				t.Run(sourceFormat.format+"/"+target.format+"/"+effort.input, func(t *testing.T) {
					source := `{"reasoning_effort":"` + effort.input + `"}`
					if sourceFormat.format == "claude" {
						source = `{"thinking":{"type":"adaptive"},"output_config":{"effort":"` + effort.input + `"}}`
					} else if sourceFormat.format == "openai-response" {
						source = `{"reasoning":{"effort":"` + effort.input + `"}}`
					}
					body := []byte(`{"messages":[{"role":"user","content":"hello"}],"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}],"tools":[{"name":"lookup","parameters":{"const":9007199254740993}}],"output_config":{"format":{"type":"json_schema"}},"reasoning":{"summary":"auto"}}`)
					out, err := thinking.ApplyThinking(body, "k3.1-pro", sourceFormat.format, target.format, "kimi", []byte(source))
					if err != nil || gjson.GetBytes(out, target.path).String() != effort.want {
						t.Fatalf("out=%s err=%v, want %s", out, err, effort.want)
					}
					for _, path := range []string{"messages", "input", "tools", "output_config.format", "reasoning.summary"} {
						if gjson.GetBytes(body, path).Raw != gjson.GetBytes(out, path).Raw {
							t.Fatalf("%s was changed", path)
						}
					}
					second, err := thinking.ApplyThinking(out, "k3.1-pro", target.format, target.format, "kimi")
					if err != nil || string(second) != string(out) {
						t.Fatalf("not idempotent: %s => %s, %v", out, second, err)
					}
				})
			}
		}
	}
}

func TestKimiCodeThinkingValidation(t *testing.T) {
	for _, model := range []string{"k3.1-pro", "k3.1-pro-highspeed", "k3.1-pro[1m]"} {
		for _, control := range []string{`"reasoning_effort":"typo"`, `"reasoning_effort":"auto"`, `"reasoning_effort":123`} {
			t.Run(model+control, func(t *testing.T) {
				body := []byte("{" + control + "}")
				out, err := thinking.ApplyThinking(body, model, "openai", "openai", "kimi")
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 || string(out) != string(body) {
					t.Fatalf("expected unchanged payload and 400; got %s, %v", out, err)
				}
			})
		}
	}
	for _, tc := range []struct {
		model, body, path, want string
		bad                     bool
	}{
		{"k3.1-pro", `{}`, "reasoning_effort", "max", false},
		{"k3.1-pro-highspeed", `{"reasoning":{"effort":null}}`, "reasoning.effort", "max", false},
		{"k3", `{}`, "reasoning_effort", "high", false},
		{"k3-256k", `{"reasoning_effort":"low"}`, "reasoning_effort", "low", false},
		{"kimi-for-coding", `{}`, "reasoning_effort", "max", false},
		{"kimi-for-coding-highspeed", `{}`, "reasoning_effort", "", false},
		{"kimi-for-coding-highspeed", `{"reasoning_effort":"low"}`, "", "", true},
		{"k3.1-pro(low)", `{"thinking":{"type":"disabled"}}`, "reasoning_effort", "low", false},
		{"k3.1-pro(ultra)", `{}`, "reasoning_effort", "max", false},
		{"k3.1-pro(off)", `{}`, "reasoning_effort", "low", false},
		{"k3.1-pro(0)", `{"reasoning_effort":"max"}`, "reasoning_effort", "low", false},
		{"k3.1-pro(max)", `{"enable_thinking":false}`, "reasoning_effort", "max", false},
		{"k3.1-pro(unknown)", `{}`, "", "", true},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			format := "openai"
			if tc.path == "reasoning.effort" {
				format = "openai-response"
			}
			out, err := thinking.ApplyThinking([]byte(tc.body), tc.model, format, format, "kimi")
			if (err != nil) != tc.bad || (!tc.bad && gjson.GetBytes(out, tc.path).String() != tc.want) {
				t.Fatalf("got %s, %v; want %s", out, err, tc.want)
			}
		})
	}
}

func TestKimiCodeNativeThinkingEffortPriority(t *testing.T) {
	for _, tc := range []struct{ model, source, want string }{
		{"k3.1-pro", `{"thinking":{"type":"enabled","effort":"low"},"reasoning_effort":"max"}`, "low"},
		{"k3.1-pro", `{"thinking":{"effort":"high"},"reasoning_effort":"typo"}`, "high"},
		{"k3.1-pro", `{"thinking":{"effort":null},"reasoning_effort":"low"}`, "low"},
		{"k3.1-pro", `{"thinking":{"type":"disabled","effort":"max"}}`, "low"},
		{"k3.1-pro(max)", `{"thinking":{"effort":"low"}}`, "max"},
	} {
		for _, target := range []struct{ format, path string }{{"openai", "reasoning_effort"}, {"kimi", "reasoning_effort"}, {"claude", "output_config.effort"}, {"openai-response", "reasoning.effort"}} {
			t.Run(tc.model+"/"+tc.source+"/"+target.format, func(t *testing.T) {
				out, err := thinking.ApplyThinking([]byte(`{}`), tc.model, "openai", target.format, "kimi", []byte(tc.source))
				if err != nil || gjson.GetBytes(out, target.path).String() != tc.want {
					t.Fatalf("effort priority: out=%s err=%v want=%s", out, err, tc.want)
				}
			})
		}
	}
}

func TestKimiCodeDisabledThinkingUsesLowestSupportedEffort(t *testing.T) {
	for _, model := range []string{"k3.1-pro", "k3.1-pro-highspeed", "k3.1-pro[1m]", "k3.1-pro-highspeed[1m]"} {
		for _, control := range []string{
			`"reasoning_effort":"off"`, `"reasoning_effort":"none"`, `"reasoning_effort":"disabled"`,
			`"reasoning":{"effort":"none"}`, `"output_config":{"effort":"disabled"}`,
			`"thinking":{"type":"disabled"}`, `"thinking":{"type":"off"}`, `"thinking":{"type":"none"}`,
			`"enable_thinking":false`, `"thinking":{"type":"enabled","budget_tokens":0}`,
			`"thinking":{"reasoning_effort":"none"}`, `"enable_thinking":false,"reasoning_effort":"max"`,
		} {
			for _, target := range []struct{ format, path string }{{"openai", "reasoning_effort"}, {"kimi", "reasoning_effort"}, {"openai-response", "reasoning.effort"}, {"claude", "output_config.effort"}} {
				t.Run(model+"/"+target.format+"/"+control, func(t *testing.T) {
					body := []byte(`{"messages":[{"role":"user","content":"hello"}],"input":[{"type":"reasoning","encrypted_content":"opaque","summary":[]}],"tools":[{"name":"lookup","parameters":{"const":9007199254740993}}]}`)
					source := []byte("{" + control + "}")
					out, err := thinking.ApplyThinking(body, model, "openai", target.format, "kimi", source)
					if err != nil || gjson.GetBytes(out, target.path).String() != "low" {
						t.Fatalf("expected low effort, got %s, %v", out, err)
					}
					for _, path := range []string{"messages", "input", "tools"} {
						if gjson.GetBytes(out, path).Raw != gjson.GetBytes(body, path).Raw {
							t.Fatalf("%s changed: %s", path, out)
						}
					}
					second, err := thinking.ApplyThinking(out, model, target.format, target.format, "kimi")
					if err != nil || string(second) != string(out) {
						t.Fatalf("not idempotent: %s => %s, %v", out, second, err)
					}
				})
			}
		}
	}
}

func TestKimiCodeOlderModelsStillAllowDisabledThinking(t *testing.T) {
	for _, model := range []string{"k3", "k3-256k", "kimi-for-coding", "kimi-for-coding-highspeed"} {
		out, err := thinking.ApplyThinking([]byte(`{"enable_thinking":false}`), model, "openai", "kimi", "kimi")
		if err != nil || gjson.GetBytes(out, "thinking.type").String() != "disabled" || gjson.GetBytes(out, "reasoning_effort").Exists() {
			t.Fatalf("%s: disabled thinking changed: %s, %v", model, out, err)
		}
	}
}

func TestKimiCodeDoesNotMatchLegacyOrFutureAliases(t *testing.T) {
	for _, model := range []string{"kimi-k3", "k3.10-pro", "k3.1", "K3.1 Pro", "k3.1-pro-typo", "k3-256k[1m]", "kimi-for-coding-highspeed[1m]"} {
		if thinking.IsKimiCodeModel(model) {
			t.Errorf("matched %s", model)
		}
	}
}
