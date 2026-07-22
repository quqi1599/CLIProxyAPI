package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestManagerAttemptRunnerRetriesThenSucceeds(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(1, time.Second, 0)
	manager.mu.Lock()
	manager.auths["retry-auth"] = &Auth{ID: "retry-auth", Provider: "retry"}
	manager.mu.Unlock()

	retryNow := time.Duration(0)
	failure := &failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusServiceUnavailable,
		Retryable:     true,
		RetryAfter:    &retryNow,
		PublicMessage: "provider unavailable",
	}
	calls := 0
	runner := managerAttemptRunner[int]{
		manager: manager,
		runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (int, error) {
			calls++
			if calls == 1 {
				return 0, failure
			}
			return 42, nil
		},
	}

	outcome := runner.run(context.Background(), []string{"retry"}, cliproxyexecutor.Request{Model: "model"}, cliproxyexecutor.Options{}, 0, time.Second)
	if calls != 2 {
		t.Fatalf("run calls = %d, want 2", calls)
	}
	if outcome.result != 42 || !outcome.success || outcome.returnErr != nil || outcome.finalErr != nil {
		t.Fatalf("outcome = %+v, want successful retry result", outcome)
	}
}

func TestManagerAttemptRunnerFallbackAndRecoveryOutcomes(t *testing.T) {
	primaryErr := errors.New("primary failure")
	fallbackErr := errors.New("fallback failure")
	tests := []struct {
		name             string
		fallback         managerAttemptFallbackFunc[int]
		recovery         managerAttemptRecoveryFunc[int]
		wantResult       int
		wantReturnErr    error
		wantFinalErr     error
		wantFinalSuccess bool
	}{
		{
			name: "fallback success",
			fallback: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, error) (int, bool, error) {
				return 7, true, nil
			},
			wantResult:       7,
			wantFinalSuccess: true,
		},
		{
			name: "fallback error",
			fallback: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, error) (int, bool, error) {
				return 0, false, fallbackErr
			},
			wantReturnErr: fallbackErr,
			wantFinalErr:  fallbackErr,
		},
		{
			name: "recovered return",
			recovery: func(error) (int, error, error, bool) {
				return 9, nil, primaryErr, true
			},
			wantResult:   9,
			wantFinalErr: primaryErr,
		},
		{
			name:          "unhandled failure",
			wantReturnErr: primaryErr,
			wantFinalErr:  primaryErr,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			runner := managerAttemptRunner[int]{
				manager: manager,
				runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (int, error) {
					return 0, primaryErr
				},
				fallback: test.fallback,
				recovery: test.recovery,
			}

			outcome := runner.run(context.Background(), []string{"provider"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, 0, 0)
			if outcome.result != test.wantResult || outcome.success != test.wantFinalSuccess {
				t.Fatalf("outcome result/success = %d/%t, want %d/%t", outcome.result, outcome.success, test.wantResult, test.wantFinalSuccess)
			}
			if !errors.Is(outcome.returnErr, test.wantReturnErr) {
				t.Fatalf("return error = %v, want %v", outcome.returnErr, test.wantReturnErr)
			}
			if !errors.Is(outcome.finalErr, test.wantFinalErr) {
				t.Fatalf("final error = %v, want %v", outcome.finalErr, test.wantFinalErr)
			}
		})
	}
}

func TestRunManagerAttemptOperationLogsFallbackSuccess(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetLevel(previousLevel) })

	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	runner := managerAttemptRunner[cliproxyexecutor.Response]{
		manager: manager,
		runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, errors.New("primary failure")
		},
		fallback: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, error) (cliproxyexecutor.Response, bool, error) {
			return cliproxyexecutor.Response{Payload: []byte("fallback")}, true, nil
		},
	}

	trace := &requestAttemptTrace{requestID: "req-fallback-success", finalStatus: http.StatusServiceUnavailable}
	ctx := context.WithValue(logging.WithRequestID(context.Background(), "req-fallback-success"), requestAttemptTraceContextKey{}, trace)
	response, errRun := runManagerAttemptOperation(ctx, manager, []string{"provider"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, runner)
	if errRun != nil {
		t.Fatalf("run error = %v, want nil", errRun)
	}
	if string(response.Payload) != "fallback" {
		t.Fatalf("response payload = %q, want fallback", response.Payload)
	}

	entry := findExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["final_success"]; got != true {
		t.Fatalf("final_success = %#v, want true", got)
	}
	if got := entry.Data["final_status"]; got != http.StatusOK {
		t.Fatalf("final_status = %#v, want %d", got, http.StatusOK)
	}
	if _, exists := entry.Data["final_error_code"]; exists {
		t.Fatalf("final_error_code unexpectedly recorded: %#v", entry.Data["final_error_code"])
	}
}
