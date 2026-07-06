package handlers

import (
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSetRequestShapeAndToolMetadataForChatPayload(t *testing.T) {
	meta := make(map[string]any)
	rawJSON := []byte(`{
		"messages":[
			{"role":"assistant","content":"calling","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"},
			{"role":"user","content":"hi"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
	}`)

	setRequestShapeAndToolMetadata(meta, rawJSON)

	if got := meta[coreexecutor.MessageCountMetadataKey]; got != 3 {
		t.Fatalf("message_count = %#v, want 3", got)
	}
	if got := meta[coreexecutor.ToolCountMetadataKey]; got != 2 {
		t.Fatalf("tool_count = %#v, want 2", got)
	}
	if got := meta[coreexecutor.DeclaredToolCountMetadataKey]; got != 1 {
		t.Fatalf("declared_tool_count = %#v, want 1", got)
	}
	if got := meta[coreexecutor.ToolInteractionCountMetadataKey]; got != 2 {
		t.Fatalf("tool_interaction_count = %#v, want 2", got)
	}
}

func TestSetRequestShapeAndToolMetadataForResponsesInput(t *testing.T) {
	meta := make(map[string]any)
	rawJSON := []byte(`{
		"input":[
			{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools":[{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.test"}]
	}`)

	setRequestShapeAndToolMetadata(meta, rawJSON)

	if got := meta[coreexecutor.MessageCountMetadataKey]; got != 3 {
		t.Fatalf("message_count = %#v, want 3", got)
	}
	if got := meta[coreexecutor.ToolCountMetadataKey]; got != 2 {
		t.Fatalf("tool_count = %#v, want 2", got)
	}
	if got := meta[coreexecutor.DeclaredToolCountMetadataKey]; got != 1 {
		t.Fatalf("declared_tool_count = %#v, want 1", got)
	}
	if got := meta[coreexecutor.ToolInteractionCountMetadataKey]; got != 2 {
		t.Fatalf("tool_interaction_count = %#v, want 2", got)
	}
	if got := meta[coreexecutor.MCPToolCountMetadataKey]; got != 1 {
		t.Fatalf("mcp_tool_count = %#v, want 1", got)
	}
}
