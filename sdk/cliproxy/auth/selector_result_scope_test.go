package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestManagerMarkSelectorResultRequestFailureOnlyReleasesInflight(t *testing.T) {
	const (
		provider = "codex"
		model    = "gpt-5.5"
		authID   = "request-scoped-selector-result"
	)

	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	manager := NewManager(nil, selector, nil)
	selector.MarkPicked(provider, model, authID)
	selector.MarkResult(authID, model, true, 100*time.Millisecond)
	selector.MarkPicked(provider, model, authID)

	cause := &failurecontract.Failure{
		Kind:          failurecontract.InvalidRequest,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusBadRequest,
		PublicMessage: "invalid request",
	}
	manager.markSelectorResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error:    resultErrorFromCause(cause),
		Cause:    cause,
	})

	selector.mu.Lock()
	snapshot := selector.load.snapshot(provider+":"+canonicalModelKey(model), []string{authID}, time.Now(), spreadLoadDefaultKeyLimit)
	record := snapshot[authID]
	selector.mu.Unlock()
	if record.inFlight != 0 {
		t.Fatalf("request-scoped failure inflight = %d, want 0", record.inFlight)
	}
	if !record.outcomeObserved || record.successEWMA != 1 {
		t.Fatalf("request-scoped failure outcome = observed:%v ewma:%v, want unchanged success EWMA", record.outcomeObserved, record.successEWMA)
	}
}

func TestManagerSelectorResultUsesExactRouteAndKeepsModelPoolInflight(t *testing.T) {
	const (
		model  = "gpt-5.5"
		authID = "exact-route-selector-result"
	)
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	manager := NewManager(nil, selector, nil)
	selector.MarkPicked("mixed", model, authID)
	selector.MarkPicked("codex", model, authID)

	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.stageSelectorSelection(selector, "mixed", model, authID)
	manager.markSelectorResult(ctx, Result{
		AuthID:            authID,
		Provider:          "codex",
		Model:             model,
		Error:             &Error{HTTPStatus: http.StatusInternalServerError, Retryable: true},
		keepSelectorLease: true,
	})

	mixed := spreadRouteRecord(selector, "mixed", model, authID)
	codex := spreadRouteRecord(selector, "codex", model, authID)
	if mixed.inFlight != 1 || codex.inFlight != 1 {
		t.Fatalf("intermediate inflight = mixed:%d codex:%d, want 1/1", mixed.inFlight, codex.inFlight)
	}

	manager.markSelectorResult(ctx, Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  true,
		TTFT:     100 * time.Millisecond,
	})
	mixed = spreadRouteRecord(selector, "mixed", model, authID)
	codex = spreadRouteRecord(selector, "codex", model, authID)
	if mixed.inFlight != 0 || codex.inFlight != 1 {
		t.Fatalf("final inflight = mixed:%d codex:%d, want 0/1", mixed.inFlight, codex.inFlight)
	}
}

func spreadRouteRecord(selector *SpreadSelector, provider, model, authID string) spreadLoadRecord {
	selector.mu.Lock()
	defer selector.mu.Unlock()
	snapshot := selector.load.snapshot(provider+":"+canonicalModelKey(model), []string{authID}, time.Now(), spreadLoadDefaultKeyLimit)
	return snapshot[authID]
}
