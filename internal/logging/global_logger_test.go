package logging

import (
	"net/http"
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
	entry.Data["selection_reason"] = "channel_spread"
	entry.Data["candidate_count"] = 12
	entry.Data["candidate_ready_count"] = 8
	entry.Data["candidate_health_downweighted"] = 2
	entry.Data["candidate_skipped_disabled"] = 1
	entry.Data["candidate_skipped_cooldown"] = 1
	entry.Data["candidate_skipped_breaker"] = 1
	entry.Data["candidate_skipped_unavailable"] = 1
	entry.Data["request_path"] = "/v1/chat/completions"
	entry.Data["round_no"] = 2
	entry.Data["gpt_round_count"] = 3
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
		"selection_reason=channel_spread",
		"candidate_count=12",
		"candidate_ready_count=8",
		"candidate_health_downweighted=2",
		"candidate_skipped_disabled=1",
		"candidate_skipped_cooldown=1",
		"candidate_skipped_breaker=1",
		"candidate_skipped_unavailable=1",
		"request_path=/v1/chat/completions",
		"round_no=2",
		"gpt_round_count=3",
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

func TestLogFormatterIncludesGPTFirstEventPolicyFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 8, 6, 4, 20, 0, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "gpt_first_event_observation"
	entry.Data["event"] = "gpt_first_event_observation"
	entry.Data["model"] = "gpt-5.6-sol"
	entry.Data["outcome"] = "deliverable"
	entry.Data["eligible"] = true
	entry.Data["delay_ms"] = 4200
	entry.Data["enforced_timeout_ms"] = 30000
	entry.Data["policy_state"] = "slow_30s"
	entry.Data["decision_source"] = "model"
	entry.Data["decision_reason"] = "f25_below_90"
	entry.Data["window_seconds"] = 300
	entry.Data["eligible_first_attempts"] = 120
	entry.Data["first_event_success_rate_25"] = 0.88
	entry.Data["failure_rate"] = 0.06
	entry.Data["hard_failure_rate"] = 0.02
	entry.Data["first_event_wait_ms"] = 4200
	entry.Data["first_event_timeout_count"] = 1
	entry.Data["first_event_policy_state"] = "slow_30s"
	entry.Data["first_event_wait_budget_ms"] = 300000

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"event=gpt_first_event_observation",
		"outcome=deliverable",
		"eligible=true",
		"delay_ms=4200",
		"enforced_timeout_ms=30000",
		"policy_state=slow_30s",
		"decision_source=model",
		"decision_reason=f25_below_90",
		"window_seconds=300",
		"eligible_first_attempts=120",
		"first_event_success_rate_25=0.88",
		"failure_rate=0.06",
		"hard_failure_rate=0.02",
		"first_event_wait_ms=4200",
		"first_event_timeout_count=1",
		"first_event_policy_state=slow_30s",
		"first_event_wait_budget_ms=300000",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %q", line, want)
		}
	}
}

func TestLogFormatterIncludesRoutingAvailabilityAndCanonicalFailureFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 8, 10, 3, 30, 0, 0, time.Local)
	entry.Level = log.WarnLevel
	entry.Message = "auth selection failed"
	entry.Data["event"] = "auth_selection_failed"
	entry.Data["candidate_route_count"] = 7
	entry.Data["eligible_route_count"] = 0
	entry.Data["blocked_route_count"] = 7
	entry.Data["breaker_open_count"] = 6
	entry.Data["blocked_reasons"] = "model_cooldown:1,route_breaker:6"
	entry.Data["breaker_statuses"] = "502,504"
	entry.Data["breaker_reasons"] = "gpt_first_event_timeout:6"
	entry.Data["earliest_recovery_ms"] = int64(8750)
	entry.Data["normalized_status"] = http.StatusGatewayTimeout
	entry.Data["outer_status"] = http.StatusOK
	entry.Data["failure_kind"] = "transport_error"
	entry.Data["failure_scope"] = "provider"
	entry.Data["scope"] = "provider"
	entry.Data["semantic_type"] = "server_error"
	entry.Data["semantic_code"] = "upstream_timeout"
	entry.Data["stream_phase"] = "before_output"
	entry.Data["output_committed"] = false

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"candidate_route_count=7",
		"eligible_route_count=0",
		"blocked_route_count=7",
		"breaker_open_count=6",
		"blocked_reasons=model_cooldown:1,route_breaker:6",
		"breaker_statuses=502,504",
		"breaker_reasons=gpt_first_event_timeout:6",
		"earliest_recovery_ms=8750",
		"normalized_status=504",
		"outer_status=200",
		"failure_kind=transport_error",
		"failure_scope=provider",
		"scope=provider",
		"semantic_type=server_error",
		"semantic_code=upstream_timeout",
		"stream_phase=before_output",
		"output_committed=false",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %q", line, want)
		}
	}
}

func TestLogFormatterIncludesPreRouteFailureCorrelationFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 8, 30, 23, 30, 0, 0, time.Local)
	entry.Level = log.WarnLevel
	entry.Message = "pre-route failure"
	entry.Data["request_id"] = "cpa-502"
	entry.Data["event"] = "pre_route_failure"
	entry.Data["client_request_id"] = "oneapi-502"
	entry.Data["requested_model"] = "unknown-model"
	entry.Data["routing_phase"] = "pre_route"
	entry.Data["failure_class"] = "pre_route_bad_gateway"
	entry.Data["failure_kind"] = "provider_resolution"
	entry.Data["failure_scope"] = "request"
	entry.Data["status_code"] = http.StatusBadGateway
	entry.Data["error_code"] = "provider_not_resolved"
	entry.Data["endpoint_method"] = http.MethodPost
	entry.Data["endpoint_path"] = "/v1/chat/completions"
	entry.Data["source_format"] = "openai"
	entry.Data["request_stream"] = false
	entry.Data["payload_bytes"] = 105
	entry.Data["payload_sha256"] = strings.Repeat("a", 64)
	entry.Data["attempt_count"] = 0

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"[cpa-502]",
		"event=pre_route_failure",
		"client_request_id=oneapi-502",
		"requested_model=unknown-model",
		"routing_phase=pre_route",
		"failure_class=pre_route_bad_gateway",
		"failure_kind=provider_resolution",
		"failure_scope=request",
		"status_code=502",
		"error_code=provider_not_resolved",
		"endpoint_method=POST",
		"endpoint_path=/v1/chat/completions",
		"source_format=openai",
		"request_stream=false",
		"payload_bytes=105",
		"payload_sha256=" + strings.Repeat("a", 64),
		"attempt_count=0",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %q", line, want)
		}
	}
}

func TestLogFormatterIncludesRemoteCompactionAvailabilityFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 8, 24, 2, 49, 27, 0, time.Local)
	entry.Level = log.WarnLevel
	entry.Message = "auth_selection_failed"
	entry.Data["event"] = "auth_selection_failed"
	entry.Data["compaction_intent"] = "legacy_endpoint"
	entry.Data["candidate_route_count"] = 1
	entry.Data["eligible_route_count"] = 0
	entry.Data["compaction_candidate_route_count"] = 1
	entry.Data["compaction_eligible_route_count"] = 0
	entry.Data["compaction_blocked_route_count"] = 1
	entry.Data["compaction_breaker_open_count"] = 1
	entry.Data["compaction_blocked_reasons"] = "health_or_unavailable:1"
	entry.Data["compaction_breaker_statuses"] = "401"
	entry.Data["compaction_breaker_reasons"] = "status_401:1"
	entry.Data["compaction_earliest_recovery_ms"] = int64(45000)
	entry.Data["ordinary_candidate_route_count"] = 4
	entry.Data["ordinary_eligible_route_count"] = 3
	entry.Data["ordinary_blocked_route_count"] = 1
	entry.Data["ordinary_breaker_open_count"] = 1
	entry.Data["ordinary_blocked_reasons"] = "health_or_unavailable:1"
	entry.Data["ordinary_breaker_statuses"] = "401"
	entry.Data["ordinary_breaker_reasons"] = "status_401:1"
	entry.Data["ordinary_earliest_recovery_ms"] = int64(45000)
	entry.Data["compaction_compatibility_group"] = "opaque-state-v1"
	entry.Data["failed_attempt"] = 1
	entry.Data["consecutive_failures"] = 3
	entry.Data["open_until"] = "2026-08-23T18:50:12Z"
	entry.Data["cause_error_code"] = "model_cooldown"
	entry.Data["cause_reset_ms"] = int64(45000)

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"compaction_intent=legacy_endpoint",
		"compaction_candidate_route_count=1",
		"compaction_eligible_route_count=0",
		"compaction_blocked_route_count=1",
		"compaction_breaker_open_count=1",
		"compaction_blocked_reasons=health_or_unavailable:1",
		"compaction_breaker_statuses=401",
		"compaction_breaker_reasons=status_401:1",
		"compaction_earliest_recovery_ms=45000",
		"ordinary_candidate_route_count=4",
		"ordinary_eligible_route_count=3",
		"ordinary_blocked_route_count=1",
		"ordinary_breaker_open_count=1",
		"ordinary_blocked_reasons=health_or_unavailable:1",
		"ordinary_breaker_statuses=401",
		"ordinary_breaker_reasons=status_401:1",
		"ordinary_earliest_recovery_ms=45000",
		"compaction_compatibility_group=opaque-state-v1",
		"failed_attempt=1",
		"consecutive_failures=3",
		"open_until=2026-08-23T18:50:12Z",
		"cause_error_code=model_cooldown",
		"cause_reset_ms=45000",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %q", line, want)
		}
	}
}

func TestLogFormatterIncludesGPTRetryPressureFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 8, 13, 17, 52, 32, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "gpt_retry_pressure"
	entry.Data["event"] = "gpt_retry_pressure"
	entry.Data["model"] = "gpt-5.6-luna"
	entry.Data["retry_pressure_state"] = "congested"
	entry.Data["retry_pressure_reason"] = "route_pool_exhausted"
	entry.Data["degraded_route_count"] = 4
	entry.Data["retry_permit_limit"] = 1
	entry.Data["retry_in_flight"] = 1
	entry.Data["retry_waiters"] = 2
	entry.Data["retry_permit_wait_ms"] = int64(750)
	entry.Data["retry_permit_acquired"] = true
	entry.Data["retry_permit_rejected"] = false
	entry.Data["retry_pressure_wait_ms"] = int64(750)
	entry.Data["retry_pressure_permit_limit"] = 1
	entry.Data["retry_pressure_eligible_routes"] = 1
	entry.Data["retry_pressure_degraded_routes"] = 4
	entry.Data["retry_pressure_throttled"] = true

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"retry_pressure_state=congested",
		"retry_pressure_reason=route_pool_exhausted",
		"degraded_route_count=4",
		"retry_permit_limit=1",
		"retry_in_flight=1",
		"retry_waiters=2",
		"retry_permit_wait_ms=750",
		"retry_permit_acquired=true",
		"retry_permit_rejected=false",
		"retry_pressure_wait_ms=750",
		"retry_pressure_permit_limit=1",
		"retry_pressure_eligible_routes=1",
		"retry_pressure_degraded_routes=4",
		"retry_pressure_throttled=true",
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
