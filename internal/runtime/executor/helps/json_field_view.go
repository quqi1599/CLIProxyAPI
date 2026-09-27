package helps

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// JSONFieldView isolates the declared read/write set of a compatibility phase.
// It is request-local: untouched values remain raw JSON, and Merge builds the
// full body at most once. Callers must declare every field their phase reads.
// This deliberately uses a safe string copy for gjson, never unsafe aliasing.
type JSONFieldView struct {
	body      []byte
	fields    []jsonViewField
	selected  map[string]bool
	projected []byte
}

type jsonViewField struct {
	name       string
	start, end int
	valueStart int
}

// NewJSONFieldView declines malformed, non-object and duplicate-key inputs so
// callers can preserve their existing handling of ambiguous JSON.
func NewJSONFieldView(body []byte, names ...string) (*JSONFieldView, bool) {
	fields, ok := indexJSONViewFields(body)
	if !ok {
		return nil, false
	}
	v := &JSONFieldView{body: body, fields: fields, selected: make(map[string]bool, len(names))}
	for _, name := range names {
		v.selected[name] = true
	}
	size := 2
	for _, field := range fields {
		if v.selected[field.name] {
			size += field.end - field.start + 1
		}
	}
	v.projected = make([]byte, 0, size)
	v.projected = append(v.projected, '{')
	for _, field := range fields {
		if v.selected[field.name] {
			if len(v.projected) > 1 {
				v.projected = append(v.projected, ',')
			}
			v.projected = append(v.projected, body[field.start:field.end]...)
		}
	}
	v.projected = append(v.projected, '}')
	return v, true
}

func indexJSONViewFields(body []byte) ([]jsonViewField, bool) {
	if !gjson.ValidBytes(body) {
		return nil, false
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, false
	}
	fields := make([]jsonViewField, 0, 16)
	seen := make(map[string]bool)
	valid := true
	root.ForEach(func(key, value gjson.Result) bool {
		name := strings.Clone(key.String())
		if seen[name] || key.Index < 0 || value.Index < key.Index || value.Index+len(value.Raw) > len(body) {
			valid = false
			return false
		}
		seen[name] = true
		fields = append(fields, jsonViewField{name: name, start: key.Index, end: value.Index + len(value.Raw), valueStart: value.Index})
		return true
	})
	return fields, valid
}

// Bytes returns the small, independently owned phase input.
func (v *JSONFieldView) Bytes() []byte { return v.projected }

// Merge rejects writes outside the declared set. Unchanged fields retain their
// exact raw values, including large integers, escapes, whitespace and ordering.
// A no-op returns the original slice, and no merge mutates the caller's input.
func (v *JSONFieldView) Merge(updated []byte) ([]byte, bool) {
	fields, ok := indexJSONViewFields(updated)
	if !ok {
		return nil, false
	}
	replacements := make(map[string]jsonViewField, len(fields))
	for _, field := range fields {
		if !v.selected[field.name] {
			return nil, false
		}
		replacements[field.name] = field
	}
	changed := false
	size := 2
	for _, field := range v.fields {
		if !v.selected[field.name] {
			size += field.end - field.start + 1
			continue
		}
		replacement, exists := replacements[field.name]
		if !exists {
			changed = true
			continue
		}
		if !bytes.Equal(v.body[field.valueStart:field.end], updated[replacement.valueStart:replacement.end]) {
			changed = true
		}
		size += field.valueStart - field.start + replacement.end - replacement.valueStart + 1
		delete(replacements, field.name)
	}
	for _, field := range replacements {
		changed = true
		size += field.end - field.start + 1
	}
	if !changed {
		return v.body, true
	}
	// Index again from the small view, not the request body.
	for _, field := range fields {
		replacements[field.name] = field
	}
	out := make([]byte, 0, size)
	out = append(out, '{')
	separator := func() {
		if len(out) > 1 {
			out = append(out, ',')
		}
	}
	for _, field := range v.fields {
		if !v.selected[field.name] {
			separator()
			out = append(out, v.body[field.start:field.end]...)
			continue
		}
		if replacement, exists := replacements[field.name]; exists {
			separator()
			out = append(out, v.body[field.start:field.valueStart]...)
			out = append(out, updated[replacement.valueStart:replacement.end]...)
			delete(replacements, field.name)
		}
	}
	// Append new fields in the phase's output order, never map iteration order.
	for _, field := range fields {
		if _, exists := replacements[field.name]; exists {
			separator()
			out = append(out, updated[field.start:field.end]...)
		}
	}
	return append(out, '}'), true
}

// RewriteJSONFields falls back to the original phase on unsupported input or an
// undeclared write. The callback must be a pure, deterministic transformation.
func RewriteJSONFields(body []byte, transform func([]byte) []byte, fields ...string) []byte {
	if view, ok := NewJSONFieldView(body, fields...); ok {
		if out, merged := view.Merge(transform(view.Bytes())); merged {
			return out
		}
	}
	return transform(body)
}

// RewriteDeepSeekControls keeps control-only compatibility rules independent of
// prompt length. Message history, schemas and canonical thinking appliers retain
// their own validation phases and are never hidden from those phases.
func RewriteDeepSeekControls(body []byte, transform func([]byte) []byte) []byte {
	return RewriteJSONFields(body, transform,
		"model", "thinking", "reasoning", "reasoning_effort", "thinking_budget",
		"enable_thinking", "max_completion_tokens", "max_tokens", "tool_choice", "output_config")
}
