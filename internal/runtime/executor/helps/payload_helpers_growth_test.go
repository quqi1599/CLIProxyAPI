package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
)

func TestApplyPayloadConfigPreservesRuleSemanticsAndFieldOrder(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "gpt-*"}}
	cfg := &config.Config{Payload: config.PayloadConfig{
		Default: []config.PayloadRule{
			{Models: models, Params: map[string]any{"metadata.first": "first", "existing": 99}},
			{Models: models, Params: map[string]any{"metadata.first": "second"}},
		},
		DefaultRaw: []config.PayloadRule{
			{Models: models, Params: map[string]any{"metadata.raw": `{"ok":true}`}},
		},
		Override: []config.PayloadRule{
			{Models: models, Params: map[string]any{
				"items.#(kind==\"keep\")#.v": 10,
				"metadata.after":             true,
				"status":                     "one",
			}},
			{Models: []config.PayloadModelRule{{Name: "gpt-*", Match: []map[string]any{{"metadata.after": true}}}}, Params: map[string]any{"status": "two"}},
		},
		OverrideRaw: []config.PayloadRule{
			{Models: models, Params: map[string]any{"metadata.raw_override": `[1,2]`}},
		},
		Filter: []config.PayloadFilterRule{
			{Models: models, Params: []string{`items.#(kind=="drop")#`}},
		},
	}}
	payload := []byte(`{"z":1,"items":[{"kind":"keep","v":0},{"kind":"drop","v":1},{"kind":"drop","v":2}],"unknown":{"x":true},"existing":7,"status":"zero"}`)
	original := append([]byte(nil), payload...)

	out := ApplyPayloadConfigWithRequest(cfg, "gpt-5.4", "openai", "responses", "", payload, nil, "", "", nil)

	if !bytes.Equal(payload, original) {
		t.Fatal("input payload was mutated")
	}
	if got := gjson.GetBytes(out, "metadata.first").String(); got != "first" {
		t.Fatalf("default first-write-wins value = %q, want first", got)
	}
	if got := gjson.GetBytes(out, "existing").Int(); got != 7 {
		t.Fatalf("default overwrote source field: got %d, want 7", got)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "two" {
		t.Fatalf("override last-write-wins value = %q, want two", got)
	}
	if !gjson.GetBytes(out, "metadata.raw.ok").Bool() || gjson.GetBytes(out, "metadata.raw_override.#").Int() != 2 {
		t.Fatalf("raw rules were not preserved: %s", out)
	}
	items := gjson.GetBytes(out, "items").Array()
	if len(items) != 1 || items[0].Get("kind").String() != "keep" || items[0].Get("v").Int() != 10 {
		t.Fatalf("query override/filter result = %s", gjson.GetBytes(out, "items").Raw)
	}
	var keys []string
	gjson.ParseBytes(out).ForEach(func(key, _ gjson.Result) bool {
		keys = append(keys, key.String())
		return true
	})
	if got := strings.Join(keys, ","); got != "z,items,unknown,existing,status,metadata" {
		t.Fatalf("top-level field order = %q", got)
	}
}

func TestApplyPayloadConfigLargeQueriedArray(t *testing.T) {
	const itemCount = 2048
	items := make([]json.RawMessage, 0, itemCount)
	for i := 0; i < itemCount; i++ {
		kind := "drop"
		if i%2 == 0 {
			kind = "keep"
		}
		items = append(items, json.RawMessage(fmt.Sprintf(`{"id":%d,"kind":%q,"unknown":true}`, i, kind)))
	}
	payload := append([]byte(`{"items":`), internalpayload.BuildRaw(items)...)
	payload = append(payload, '}')
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "gpt-*"}},
			Params: map[string]any{`items.#(kind=="keep")#.enabled`: true},
		}},
		Filter: []config.PayloadFilterRule{{
			Models: []config.PayloadModelRule{{Name: "gpt-*"}},
			Params: []string{`items.#(kind=="drop")#`},
		}},
	}}

	out := ApplyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai", "", payload, nil, "", "")
	result := gjson.GetBytes(out, "items")
	if got := len(result.Array()); got != itemCount/2 {
		t.Fatalf("kept item count = %d, want %d", got, itemCount/2)
	}
	for _, item := range result.Array() {
		if item.Get("kind").String() != "keep" || !item.Get("enabled").Bool() || !item.Get("unknown").Bool() {
			t.Fatalf("unexpected kept item: %s", item.Raw)
		}
	}
}

func TestApplyPayloadConfigSupportsEscapedAndCreatingPathsWithRoot(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gemini-*"}},
		Params: map[string]any{
			`a\.b.:0`:      "new",
			`items.2.name`: "created",
		},
	}}}}
	payload := []byte(`{"request":{"a.b":{"0":"old"},"items":[],"untouched":{"nested":true}}}`)

	out := ApplyPayloadConfigWithRoot(cfg, "gemini-2.5", "gemini", "request", payload, nil, "", "")

	if got := gjson.GetBytes(out, `request.a\.b.0`).String(); got != "new" {
		t.Fatalf("escaped forced-string path = %q, want new; payload=%s", got, out)
	}
	if got := gjson.GetBytes(out, "request.items.2.name").String(); got != "created" {
		t.Fatalf("created array path = %q, want created; payload=%s", got, out)
	}
	if gjson.GetBytes(out, "request.items.0").Type != gjson.Null || gjson.GetBytes(out, "request.items.1").Type != gjson.Null {
		t.Fatalf("array path was not padded with nulls: %s", gjson.GetBytes(out, "request.items").Raw)
	}
	if !gjson.GetBytes(out, "request.untouched.nested").Bool() {
		t.Fatalf("unknown nested field was lost: %s", out)
	}
}

func TestApplyPayloadConfigInvalidOriginalKeepsLegacyDefaultSemantics(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Default: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-*"}},
		Params: map[string]any{"existing": 99},
	}}}}

	out := ApplyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai", "", []byte(`{"existing":7}`), []byte(`{`), "", "")

	if got := gjson.GetBytes(out, "existing").Int(); got != 99 {
		t.Fatalf("default with invalid original = %d, want 99", got)
	}
}
