package gemini

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var benchmarkClaudeGeminiRequestOutput []byte

func BenchmarkPayloadGrowthClaudeGeminiRequest(b *testing.B) {
	for _, contentCount := range []int{16, 64, 256, 1024} {
		input := makeClaudeGeminiRequestPayload(contentCount)
		b.Run(strconv.Itoa(contentCount), func(b *testing.B) {
			original := bytes.Clone(input)
			output := ConvertGeminiRequestToClaude("claude-sonnet-4-5", input, false)
			if !bytes.Equal(input, original) {
				b.Fatal("input payload was mutated")
			}
			if got, want := len(gjson.GetBytes(output, "messages").Array()), contentCount+1; got != want {
				b.Fatalf("message count = %d, want %d", got, want)
			}
			if got := gjson.GetBytes(output, "messages.1.content.#").Int(); got != 4 {
				b.Fatalf("first content block count = %d, want 4", got)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for b.Loop() {
				output = ConvertGeminiRequestToClaude("claude-sonnet-4-5", input, false)
			}
			benchmarkClaudeGeminiRequestOutput = output
		})
	}
}

func makeClaudeGeminiRequestPayload(contentCount int) []byte {
	var builder strings.Builder
	builder.Grow(contentCount * 420)
	builder.WriteString(`{"system_instruction":{"parts":[{"text":"system one"},{"text":"system two"}]},"contents":[`)
	for i := 0; i < contentCount; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		if i%2 == 0 {
			fmt.Fprintf(&builder, `{"role":"model","parts":[{"text":"model %d"},{"functionCall":{"id":"call_%d","name":"tool_%d","args":{"value":%d}}},{"inline_data":{"mime_type":"image/png","data":"AAAA"}},{"file_data":{"file_uri":"gs://bucket/%d","mime_type":"text/plain"}}]}`, i, i, i, i, i)
			continue
		}
		fmt.Fprintf(&builder, `{"role":"user","parts":[{"text":"user %d"},{"functionResponse":{"id":"call_%d","name":"tool_%d","response":{"result":"ok"}}},{"inline_data":{"mime_type":"image/png","data":"AAAA"}},{"file_data":{"file_uri":"gs://bucket/%d","mime_type":"text/plain"}}]}`, i, i-1, i-1, i)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}
