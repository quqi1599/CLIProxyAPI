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
	requireJSONNumberField(t, payload, "normalized_status", http.StatusBadRequest)
	requireJSONField(t, payload, "error_type", "invalid_request_error")
	requireJSONField(t, payload, "error_code", "content_policy_violation")
	requireJSONField(t, payload, "upstream_error_code", "1026")
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
