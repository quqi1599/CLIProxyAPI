package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestCodexModelsStatusErrorDoesNotExposeBody(t *testing.T) {
	const secret = "codex-models-error-sentinel"
	err := codexModelsStatusError(http.StatusBadGateway, "application/json", []byte(`{"error":"`+secret+`"}`))
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), `"sha256":`) || !strings.Contains(err.Error(), `"content_type":"application/json"`) {
		t.Fatalf("unsafe models error: %v", err)
	}
}
