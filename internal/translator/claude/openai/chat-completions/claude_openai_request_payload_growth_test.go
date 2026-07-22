package chat_completions

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var benchmarkClaudeOpenAIRequestOutput []byte

func TestConvertOpenAIRequestToClaudePayloadGrowth(t *testing.T) {
	for _, messageCount := range []int{16, 64, 256, 1024} {
		t.Run(strconv.Itoa(messageCount), func(t *testing.T) {
			input := makeClaudeOpenAIRequestPayload(messageCount)
			original := bytes.Clone(input)

			output := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", input, false)
			if !bytes.Equal(input, original) {
				t.Fatal("input payload was mutated")
			}
			if !gjson.ValidBytes(output) {
				t.Fatalf("invalid output JSON: %s", output)
			}

			root := gjson.ParseBytes(output)
			if got, want := len(root.Get("system").Array()), messageCount/2; got != want {
				t.Fatalf("system block count = %d, want %d", got, want)
			}
			if got, want := len(root.Get("messages").Array()), messageCount-messageCount/4; got != want {
				t.Fatalf("message count = %d, want %d", got, want)
			}
			if got := root.Get("system.0.text").String(); got != "system 0" {
				t.Fatalf("first system block = %q, want %q", got, "system 0")
			}
			if got := root.Get("messages.0.role").String(); got != "user" {
				t.Fatalf("first message role = %q, want user", got)
			}
			if got := root.Get("messages.1.content.#").Int(); got != 3 {
				t.Fatalf("assistant content count = %d, want 3", got)
			}
			if got := root.Get("messages.1.content.0.type").String(); got != "thinking" {
				t.Fatalf("assistant first content type = %q, want thinking", got)
			}
			if got := root.Get("messages.1.content.2.type").String(); got != "tool_use" {
				t.Fatalf("assistant last content type = %q, want tool_use", got)
			}
			if got := root.Get("messages.2.content.0.content.#").Int(); got != 2 {
				t.Fatalf("tool result content count = %d, want 2", got)
			}
			if got, want := len(root.Get("tools").Array()), messageCount/2; got != want {
				t.Fatalf("tool count = %d, want %d", got, want)
			}
			if got := root.Get("tools.0.name").String(); got != "tool_0" {
				t.Fatalf("first tool name = %q, want tool_0", got)
			}

			secondOutput := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", input, false)
			if !bytes.Equal(output, secondOutput) {
				t.Fatal("translation changed output ordering between identical inputs")
			}
		})
	}
}

func BenchmarkPayloadGrowthClaudeOpenAIRequest(b *testing.B) {
	for _, messageCount := range []int{16, 64, 256, 1024} {
		input := makeClaudeOpenAIRequestPayload(messageCount)
		b.Run(strconv.Itoa(messageCount), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))

			var output []byte
			for b.Loop() {
				output = ConvertOpenAIRequestToClaude("claude-sonnet-4-5", input, false)
			}
			benchmarkClaudeOpenAIRequestOutput = output
		})
	}
}

func makeClaudeOpenAIRequestPayload(messageCount int) []byte {
	var builder strings.Builder
	builder.Grow(messageCount * 320)
	builder.WriteString(`{"model":"gpt-4.1","messages":[`)
	for i := 0; i < messageCount; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		switch i % 4 {
		case 0:
			fmt.Fprintf(&builder, `{"role":"system","content":[{"type":"text","text":"system %d"},{"type":"unknown","value":%d},{"type":"text","text":"system tail %d"}]}`, i, i, i)
		case 1:
			fmt.Fprintf(&builder, `{"role":"user","content":[{"type":"text","text":"user %d"},{"type":"unknown","value":%d},{"type":"image_url","image_url":{"url":"https://example.com/%d.png"}}]}`, i, i, i)
		case 2:
			fmt.Fprintf(&builder, `{"role":"assistant","reasoning_content":"reason %d","content":[{"type":"text","text":"assistant %d"},{"type":"unknown","value":%d}],"tool_calls":[{"id":"call_%d","type":"function","function":{"name":"fn_%d","arguments":"{}"}},{"id":"ignored_%d","type":"custom"}]}`, i, i, i, i, i, i)
		case 3:
			fmt.Fprintf(&builder, `{"role":"tool","tool_call_id":"call_%d","content":["raw result %d",{"type":"text","text":"result %d"},{"type":"unknown","value":%d}]}`, i-1, i, i, i)
		}
	}
	builder.WriteString(`],"tools":[`)
	for i := 0; i < messageCount; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		if i%2 == 1 {
			fmt.Fprintf(&builder, `{"type":"custom","name":"ignored_%d"}`, i)
			continue
		}
		fmt.Fprintf(&builder, `{"type":"function","function":{"name":"tool_%d","description":"tool %d","parameters":{"type":"object","properties":{"value":{"type":"string"}}}}}`, i, i)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}
