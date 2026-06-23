package auth

import (
	"context"
	"testing"
	"time"
)

func TestManager_MarkResult_524WaitsForRepeatedFailuresBeforeModelCooldown(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-524", Provider: "claude"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet-4-6",
		Success:  false,
		Error:    &Error{HTTPStatus: 524, Message: "gateway timeout"},
	})

	manager.mu.RLock()
	updated := manager.auths[auth.ID]
	manager.mu.RUnlock()
	if updated == nil {
		t.Fatal("expected registered auth to remain present")
	}
	state := updated.ModelStates["claude-sonnet-4-6"]
	if state == nil {
		t.Fatal("expected model state for claude-sonnet-4-6")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("first 524 model cooldown = %v, want zero", state.NextRetryAfter)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet-4-6",
		Success:  false,
		Error:    &Error{HTTPStatus: 524, Message: "gateway timeout"},
	})
	before := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "claude-sonnet-4-6",
		Success:  false,
		Error:    &Error{HTTPStatus: 524, Message: "gateway timeout"},
	})
	after := time.Now()

	manager.mu.RLock()
	updated = manager.auths[auth.ID]
	manager.mu.RUnlock()
	state = updated.ModelStates["claude-sonnet-4-6"]
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected repeated 524 to set model cooldown")
	}
	minExpected := before.Add(85 * time.Second)
	maxExpected := after.Add(95 * time.Second)
	if state.NextRetryAfter.Before(minExpected) || state.NextRetryAfter.After(maxExpected) {
		t.Fatalf("model cooldown = %v, want within [%v, %v]", state.NextRetryAfter, minExpected, maxExpected)
	}
}

func TestApplyAuthFailureState_524WaitsForRepeatedFailuresBeforeAuthCooldown(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "auth-524", Provider: "claude"}
	before := time.Now()
	applyAuthFailureState(auth, &Error{HTTPStatus: 524, Message: "gateway timeout"}, nil, before, false)

	if auth.StatusMessage != "transient upstream error" {
		t.Fatalf("status message = %q, want %q", auth.StatusMessage, "transient upstream error")
	}
	if !auth.NextRetryAfter.IsZero() {
		t.Fatalf("first 524 auth cooldown = %v, want zero", auth.NextRetryAfter)
	}

	applyAuthFailureState(auth, &Error{HTTPStatus: 524, Message: "gateway timeout"}, nil, before.Add(30*time.Second), false)
	applyAuthFailureState(auth, &Error{HTTPStatus: 524, Message: "gateway timeout"}, nil, before.Add(time.Minute), false)

	if auth.NextRetryAfter.IsZero() {
		t.Fatal("expected repeated 524 to set auth cooldown")
	}
	minExpected := before.Add(145 * time.Second)
	maxExpected := before.Add(155 * time.Second)
	if auth.NextRetryAfter.Before(minExpected) || auth.NextRetryAfter.After(maxExpected) {
		t.Fatalf("auth cooldown = %v, want within [%v, %v]", auth.NextRetryAfter, minExpected, maxExpected)
	}
}
