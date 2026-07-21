package executor

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPatchCodexCompletedOutput(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"output":[]}}`)
	indexed := map[int64][]byte{
		2: []byte(`{"id":"two"}`),
		0: []byte(`{"id":"zero"}`),
	}
	fallback := [][]byte{[]byte(`{"id":"fallback"}`)}

	got := patchCodexCompletedOutput(event, indexed, fallback)
	items := gjson.GetBytes(got, "response.output").Array()
	if len(items) != 3 {
		t.Fatalf("response.output length = %d, want 3: %s", len(items), got)
	}
	for idx, want := range []string{"zero", "two", "fallback"} {
		if id := items[idx].Get("id").String(); id != want {
			t.Fatalf("response.output[%d].id = %q, want %q", idx, id, want)
		}
	}
}

func TestPatchCodexCompletedOutputKeepsExistingOutput(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"output":[{"id":"upstream"}]}}`)
	got := patchCodexCompletedOutput(event, map[int64][]byte{0: []byte(`{"id":"collected"}`)}, nil)
	if string(got) != string(event) {
		t.Fatalf("patchCodexCompletedOutput() changed non-empty output: %s", got)
	}
}

func BenchmarkPatchCodexCompletedOutput(b *testing.B) {
	for _, count := range []int{16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("items_%d", count), func(b *testing.B) {
			items := make([][]byte, count)
			for idx := range items {
				items[idx] = []byte(fmt.Sprintf(`{"id":"item-%d","type":"message"}`, idx))
			}
			event := []byte(`{"type":"response.completed","response":{"output":[]}}`)
			outputBytes := len(patchCodexCompletedOutput(event, nil, items))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = patchCodexCompletedOutput(event, nil, items)
			}
			b.ReportMetric(float64(outputBytes), "output_bytes/op")
		})
	}
}
