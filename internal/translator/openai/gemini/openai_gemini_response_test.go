package gemini

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTolerantParseJSONObjectRaw_LargeObjectPreservesFieldsAndOrder(t *testing.T) {
	const fieldCount = 512
	out := tolerantParseJSONObjectRaw(buildTolerantObject(fieldCount))
	if !gjson.Valid(out) {
		t.Fatalf("invalid output: %s", out)
	}
	index := 0
	gjson.Parse(out).ForEach(func(key, value gjson.Result) bool {
		wantKey := fmt.Sprintf("field.%03d", index)
		wantValue := fmt.Sprintf("value-%03d", index)
		if key.String() != wantKey || value.String() != wantValue {
			t.Fatalf("field %d = %q:%q, want %q:%q", index, key.String(), value.String(), wantKey, wantValue)
		}
		index++
		return true
	})
	if index != fieldCount {
		t.Fatalf("field count = %d, want %d", index, fieldCount)
	}
}

func TestTolerantParseJSONObjectRaw_PreservesReplacementSemanticsAndValidNumbers(t *testing.T) {
	out := tolerantParseJSONObjectRaw(`{"x": first, "leading": 01, "fraction": .5, "x": last}`)
	if !gjson.Valid(out) {
		t.Fatalf("invalid output: %s", out)
	}
	if got := gjson.Get(out, "x").String(); got != "last" {
		t.Fatalf("duplicate key value = %q, want last", got)
	}
	if got := gjson.Get(out, "leading").Int(); got != 1 {
		t.Fatalf("leading-zero number = %d, want 1", got)
	}
	if got := gjson.Get(out, "fraction").Float(); got != 0.5 {
		t.Fatalf("fraction = %v, want 0.5", got)
	}
	keys := make([]string, 0, 3)
	gjson.Parse(out).ForEach(func(key, _ gjson.Result) bool {
		keys = append(keys, key.String())
		return true
	})
	if got := strings.Join(keys, ","); got != "x,leading,fraction" {
		t.Fatalf("key order = %q", got)
	}
}

func TestConvertOpenAIResponseToGeminiNonStreamPayloadGrowth(t *testing.T) {
	for _, size := range []int{16, 64, 256, 1024} {
		t.Run(fmt.Sprintf("choices_%d", size), func(t *testing.T) {
			raw := buildOpenAIToGeminiGrowthResponse(size)
			original := bytes.Clone(raw)

			output := ConvertOpenAIResponseToGeminiNonStream(context.Background(), "model", nil, nil, raw, nil)

			if !bytes.Equal(raw, original) {
				t.Fatal("converter mutated the input payload")
			}
			candidate := gjson.GetBytes(output, "candidates.0")
			if candidate.Get("index").Int() != int64(size-1) {
				t.Fatalf("candidate index = %d, want %d", candidate.Get("index").Int(), size-1)
			}
			if got := candidate.Get("finishReason").String(); got != "MAX_TOKENS" {
				t.Fatalf("finishReason = %q, want MAX_TOKENS", got)
			}
			parts := candidate.Get("content.parts").Array()
			if len(parts) != 3 {
				t.Fatalf("part count = %d, want 3", len(parts))
			}
			last := size - 1
			if !parts[0].Get("thought").Bool() || parts[0].Get("text").String() != fmt.Sprintf("reason-%04d", last) {
				t.Fatalf("reasoning part lost last-choice semantics: %s", parts[0].Raw)
			}
			if got, want := parts[1].Get("text").String(), fmt.Sprintf("text-%04d", last); got != want {
				t.Fatalf("text part = %q, want %q", got, want)
			}
			if got, want := parts[2].Get("functionCall.name").String(), fmt.Sprintf("tool_%04d", last); got != want {
				t.Fatalf("function name = %q, want %q", got, want)
			}
			args := parts[2].Get("functionCall.args")
			if args.Get("index").Int() != int64(last) || !args.Get("unknown.keep").Bool() {
				t.Fatalf("function args lost unknown fields: %s", args.Raw)
			}
		})
	}
}

var benchmarkOpenAIToGeminiNonStreamOutput []byte

func BenchmarkPayloadGrowthOpenAIToGeminiNonStream(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("choices_%d", size), func(b *testing.B) {
			raw := buildOpenAIToGeminiGrowthResponse(size)
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				benchmarkOpenAIToGeminiNonStreamOutput = ConvertOpenAIResponseToGeminiNonStream(context.Background(), "model", nil, nil, raw, nil)
			}
		})
	}
}

func buildOpenAIToGeminiGrowthResponse(size int) []byte {
	var response strings.Builder
	response.Grow(size * 320)
	response.WriteString(`{"id":"chatcmpl_growth","model":"growth-model","choices":[`)
	for index := 0; index < size; index++ {
		if index > 0 {
			response.WriteByte(',')
		}
		arguments := fmt.Sprintf(`{"index":%d,"unknown":{"keep":true}}`, index)
		fmt.Fprintf(&response, `{"index":%d,"message":{"role":"assistant","reasoning_content":"reason-%04d","content":"text-%04d","tool_calls":[{"type":"function","function":{"name":"tool_%04d","arguments":%q}}]},"finish_reason":"length"}`, index, index, index, index, arguments)
	}
	response.WriteString(`],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	return []byte(response.String())
}

var benchmarkTolerantObject string

func BenchmarkTolerantParseJSONObjectRaw_LargeObject(b *testing.B) {
	raw := buildTolerantObject(512)
	for b.Loop() {
		benchmarkTolerantObject = tolerantParseJSONObjectRaw(raw)
	}
}

func buildTolerantObject(fieldCount int) string {
	var input strings.Builder
	input.WriteByte('{')
	for i := 0; i < fieldCount; i++ {
		if i > 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `"field.%03d": value-%03d`, i, i)
	}
	input.WriteByte('}')
	return input.String()
}
