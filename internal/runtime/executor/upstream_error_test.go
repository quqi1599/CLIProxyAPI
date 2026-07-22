package executor

import (
	"net/http"
	"strings"
	"testing"
)

func TestNewUpstreamStatusErrDoesNotExposeBody(t *testing.T) {
	secret := "sentinel-upstream-secret-do-not-log"
	body := []byte(`{"error":{"message":"context length exceeded ` + secret + `","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	err := newUpstreamStatusErr(http.StatusBadRequest, http.Header{"Content-Type": {"application/json"}}, "application/json", body)

	got := err.Error()
	if strings.Contains(got, secret) || strings.Contains(got, `"message"`) {
		t.Fatalf("status error exposed upstream body: %s", got)
	}
	for _, want := range []string{"reason=context_length_exceeded", "error_code=context_length_exceeded", `"bytes":`, `"sha256":`, `"content_type":"application/json"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("status error = %q, want %q", got, want)
		}
	}
	if err.StatusCode() != http.StatusBadRequest || err.ErrorCode() != "context_length_exceeded" {
		t.Fatalf("status/code = %d/%q", err.StatusCode(), err.ErrorCode())
	}
}

func TestSafeUpstreamFailureMessageKeepsRoutingSignalsCanonical(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "model", body: `{"error":{"message":"requested model does not exist: private-model","code":"model_not_found"}}`, want: "model_not_supported"},
		{name: "quota", body: `{"error":{"message":"You've reached your usage limit for this billing cycle. private-account"}}`, want: "usage limit billing cycle quota will be refreshed"},
		{name: "content safety", body: `{"error":{"message":"request blocked by content policy private-prompt","code":"content_policy_violation"}}`, want: "content_policy_violation"},
		{name: "previous response", body: `{"error":{"message":"Item with id secret-id not found. Items are not persisted when store is set to false."}}`, want: "item with id not found items are not persisted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := safeUpstreamFailureMessage("application/json", []byte(tc.body))
			if !strings.Contains(got, tc.want) {
				t.Fatalf("summary = %q, want signal %q", got, tc.want)
			}
			if strings.Contains(got, "private-") || strings.Contains(got, "secret-id") {
				t.Fatalf("summary exposed body: %s", got)
			}
		})
	}
}
