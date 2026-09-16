package helps

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestDeepSeekNamespaceRoundtrip(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"files","description":"Local files","tools":[{"type":"function","name":"read","strict":true,"parameters":{"type":"object","properties":{"id":{"const":9007199254740993}}}}]},{"type":"custom","name":"apply_patch"}],"tool_choice":{"type":"function","namespace":"files","name":"read"},"input":[{"type":"function_call","namespace":"files","name":"read","call_id":"call_1","arguments":"{\"id\":1}"},{"type":"function_call_output","call_id":"call_1","output":"unchanged"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"original reasoning"}]}]}`)
	out, names, err := FlattenDeepSeekNamespaces(body)
	if err != nil {
		t.Fatal(err)
	}
	wire := gjson.GetBytes(out, "tools.0.name").String()
	for path, want := range map[string]string{
		"tools.0.type": "function", "tools.0.strict": "true",
		"tools.0.parameters.properties.id.const": "9007199254740993",
		"tools.1.name":                           "apply_patch", "tool_choice.name": wire,
		"input.0.name": wire, "input.0.call_id": "call_1", "input.0.arguments": `{"id":1}`,
		"input.1.output": "unchanged", "input.1.call_id": "call_1", "input.2.content.0.text": "original reasoning",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s=%s want %s", path, got, want)
		}
	}
	if gjson.GetBytes(out, "input.0.namespace").Exists() || gjson.GetBytes(out, "tool_choice.namespace").Exists() {
		t.Fatal("namespace leaked upstream")
	}
	for _, tc := range []struct{ raw, path string }{
		{fmt.Sprintf(`{"output":[{"type":"function_call","name":%q,"call_id":"call_2","arguments":"{}"}]}`, wire), "output.0."},
		{fmt.Sprintf(`data: {"type":"response.output_item.added","item":{"type":"function_call","name":%q,"call_id":"call_2"}}`, wire), "item."},
		{fmt.Sprintf(`data: {"type":"response.output_item.done","item":{"type":"function_call","name":%q,"call_id":"call_2"}}`, wire), "item."},
		{fmt.Sprintf(`data: {"type":"response.completed","response":{"output":[{"type":"function_call","name":%q,"call_id":"call_2"}]}}`, wire), "response.output.0."},
	} {
		restored := strings.TrimSpace(strings.TrimPrefix(string(names.Restore([]byte(tc.raw))), "data:"))
		if gjson.Get(restored, tc.path+"name").String() != "read" || gjson.Get(restored, tc.path+"namespace").String() != "files" || gjson.Get(restored, tc.path+"call_id").String() != "call_2" {
			t.Fatal(restored)
		}
	}
	for _, raw := range []string{"data: [DONE]", `data: {"type":"response.output_text.delta","delta":"` + wire + `"}`} {
		if got := string(names.Restore([]byte(raw))); got != raw {
			t.Fatalf("unrelated bytes changed: %s", got)
		}
	}
}

func TestDeepSeekNamespaceRejectsLossyShapes(t *testing.T) {
	for _, raw := range []string{
		`{"tools":[{"type":"namespace","name":"files"}]}`,
		`{"tools":[{"type":"namespace","name":"files","tools":[{"type":"custom","name":"read"}]}]}`,
		`{"tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read"},{"type":"function","name":"read"}]}]}`,
		`{"tools":[{"type":"namespace","name":"files","tools":[{"type":"namespace","name":"nested"}]}]}`,
		`{"input":[{"type":"function_call","namespace":"bad.name","name":"read"}]}`,
	} {
		out, names, err := FlattenDeepSeekNamespaces([]byte(raw))
		if err == nil || string(out) != raw || names != nil {
			t.Fatalf("lossy shape accepted: %s", raw)
		}
	}
	base := `{"tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read"}]}]}`
	out, _, _ := FlattenDeepSeekNamespaces([]byte(base))
	wire := gjson.GetBytes(out, "tools.0.name").String()
	collision := fmt.Sprintf(`{"tools":[{"type":"function","name":%q},{"type":"namespace","name":"files","tools":[{"type":"function","name":"read"}]}]}`, wire)
	if _, _, err := FlattenDeepSeekNamespaces([]byte(collision)); err == nil {
		t.Fatal("collision accepted")
	}
}

func TestDeepSeekCodingResponsesScope(t *testing.T) {
	for _, tc := range []struct {
		base, endpoint string
		want           bool
	}{
		{"https://ark.cn-beijing.volces.com/api/coding/v1", "/responses", true},
		{"https://ark.cn-beijing.volces.com/api/coding", "/responses", true},
		{"https://ark.cn-beijing.volces.com/api/coding/v1", "/responses/compact", true},
		{"https://ark.cn-beijing.volces.com/api/v3", "/responses", false},
		{"https://ark.cn-beijing.volces.com/api/coding/v1", "/chat/completions", false},
		{"https://volces.com.example/api/coding/v1", "/responses", false},
		{"https://api.deepseek.com", "/responses", false},
	} {
		if got := DeepSeekCodingResponsesUnsupported(tc.base, tc.endpoint); got != tc.want {
			t.Fatalf("%+v got %v", tc, got)
		}
	}
}

func TestDeepSeekIsResponsesEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		want     bool
	}{
		{"/responses", true},
		{"/responses/compact", true},
		{"/chat/completions", false},
		{"/completions", false},
		{"", false},
	} {
		if got := DeepSeekIsResponsesEndpoint(tc.endpoint); got != tc.want {
			t.Fatalf("DeepSeekIsResponsesEndpoint(%q)=%v want %v", tc.endpoint, got, tc.want)
		}
	}
}

func TestDeepSeekErrorDiagnosticNeverEchoesContent(t *testing.T) {
	for _, tc := range []struct{ message, reason, field string }{
		{"messages[3].tool_calls[0] missing tool_call_id private customer text", "tool_history_pairing", "messages[3].tool_calls[0]"},
		{"messages[12].reasoning_content is missing", "thinking_history_or_parameter", "messages[12].reasoning_content"},
		{"Invalid schema for function customer-private-name", "tool_or_output_schema", ""},
		{"private customer text secret-key", "unclassified", ""},
	} {
		reason, field := DeepSeekErrorDiagnostic([]byte(fmt.Sprintf(`{"error":{"message":%q}}`, tc.message)))
		if reason != tc.reason || field != tc.field {
			t.Fatalf("reason=%q field=%q", reason, field)
		}
	}
}
