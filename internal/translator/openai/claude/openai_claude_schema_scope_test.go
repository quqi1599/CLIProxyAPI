package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestGenericCompatibleProviderPreservesSchemaConstraints(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object","$id":"lookup-schema","properties":{"query":{"type":"string","pattern":"\\p{L}+"},"options":{"type":["object","null"]}},"patternProperties":{"\\p{L}+":{"type":"string"}},"default":{"type":"object","pattern":"\\p{L}+"}}}]}`)
	// The generic converter has no capability evidence for any provider or alias.
	for _, model := range []string{"deepseek-chat", "qwen-plus", "glm-5", "kimi-k2", "minimax", "custom-channel-alias"} {
		t.Run(model, func(t *testing.T) {
			out := ConvertClaudeRequestToOpenAI(model, input, false)
			params := gjson.GetBytes(out, "tools.0.function.parameters")
			if params.Get("properties.query.pattern").String() != `\p{L}+` || params.Get("$id").String() != "lookup-schema" || len(params.Get("patternProperties").Map()) != 1 {
				t.Fatalf("generic provider constraints were removed: %s", out)
			}
			if !params.Get("properties.options.properties").IsObject() || params.Get("default.properties").Exists() || params.Get("default.pattern").String() != `\p{L}+` {
				t.Fatalf("schema normalization changed ordinary data: %s", out)
			}
		})
	}
}
