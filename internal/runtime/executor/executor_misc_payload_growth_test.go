package executor

import (
	"bytes"
	"fmt"
	"testing"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
)

func TestMiscPayloadGrowthLargeArrays(t *testing.T) {
	const itemCount = 1024

	t.Run("gemini image parts", func(t *testing.T) {
		parts := make([][]byte, itemCount)
		for i := range parts {
			parts[i] = []byte(fmt.Sprintf(`{"text":"part-%d"}`, i))
		}
		payload := append([]byte(`{"first":1,"generationConfig":{"imageConfig":{"aspectRatio":"1:1"}},"contents":[{"parts":`), internalpayload.BuildRaw(parts)...)
		payload = append(payload, []byte(`}],"last":2}`)...)
		original := bytes.Clone(payload)

		out := fixGeminiImageAspectRatio("gemini-2.5-flash-image-preview", payload)

		if !bytes.Equal(payload, original) {
			t.Fatal("fixGeminiImageAspectRatio mutated its input")
		}
		if got := len(gjson.GetBytes(out, "contents.0.parts").Array()); got != itemCount+2 {
			t.Fatalf("parts = %d, want %d", got, itemCount+2)
		}
		if bytes.Index(out, []byte(`"first"`)) > bytes.Index(out, []byte(`"last"`)) {
			t.Fatalf("top-level field order changed: %s", out)
		}
	})

	t.Run("xai namespace tools", func(t *testing.T) {
		tools := make([][]byte, itemCount)
		for i := range tools {
			tools[i] = []byte(fmt.Sprintf(`{"type":"function","name":"tool_%d","parameters":{"type":"object"}}`, i))
		}
		namespace := append([]byte(`{"type":"namespace","tools":`), internalpayload.BuildRaw(tools)...)
		namespace = append(namespace, '}')
		payload := append([]byte(`{"first":1,"tools":`), internalpayload.BuildRaw([][]byte{namespace})...)
		payload = append(payload, []byte(`,"last":2}`)...)
		original := bytes.Clone(payload)

		out := normalizeXAITools(payload)

		if !bytes.Equal(payload, original) {
			t.Fatal("normalizeXAITools mutated its input")
		}
		if got := len(gjson.GetBytes(out, "tools").Array()); got != itemCount {
			t.Fatalf("tools = %d, want %d", got, itemCount)
		}
	})

	t.Run("xai reasoning summaries", func(t *testing.T) {
		payload := buildXAIReasoningPayload(itemCount)
		original := bytes.Clone(payload)

		out := normalizeXAIInputReasoningItems(payload)

		if !bytes.Equal(payload, original) {
			t.Fatal("normalizeXAIInputReasoningItems mutated its input")
		}
		if got := len(gjson.GetBytes(out, "input").Array()); got != 1 {
			t.Fatalf("input items = %d, want 1", got)
		}
		if got := len(gjson.GetBytes(out, "input.0.summary").Array()); got != itemCount {
			t.Fatalf("summary items = %d, want %d", got, itemCount)
		}
	})

	t.Run("xai websocket ids", func(t *testing.T) {
		destination := make([][]byte, itemCount)
		source := make([][]byte, itemCount)
		for i := range destination {
			destination[i] = []byte(fmt.Sprintf(`{"type":"message","value":%d}`, i))
			source[i] = []byte(fmt.Sprintf(`{"type":"message","id":"item_%d"}`, i))
		}
		payload := append([]byte(`{"first":1,"input":`), internalpayload.BuildRaw(destination)...)
		payload = append(payload, []byte(`,"last":2}`)...)
		downstream := append([]byte(`{"input":`), internalpayload.BuildRaw(source)...)
		downstream = append(downstream, '}')
		original := bytes.Clone(payload)

		out := preserveXAIInputIDsFromDownstreamTail(payload, downstream)

		if !bytes.Equal(payload, original) {
			t.Fatal("preserveXAIInputIDsFromDownstreamTail mutated its input")
		}
		if got := gjson.GetBytes(out, fmt.Sprintf("input.%d.id", itemCount-1)).String(); got != fmt.Sprintf("item_%d", itemCount-1) {
			t.Fatalf("last id = %q", got)
		}
	})

	t.Run("antigravity replay", func(t *testing.T) {
		payload, items := buildAntigravityReplayFixture(itemCount)
		original := bytes.Clone(payload)

		out, changed := insertAntigravityReasoningReplayItems(payload, items)

		if !changed {
			t.Fatal("replay did not change payload")
		}
		if !bytes.Equal(payload, original) {
			t.Fatal("insertAntigravityReasoningReplayItems mutated its input")
		}
		if got := gjson.GetBytes(out, fmt.Sprintf("request.contents.0.parts.%d.thoughtSignature", itemCount-1)).String(); got != fmt.Sprintf("sig_%d", itemCount-1) {
			t.Fatalf("last signature = %q", got)
		}
	})
}

func TestOpenAICompatMutationPreservesFieldOrderAndInput(t *testing.T) {
	payload := []byte(`{"first":1,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"high","unknown":1},"thinking":{"reasoning_effort":"max","keep":true},"last":2}`)
	original := bytes.Clone(payload)

	out := stripOpenAICompatReasoningEffort(payload)

	if !bytes.Equal(payload, original) {
		t.Fatal("stripOpenAICompatReasoningEffort mutated its input")
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() || gjson.GetBytes(out, "reasoning.effort").Exists() || gjson.GetBytes(out, "thinking.reasoning_effort").Exists() {
		t.Fatalf("reasoning effort survived: %s", out)
	}
	if gjson.GetBytes(out, "reasoning.unknown").Int() != 1 || !gjson.GetBytes(out, "thinking.keep").Bool() {
		t.Fatalf("unknown fields were not preserved: %s", out)
	}
	previous := -1
	for _, field := range []string{"first", "messages", "reasoning", "thinking", "last"} {
		position := bytes.Index(out, []byte(`"`+field+`"`))
		if position <= previous {
			t.Fatalf("field %q is out of order in %s", field, out)
		}
		previous = position
	}
}

func BenchmarkPayloadGrowthXAIReasoningMerge(b *testing.B) {
	for _, size := range []int{100, 1000, 5000} {
		payload := buildXAIReasoningPayload(size)
		b.Run(fmt.Sprintf("items_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				_ = normalizeXAIInputReasoningItems(payload)
			}
		})
	}
}

func BenchmarkPayloadGrowthAntigravityReplayBatch(b *testing.B) {
	for _, size := range []int{100, 1000, 5000} {
		payload, items := buildAntigravityReplayFixture(size)
		b.Run(fmt.Sprintf("items_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				_, _ = insertAntigravityReasoningReplayItems(payload, items)
			}
		})
	}
}

func buildXAIReasoningPayload(size int) []byte {
	items := make([][]byte, size)
	for i := range items {
		items[i] = []byte(fmt.Sprintf(`{"type":"reasoning","summary":[{"type":"summary_text","text":"item_%d"}]}`, i))
	}
	payload := append([]byte(`{"first":1,"input":`), internalpayload.BuildRaw(items)...)
	return append(payload, []byte(`,"last":2}`)...)
}

func buildAntigravityReplayFixture(size int) ([]byte, [][]byte) {
	parts := make([][]byte, size)
	items := make([][]byte, size)
	for i := range parts {
		parts[i] = []byte(`{"text":"visible"}`)
		items[i] = []byte(fmt.Sprintf(`{"type":"thought_signature","contentIndex":0,"partIndex":%d,"thoughtSignature":"sig_%d"}`, i, i))
	}
	payload := append([]byte(`{"first":1,"request":{"contents":[{"role":"model","parts":`), internalpayload.BuildRaw(parts)...)
	payload = append(payload, []byte(`}]},"last":2}`)...)
	return payload, items
}
