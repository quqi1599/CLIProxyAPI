// Package thinking provides unified thinking configuration processing.
package thinking

import (
	"strings"

	"github.com/tidwall/gjson"
)

type thinkingStripPath struct {
	segments   []string
	pruneEmpty bool
}

// StripThinkingConfig removes thinking configuration fields from request body.
//
// This function is used when a model doesn't support thinking but the request
// contains thinking configuration. The configuration is silently removed to
// prevent upstream API errors.
//
// Parameters:
//   - body: Original request body JSON
//   - provider: Provider name (determines which fields to strip)
//
// Returns:
//   - Modified request body JSON with thinking configuration removed
//   - Original body is returned unchanged if:
//   - body is empty or invalid JSON
//   - provider is unknown
//   - no thinking configuration found
func StripThinkingConfig(body []byte, provider string) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	var paths []thinkingStripPath
	switch provider {
	case "claude":
		paths = []thinkingStripPath{
			{segments: []string{"thinking"}},
			{segments: []string{"output_config", "effort"}, pruneEmpty: true},
		}
	case "gemini":
		paths = []thinkingStripPath{{segments: []string{"generationConfig", "thinkingConfig"}}}
	case "antigravity":
		paths = []thinkingStripPath{{segments: []string{"request", "generationConfig", "thinkingConfig"}}}
	case "openai":
		paths = []thinkingStripPath{
			{segments: []string{"reasoning_effort"}},
			{segments: []string{"reasoning", "effort"}, pruneEmpty: true},
			{segments: []string{"thinking", "reasoning_effort"}, pruneEmpty: true},
		}
	case "kimi":
		paths = []thinkingStripPath{
			{segments: []string{"reasoning_effort"}},
			{segments: []string{"thinking"}},
		}
	case "codex", "xai":
		paths = []thinkingStripPath{{segments: []string{"reasoning", "effort"}}}
	default:
		return body
	}

	result, changed, _ := stripThinkingObject(gjson.ParseBytes(body), paths)
	if !changed {
		return body
	}
	return []byte(result)
}

func stripThinkingObject(object gjson.Result, paths []thinkingStripPath) (string, bool, int) {
	if !object.IsObject() {
		return object.Raw, false, 0
	}

	var builder strings.Builder
	builder.Grow(len(object.Raw))
	builder.WriteByte('{')
	changed := false
	fields := 0
	object.ForEach(func(key, value gjson.Result) bool {
		fieldPaths := make([]thinkingStripPath, 0, len(paths))
		remove := false
		pruneEmpty := false
		for _, path := range paths {
			if len(path.segments) == 0 || path.segments[0] != key.String() {
				continue
			}
			if len(path.segments) == 1 {
				remove = true
				break
			}
			fieldPaths = append(fieldPaths, thinkingStripPath{segments: path.segments[1:], pruneEmpty: path.pruneEmpty})
			pruneEmpty = pruneEmpty || path.pruneEmpty
		}
		if remove {
			changed = true
			return true
		}

		fieldRaw := value.Raw
		if len(fieldPaths) > 0 && value.IsObject() {
			updated, nestedChanged, nestedFields := stripThinkingObject(value, fieldPaths)
			if pruneEmpty && nestedFields == 0 {
				changed = true
				return true
			}
			if nestedChanged {
				fieldRaw = updated
				changed = true
			}
		}
		if fields > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(key.Raw)
		builder.WriteByte(':')
		builder.WriteString(fieldRaw)
		fields++
		return true
	})
	builder.WriteByte('}')
	if !changed {
		return object.Raw, false, fields
	}
	return builder.String(), true, fields
}
