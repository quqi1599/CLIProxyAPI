package failure

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKimiRejectionDiagnostic(t *testing.T) {
	for _, tc := range []struct{ message, code, param string }{
		{"total message size 3000000 exceeds limit 2000000", "kimi_message_size_exceeded", "messages"},
		{"Invalid request: Your request exceeded model token limit: 262144 (requested: 270000)", "kimi_context_length_exceeded", "messages"},
		{"thinking is enabled but reasoning_content is missing in assistant tool call message at index 2", "kimi_thinking_history_missing", "messages[].reasoning_content"},
		{"function name private-customer-tool is duplicated", "kimi_duplicate_tool_name", "tools[].function.name"},
		{"Your model id does not exist, recognized as private-customer-model", "kimi_model_id_invalid", "model"},
		{"K3.1 Pro does not support disabled thinking mode. private-customer", "kimi_thinking_required", "thinking"},
		{"Invalid request: unsupported image url: https://private-customer.invalid/secret", "kimi_image_url_unsupported", "messages[].content[].image_url"},
		{"temperature must be 1; private-customer", "kimi_request_parameter_invalid", "temperature"},
		{"Invalid value for 'response_format': private-customer", "kimi_request_parameter_invalid", "response_format"},
		{"private-customer mentions temperature and invalid", "", ""},
		{"temperature documentation is helpful", "", ""},
	} {
		t.Run(tc.code+tc.param+tc.message[:6], func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": tc.message}})
			code, param, message := KimiRejectionDiagnostic(body)
			if code != tc.code || param != tc.param || strings.Contains(message, "private-customer") {
				t.Fatalf("unexpected diagnostic: code=%q param=%q message=%q", code, param, message)
			}
			if normalized := (UpstreamErrorDiagnostic{Param: param}).Normalized(); normalized.Param != param {
				t.Fatalf("parameter lost before usage: %q -> %q", param, normalized.Param)
			}
		})
	}
}

func TestKimiRejectionDiagnosticRejectsUntrustedEnvelopes(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"content_policy_violation","message":"temperature must be 1"}}`,
		`{"error":{"type":"permission_error","message":"temperature must be 1"}}`,
		`{"error":{"type":400,"message":"temperature must be 1"}}`,
		`{"error":{"message":"temperature must be 1","message":"private"}}`,
		`{"error":{"message":"temperature must be 1"},"error":{"message":"private"}}`,
		`{"messages":[{"content":"temperature must be 1"}]}`,
		`{"error":{"message":{"text":"temperature must be 1"}}}`,
		`{"error":{"message":"temperature must be 1` + strings.Repeat("x", 4096) + `"}}`,
		`{"error":{"message":"temperature must be 1"}} trailing`,
	} {
		if code, param, message := KimiRejectionDiagnostic([]byte(body)); code != "" || param != "" || message != "" {
			t.Fatalf("ambiguous or non-validation error classified: %q %q %q", code, param, message)
		}
	}
}
