package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/codex"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/openai"
	"github.com/tidwall/gjson"
)

func TestMiniMaxM31CanonicalThinking(t *testing.T) {
	for _, tc := range []struct {
		name, model, from, to, source, translated, path, want string
		wantError                                             bool
	}{
		{name: "default", to: "claude", translated: `{}`, path: "thinking.type"},
		{name: "standalone effort", to: "claude", translated: `{"output_config":{"effort":"low"}}`, path: "output_config.effort", want: "low"},
		{name: "preserve max across translation", from: "openai", to: "claude", source: `{"reasoning_effort":"max"}`, translated: `{"thinking":{"type":"enabled","budget_tokens":128000}}`, path: "output_config.effort", want: "max"},
		{name: "preserve xhigh across translation", from: "claude", to: "openai", source: `{"output_config":{"effort":"xhigh"}}`, translated: `{}`, path: "reasoning_effort", want: "xhigh"},
		{name: "suffix wins", model: "MiniMax-M3.1-flash(low)", to: "openai", translated: `{"thinking":{"type":"disabled"}}`, path: "reasoning_effort", want: "low"},
		{name: "budget", to: "claude", translated: `{"thinking":{"type":"enabled","budget_tokens":8192}}`, path: "output_config.effort", want: "medium"},
		{name: "auto", to: "openai", translated: `{"reasoning_effort":"auto"}`, path: "reasoning_effort", want: "max"},
		{name: "disabled", to: "claude", translated: `{"thinking":{"type":"disabled"}}`, wantError: true},
		{name: "none", to: "openai", translated: `{"reasoning_effort":"none"}`, wantError: true},
		{name: "zero budget", to: "openai", translated: `{"thinking":{"type":"enabled","budget_tokens":0}}`, wantError: true},
		{name: "boolean off", to: "openai", translated: `{"enable_thinking":false,"reasoning_effort":"max"}`, wantError: true},
		{name: "invalid level", to: "openai", translated: `{"reasoning_effort":"ultra"}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.model == "" {
				tc.model = "MiniMax-M3.1-Flash-Preview"
			}
			if tc.from == "" {
				tc.from = tc.to
			}
			out, err := thinking.ApplyThinking([]byte(tc.translated), tc.model, tc.from, tc.to, "minimax", []byte(tc.source))
			if tc.wantError {
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 {
					t.Fatalf("expected local 400, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, tc.path).String(); got != tc.want {
				t.Fatalf("%s = %q, want %q: %s", tc.path, got, tc.want, out)
			}
			if gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
				t.Fatalf("manual budget remained: %s", out)
			}
		})
	}
}

func TestMiniMaxM31DisabledSourceControlsAreNotLost(t *testing.T) {
	for _, tc := range []struct{ from, source string }{
		{"claude", `{"thinking":{"type":"off"},"output_config":{"effort":"max"}}`},
		{"claude", `{"thinking":{"type":" NONE "},"output_config":{"effort":"high"}}`},
		{"claude", `{"enable_thinking":false,"output_config":{"effort":"high"}}`},
		{"codex", `{"thinking":{"type":"disabled"},"reasoning":{"effort":"max"}}`},
		{"codex", `{"enable_thinking":false,"reasoning":{"effort":"max"}}`},
		{"openai-response", `{"thinking":{"type":"off"},"reasoning":{"effort":"high"}}`},
	} {
		for _, to := range []string{"claude", "openai", "codex"} {
			t.Run(tc.from+"/"+to+"/"+tc.source, func(t *testing.T) {
				body := []byte(`{"output_config":{"effort":"max"},"reasoning_effort":"max","reasoning":{"effort":"max"}}`)
				original := string(body)
				_, err := thinking.ApplyThinking(body, "MiniMax-M3.1-Flash-Preview", tc.from, to, "minimax", []byte(tc.source))
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 {
					t.Fatalf("explicit disable became mandatory thinking: %v", err)
				}
				if string(body) != original {
					t.Fatal("source mutated on rejection")
				}
			})
		}
	}
}

func TestMiniMaxM31ResponsesEffortCanonicalization(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":" MAX ","summary":"auto"},"input":[{"type":"function_call","call_id":"call_fixture","arguments":"{\"n\":9007199254740993}"}]}`)
	out, err := thinking.ApplyThinking(body, "MiniMax-M3.1-Flash-Preview", "codex", "codex", "minimax")
	if err != nil || gjson.GetBytes(out, "reasoning.effort").String() != "max" || gjson.GetBytes(out, "reasoning.summary").String() != "auto" || gjson.GetBytes(out, "input").Raw != gjson.GetBytes(body, "input").Raw {
		t.Fatalf("Responses thinking/history not preserved: %s, %v", out, err)
	}
}

func TestMiniMaxM31StandaloneSDKEffortSpellings(t *testing.T) {
	for _, from := range []string{"openai", "claude", "codex"} {
		for _, source := range []string{`{"reasoning_effort":" LOW "}`, `{"reasoning":{"effort":" LOW "}}`, `{"output_config":{"effort":" LOW "}}`} {
			for _, to := range []string{"openai", "claude", "codex"} {
				out, err := thinking.ApplyThinking([]byte(`{}`), "MiniMax-M3.1-Flash-Preview", from, to, "minimax", []byte(source))
				path := "reasoning_effort"
				if to == "claude" {
					path = "output_config.effort"
				} else if to == "codex" {
					path = "reasoning.effort"
				}
				if err != nil || gjson.GetBytes(out, path).String() != "low" {
					t.Fatalf("standalone effort lost %s/%s/%s: %s, %v", from, to, source, out, err)
				}
			}
		}
		for _, source := range []string{`{"reasoning_effort":"none"}`, `{"reasoning":{"effort":"none"}}`, `{"output_config":{"effort":"none"}}`, `{"output_config":{"effort":"ultra"}}`, `{"thinking":{"type":"disabled"},"output_config":{"effort":"low"}}`} {
			_, err := thinking.ApplyThinking([]byte(`{}`), "MiniMax-M3.1-Flash-Preview", from, "openai", "minimax", []byte(source))
			if err == nil {
				t.Fatalf("invalid or disabled control silently accepted %s/%s", from, source)
			}
		}
	}
}

func TestMiniMaxModelBoundaries(t *testing.T) {
	for _, model := range []string{"MiniMax-M3.1", "MiniMax-M3.1-flash", "MiniMax-M3.1-Flash-Preview", " minimax-m3.1-flash-preview(max) "} {
		if !thinking.IsMiniMaxM31Model(model) || !thinking.IsMiniMaxM3Model(model) {
			t.Errorf("M3.1 model not recognized: %s", model)
		}
	}
	for _, model := range []string{"MiniMax-M2.7", "MiniMax-M30", "MiniMax-M3.10", "MiniMax-M3.1-image", "other/MiniMax-M3.1"} {
		if thinking.IsMiniMaxM31Model(model) || thinking.IsMiniMaxM3Model(model) {
			t.Errorf("unrelated model recognized: %s", model)
		}
	}
}
