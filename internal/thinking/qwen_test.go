package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
	"github.com/tidwall/gjson"
)

func TestQwenClaudeCanonicalThinking(t *testing.T) {
	for _, tc := range []struct {
		name, model, sourceFormat, source, translated, wantType, wantEffort string
		wantBudget                                                          int64
		wantError                                                           bool
	}{
		{name: "xhigh survives translation", source: `{"reasoning_effort":"xhigh"}`, translated: `{"thinking":{"type":"enabled","budget_tokens":32768}}`, wantType: "enabled", wantEffort: "xhigh"},
		{name: "max maps to xhigh", source: `{"reasoning_effort":"max"}`, wantType: "enabled", wantEffort: "xhigh"},
		{name: "high maps to xhigh", source: `{"reasoning_effort":"high"}`, wantType: "enabled", wantEffort: "xhigh"},
		{name: "medium remains medium", source: `{"reasoning_effort":"medium"}`, wantType: "enabled", wantEffort: "medium"},
		{name: "minimal maps to low", source: `{"reasoning_effort":"minimal"}`, wantType: "enabled", wantEffort: "low"},
		{name: "native standalone effort", sourceFormat: "claude", source: `{"output_config":{"effort":"xhigh"}}`, wantType: "enabled", wantEffort: "xhigh"},
		{name: "native adaptive compatibility", sourceFormat: "claude", source: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`, wantType: "enabled", wantEffort: "medium"},
		{name: "auto delegates to upstream", source: `{"reasoning_effort":"auto"}`, wantType: "enabled"},
		{name: "boolean off overrides effort", source: `{"enable_thinking":false,"reasoning_effort":"xhigh"}`, wantType: "disabled"},
		{name: "native off overrides effort", sourceFormat: "claude", source: `{"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"}}`, wantType: "disabled"},
		{name: "none remains off", source: `{"reasoning_effort":"none"}`, wantType: "disabled"},
		{name: "suffix wins", model: "qwen3.8-flash(low)", source: `{"enable_thinking":false}`, wantType: "enabled", wantEffort: "low"},
		{name: "no default manufactured", source: `{}`},
		{name: "null is not off", source: `{"enable_thinking":null}`},
		{name: "legacy budget retained", sourceFormat: "claude", source: `{"thinking":{"type":"enabled","budget_tokens":1024}}`, wantType: "enabled", wantBudget: 1024},
		{name: "legacy budget capped below output", sourceFormat: "claude", source: `{"thinking":{"type":"enabled","budget_tokens":32768}}`, wantType: "enabled", wantBudget: 4095},
		{name: "zero budget is off", sourceFormat: "claude", source: `{"thinking":{"type":"enabled","budget_tokens":0}}`, wantType: "disabled"},
		{name: "invalid effort", source: `{"reasoning_effort":"ultra"}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.model == "" {
				tc.model = "qwen3.8-flash"
			}
			if tc.sourceFormat == "" {
				tc.sourceFormat = "openai"
			}
			body := `{"model":"qwen3.8-flash","max_tokens":4096,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"keep history"}]}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`
			if tc.translated != "" {
				body = body[:len(body)-1] + "," + tc.translated[1:]
			}
			out, err := thinking.ApplyThinking([]byte(body), tc.model, tc.sourceFormat, "claude", "claude", []byte(tc.source))
			if tc.wantError {
				if err == nil {
					t.Fatal("expected invalid effort error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "thinking.type").String(); got != tc.wantType {
				t.Fatalf("thinking.type = %q, want %q: %s", got, tc.wantType, out)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != tc.wantEffort {
				t.Fatalf("effort = %q, want %q", got, tc.wantEffort)
			}
			budget := gjson.GetBytes(out, "thinking.budget_tokens")
			if budget.Int() != tc.wantBudget || (tc.wantBudget == 0 && budget.Exists()) {
				t.Fatalf("budget = %s, want %d", budget, tc.wantBudget)
			}
			for _, path := range []string{"messages", "tools", "output_config.format", "max_tokens"} {
				if gjson.GetBytes(out, path).Raw != gjson.Get(body, path).Raw {
					t.Fatalf("unrelated field changed: %s", path)
				}
			}
		})
	}
}

func TestQwen38ModelBoundaries(t *testing.T) {
	for _, model := range []string{"qwen3.8-flash", "qwen3.8-max", "qwen3.8-max-0902", "QWEN3.8-FLASH(low)"} {
		if !thinking.IsQwen38Model(model) {
			t.Errorf("known model not recognized: %s", model)
		}
	}
	for _, model := range []string{"qwen3.8-max-preview", "qwen3.7-plus", "qwen3.8-flash-next", "provider/qwen3.8-flash", "claude-sonnet-4-6"} {
		if thinking.IsQwen38Model(model) {
			t.Errorf("unrelated model matched: %s", model)
		}
	}
}
