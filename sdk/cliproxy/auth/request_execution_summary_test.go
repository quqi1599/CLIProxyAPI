package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestManager_Execute_LogsRequestExecutionSummary(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 1)
	executor := &authFallbackExecutor{
		id: "claude",
		executeErrors: map[string]error{
			"aa-rate-limited-auth": &Error{
				Code:       "rate_limit_error",
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "upstream rate limited",
				Retryable:  true,
			},
		},
	}
	m.RegisterExecutor(executor)

	model := "claude-sonnet-4-6"
	blockedAuth := &Auth{ID: "aa-rate-limited-auth", Provider: "claude"}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "claude"}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(blockedAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(blockedAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), blockedAuth); errRegister != nil {
		t.Fatalf("register blocked auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	ctx := logging.WithRequestID(context.Background(), "req-summary-1")
	resp, errExecute := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want success", errExecute)
	}
	if string(resp.Payload) != goodAuth.ID {
		t.Fatalf("payload = %q, want %q", string(resp.Payload), goodAuth.ID)
	}

	entry := findExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["request_id"]; got != "req-summary-1" {
		t.Fatalf("request_id = %#v, want req-summary-1", got)
	}
	if got := entry.Data["final_success"]; got != true {
		t.Fatalf("final_success = %#v, want true", got)
	}
	if got := entry.Data["attempt_count"]; got != 2 {
		t.Fatalf("attempt_count = %#v, want 2", got)
	}
	if got := entry.Data["fallback_count"]; got != 1 {
		t.Fatalf("fallback_count = %#v, want 1", got)
	}
	if got := entry.Data["max_attempts"]; got != 4 {
		t.Fatalf("max_attempts = %#v, want 4", got)
	}
	if got := entry.Data["max_fallbacks"]; got != 1 {
		t.Fatalf("max_fallbacks = %#v, want 1", got)
	}
	if got := entry.Data["translator_run_count"]; got != 2 {
		t.Fatalf("translator_run_count = %#v, want 2", got)
	}
	if got := entry.Data["final_status"]; got != http.StatusOK {
		t.Fatalf("final_status = %#v, want %d", got, http.StatusOK)
	}
	if got := entry.Data["final_provider"]; got != "claude" {
		t.Fatalf("final_provider = %#v, want claude", got)
	}
	if got := entry.Data["final_model"]; got != model {
		t.Fatalf("final_model = %#v, want %q", got, model)
	}
	if got := entry.Data["final_executor"]; got != "authFallbackExecutor" {
		t.Fatalf("final_executor = %#v, want authFallbackExecutor", got)
	}
}

func TestLogRequestExecutionSummaryTreatsFinal4xxAsFailure(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	ctx := logging.WithRequestID(context.Background(), "req-summary-400")
	trace := &requestAttemptTrace{
		requestID:      "req-summary-400",
		attempts:       1,
		maxAttempts:    1,
		maxFallbacks:   0,
		translatorRuns: 1,
		finalStatus:    http.StatusBadRequest,
		finalProvider:  "claude",
		finalModel:     "MiniMax-M3",
		finalExecutor:  "ClaudeExecutor",
	}

	logRequestExecutionSummary(ctx, trace, true, nil)

	entry := findExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["final_success"]; got != false {
		t.Fatalf("final_success = %#v, want false", got)
	}
	if got := entry.Data["final_status"]; got != http.StatusBadRequest {
		t.Fatalf("final_status = %#v, want %d", got, http.StatusBadRequest)
	}
	if got := entry.Data["final_error_type"]; got != "invalid_request_error" {
		t.Fatalf("final_error_type = %#v, want invalid_request_error", got)
	}
	if got := entry.Data["final_error_code"]; got != "status_400" {
		t.Fatalf("final_error_code = %#v, want status_400", got)
	}
}

func TestLogRequestExecutionSummaryNormalizesContentSafetyError(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	ctx := logging.WithRequestID(context.Background(), "req-summary-sensitive")
	trace := &requestAttemptTrace{
		requestID:      "req-summary-sensitive",
		attempts:       1,
		maxAttempts:    1,
		translatorRuns: 1,
		finalStatus:    http.StatusInternalServerError,
		finalProvider:  "openai-compatibility",
		finalModel:     "MiniMax-M3-highspeed",
		finalExecutor:  "OpenAICompatExecutor",
	}
	errSensitive := &Error{
		Code:       "1026",
		HTTPStatus: http.StatusInternalServerError,
		Message:    miniMaxNewSensitiveMessage,
	}

	logRequestExecutionSummary(ctx, trace, false, errSensitive)

	entry := findExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["final_success"]; got != false {
		t.Fatalf("final_success = %#v, want false", got)
	}
	if got := entry.Data["final_status"]; got != http.StatusBadRequest {
		t.Fatalf("final_status = %#v, want %d", got, http.StatusBadRequest)
	}
	if got := entry.Data["final_error_type"]; got != "invalid_request_error" {
		t.Fatalf("final_error_type = %#v, want invalid_request_error", got)
	}
	if got := entry.Data["final_error_code"]; got != "content_policy_violation" {
		t.Fatalf("final_error_code = %#v, want content_policy_violation", got)
	}
}

func findExecutionSummaryEntry(t *testing.T, entries []*log.Entry) *log.Entry {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i] == nil {
			continue
		}
		if entries[i].Data["event"] == "request_execution_summary" {
			return entries[i]
		}
	}
	t.Fatalf("request_execution_summary log entry not found; entries=%d", len(entries))
	return nil
}
