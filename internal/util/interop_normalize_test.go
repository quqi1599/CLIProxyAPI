package util

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var benchmarkNormalizedJSON []byte

func TestNormalizeOpenAIResponsesRequestJSON_ConvertsClaudeBlocks(t *testing.T) {
	input := []byte(`{
		"input":[
			{
				"role":"assistant",
				"content":[
					{"type":"text","text":"checking"},
					{"type":"tool_use","id":"call_1","name":"sessions_list","input":{"limit":10}}
				]
			},
			{
				"role":"user",
				"content":[
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"}
				]
			}
		]
	}`)

	out := NormalizeOpenAIResponsesRequestJSON(input)
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 4 {
		t.Fatalf("expected 4 normalized items, got %d: %s", len(items), gjson.GetBytes(out, "input").Raw)
	}
	if items[1].Get("type").String() != "function_call" {
		t.Fatalf("expected item 1 function_call, got %s", items[1].Raw)
	}
	if items[2].Get("type").String() != "message" || items[3].Get("type").String() != "function_call_output" {
		t.Fatalf("expected message + function_call_output tail: %s", gjson.GetBytes(out, "input").Raw)
	}
}

func TestNormalizeOpenAIResponsesRequestJSON_PreservesUnknownMessageFields(t *testing.T) {
	const vendorExtension = `{"trace_id":"trace-1","nested":{"keep":["a",{"b":2}]},"enabled":true}`
	input := []byte(`{"model":"test-model","input":[{"type":"message","role":"assistant","vendor_extension":` + vendorExtension + `,"status":"in_progress","content":[{"type":"text","text":"hello"}]}]}`)
	original := append([]byte(nil), input...)

	out := NormalizeOpenAIResponsesRequestJSON(input)
	if !bytes.Equal(input, original) {
		t.Fatal("normalization mutated the input buffer")
	}

	message := gjson.GetBytes(out, "input.0")
	if got := message.Get("vendor_extension").Raw; got != vendorExtension {
		t.Fatalf("vendor_extension changed or was dropped: got %s, want %s", got, vendorExtension)
	}
	if got := message.Get("status").String(); got != "in_progress" {
		t.Fatalf("unknown message-level status changed or was dropped: %q", got)
	}
	if got := message.Get("content.0.type").String(); got != "output_text" {
		t.Fatalf("known text conversion changed: got %q, want output_text", got)
	}

	again := NormalizeOpenAIResponsesRequestJSON(out)
	if !bytes.Equal(again, out) {
		t.Fatalf("normalization is not idempotent:\nfirst:  %s\nsecond: %s", out, again)
	}
}

func TestNormalizeOpenAIResponsesRequestJSON_PreservesUnknownContentPart(t *testing.T) {
	const unknownPart = `{"type":"vendor_blob","payload":{"opaque":"keep-me","items":[1,2,3]},"vendor_flag":true}`
	input := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"text","text":"hello"},` + unknownPart + `]}]}`)

	out := NormalizeOpenAIResponsesRequestJSON(input)
	content := gjson.GetBytes(out, "input.0.content").Array()
	if len(content) != 2 {
		t.Fatalf("expected 2 content parts, got %d: %s", len(content), gjson.GetBytes(out, "input.0.content").Raw)
	}
	if got := content[0].Get("type").String(); got != "input_text" {
		t.Fatalf("known text conversion changed: got %q, want input_text", got)
	}
	if got := content[1].Raw; got != unknownPart {
		t.Fatalf("unknown content part changed or was dropped: got %s, want %s", got, unknownPart)
	}
}

func TestNormalizeOpenAIResponsesRequestJSON_CanonicalInputReturnsOriginalSlice(t *testing.T) {
	input := []byte(`{"model":"test-model","request_extension":{"keep":true},"input":[{"type":"message","role":"user","message_extension":{"version":1},"content":[{"type":"input_text","text":"hello"},{"type":"vendor_part","payload":{"opaque":"x"}}]}]}`)
	original := append([]byte(nil), input...)

	out := NormalizeOpenAIResponsesRequestJSON(input)
	if !bytes.Equal(input, original) {
		t.Fatal("normalization mutated canonical input")
	}
	if !bytes.Equal(out, input) {
		t.Fatalf("canonical Responses input changed:\ninput:  %s\noutput: %s", input, out)
	}
	if len(out) == 0 || &out[0] != &input[0] {
		t.Fatal("unchanged canonical request should reuse the input byte slice")
	}

	again := NormalizeOpenAIResponsesRequestJSON(out)
	if !bytes.Equal(again, out) {
		t.Fatalf("canonical normalization is not idempotent:\nfirst:  %s\nsecond: %s", out, again)
	}
	if len(again) == 0 || &again[0] != &out[0] {
		t.Fatal("idempotent normalization should reuse the normalized byte slice")
	}
}

func TestNormalizeOpenAIChatRequestJSON_ConvertsClaudeBlocks(t *testing.T) {
	input := []byte(`{
		"messages":[
			{
				"role":"assistant",
				"content":[
					{"type":"text","text":"checking"},
					{"type":"tool_use","id":"call_1","name":"sessions_list","input":{"limit":10}},
					{"type":"thinking","thinking":"internal"}
				]
			},
			{
				"role":"user",
				"content":[
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"}
				]
			}
		]
	}`)

	out := NormalizeOpenAIChatRequestJSON(input)
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 normalized messages, got %d: %s", len(msgs), gjson.GetBytes(out, "messages").Raw)
	}
	if !msgs[0].Get("tool_calls").IsArray() {
		t.Fatalf("assistant tool_calls should be synthesized: %s", msgs[0].Raw)
	}
	if got := msgs[0].Get("reasoning_content").String(); got != "internal" {
		t.Fatalf("expected reasoning_content=internal, got %q", got)
	}
	if got := msgs[0].Get("content.0.text").String(); got != "checking" {
		t.Fatalf("expected assistant text to be preserved, got %q", got)
	}
	if got := msgs[1].Get("role").String(); got != "tool" {
		t.Fatalf("expected appended tool role, got %q: %s", got, msgs[1].Raw)
	}
}

func TestNormalizeOpenAIChatRequestJSON_NormalizesImageVariants(t *testing.T) {
	input := []byte(`{
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
					{"type":"input_image","image_url":"data:image/jpeg;base64,BBBB","detail":"high"},
					{"type":"image_url","image_url":"data:image/gif;base64,CCCC"}
				]
			}
		]
	}`)

	out := NormalizeOpenAIChatRequestJSON(input)
	content := gjson.GetBytes(out, "messages.0.content")
	if got := content.Get("0.type").String(); got != "image_url" {
		t.Fatalf("expected Claude image to become image_url, got %q: %s", got, content.Raw)
	}
	if got := content.Get("0.image_url.url").String(); got != "data:image/png;base64,AAAA" {
		t.Fatalf("unexpected first image URL %q", got)
	}
	if got := content.Get("1.image_url.url").String(); got != "data:image/jpeg;base64,BBBB" {
		t.Fatalf("unexpected second image URL %q", got)
	}
	if got := content.Get("1.image_url.detail").String(); got != "high" {
		t.Fatalf("expected detail=high, got %q", got)
	}
	if got := content.Get("2.image_url.url").String(); got != "data:image/gif;base64,CCCC" {
		t.Fatalf("unexpected string image_url normalization %q", got)
	}
}

func TestNormalizeOpenAIChatRequestJSON_PlacesToolResultBeforeUserText(t *testing.T) {
	input := []byte(`{
		"messages":[
			{
				"role":"assistant",
				"content":[
					{"type":"tool_use","id":"call_1","name":"sessions_list","input":{"limit":10}}
				]
			},
			{
				"role":"user",
				"content":[
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"},
					{"type":"text","text":"continue"}
				]
			}
		]
	}`)

	out := NormalizeOpenAIChatRequestJSON(input)
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 normalized messages, got %d: %s", len(msgs), gjson.GetBytes(out, "messages").Raw)
	}
	if got := msgs[1].Get("role").String(); got != "tool" {
		t.Fatalf("expected tool result to immediately follow assistant tool_calls, got %q: %s", got, msgs[1].Raw)
	}
	if got := msgs[1].Get("tool_call_id").String(); got != "call_1" {
		t.Fatalf("expected tool_call_id call_1, got %q", got)
	}
	if got := msgs[2].Get("role").String(); got != "user" {
		t.Fatalf("expected trailing user message after tool result, got %q: %s", got, msgs[2].Raw)
	}
	if got := msgs[2].Get("content.0.text").String(); got != "continue" {
		t.Fatalf("expected user text to remain after tool result, got %q", got)
	}
}

func TestNormalizeOpenAIResponsesRequestJSON_NormalizesImageVariants(t *testing.T) {
	input := []byte(`{
		"input":[
			{
				"role":"user",
				"content":[
					{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"low"}},
					{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"BBBB"}},
					{"type":"input_image","url":"https://example.com/cat.png"}
				]
			}
		]
	}`)

	out := NormalizeOpenAIResponsesRequestJSON(input)
	content := gjson.GetBytes(out, "input.0.content")
	if got := content.Get("0.type").String(); got != "input_image" {
		t.Fatalf("expected image_url to become input_image, got %q: %s", got, content.Raw)
	}
	if got := content.Get("0.image_url").String(); got != "data:image/png;base64,AAAA" {
		t.Fatalf("unexpected first image URL %q", got)
	}
	if got := content.Get("0.detail").String(); got != "low" {
		t.Fatalf("expected detail=low, got %q", got)
	}
	if got := content.Get("1.image_url").String(); got != "data:image/jpeg;base64,BBBB" {
		t.Fatalf("unexpected Claude image URL %q", got)
	}
	if got := content.Get("2.image_url").String(); got != "https://example.com/cat.png" {
		t.Fatalf("unexpected url alias normalization %q", got)
	}
}

func TestNormalizeOpenAIChatRequestJSON_LargeHistoryPreservesMessages(t *testing.T) {
	const messageCount = 256
	input := buildChatHistory(messageCount, 1024)

	out := NormalizeOpenAIChatRequestJSON(input)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != messageCount {
		t.Fatalf("expected %d messages, got %d", messageCount, len(messages))
	}
	if got := messages[0].Get("content.0.type").String(); got != "text" {
		t.Fatalf("expected normalized text content, got %q", got)
	}
	if got := messages[messageCount-1].Get("metadata.index").Int(); got != messageCount-1 {
		t.Fatalf("expected final metadata index %d, got %d", messageCount-1, got)
	}
	if got := len(messages[messageCount-1].Get("content.0.text").String()); got != 1024 {
		t.Fatalf("expected final text length 1024, got %d", got)
	}
}

func TestNormalizeOpenAIChatRequestJSON_NoChangeReturnsInput(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	out := NormalizeOpenAIChatRequestJSON(input)
	if len(out) == 0 || &out[0] != &input[0] {
		t.Fatal("unchanged request should reuse the input byte slice")
	}
}

func BenchmarkNormalizeOpenAIChatRequestJSON_LongHistory(b *testing.B) {
	for _, messageCount := range []int{16, 64, 256, 1024} {
		b.Run(strconv.Itoa(messageCount), func(b *testing.B) {
			input := buildChatHistory(messageCount, 256)
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for range b.N {
				benchmarkNormalizedJSON = NormalizeOpenAIChatRequestJSON(input)
			}
		})
	}
}

func BenchmarkNormalizeOpenAIResponsesRequestJSON_LongHistory(b *testing.B) {
	for _, itemCount := range []int{16, 64, 256, 1024} {
		b.Run(strconv.Itoa(itemCount), func(b *testing.B) {
			input := buildResponsesHistory(itemCount, 256)
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for range b.N {
				benchmarkNormalizedJSON = NormalizeOpenAIResponsesRequestJSON(input)
			}
		})
	}
}

func buildChatHistory(messageCount, textSize int) []byte {
	text := strings.Repeat("x", textSize)
	var builder strings.Builder
	builder.Grow(messageCount * (textSize + 96))
	builder.WriteString(`{"messages":[`)
	for idx := 0; idx < messageCount; idx++ {
		if idx > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"role":"user","metadata":{"index":`)
		builder.WriteString(strconv.Itoa(idx))
		builder.WriteString(`},"content":[{"type":"input_text","text":`)
		builder.WriteString(fmt.Sprintf("%q", text))
		builder.WriteString(`}]}`)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func buildResponsesHistory(itemCount, textSize int) []byte {
	text := strings.Repeat("x", textSize)
	var builder strings.Builder
	builder.Grow(itemCount * (textSize + 80))
	builder.WriteString(`{"input":[`)
	for idx := 0; idx < itemCount; idx++ {
		if idx > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"type":"message","role":"user","content":[{"type":"text","text":`)
		builder.WriteString(fmt.Sprintf("%q", text))
		builder.WriteString(`}]}`)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}
