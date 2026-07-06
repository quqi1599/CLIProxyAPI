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

func TestLogFormatterIncludesCompatibilityDiagnosticFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 7, 6, 2, 14, 0, 0, time.Local)
	entry.Level = log.WarnLevel
	entry.Message = "compatibility diagnostic"
	entry.Data["request_id"] = "req-compat-fields"
	entry.Data["event"] = "compatibility_diagnostic"
	entry.Data["provider"] = "openai-compatibility"
	entry.Data["model"] = "deepseek-v4-pro"
	entry.Data["channel"] = "8"
	entry.Data["compat_name"] = "deepseek-official"
	entry.Data["compat_kind"] = "deepseek"
	entry.Data["compat_mapping"] = "deepseek_v4_via_doubao_volcengine"
	entry.Data["upstream_request_id"] = "deepseek-log-1"
	entry.Data["payload_fields"] = []string{"messages", "model", "reasoning_effort"}
	entry.Data["message_roles"] = []string{"system:1", "user:1"}
	entry.Data["message_role_sequence"] = "system>user"
	entry.Data["message_content_kinds"] = []string{"array:1", "string:1"}
	entry.Data["content_part_types"] = []string{"image_url:1", "text:1"}
	entry.Data["input_item_types"] = "message:2"
	entry.Data["tool_choice_type"] = "auto"
	entry.Data["thinking_type"] = "enabled"
	entry.Data["response_format_type"] = "json_schema"
	entry.Data["parallel_tool_calls"] = "false"
	entry.Data["assistant_tool_call_messages"] = 1
	entry.Data["tool_result_messages"] = 1
	entry.Data["reasoning_messages"] = 1
	entry.Data["max_content_parts"] = 2
	entry.Data["removed_fields"] = []string{"tool_choice"}
	entry.Data["modified_fields"] = []string{"temperature"}
	entry.Data["added_fields"] = []string{"thinking"}

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"[req-compat-fields]",
		"event=compatibility_diagnostic",
		"provider=openai-compatibility",
		"model=deepseek-v4-pro",
		"channel=8",
		"compat_name=deepseek-official",
		"compat_kind=deepseek",
		"compat_mapping=deepseek_v4_via_doubao_volcengine",
		"upstream_request_id=deepseek-log-1",
		"payload_fields=[messages model reasoning_effort]",
		"message_roles=[system:1 user:1]",
		"message_role_sequence=system>user",
		"message_content_kinds=[array:1 string:1]",
		"content_part_types=[image_url:1 text:1]",
		"input_item_types=message:2",
		"tool_choice_type=auto",
		"thinking_type=enabled",
		"response_format_type=json_schema",
		"parallel_tool_calls=false",
		"assistant_tool_call_messages=1",
		"tool_result_messages=1",
		"reasoning_messages=1",
		"max_content_parts=2",
		"removed_fields=[tool_choice]",
		"modified_fields=[temperature]",
		"added_fields=[thinking]",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %q", line, want)
		}
	}
}
