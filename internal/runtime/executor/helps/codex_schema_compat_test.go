package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

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
