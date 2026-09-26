package helps

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func BenchmarkPayloadGrowthNormalizeCodexRequestSchemas(b *testing.B) {
	var builder strings.Builder
	builder.WriteString(`{"tools":[`)
	for index := 0; index < 64; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"type":"function","name":"tool_%d","parameters":{"type":"object","properties":{"value":{"type":"string","pattern":"\\p{L}"}}}}`, index)
	}
	builder.WriteString(`]}`)
	body := []byte(builder.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		out := NormalizeCodexRequestSchemas(body)
		if len(out) == 0 {
			b.Fatal("schema normalization returned an empty payload")
		}
	}
}

func TestCodexRequestSchemaScope(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","output":{"pattern":"\\p{L}"}}],"tools":[{"type":"function","parameters":{"type":"object","properties":{"x":{"pattern":"\\p{L}"}},"default":{"pattern":"\\p{L}"}}},{"type":"namespace","tools":[{"type":"function","parameters":{"$id":"remove","type":"object"}}]},{"type":"custom","format":{"pattern":"\\p{L}"}}]}`)
	out := NormalizeCodexRequestSchemas(body)
	if gjson.GetBytes(out, "tools.0.parameters.properties.x.pattern").Exists() || gjson.GetBytes(out, "tools.1.tools.0.parameters.$id").Exists() {
		t.Fatalf("schema survived: %s", out)
	}
	for _, path := range []string{"input", "tools.0.parameters.default", "tools.2"} {
		if gjson.GetBytes(out, path).Raw != gjson.GetBytes(body, path).Raw {
			t.Errorf("data at %s changed", path)
		}
	}
}

func TestCodexRequestSchemaScopeIncludesAdditionalTools(t *testing.T) {
	body := []byte(`{"input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"value":{"pattern":"\\p{L}"}}}}]}]}`)
	out := NormalizeCodexRequestSchemas(body)
	if gjson.GetBytes(out, "input.0.tools.0.parameters.properties.value.pattern").Exists() {
		t.Fatalf("additional tool schema was not normalized: %s", out)
	}
	if got := gjson.GetBytes(out, "input.0.tools.0.parameters.type").String(); got != "object" {
		t.Fatalf("parameters.type = %q, want object", got)
	}
}
