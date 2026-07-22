package helps

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeToolHistoryLargePayloadPreservesOrderAndInput(t *testing.T) {
	const items = 512
	dedupeInput := buildClaudeToolResultDedupePayload(items)
	dedupeOriginal := bytes.Clone(dedupeInput)

	deduped, removed, errDedupe := DedupeClaudeToolResultParts(dedupeInput)
	if errDedupe != nil {
		t.Fatalf("DedupeClaudeToolResultParts() error = %v", errDedupe)
	}
	if removed != items {
		t.Fatalf("removed = %d, want %d", removed, items)
	}
	if !bytes.Equal(dedupeInput, dedupeOriginal) {
		t.Fatal("DedupeClaudeToolResultParts mutated its input")
	}
	if got := len(gjson.GetBytes(deduped, "messages").Array()); got != items {
		t.Fatalf("messages = %d, want %d", got, items)
	}
	lastContent := gjson.GetBytes(deduped, "messages.511.content")
	if got := len(lastContent.Array()); got != 2 {
		t.Fatalf("last content parts = %d, want 2", got)
	}
	if got := lastContent.Get("1.content").String(); got != "new-511" {
		t.Fatalf("last tool result = %q, want new-511", got)
	}
	if got := lastContent.Get("1.unknown").Int(); got != 511 {
		t.Fatalf("unknown field = %d, want 511", got)
	}
	assertTopLevelOrder(t, deduped, `"before"`, `"messages"`, `"after"`)

	reorderInput := buildClaudeToolResultReorderPayload(items)
	reorderOriginal := bytes.Clone(reorderInput)
	reordered, count, errReorder := MoveClaudeToolResultsBeforeUserContent(reorderInput)
	if errReorder != nil {
		t.Fatalf("MoveClaudeToolResultsBeforeUserContent() error = %v", errReorder)
	}
	if count != items {
		t.Fatalf("reordered = %d, want %d", count, items)
	}
	if !bytes.Equal(reorderInput, reorderOriginal) {
		t.Fatal("MoveClaudeToolResultsBeforeUserContent mutated its input")
	}
	lastUserContent := gjson.GetBytes(reordered, "messages.1023.content")
	if got := lastUserContent.Get("0.type").String(); got != "tool_result" {
		t.Fatalf("first reordered type = %q, want tool_result", got)
	}
	if got := lastUserContent.Get("1.type").String(); got != "text" {
		t.Fatalf("second reordered type = %q, want text", got)
	}
	if got := lastUserContent.Get("0.unknown").Int(); got != 511 {
		t.Fatalf("reordered unknown field = %d, want 511", got)
	}
	assertTopLevelOrder(t, reordered, `"before"`, `"messages"`, `"after"`)
}

func TestStripVertexOpenAIResponsesToolCallIDsLargePayloadPreservesOrderAndInput(t *testing.T) {
	const items = 512
	input := buildVertexToolCallPayload(items)
	original := bytes.Clone(input)

	output := StripVertexOpenAIResponsesToolCallIDs(input, "openai-response")
	if !bytes.Equal(input, original) {
		t.Fatal("StripVertexOpenAIResponsesToolCallIDs mutated its input")
	}
	if got := len(gjson.GetBytes(output, "contents").Array()); got != items {
		t.Fatalf("contents = %d, want %d", got, items)
	}
	last := gjson.GetBytes(output, "contents.511")
	if last.Get("parts.0.functionCall.id").Exists() || last.Get("parts.1.functionResponse.id").Exists() {
		t.Fatal("Vertex call IDs were not removed")
	}
	if got := last.Get("parts.0.functionCall.unknown").Int(); got != 511 {
		t.Fatalf("functionCall unknown field = %d, want 511", got)
	}
	if got := last.Get("content_unknown").Int(); got != 511 {
		t.Fatalf("content unknown field = %d, want 511", got)
	}
	assertTopLevelOrder(t, output, `"before"`, `"contents"`, `"after"`)
}

func BenchmarkClaudeToolHistoryLargePayload(b *testing.B) {
	input := buildClaudeToolResultDedupePayload(1024)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := DedupeClaudeToolResultParts(input); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStripVertexOpenAIResponsesToolCallIDsLargePayload(b *testing.B) {
	input := buildVertexToolCallPayload(1024)
	b.ReportAllocs()
	for b.Loop() {
		_ = StripVertexOpenAIResponsesToolCallIDs(input, "openai-response")
	}
}

func buildClaudeToolResultDedupePayload(items int) []byte {
	var out strings.Builder
	out.WriteString(`{"before":{"keep":true},"messages":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&out, `{"role":"user","content":[{"type":"text","text":"text-%d"},{"type":"tool_result","tool_use_id":"call-%d","content":"old-%d"},{"type":"tool_result","tool_use_id":"call-%d","content":"new-%d","unknown":%d}],"message_unknown":%d}`, i, i, i, i, i, i, i)
	}
	out.WriteString(`],"after":{"keep":true}}`)
	return []byte(out.String())
}

func buildClaudeToolResultReorderPayload(items int) []byte {
	var out strings.Builder
	out.WriteString(`{"before":{"keep":true},"messages":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&out, `{"role":"assistant","content":[{"type":"tool_use","id":"call-%d","name":"tool","unknown":%d}]},{"role":"user","content":[{"type":"text","text":"text-%d"},{"type":"tool_result","tool_use_id":"call-%d","content":"result-%d","unknown":%d}],"message_unknown":%d}`, i, i, i, i, i, i, i)
	}
	out.WriteString(`],"after":{"keep":true}}`)
	return []byte(out.String())
}

func buildVertexToolCallPayload(items int) []byte {
	var out strings.Builder
	out.WriteString(`{"before":{"keep":true},"contents":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&out, `{"role":"model","parts":[{"functionCall":{"name":"tool","id":"call-%d","unknown":%d},"part_unknown":"call"},{"functionResponse":{"name":"tool","id":"call-%d","response":{"ok":true}},"part_unknown":"response"}],"content_unknown":%d}`, i, i, i, i)
	}
	out.WriteString(`],"after":{"keep":true}}`)
	return []byte(out.String())
}

func assertTopLevelOrder(t *testing.T, payload []byte, fields ...string) {
	t.Helper()
	previous := -1
	for _, field := range fields {
		index := bytes.Index(payload, []byte(field))
		if index <= previous {
			t.Fatalf("field %s index = %d after %d; payload order changed", field, index, previous)
		}
		previous = index
	}
}
