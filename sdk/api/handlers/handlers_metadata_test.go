package handlers

import (
	"strings"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

func TestRequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey(t *testing.T) {
	ctx := WithExecutionSessionID(context.Background(), "session-1")

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("ExecutionSessionMetadataKey = %v, want %q", got, "session-1")
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetRequestShapeMetadataCountsChatMessagesAndToolCalls(t *testing.T) {
	meta := make(map[string]any)

	setRequestShapeMetadata(meta, []byte(`{
		"messages": [
			{"role":"user","content":"secret prompt"},
			{"role":"assistant","tool_calls":[{"id":"call_1"},{"id":"call_2"}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		],
		"tools": [{"type":"function"},{"type":"function"},{"type":"function"}]
	}`))

	if got := meta[coreexecutor.MessageCountMetadataKey]; got != 3 {
		t.Fatalf("MessageCountMetadataKey = %v, want 3", got)
	}
	if got := meta[coreexecutor.ToolCountMetadataKey]; got != 3 {
		t.Fatalf("ToolCountMetadataKey = %v, want 3", got)
	}
}

func TestSetRequestShapeMetadataCountsResponsesInputAndDeclaredTools(t *testing.T) {
	meta := make(map[string]any)

	setRequestShapeMetadata(meta, []byte(`{
		"input": [
			{"type":"message","role":"user","content":"hello"},
			{"type":"message","role":"assistant","content":"world"}
		],
		"tools": [{"type":"function"},{"type":"web_search"}]
	}`))

	if got := meta[coreexecutor.MessageCountMetadataKey]; got != 2 {
		t.Fatalf("MessageCountMetadataKey = %v, want 2", got)
	}
	if got := meta[coreexecutor.ToolCountMetadataKey]; got != 2 {
		t.Fatalf("ToolCountMetadataKey = %v, want 2", got)
	}
}

func TestSetRequestShapeMetadataAddsRedactedToolShapeTelemetry(t *testing.T) {
	meta := make(map[string]any)

	setRequestShapeMetadata(meta, []byte(`{
		"input": [
			{"type":"message","role":"user","content":"hello"},
			{"type":"mcp_call","server_label":"private-docs","name":"mcp__files__read"},
			{"type":"web_search_call","name":"$web_search"}
		],
		"messages": [
			{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"lookup_customer","arguments":"{\"id\":\"secret\"}"}}]},
			{"role":"tool","name":"lookup_customer","content":"secret result"}
		],
		"tools": [
			{"type":"mcp","server_label":"private-docs"},
			{"type":"builtin_function","function":{"name":"$web_search"}}
		]
	}`))

	if got := meta[coreexecutor.DeclaredToolCountMetadataKey]; got != 2 {
		t.Fatalf("DeclaredToolCountMetadataKey = %v, want 2", got)
	}
	if got := meta[coreexecutor.ToolInteractionCountMetadataKey]; got != 4 {
		t.Fatalf("ToolInteractionCountMetadataKey = %v, want 4", got)
	}
	if got := meta[coreexecutor.MCPToolCountMetadataKey]; got != 2 {
		t.Fatalf("MCPToolCountMetadataKey = %v, want 2", got)
	}
	if got := meta[coreexecutor.BuiltinToolCountMetadataKey]; got != 2 {
		t.Fatalf("BuiltinToolCountMetadataKey = %v, want 2", got)
	}

	types, _ := meta[coreexecutor.ToolShapeTypesMetadataKey].(string)
	for _, want := range []string{"mcp", "mcp_call", "web_search_call", "tool_call", "tool_result"} {
		if !strings.Contains(types, want) {
			t.Fatalf("tool types %q missing %q", types, want)
		}
	}

	hashes, _ := meta[coreexecutor.ToolNameHashesMetadataKey].(string)
	if hashes == "" {
		t.Fatal("expected tool name hashes")
	}
	for _, raw := range []string{"private-docs", "mcp__files__read", "$web_search", "lookup_customer", "secret"} {
		if strings.Contains(hashes, raw) {
			t.Fatalf("tool name hashes leaked raw value %q in %q", raw, hashes)
		}
	}
}
