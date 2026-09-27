package helps

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestJSONFieldViewPreservesOpaqueValues(t *testing.T) {
	body := []byte(" \n" + `{"input" : [ {"id":9007199254740993,"text":"\u4e2d\\\"","unknown":1e100} ],"reasoning":{"effort":"high"},"unknown" : { "ordered":true },"\u0074hinking":null}` + " \t")
	original := bytes.Clone(body)
	view, ok := NewJSONFieldView(body, "thinking", "reasoning", "enable_thinking")
	if !ok || gjson.GetBytes(view.Bytes(), "input").Exists() {
		t.Fatal("could not isolate controls")
	}
	out, ok := view.Merge([]byte(`{"reasoning":{"effort":"none"},"enable_thinking":false}`))
	if !ok || !json.Valid(out) {
		t.Fatalf("invalid merged body: %s", out)
	}
	for _, field := range []string{"input", "unknown"} {
		if gjson.GetBytes(out, field).Raw != gjson.GetBytes(body, field).Raw {
			t.Fatalf("opaque %s was rewritten", field)
		}
	}
	if gjson.GetBytes(out, "thinking").Exists() || gjson.GetBytes(out, "enable_thinking").Type != gjson.False {
		t.Fatalf("removal/addition failed: %s", out)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("input mutated")
	}
	unchanged, ok := view.Merge(view.Bytes())
	if !ok || &unchanged[0] != &body[0] {
		t.Fatal("no-op did not reuse original body")
	}
	if _, ok = view.Merge([]byte(`{"input":"undeclared write"}`)); ok {
		t.Fatal("accepted undeclared write")
	}
}

func TestJSONFieldViewDeclinesAmbiguousInput(t *testing.T) {
	for _, body := range []string{``, `null`, `[]`, `{"x":`, `{"x":1,"x":2}`, `{"thinking":1,"\u0074hinking":2}`} {
		if _, ok := NewJSONFieldView([]byte(body), "thinking"); ok {
			t.Fatalf("accepted %q", body)
		}
		calls := 0
		got := RewriteJSONFields([]byte(body), func(input []byte) []byte {
			calls++
			if string(input) != body {
				t.Fatal("fallback did not see complete input")
			}
			return []byte("fallback")
		}, "thinking")
		if calls != 1 || string(got) != "fallback" {
			t.Fatal("fallback not preserved")
		}
	}
}

func FuzzJSONFieldViewMerge(f *testing.F) {
	for _, body := range []string{`{}`, ` {"input":[{"text":"x"}],"thinking":null}`, `{"thinking":{"type":"enabled"},"reasoning":{"effort":"high"}}`, `{"thinking":1,"thinking":2}`, `{"\u0074hinking":{},"unknown":9007199254740993}`, `{"input":null,"reasoning":1e100}`} {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 65536 {
			t.Skip()
		}
		view, ok := NewJSONFieldView(body, "thinking", "reasoning")
		if !ok {
			return
		}
		transform := func(input []byte) []byte {
			out, err := sjson.SetBytes(input, "thinking.type", "disabled")
			if err != nil {
				t.Fatal(err)
			}
			out, _ = sjson.DeleteBytes(out, "reasoning")
			return out
		}
		original := bytes.Clone(body)
		got, ok := view.Merge(transform(view.Bytes()))
		if !ok || !json.Valid(got) {
			t.Fatalf("invalid merge: %q", got)
		}
		decode := func(data []byte) any {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		if !reflect.DeepEqual(decode(got), decode(transform(body))) {
			t.Fatalf("semantic mismatch: %q", body)
		}
		if !bytes.Equal(original, body) {
			t.Fatal("source mutated")
		}
	})
}
