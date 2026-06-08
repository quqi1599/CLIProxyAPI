package helps

import "testing"

func TestHasClaudeToolUseMarker(t *testing.T) {
	if HasClaudeToolUseMarker([]byte(`{"content":[{"type":"text"}]}`)) {
		t.Fatal("expected false without tool_use")
	}
	if !HasClaudeToolUseMarker([]byte(`{"content":[{"type":"tool_use"}]}`)) {
		t.Fatal("expected true with tool_use")
	}
}

func TestHasClaudeToolUseOrResultMarkers(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{name: "no markers", body: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), want: false},
		{name: "tool use", body: []byte(`{"content":[{"type":"tool_use"}]}`), want: true},
		{name: "tool result", body: []byte(`{"content":[{"type":"tool_result"}]}`), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasClaudeToolUseOrResultMarkers(tt.body); got != tt.want {
				t.Fatalf("HasClaudeToolUseOrResultMarkers() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasClaudeToolResultMarker(t *testing.T) {
	if HasClaudeToolResultMarker([]byte(`{"content":[{"type":"text"}]}`)) {
		t.Fatal("expected false without tool_result")
	}
	if !HasClaudeToolResultMarker([]byte(`{"content":[{"type":"tool_result"}]}`)) {
		t.Fatal("expected true with tool_result")
	}
}

func TestHasClaudeCountTokensPreflightMarkers(t *testing.T) {
	body := []byte(`{
		"betas":["prompt-caching-2026-01-05"],
		"tools":[{"type":"tool_search"}],
		"messages":[{"role":"system","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]
	}`)

	if !HasClaudeSystemRoleMarker(body) {
		t.Fatal("expected system-role marker")
	}
	if !HasClaudeCacheControlMarker(body) {
		t.Fatal("expected cache_control marker")
	}
	if !HasClaudeBetasMarker(body) {
		t.Fatal("expected betas marker")
	}
	if !HasClaudeToolsMarker(body) {
		t.Fatal("expected tools marker")
	}
	if !HasClaudeToolSearchMarker(body) {
		t.Fatal("expected tool_search marker")
	}
}
