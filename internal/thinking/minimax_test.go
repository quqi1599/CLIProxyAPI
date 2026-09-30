package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
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
