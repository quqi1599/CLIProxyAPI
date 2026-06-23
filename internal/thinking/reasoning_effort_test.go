package thinking

import "testing"

func TestExtractReasoningEffortUsesSuffixOverBody(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning_effort":"low"}`), "openai", "gpt-5.4(high)")
	if got != "high" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "high")
	}
}

func TestExtractReasoningEffortConvertsBudgetToLevel(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`), "claude", "claude-sonnet-4-5")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortSupportsOpenAIResponses(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning":{"effort":"medium"}}`), "openai-response", "gpt-5.4")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortSupportsOpenAIStyleThinkingToggle(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "enabled without effort maps to auto",
			body: `{"thinking":{"type":"enabled"}}`,
			want: "auto",
		},
		{
			name: "disabled maps to none",
			body: `{"thinking":{"type":"disabled"},"reasoning_effort":"high"}`,
			want: "none",
		},
		{
			name: "enabled uses explicit effort",
			body: `{"thinking":{"type":"enabled"},"reasoning_effort":"max"}`,
			want: "max",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractReasoningEffort([]byte(tt.body), "openai", "deepseek-v4-pro")
			if got != tt.want {
				t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractReasoningEffortMissingConfigIsEmpty(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai", "gpt-5.4")
	if got != "" {
		t.Fatalf("ExtractReasoningEffort() = %q, want empty", got)
	}
}
