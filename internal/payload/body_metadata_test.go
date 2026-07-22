package payload

import (
	"strings"
	"testing"
)

func TestSummarizeBodyMetadataDoesNotExposeBody(t *testing.T) {
	secret := "oauth-secret-marker"
	metadata := SummarizeBodyMetadata([]byte(`{"error":"`+secret+`"}`), "application/json\r\nX-Injected: yes")
	if strings.Contains(metadata, secret) || strings.Contains(metadata, "\r") || strings.Contains(metadata, "\n") {
		t.Fatalf("unsafe body metadata: %s", metadata)
	}
	if !strings.Contains(metadata, `"bytes":`) || !strings.Contains(metadata, `"sha256":"`) {
		t.Fatalf("incomplete body metadata: %s", metadata)
	}
}
