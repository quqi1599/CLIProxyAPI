package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsVersionField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 9, 11, 10, 2, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "fetched latest antigravity version"
	entry.Data["version"] = "2.1.0"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	if !strings.Contains(line, "version=2.1.0") {
		t.Fatalf("formatted line %q missing version field", line)
	}
}

func TestLogFormatterIncludesOperationalTroubleshootingFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 23, 22, 58, 0, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "stream execution summary"
	entry.Data["request_id"] = "req-log-fields"
	entry.Data["event"] = "stream_execution_summary"
	entry.Data["provider"] = "codex"
	entry.Data["model"] = "gpt-5.5"
	entry.Data["auth_index"] = "auth-idx"
	entry.Data["routing_group"] = "primary"
	entry.Data["prefix"] = "team-a"
	entry.Data["base_url"] = "https://upstream.example/v1"
	entry.Data["token_hash"] = "abc123"
	entry.Data["request_path"] = "/v1/chat/completions"
	entry.Data["status_code"] = 524
	entry.Data["upstream_status"] = 524
	entry.Data["total_duration_ms"] = 12000
	entry.Data["chunks_count"] = 42
	entry.Data["finish_reason"] = "stop"
	entry.Data["client_gone"] = false

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"[req-log-fields]",
		"event=stream_execution_summary",
		"provider=codex",
		"model=gpt-5.5",
		"auth_index=auth-idx",
		"routing_group=primary",
		"prefix=team-a",
		"base_url=https://upstream.example/v1",
		"token_hash=abc123",
		"request_path=/v1/chat/completions",
		"status_code=524",
		"upstream_status=524",
		"total_duration_ms=12000",
		"chunks_count=42",
		"finish_reason=stop",
		"client_gone=false",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %q", line, want)
		}
	}
}
