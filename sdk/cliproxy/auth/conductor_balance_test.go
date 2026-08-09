package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestManagerMarkResult_DisablesAuthOnInsufficientBalance402(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
	}{
		{name: "english", message: "Insufficient Balance"},
		{name: "chinese", message: "余额不足，请充值后重试"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:       "balance-auth-" + tt.name,
				Provider: "deepseek",
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			manager.MarkResult(context.Background(), Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    "deepseek-v4-pro",
				Success:  false,
				Error: &Error{
					Code:       "upstream_error",
					Message:    tt.message,
					HTTPStatus: http.StatusPaymentRequired,
				},
			})

			updated, ok := manager.GetByID(auth.ID)
			if !ok {
				t.Fatal("auth not found")
			}
			if !updated.Disabled {
				t.Fatal("expected auth to be disabled after insufficient balance")
			}
			if updated.Status != StatusDisabled {
				t.Fatalf("status = %q, want %q", updated.Status, StatusDisabled)
			}
			if updated.StatusMessage != "disabled due to insufficient balance" {
				t.Fatalf("status message = %q", updated.StatusMessage)
			}
			state := updated.ModelStates["deepseek-v4-pro"]
			if state == nil || state.Status != StatusDisabled {
				t.Fatalf("model state = %+v, want disabled", state)
			}
		})
	}
}

func TestManagerMarkResult_DoesNotDisableBillingCycleQuota402(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "billing-cycle-auth",
		Provider: "claude",
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet-4-6",
		Success:  false,
		Error: &Error{
			Code:       "quota_exceeded",
			Message:    "You have reached your usage limit for this billing cycle.",
			HTTPStatus: http.StatusPaymentRequired,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth not found")
	}
	if updated.Disabled || updated.Status == StatusDisabled {
		t.Fatalf("auth disabled=%v status=%q, want quota cooldown but not disabled", updated.Disabled, updated.Status)
	}
	state := updated.ModelStates["claude-sonnet-4-6"]
	if state == nil || state.Status == StatusDisabled {
		t.Fatalf("model state = %+v, want non-disabled quota state", state)
	}
	if state.Quota.Reason != "billing_cycle_quota" {
		t.Fatalf("quota reason = %q, want billing_cycle_quota", state.Quota.Reason)
	}
}

func TestManagerMarkResult_TypedOuter403InsufficientBalanceDisablesAuthWithoutPublicText(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "typed-balance-auth", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	cause := &failurecontract.Failure{
		Kind:          failurecontract.QuotaExceeded,
		Scope:         failurecontract.ScopeCredential,
		HTTPStatus:    http.StatusTooManyRequests,
		OuterStatus:   http.StatusForbidden,
		SemanticCode:  "insufficient_balance",
		SemanticType:  "insufficient_balance",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		Retryable:     true,
		PublicMessage: `upstream request failed [BODY METADATA v1] {"bytes":97,"sha256":"redacted","truncated":false}`,
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.6-sol",
		Success:  false,
		Cause:    cause,
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth not found")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("typed insufficient balance state = disabled:%v status:%q", updated.Disabled, updated.Status)
	}
	if updated.LastError == nil || updated.LastError.Code != "insufficient_balance" || updated.LastError.Kind != string(failurecontract.QuotaExceeded) {
		t.Fatalf("typed insufficient balance metadata = %+v", updated.LastError)
	}
}

func TestManagerMarkResult_TypedUsageLimitUsesSemanticQuotaAndRetryAfterWithoutPublicText(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "typed-usage-limit-auth", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	retryAfter := 37 * time.Second
	cause := &failurecontract.Failure{
		Kind:          failurecontract.QuotaExceeded,
		Scope:         failurecontract.ScopeCredential,
		HTTPStatus:    http.StatusTooManyRequests,
		OuterStatus:   http.StatusBadRequest,
		SemanticCode:  "usage_limit_reached",
		SemanticType:  "usage_limit_reached",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		RetryAfter:    &retryAfter,
		Retryable:     false,
		PublicMessage: `upstream request failed [BODY METADATA v1] {"bytes":97,"sha256":"redacted","truncated":false}`,
	}
	startedAt := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.6-sol",
		Success:  false,
		Cause:    cause,
	})
	finishedAt := time.Now()

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth not found")
	}
	if updated.Disabled || updated.Status == StatusDisabled {
		t.Fatalf("typed usage limit disabled auth: disabled=%v status=%q", updated.Disabled, updated.Status)
	}
	if !updated.Unavailable || !updated.Quota.Exceeded || updated.Quota.Reason != "billing_cycle_quota" {
		t.Fatalf("typed usage limit quota state = unavailable:%v quota:%+v", updated.Unavailable, updated.Quota)
	}
	earliest := startedAt.Add(retryAfter)
	latest := finishedAt.Add(retryAfter)
	if updated.NextRetryAfter.Before(earliest) || updated.NextRetryAfter.After(latest) {
		t.Fatalf("typed usage limit retry at = %v, want between %v and %v", updated.NextRetryAfter, earliest, latest)
	}
	if updated.LastError == nil || updated.LastError.Code != "usage_limit_reached" || updated.LastError.Kind != string(failurecontract.QuotaExceeded) {
		t.Fatalf("typed usage limit metadata = %+v", updated.LastError)
	}
}
