package util

import (
	"bytes"
	"encoding/json"
)

// NormalizeToolSchema walks schema locations only, preserving defaults and examples.
// Codex-specific restrictions must not be applied to generic compatible providers.
func NormalizeToolSchema(value any, codexCompat bool) any {
	schema, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if codexCompat {
		delete(schema, "$schema")
		delete(schema, "$id")
	}
	if pattern, ok := schema["pattern"].(string); codexCompat && ok && unsupportedToolPattern(pattern) {
		delete(schema, "pattern")
	}
	isObject := schema["type"] == "object"
	if types, ok := schema["type"].([]any); ok {
		for _, typ := range types {
			if name, ok := typ.(string); ok && name == "object" {
				isObject = true
			}
		}
	}
	if isObject && schema["properties"] == nil {
		schema["properties"] = map[string]any{}
	}
	for _, key := range []string{"properties", "$defs", "definitions", "patternProperties", "dependentSchemas", "dependencies"} {
		if children, ok := schema[key].(map[string]any); ok {
			for name, child := range children {
				if codexCompat && key == "patternProperties" && unsupportedToolPattern(name) {
					delete(children, name)
					continue
				}
				NormalizeToolSchema(child, codexCompat)
			}
		}
	}
	for _, key := range []string{"items", "prefixItems", "contains", "additionalProperties", "propertyNames", "unevaluatedProperties", "unevaluatedItems", "additionalItems", "contentSchema", "anyOf", "oneOf", "allOf", "not", "if", "then", "else"} {
		if children, ok := schema[key].([]any); ok {
			for _, child := range children {
				NormalizeToolSchema(child, codexCompat)
			}
		} else {
			NormalizeToolSchema(schema[key], codexCompat)
		}
	}
	return schema
}

func unsupportedToolPattern(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '\\' {
			continue
		}
		if i+2 < len(pattern) && (pattern[i+1] == 'p' || pattern[i+1] == 'P') && pattern[i+2] == '{' {
			return true
		}
		i++
	}
	return false
}

// NormalizeCodexToolParameters also supplies a default object schema for absent input.
func NormalizeCodexToolParameters(raw []byte) []byte {
	const fallback = `{"type":"object","properties":{}}`
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if !json.Valid(raw) || decoder.Decode(&root) != nil || root == nil {
		return []byte(fallback)
	}
	if typ, exists := root["type"]; !exists || typ == nil || typ == "" {
		root["type"] = "object"
	}
	NormalizeToolSchema(root, true)
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(root) != nil {
		return []byte(fallback)
	}
	return bytes.TrimSpace(buf.Bytes())
}
