package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func TestFailureMetadataLoggerLogsOnlySafeFields(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOut := logger.Out
	oldFormatter := logger.Formatter
	oldLevel := logger.Level
	log.SetOutput(&buf)
	log.SetFormatter(&log.JSONFormatter{})
	log.SetLevel(log.WarnLevel)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	}()

	ctx := internallogging.WithRequestID(context.Background(), "req-safe-1")
	ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
	ctx = coreusage.WithRequestShape(ctx, coreusage.RequestShape{MessageCount: 127, ToolCount: 49})
	ctx = coreusage.WithRequestAttempt(ctx, coreusage.RequestAttempt{AttemptNo: 4})
	ctx = coreusage.WithReasoningEffort(ctx, "minimal")
	ctx = coreusage.WithRoutingGroup(ctx, "codex-primary")

	plugin := &FailureMetadataLogger{}
	plugin.HandleUsage(ctx, coreusage.Record{
		Model:              "gpt-5.5",
		AuthIndex:          "safe-auth-index",
		RequestedAt:        time.Now(),
		Latency:            3*time.Second + 25*time.Millisecond,
		Failed:             true,
		ProviderStatusCode: http.StatusInternalServerError,
		ErrorCode:          "api_error",
		Fail: coreusage.Failure{
			StatusCode: http.StatusInternalServerError,
			ErrorCode:  "api_error",
			Body:       "secret prompt sk-test-token must not be logged",
		},
	})

	raw := buf.String()
	for _, forbidden := range []string{"secret prompt", "sk-test-token", "api_key", "authorization"} {
		if bytes.Contains([]byte(raw), []byte(forbidden)) {
			t.Fatalf("failure metadata log leaked %q: %s", forbidden, raw)
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("unmarshal log payload: %v; raw=%s", err, raw)
	}
	requireJSONField(t, payload, "msg", "failure_metadata")
	requireJSONField(t, payload, "event", "failure_metadata")
	requireJSONField(t, payload, "failure_class", "upstream_api_error")
	requireJSONField(t, payload, "model", "gpt-5.5")
	requireMissingJSONField(t, payload, "endpoint")
	requireJSONField(t, payload, "endpoint_method", "POST")
	requireJSONField(t, payload, "endpoint_path", "/v1/chat/completions")
	requireJSONField(t, payload, "reasoning_effort", "minimal")
	requireJSONNumberField(t, payload, "message_count", 127)
	requireJSONNumberField(t, payload, "tool_count", 49)
	requireJSONNumberField(t, payload, "attempt_count", 4)
	requireJSONNumberField(t, payload, "duration_ms", 3025)
	requireJSONNumberField(t, payload, "normalized_status", http.StatusInternalServerError)
	requireJSONField(t, payload, "error_type", "server_error")
	requireJSONField(t, payload, "error_code", "internal_server_error")
	requireJSONNumberField(t, payload, "upstream_status", http.StatusInternalServerError)
	requireJSONNumberField(t, payload, "status_code", http.StatusInternalServerError)
	requireJSONField(t, payload, "upstream_error_code", "api_error")
	requireJSONField(t, payload, "request_id", "req-safe-1")
	requireJSONField(t, payload, "auth_index", "safe-auth-index")
	requireJSONField(t, payload, "routing_group", "codex-primary")
}

func TestFailureMetadataLoggerNormalizesContentSafetyFields(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOut := logger.Out
	oldFormatter := logger.Formatter
	oldLevel := logger.Level
	log.SetOutput(&buf)
	log.SetFormatter(&log.JSONFormatter{})
	log.SetLevel(log.WarnLevel)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	}()

	ctx := internallogging.WithRequestID(context.Background(), "req-safe-2")
	ctx = internallogging.WithEndpointParts(ctx, http.MethodPost, "/v1/messages")

	plugin := &FailureMetadataLogger{}
	plugin.HandleUsage(ctx, coreusage.Record{
		Model:              "MiniMax-M3-highspeed",
		Failed:             true,
		ProviderStatusCode: http.StatusInternalServerError,
		ErrorCode:          "1026",
		Latency:            time.Second,
		Fail: coreusage.Failure{
			StatusCode: http.StatusInternalServerError,
			ErrorCode:  "1026",
		},
	})

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("unmarshal log payload: %v; raw=%s", err, buf.String())
	}
	requireJSONField(t, payload, "endpoint_method", "POST")
	requireJSONField(t, payload, "endpoint_path", "/v1/messages")
	requireJSONNumberField(t, payload, "upstream_status", http.StatusInternalServerError)
	requireJSONNumberField(t, payload, "status_code", http.StatusInternalServerError)
	requireJSONNumberField(t, payload, "normalized_status", http.StatusBadRequest)
	requireJSONField(t, payload, "error_type", "invalid_request_error")
	requireJSONField(t, payload, "error_code", "content_policy_violation")
	requireJSONField(t, payload, "upstream_error_code", "1026")
}

func TestFailureMetadataLoggerIncludesToolShapeFields(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOut := logger.Out
	oldFormatter := logger.Formatter
	oldLevel := logger.Level
	log.SetOutput(&buf)
	log.SetFormatter(&log.JSONFormatter{})
	log.SetLevel(log.WarnLevel)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	}()

	ctx := internallogging.WithRequestID(context.Background(), "req-safe-3")
	ctx = internallogging.WithEndpointParts(ctx, http.MethodPost, "/v1/messages")
	ctx = coreusage.WithRequestShape(ctx, coreusage.RequestShape{MessageCount: 307, ToolCount: 558})
	ctx = coreusage.WithToolShape(ctx, coreusage.ToolShape{
		ToolTypes:         "function:78,mcp:54",
		DeclaredToolCount: 78,
		InteractionCount:  558,
		MCPToolCount:      54,
	})

	plugin := &FailureMetadataLogger{}
	plugin.HandleUsage(ctx, coreusage.Record{
		Model:              "deepseek-v4-pro",
		Failed:             true,
		ProviderStatusCode: http.StatusBadRequest,
		ErrorCode:          "invalid_request_error",
		Latency:            1450 * time.Millisecond,
		Fail: coreusage.Failure{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  "invalid_request_error",
		},
	})

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("unmarshal log payload: %v; raw=%s", err, buf.String())
	}
	requireJSONField(t, payload, "endpoint_method", "POST")
	requireJSONField(t, payload, "endpoint_path", "/v1/messages")
	requireJSONNumberField(t, payload, "message_count", 307)
	requireJSONNumberField(t, payload, "tool_count", 558)
	requireJSONNumberField(t, payload, "declared_tool_count", 78)
	requireJSONNumberField(t, payload, "tool_interaction_count", 558)
	requireJSONNumberField(t, payload, "mcp_tool_count", 54)
	requireJSONField(t, payload, "tool_types", "function:78,mcp:54")
}

func TestFailureMetadataLoggerIncludesFailureDiagnosticFields(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOut := logger.Out
	oldFormatter := logger.Formatter
	oldLevel := logger.Level
	log.SetOutput(&buf)
	log.SetFormatter(&log.JSONFormatter{})
	log.SetLevel(log.WarnLevel)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	}()

	ctx := internallogging.WithRequestID(context.Background(), "req-safe-4")
	ctx = internallogging.WithEndpointParts(ctx, http.MethodPost, "/v1/chat/completions")
	ctx = coreusage.WithFailureDiagnostic(ctx, coreusage.FailureDiagnostic{
		Channel:             "8",
		CompatName:          "deepseek-official",
		CompatKind:          "deepseek",
		CompatMapping:       "deepseek_v4_via_doubao_volcengine",
		UpstreamRequestID:   "deepseek-log-1",
		PayloadFields:       "messages,model,reasoning_effort",
		MessageRoles:        "assistant:1,system:1,tool:1,user:1",
		MessageRoleSequence: "system>assistant>tool>user",
		MessageContentKinds: "array:3,string:1",
		ContentPartTypes:    "image_url:1,text:3",
		InputItemTypes:      "message:4",
		ThinkingType:        "enabled",
		ResponseFormatType:  "json_schema",
		ParallelToolCalls:   "false",
		AddedFields:         "thinking",
		RemovedFields:       "tool_choice",
		ModifiedFields:      "messages,tools",
		AssistantToolCalls:  1,
		ToolResultMessages:  1,
		ReasoningMessages:   1,
		MaxContentParts:     2,
	})

	plugin := &FailureMetadataLogger{}
	plugin.HandleUsage(ctx, coreusage.Record{
		Model:              "deepseek-v4-pro",
		Failed:             true,
		ProviderStatusCode: http.StatusBadRequest,
		ErrorCode:          "invalid_request_error",
		Latency:            2 * time.Second,
		Fail: coreusage.Failure{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  "invalid_request_error",
		},
	})

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("unmarshal log payload: %v; raw=%s", err, buf.String())
	}
	requireJSONField(t, payload, "channel", "8")
	requireJSONField(t, payload, "compat_name", "deepseek-official")
	requireJSONField(t, payload, "compat_kind", "deepseek")
	requireJSONField(t, payload, "compat_mapping", "deepseek_v4_via_doubao_volcengine")
	requireJSONField(t, payload, "upstream_request_id", "deepseek-log-1")
	requireJSONField(t, payload, "payload_fields", "messages,model,reasoning_effort")
	requireJSONField(t, payload, "message_roles", "assistant:1,system:1,tool:1,user:1")
	requireJSONField(t, payload, "message_role_sequence", "system>assistant>tool>user")
	requireJSONField(t, payload, "message_content_kinds", "array:3,string:1")
	requireJSONField(t, payload, "content_part_types", "image_url:1,text:3")
	requireJSONField(t, payload, "input_item_types", "message:4")
	requireJSONField(t, payload, "thinking_type", "enabled")
	requireJSONField(t, payload, "response_format_type", "json_schema")
	requireJSONField(t, payload, "parallel_tool_calls", "false")
	requireJSONField(t, payload, "added_fields", "thinking")
	requireJSONField(t, payload, "removed_fields", "tool_choice")
	requireJSONField(t, payload, "modified_fields", "messages,tools")
	requireJSONNumberField(t, payload, "assistant_tool_call_messages", 1)
	requireJSONNumberField(t, payload, "tool_result_messages", 1)
	requireJSONNumberField(t, payload, "reasoning_messages", 1)
	requireJSONNumberField(t, payload, "max_content_parts", 2)
}

func TestFailureMetadataLoggerSkipsSuccessfulRecords(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOut := logger.Out
	oldFormatter := logger.Formatter
	log.SetOutput(&buf)
	log.SetFormatter(&log.JSONFormatter{})
	defer func() {
		log.SetOutput(oldOut)
		log.SetFormatter(oldFormatter)
	}()

	plugin := &FailureMetadataLogger{}
	plugin.HandleUsage(context.Background(), coreusage.Record{
		Model:   "gpt-5.5",
		Failed:  false,
		Latency: time.Second,
	})

	if buf.Len() != 0 {
		t.Fatalf("successful usage should not be logged: %s", buf.String())
	}
}

func requireMissingJSONField(t *testing.T, payload map[string]any, key string) {
	t.Helper()
	if _, ok := payload[key]; ok {
		t.Fatalf("%s = %v, want missing", key, payload[key])
	}
}

func requireJSONField(t *testing.T, payload map[string]any, key string, want string) {
	t.Helper()
	got, ok := payload[key].(string)
	if !ok || got != want {
		t.Fatalf("%s = %v, want %q", key, payload[key], want)
	}
}

func requireJSONNumberField(t *testing.T, payload map[string]any, key string, want int) {
	t.Helper()
	got, ok := payload[key].(float64)
	if !ok || int(got) != want {
		t.Fatalf("%s = %v, want %d", key, payload[key], want)
	}
}
