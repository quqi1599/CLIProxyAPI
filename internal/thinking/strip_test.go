package thinking

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStripThinkingConfigPreservesUnknownFieldsOrderAndInput(t *testing.T) {
	input := []byte(`{"z":1,"thinking":{"type":"enabled"},"output_config":{"before":1,"effort":"high","after":2},"a":2}`)
	original := bytes.Clone(input)

	out := StripThinkingConfig(input, "claude")

	if !bytes.Equal(input, original) {
		t.Fatal("StripThinkingConfig mutated caller input")
	}
	want := `{"z":1,"output_config":{"before":1,"after":2},"a":2}`
	if string(out) != want {
		t.Fatalf("output = %s, want %s", out, want)
	}

	empty := StripThinkingConfig([]byte(`{"z":1,"output_config":{"effort":"low"},"a":2}`), "claude")
	if gjson.GetBytes(empty, "output_config").Exists() {
		t.Fatalf("empty output_config was retained: %s", empty)
	}
}

func TestStripThinkingConfigNestedProviders(t *testing.T) {
	tests := []struct {
		provider string
		input    string
		path     string
	}{
		{provider: "gemini", input: `{"generationConfig":{"thinkingConfig":{"budget":1},"temperature":1}}`, path: "generationConfig.thinkingConfig"},
		{provider: "antigravity", input: `{"request":{"generationConfig":{"thinkingConfig":{"budget":1},"temperature":1}}}`, path: "request.generationConfig.thinkingConfig"},
		{provider: "codex", input: `{"reasoning":{"effort":"high","summary":"auto"}}`, path: "reasoning.effort"},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			out := StripThinkingConfig([]byte(test.input), test.provider)
			if gjson.GetBytes(out, test.path).Exists() {
				t.Fatalf("%s was retained: %s", test.path, out)
			}
		})
	}
}
