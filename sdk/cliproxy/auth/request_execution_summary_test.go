package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestLogRequestExecutionSummaryCanonicalizesOpaque500(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	ctx := logging.WithRequestID(context.Background(), "req-summary-opaque-500")
	trace := &requestAttemptTrace{requestID: "req-summary-opaque-500", attempts: 1, maxAttempts: 1}
	logRequestExecutionSummary(ctx, trace, false, errors.New("opaque failure"))

	entry := findExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["final_status"]; got != http.StatusInternalServerError {
		t.Fatalf("final_status = %#v, want 500", got)
	}
	if got := entry.Data["final_error_type"]; got != "server_error" {
		t.Fatalf("final_error_type = %#v, want server_error", got)
	}
	if got := entry.Data["final_error_code"]; got != "internal_server_error" {
		t.Fatalf("final_error_code = %#v, want internal_server_error", got)
	}
}

func TestLogRequestExecutionSummaryTerminalCancellationOverridesStale503(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	ctx := logging.WithRequestID(context.Background(), "req-summary-cancel-after-503")
	trace := &requestAttemptTrace{
		requestID:   "req-summary-cancel-after-503",
		attempts:    1,
		maxAttempts: 1,
		finalStatus: http.StatusServiceUnavailable,
	}
	logRequestExecutionSummary(ctx, trace, false, newCallerRequestFailure(context.DeadlineExceeded))

	entry := findExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["final_status"]; got != 499 {
		t.Fatalf("final_status = %#v, want 499", got)
	}
	if got := entry.Data["final_error_type"]; got != "cancelled" {
		t.Fatalf("final_error_type = %#v, want cancelled", got)
	}
	if got := entry.Data["final_error_code"]; got != "request_canceled" {
		t.Fatalf("final_error_code = %#v, want request_canceled", got)
	}
}

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

func TestLogRequestExecutionSummaryIncludesGPTFirstEventMetrics(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	ctx := logging.WithRequestID(context.Background(), "req-summary-gpt-first-event")
	trace := &requestAttemptTrace{
		requestID:             "req-summary-gpt-first-event",
		attempts:              2,
		maxAttempts:           12,
		translatorRuns:        2,
		finalStatus:           http.StatusOK,
		finalProvider:         "codex",
		finalModel:            "gpt-5.6-sol",
		finalExecutor:         "CodexExecutor",
		gptFirstEventTimeouts: 1,
		gptFirstEventWait:     27 * time.Second,
		gptFirstEventPolicy: GPTFirstEventPolicySnapshot{
			DecisionSource:    "gpt-5.6-sol",
			PolicyState:       gptFirstEventPolicyStateSlow30,
			DecisionReason:    "first_event_rate_below_90_percent",
			EnforcedTimeoutMs: 30000,
			MaxChannels:       6,
			MaxRounds:         2,
			WaitBudgetMs:      300000,
		},
	}

	logRequestExecutionSummary(ctx, trace, true, nil)

	entry := findExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["first_event_timeout_count"]; got != 1 {
		t.Fatalf("first_event_timeout_count = %#v, want 1", got)
	}
	if got := entry.Data["first_event_wait_ms"]; got != int64(27000) {
		t.Fatalf("first_event_wait_ms = %#v, want 27000", got)
	}
	if got := entry.Data["first_event_timeout_ms"]; got != int64(30000) {
		t.Fatalf("first_event_timeout_ms = %#v, want 30000", got)
	}
	if got := entry.Data["first_event_policy_state"]; got != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("first_event_policy_state = %#v, want %q", got, gptFirstEventPolicyStateSlow30)
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
