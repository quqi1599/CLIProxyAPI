package helps

import (
	"encoding/json"
	"testing"
)

func TestReduceCodexToolSurfaceKeepsReferencedAndBuiltinTools(t *testing.T) {
	tools := []any{
		map[string]any{"type": "function", "name": "a"},
		map[string]any{"type": "function", "name": "b"},
		map[string]any{"type": "function", "name": "c"},
		map[string]any{"type": "function", "name": "d"},
		map[string]any{"type": "web_search"},
	}
	body, err := json.Marshal(map[string]any{
		"input": []any{map[string]any{"type": "function_call", "name": "d"}},
		"tools": tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, reduction := ReduceCodexToolSurface(body, 2)
	if reduction.Dropped != 2 || reduction.Kept != 2 {
		t.Fatalf("reduction = %+v, want dropped=2 kept=2", reduction)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["tools"].([]any)
	if len(result) != 3 {
		t.Fatalf("tools length = %d, want 3", len(result))
	}
	seen := map[string]bool{}
	for _, raw := range result {
		tool := raw.(map[string]any)
		seen[stringValue(tool["name"])] = true
		seen[stringValue(tool["type"])] = true
	}
	if !seen["d"] || !seen["web_search"] {
		t.Fatalf("referenced or builtin tool was dropped: %#v", result)
	}
}

func TestReduceCodexToolSurfaceHandlesAdditionalTools(t *testing.T) {
	body := []byte("{\"input\":[{\"type\":\"additional_tools\",\"tools\":[{\"type\":\"function\",\"name\":\"a\"},{\"type\":\"function\",\"name\":\"b\"},{\"type\":\"function\",\"name\":\"c\"}]}]}")
	got, reduction := ReduceCodexToolSurface(body, 2)
	if reduction.Dropped != 1 {
		t.Fatalf("reduction = %+v, want one dropped tool", reduction)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	tools := decoded["input"].([]any)[0].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("additional tools length = %d, want 2", len(tools))
	}
}
