package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAICompatDisabledThinking(t *testing.T) {
	for _, kind := range []string{"qwen", "zhipu", "doubao"} {
		for _, payload := range []string{`{}`, `{"enable_thinking":null}`, `{"thinking":{"type":"enabled"}}`, `{"reasoning_effort":"high"}`} {
			if got := NormalizeOpenAICompatDisabledThinking([]byte(payload), kind); string(got) != payload {
				t.Fatalf("%s changed non-disabled control: %s", kind, got)
			}
		}
		payload := []byte(`{"thinking":{"type":"disabled","budget_tokens":1024},"reasoning_effort":"high"}`)
		got := NormalizeOpenAICompatDisabledThinking(payload, kind)
		if kind == "qwen" {
			if gjson.GetBytes(got, "enable_thinking").Type != gjson.False {
				t.Fatalf("Qwen off missing: %s", got)
			}
		} else if gjson.GetBytes(got, "thinking.budget_tokens").Exists() {
			t.Fatalf("disabled budget not removed: %s", got)
		}
	}
	for _, kind := range []string{"openai", "deepseek", "kimi", "unknown"} {
		payload := `{"reasoning_effort":"none"}`
		if got := NormalizeOpenAICompatDisabledThinking([]byte(payload), kind); string(got) != payload {
			t.Fatalf("unrelated %s changed: %s", kind, got)
		}
	}
}

func TestDoubaoSeedThinkingType(t *testing.T) {
	for _, model := range []string{"doubao-seed-2.0-pro", "doubao-seed-2-0-lite-260215", "doubao-seed-2.1-turbo"} {
		for _, mode := range []string{"enabled", "disabled", "auto"} {
			if got := DoubaoSeedThinkingType([]byte(`{"thinking":{"type":"`+mode+`"}}`), model); got != mode {
				t.Fatalf("%s: got %q, want %q", model, got, mode)
			}
		}
	}
	for _, model := range []string{"deepseek-v4-pro", "doubao-seed-1.6-thinking"} {
		if got := DoubaoSeedThinkingType([]byte(`{"thinking":{"type":"disabled"}}`), model); got != "" {
			t.Fatalf("unrelated model %s: %q", model, got)
		}
	}
}

func TestNormalizeDeepSeekResponsesThinking(t *testing.T) {
	for _, payload := range []string{`{"thinking":{"type":"disabled","budget_tokens":1024},"reasoning":{"summary":"auto"}}`, `{"reasoning":{"effort":"none","summary":"auto"}}`} {
		got := NormalizeDeepSeekResponsesThinking([]byte(payload))
		if gjson.GetBytes(got, "reasoning.effort").String() != "none" || gjson.GetBytes(got, "reasoning.summary").String() != "auto" || gjson.GetBytes(got, "thinking").Exists() {
			t.Fatalf("invalid Responses controls: %s", got)
		}
	}
	for _, payload := range []string{`{}`, `{"reasoning":{"effort":"low"}}`, `{"reasoning":{"effort":"max"}}`} {
		if got := NormalizeDeepSeekResponsesThinking([]byte(payload)); string(got) != payload {
			t.Fatalf("non-disabled payload changed: %s", got)
		}
	}
}
