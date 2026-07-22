package claude

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var benchmarkCodexClaudeResponseOutput []byte

func BenchmarkPayloadGrowthCodexClaudeNonStreamResponse(b *testing.B) {
	for _, itemCount := range []int{16, 64, 256, 1024} {
		rawJSON := makeCodexClaudeResponsePayload(itemCount)
		b.Run(strconv.Itoa(itemCount), func(b *testing.B) {
			original := bytes.Clone(rawJSON)
			output := ConvertCodexResponseToClaudeNonStream(context.Background(), "", nil, nil, rawJSON, nil)
			if !bytes.Equal(rawJSON, original) {
				b.Fatal("input payload was mutated")
			}
			if got, want := len(gjson.GetBytes(output, "content").Array()), itemCount/4*6; got != want {
				b.Fatalf("content block count = %d, want %d", got, want)
			}
			if got := gjson.GetBytes(output, "content.3.type").String(); got != "server_tool_use" {
				b.Fatalf("web search block type = %q, want server_tool_use", got)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(rawJSON)))
			b.ResetTimer()
			for b.Loop() {
				output = ConvertCodexResponseToClaudeNonStream(context.Background(), "", nil, nil, rawJSON, nil)
			}
			benchmarkCodexClaudeResponseOutput = output
		})
	}
}

func makeCodexClaudeResponsePayload(itemCount int) []byte {
	var builder strings.Builder
	builder.Grow(itemCount * 260)
	builder.WriteString(`{"type":"response.completed","response":{"id":"response_1","model":"gpt-5","usage":{"input_tokens":1,"output_tokens":2},"output":[`)
	for i := 0; i < itemCount; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		switch i % 4 {
		case 0:
			fmt.Fprintf(&builder, `{"type":"reasoning","encrypted_content":"signature_%d","summary":[{"text":"reason %d"}]}`, i, i)
		case 1:
			fmt.Fprintf(&builder, `{"type":"message","content":[{"type":"output_text","text":"text %d a"},{"type":"output_text","text":"text %d b"}]}`, i, i)
		case 2:
			fmt.Fprintf(&builder, `{"type":"web_search_call","id":"web_%d","action":{"query":"query %d"},"results":[{"title":"A","url":"https://example.com/%d/a"},{"title":"B","url":"https://example.com/%d/b"}]}`, i, i, i, i)
		case 3:
			fmt.Fprintf(&builder, `{"type":"function_call","call_id":"call_%d","name":"tool_%d","arguments":"{\"value\":%d}"}`, i, i, i)
		}
	}
	builder.WriteString(`]}}`)
	return []byte(builder.String())
}
