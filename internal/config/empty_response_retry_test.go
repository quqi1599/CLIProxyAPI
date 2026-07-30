package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptionalEmptyResponseRetry(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := []byte(`
empty-response-retry:
  enabled: true
  audit-only: true
  models:
    - gpt-5.6-sol
  client-profiles:
    - workbuddy
  source-formats:
    - openai
    - openai-response
  max-buffer-bytes: 8388608
  max-buffer-events: 2000
`)
	if errWrite := os.WriteFile(configPath, configYAML, 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	cfg, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	got := cfg.EmptyResponseRetry
	if !got.Enabled || !got.AuditOnly {
		t.Fatalf("enabled/audit-only = %v/%v, want true/true", got.Enabled, got.AuditOnly)
	}
	if len(got.Models) != 1 || got.Models[0] != "gpt-5.6-sol" {
		t.Fatalf("models = %v", got.Models)
	}
	if len(got.ClientProfiles) != 1 || got.ClientProfiles[0] != "workbuddy" {
		t.Fatalf("client profiles = %v", got.ClientProfiles)
	}
	if len(got.SourceFormats) != 2 ||
		got.SourceFormats[0] != "openai" ||
		got.SourceFormats[1] != "openai-response" {
		t.Fatalf("source formats = %v", got.SourceFormats)
	}
	if got.MaxBufferBytes != 8*1024*1024 || got.MaxBufferEvents != 2000 {
		t.Fatalf("buffer limits = %d/%d", got.MaxBufferBytes, got.MaxBufferEvents)
	}
}
