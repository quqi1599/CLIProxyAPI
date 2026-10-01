package helps

import "testing"

func TestClaudeSSEErrorPayload(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		want        bool
	}{
		{"named_error", "event: error\ndata: {\"error\":{\"code\":\"1234\"}}\n\n", true},
		{"typed_error", "data: {\"type\":\"error\",\"message\":\"failure\"}\n\n", true},
		{"untyped_error", "data: {\"error\":{\"code\":1234}}\n\n", true},
		{"bare_message", "data: {\"message\":\"failure\"}\n\n", true},
		{"malformed_named_error", "event: error\ndata: not-json\n\n", true},
		{"empty_named_error", "event: error\n\n", true},
		{"multiline_crlf", "event: error\r\ndata: {\"type\":\"error\",\r\ndata: \"error\":{\"code\":\"1234\"}}\r\n\r\n", true},
		{"null_error", "data: {\"type\":\"message_start\",\"message\":{},\"error\":null}\n\n", false},
		{"generated_error_text", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"event: error 1234\"}}\n\n", false},
		{"ping", "event: ping\ndata: {\"type\":\"ping\"}\n\n", false},
		{"message_stop", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := ClaudeSSEErrorPayload([]byte(tc.event))
			if got != tc.want {
				t.Fatalf("error event = %t, want %t", got, tc.want)
			}
		})
	}
}
