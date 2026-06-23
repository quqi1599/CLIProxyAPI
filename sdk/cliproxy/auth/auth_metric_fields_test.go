package auth

import (
	"fmt"
	"strings"
	"testing"
)

func TestAuthMetricFieldsIncludeSafeChannelIdentity(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Prefix:   "team-a",
		Attributes: map[string]string{
			"api_key":       "sk-secret-token",
			"base_url":      "https://user:pass@upstream.example/v1?token=secret",
			"routing_group": "primary",
		},
	}

	fields := (*Manager)(nil).authMetricFields(auth, "codex", "gpt-5.5")

	if got := fields["prefix"]; got != "team-a" {
		t.Fatalf("prefix field = %v, want team-a", got)
	}
	if got := fields["base_url"]; got != "https://upstream.example/v1" {
		t.Fatalf("base_url field = %v, want sanitized URL", got)
	}
	tokenHash, _ := fields["token_hash"].(string)
	if tokenHash == "" {
		t.Fatal("token_hash should be populated for API-key auth")
	}
	rendered := fmt.Sprint(fields)
	for _, forbidden := range []string{"sk-secret-token", "user:pass", "token=secret"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("fields leaked sensitive value %q: %v", forbidden, fields)
		}
	}
}
