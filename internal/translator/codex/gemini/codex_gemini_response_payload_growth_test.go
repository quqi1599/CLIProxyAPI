package gemini

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var benchmarkCodexGeminiResponseOutput [][]byte

func BenchmarkPayloadGrowthCodexGeminiStreamMessage(b *testing.B) {
	for _, partCount := range []int{16, 64, 256, 1024} {
		rawJSON := makeCodexGeminiStreamMessagePayload(partCount)
		b.Run(strconv.Itoa(partCount), func(b *testing.B) {
			original := bytes.Clone(rawJSON)
			var params any
			output := ConvertCodexResponseToGemini(context.Background(), "gemini-2.5-pro", nil, nil, rawJSON, &params)
			if !bytes.Equal(rawJSON, original) {
				b.Fatal("input payload was mutated")
			}
			if len(output) != 1 {
				b.Fatalf("output count = %d, want 1", len(output))
			}
			if got, want := len(gjson.GetBytes(output[0], "candidates.0.content.parts").Array()), partCount-partCount/4; got != want {
				b.Fatalf("content part count = %d, want %d", got, want)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(rawJSON)))
			b.ResetTimer()
			for b.Loop() {
				params = nil
				output = ConvertCodexResponseToGemini(context.Background(), "gemini-2.5-pro", nil, nil, rawJSON, &params)
			}
			benchmarkCodexGeminiResponseOutput = output
		})
	}
}

func makeCodexGeminiStreamMessagePayload(partCount int) []byte {
	var builder strings.Builder
	builder.Grow(partCount * 64)
	builder.WriteString(`data: {"type":"response.output_item.done","item":{"type":"message","content":[`)
	for i := 0; i < partCount; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		if i%4 == 3 {
			fmt.Fprintf(&builder, `{"type":"future_part","value":%d}`, i)
			continue
		}
		fmt.Fprintf(&builder, `{"type":"output_text","text":"text %d"}`, i)
	}
	builder.WriteString(`]}}`)
	return []byte(builder.String())
}
