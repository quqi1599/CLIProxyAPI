package handlers

import (
	"fmt"
	"strings"
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

	if got := meta[coreexecutor.RequestBodyBytesMetadataKey]; got != len(rawJSON) {
		t.Fatalf("request_body_bytes = %#v, want %d", got, len(rawJSON))
	}
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
	if got := meta[coreexecutor.ContentPartCountMetadataKey]; got != 3 {
		t.Fatalf("content_part_count = %#v, want 3", got)
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

	if got := meta[coreexecutor.RequestBodyBytesMetadataKey]; got != len(rawJSON) {
		t.Fatalf("request_body_bytes = %#v, want %d", got, len(rawJSON))
	}
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
	if got := meta[coreexecutor.ContentPartCountMetadataKey]; got != 1 {
		t.Fatalf("content_part_count = %#v, want 1", got)
	}
}

func TestInspectRequestComplexitySingleTraversalParity(t *testing.T) {
	rawJSON := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{},{}],"function_call":{"name":"legacy"},"content":[{"type":"tool_result"},{"type":"text"}]},
			{"role":"tool","content":null}
		],
		"input":[
			{"type":"function_call","role":"tool","content":[{"type":"function_call_output"}]},
			{"type":"message","content":"scalar"}
		],
		"tools":[{"type":"function"},{"type":"web_search"},{"type":"mcp","server_label":"docs"}]
	}`)

	vector, ok := inspectRequestComplexity(rawJSON)
	if !ok {
		t.Fatal("inspectRequestComplexity() rejected valid JSON")
	}
	if vector.BodyBytes != len(rawJSON) {
		t.Fatalf("BodyBytes = %d, want %d", vector.BodyBytes, len(rawJSON))
	}
	if vector.MessageCount != 2 {
		t.Fatalf("MessageCount = %d, want messages precedence count 2", vector.MessageCount)
	}
	if vector.DeclaredToolCount != 3 {
		t.Fatalf("DeclaredToolCount = %d, want 3", vector.DeclaredToolCount)
	}
	if vector.InteractionCount != 7 {
		t.Fatalf("InteractionCount = %d, want 7", vector.InteractionCount)
	}
	if vector.ContentPartCount != 4 {
		t.Fatalf("ContentPartCount = %d, want 4", vector.ContentPartCount)
	}
	if vector.MCPToolCount != 1 || vector.BuiltinToolCount != 1 {
		t.Fatalf("tool shape counts = mcp:%d builtin:%d, want 1/1", vector.MCPToolCount, vector.BuiltinToolCount)
	}
}

func TestInspectRequestComplexityKeepsLegacyFirstKeyAndFallbackSemantics(t *testing.T) {
	rawJSON := []byte(`{
		"messages":[],
		"messages":[{"role":"user","content":"ignored"}],
		"input":[{"type":"function_call","name":"lookup"}],
		"tools":[],
		"tools":[{"type":"function","function":{"name":"ignored"}}]
	}`)

	vector, ok := inspectRequestComplexity(rawJSON)
	if !ok {
		t.Fatal("inspectRequestComplexity() rejected valid JSON")
	}
	if vector.MessageCount != 0 {
		t.Fatalf("MessageCount = %d, want first empty messages array to win", vector.MessageCount)
	}
	if vector.DeclaredToolCount != 0 {
		t.Fatalf("DeclaredToolCount = %d, want first empty tools array to win", vector.DeclaredToolCount)
	}
	if vector.InteractionCount != 1 {
		t.Fatalf("InteractionCount = %d, want input interaction to remain included", vector.InteractionCount)
	}
	if vector.ContentPartCount != 0 {
		t.Fatalf("ContentPartCount = %d, want 0", vector.ContentPartCount)
	}
}

func TestSetRequestShapeAndToolMetadataRejectsInvalidJSONWithoutMutation(t *testing.T) {
	meta := map[string]any{"existing": "value"}
	setRequestShapeAndToolMetadata(meta, []byte(`{"messages":[}`))

	if len(meta) != 1 || meta["existing"] != "value" {
		t.Fatalf("metadata changed for invalid JSON: %#v", meta)
	}
}

func BenchmarkInspectRequestComplexity(b *testing.B) {
	for _, messageCount := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("messages_%d", messageCount), func(b *testing.B) {
			rawJSON := buildComplexityBenchmarkBody(messageCount)
			b.ReportAllocs()
			b.SetBytes(int64(len(rawJSON)))
			b.ResetTimer()
			for range b.N {
				vector, ok := inspectRequestComplexity(rawJSON)
				if !ok {
					b.Fatal("inspectRequestComplexity() rejected benchmark payload")
				}
				benchmarkRequestComplexity = vector
			}
		})
	}
}

var benchmarkRequestComplexity complexityVector

func buildComplexityBenchmarkBody(messageCount int) []byte {
	var builder strings.Builder
	builder.Grow(messageCount * 256)
	builder.WriteString(`{"messages":[`)
	for idx := 0; idx < messageCount; idx++ {
		if idx > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"role":"assistant","content":[{"type":"text","text":"message-%d"},{"type":"tool_result","tool_call_id":"call-%d","content":"ok"}],"tool_calls":[{"id":"call-%d","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, idx, idx, idx)
	}
	builder.WriteString(`],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	return []byte(builder.String())
}
