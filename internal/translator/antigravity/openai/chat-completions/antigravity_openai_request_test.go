package chat_completions

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToAntigravitySkipsEmptyTextPartsWithoutNulls(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3-flash",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": ""},
					{"type": "input_audio", "input_audio": {"data": "SUQzBA==", "format": "mp3"}}
				]
			},
			{
				"role": "assistant",
				"content": [{"type": "text", "text": ""}],
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "read_file", "arguments": "{\"path\":\"a.txt\"}"}
				}]
			},
			{"role": "tool", "tool_call_id": "call_1", "content": "{\"output\":\"ok\"}"},
			{"role": "user", "content": "done"}
		]
	}`

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), false)
	userParts := gjson.GetBytes(result, "request.contents.0.parts").Array()
	if len(userParts) != 1 {
		t.Fatalf("user parts length = %d, want 1. Output: %s", len(userParts), result)
	}
	if userParts[0].Type == gjson.Null {
		t.Fatalf("user parts.0 is null. Output: %s", result)
	}
	if got := userParts[0].Get("inlineData.mime_type").String(); got != "audio/mpeg" {
		t.Fatalf("audio mime_type = %q, want audio/mpeg. Output: %s", got, result)
	}

	assistantParts := gjson.GetBytes(result, "request.contents.1.parts").Array()
	if len(assistantParts) != 1 {
		t.Fatalf("assistant parts length = %d, want 1. Output: %s", len(assistantParts), result)
	}
	if assistantParts[0].Type == gjson.Null {
		t.Fatalf("assistant parts.0 is null. Output: %s", result)
	}
	if !assistantParts[0].Get("functionCall").Exists() {
		t.Fatalf("functionCall missing. Output: %s", result)
	}
}

func TestConvertOpenAIRequestToAntigravityPreservesLargePartOrder(t *testing.T) {
	const partCount = 2048
	parts := make([]map[string]any, 0, partCount)
	for index := 0; index < partCount; index++ {
		parts = append(parts, map[string]any{"type": "text", "text": fmt.Sprintf("part-%04d", index)})
	}
	input, err := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": parts}},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", input, false)
	outputParts := gjson.GetBytes(result, "request.contents.0.parts").Array()
	if len(outputParts) != partCount {
		t.Fatalf("parts length = %d, want %d", len(outputParts), partCount)
	}
	for _, index := range []int{0, partCount / 2, partCount - 1} {
		want := fmt.Sprintf("part-%04d", index)
		if got := outputParts[index].Get("text").String(); got != want {
			t.Fatalf("parts[%d] = %q, want %q", index, got, want)
		}
	}
}

func TestConvertOpenAIRequestToAntigravityPreservesToolFieldsAndCategoryOrder(t *testing.T) {
	input := []byte(`{
		"messages":[{"role":"user","content":"search"}],
		"tools":[
			{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object","x-schema":"keep"},"x-vendor":{"enabled":true},"strict":true}},
			{"type":"web_search"},
			{"google_search":{"mode":"dynamic"}},
			{"code_execution":{"runtime":"go"}},
			{"url_context":{"scope":"page"}}
		]
	}`)

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", input, false)
	tools := gjson.GetBytes(result, "request.tools").Array()
	if len(tools) != 5 {
		t.Fatalf("tools length = %d, want 5: %s", len(tools), result)
	}
	declaration := tools[0].Get("functionDeclarations.0")
	if declaration.Get("strict").Exists() {
		t.Fatalf("strict should be removed: %s", declaration.Raw)
	}
	if !declaration.Get("x-vendor.enabled").Bool() || declaration.Get("parametersJsonSchema.x-schema").String() != "keep" {
		t.Fatalf("function declaration fields were not preserved: %s", declaration.Raw)
	}
	for index, path := range []string{"googleSearch", "googleSearch.mode", "codeExecution.runtime", "urlContext.scope"} {
		if !tools[index+1].Get(path).Exists() {
			t.Fatalf("tools[%d] missing %s: %s", index+1, path, tools[index+1].Raw)
		}
	}
}

func TestConvertOpenAIRequestToAntigravityPreservesReasoningAndSkipsEmptyAssistant(t *testing.T) {
	inputJSON := `{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"visible","reasoning_content":"thinking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"{\"output\":\"ok\"}"},
		{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"","arguments":"{}"}}]},
		{"role":"user","content":"done"}
	]}`

	result := ConvertOpenAIRequestToAntigravity("gemini-3-flash", []byte(inputJSON), true)
	contents := gjson.GetBytes(result, "request.contents").Array()
	if len(contents) != 4 {
		t.Fatalf("contents length = %d, want 4. Output: %s", len(contents), result)
	}
	parts := contents[1].Get("parts").Array()
	if len(parts) != 3 || parts[0].Get("text").String() != "thinking" || !parts[0].Get("thought").Bool() {
		t.Fatalf("reasoning part was not preserved first. Output: %s", result)
	}
	if parts[1].Get("text").String() != "visible" || parts[2].Get("functionCall.name").String() != "read_file" {
		t.Fatalf("visible content or function call missing. Output: %s", result)
	}
}
