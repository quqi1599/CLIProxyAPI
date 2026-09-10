package util

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestToolSchemaCompatPreservesDataAndNumbers(t *testing.T) {
	raw := []byte(`{"$schema":"draft","type":["object","null"],"$defs":{"x":{"$id":"x","type":"object"}},"properties":{"x":{"type":"string","pattern":"\\p{L}+"}},"default":{"$id":"data","type":"object","pattern":"\\p{L}","n":9007199254740993},"description":"<keep>&"}`)
	out := NormalizeCodexToolParameters(raw)
	if gjson.GetBytes(out, "$schema").Exists() || gjson.GetBytes(out, "$defs.x.$id").Exists() || gjson.GetBytes(out, "properties.x.pattern").Exists() {
		t.Fatalf("unsupported schema survived: %s", out)
	}
	if !gjson.GetBytes(out, "$defs.x.properties").IsObject() || gjson.GetBytes(out, "default.properties").Exists() || gjson.GetBytes(out, "default.n").Raw != "9007199254740993" || gjson.GetBytes(out, "default.$id").String() != "data" || !strings.Contains(string(out), "<keep>&") {
		t.Fatalf("data changed: %s", out)
	}
}

func TestToolSchemaCompatEncodedPatternsAndNestedLocations(t *testing.T) {
	out := NormalizeCodexToolParameters([]byte(`{"type":["object","null"],"patternProperties":{"\\\u0070{L}":{"type":"string"},"^[a-z]+$":{"pattern":"\\P{N}"}},"allOf":[{"properties":{"a":{"items":{"pattern":"\\p{L}"}}}}],"enum":[{"pattern":"\\p{L}"}]}`))
	patterns := gjson.GetBytes(out, "patternProperties").Map()
	if len(patterns) != 1 || patterns["^[a-z]+$"].Get("pattern").Exists() || gjson.GetBytes(out, "allOf.0.properties.a.items.pattern").Exists() || !gjson.GetBytes(out, "enum.0.pattern").Exists() || !gjson.GetBytes(out, "properties").IsObject() {
		t.Fatalf("incorrect traversal: %s", out)
	}
}

func TestUnsupportedToolPatternEscaping(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    bool
	}{{`\p{L}`, true}, {`\P{N}`, true}, {`\\p{L}`, false}, {`\\\p{L}`, true}, {`(?=foo)[a-z]+`, false}} {
		if got := unsupportedToolPattern(tc.pattern); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.pattern, got, tc.want)
		}
	}
}
