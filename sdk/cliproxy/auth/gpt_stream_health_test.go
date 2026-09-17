package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestGPTCommittedFailureHealthClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  failurecontract.Kind
		scope failurecontract.Scope
		count bool
	}{
		{"provider", failurecontract.ProviderUnavailable, failurecontract.ScopeProvider, true},
		{"transport", failurecontract.TransportError, failurecontract.ScopeProvider, true},
		{"protocol", failurecontract.UpstreamProtocolError, failurecontract.ScopeProvider, true},
		{"model", failurecontract.ModelUnavailable, failurecontract.ScopeModel, true},
		{"client_cancel", failurecontract.Cancelled, failurecontract.ScopeRequest, false},
		{"mis_scoped_cancel", failurecontract.Cancelled, failurecontract.ScopeProvider, false},
		{"input", failurecontract.InvalidRequest, failurecontract.ScopeRequest, false},
		{"credentials", failurecontract.AuthenticationFailed, failurecontract.ScopeCredential, false},
		{"rate_limit", failurecontract.RateLimited, failurecontract.ScopeModel, false},
		{"transform", failurecontract.InternalTransformError, failurecontract.ScopeProvider, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &failurecontract.Failure{
				Kind: tc.kind, Scope: tc.scope, HTTPStatus: http.StatusBadGateway,
				OutputCommitted: true, StreamPhase: failurecontract.StreamPhaseAfterOutput,
				Retryable: false,
			}
			result := Result{Error: resultErrorFromCause(failure), Cause: failure}
			if got := shouldCountCodexChannelBreakerFailure(result); got != tc.count {
				t.Fatalf("count health failure = %v, want %v", got, tc.count)
			}
			if shouldFailoverGPTChannel(failure, []string{"codex"}, "gpt-6-astra") {
				t.Fatal("committed output became replayable")
			}
		})
	}
}

func TestGPTCommittedFailuresEscapeAffinityAndRecoverWithProbe(t *testing.T) {
	const model = "gpt-6-astra"
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	primary := gptChannelBreakerTestAuth("late-primary", "https://bad.example/v1")
	peer := gptChannelBreakerTestAuth("late-peer", "https://bad.example/v1")
	backup := gptChannelBreakerTestAuth("late-backup", "https://good.example/v1")
	for _, auth := range []*Auth{primary, peer, backup} {
		manager.auths[auth.ID] = auth
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": {"late-session"}}}
	cacheKey := "codex::" + ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata) + "::" + model
	selector.cache.SetBinding(cacheKey, primary.ID, routingChannelBaseKey(primary))
	failure := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusBadGateway, OuterStatus: http.StatusOK,
		SemanticType: "fastapi_error", OutputCommitted: true,
		StreamPhase: failurecontract.StreamPhaseAfterOutput, Retryable: false,
	}
	for range codexChannelBreakerOpen5xxFailures {
		manager.MarkResult(context.Background(), Result{AuthID: primary.ID, Provider: "codex", Model: model, Error: resultErrorFromCause(failure), Cause: failure})
	}
	for _, auth := range []*Auth{primary, peer} {
		if state := auth.ModelStates[model]; state == nil || state.Health.BreakerState != HealthBreakerOpen {
			t.Fatalf("failed channel credential %s was not blocked", auth.ID)
		}
		if state := auth.ModelStates["gpt-5.6-sol"]; state != nil && state.Health.Observed {
			t.Fatal("stream failures affected another model")
		}
	}
	ctx, _ := ensureRequestAttemptTrace(context.Background())
	picked, errPick := selector.Pick(ctx, "codex", model, opts, []*Auth{primary, peer, backup})
	if errPick != nil || picked == nil || picked.ID != backup.ID {
		t.Fatalf("next request remained on failed affinity channel: picked=%+v error=%v", picked, errPick)
	}
	manager.MarkResult(ctx, Result{AuthID: backup.ID, Provider: "codex", Model: model, Success: true})
	if bound, _ := selector.cache.Get(cacheKey); bound != backup.ID {
		t.Fatalf("successful fallback binding = %q, want %q", bound, backup.ID)
	}

	state := manager.gptChannelBreakers[gptChannelBreakerKey(primary, model)]
	probeAt := state.Health.OpenUntil
	if !reserveCodexChannelProbe(state, "late-probe", probeAt) {
		t.Fatal("recovery probe rejected")
	}
	applyCodexChannelBreakerResult(state, Result{Error: resultErrorFromCause(failure), Cause: failure}, probeAt, "late-probe")
	assertGPTChannelCooldown(t, *state, probeAt, time.Minute, 2)
}

func TestGPTCommittedFailureLowersSpreadOutcome(t *testing.T) {
	const model = "gpt-6-astra"
	spread := &SpreadSelector{}
	manager := NewManager(nil, spread, nil)
	bad := gptChannelBreakerTestAuth("spread-late-bad", "https://bad.example/v1")
	good := gptChannelBreakerTestAuth("spread-late-good", "https://good.example/v1")
	failure := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusBadGateway, OutputCommitted: true, Retryable: false,
	}
	for _, auth := range []*Auth{bad, good} {
		spread.markPickedAuth("codex", model, auth)
		ctx, trace := ensureRequestAttemptTrace(context.Background())
		trace.stageSelectorSelection(spread, "codex", model, auth.ID)
		result := Result{AuthID: auth.ID, Provider: "codex", Model: model, Success: auth == good, TTFT: time.Second}
		if !result.Success {
			result.Cause = failure
			result.Error = resultErrorFromCause(failure)
		}
		manager.markSelectorResult(ctx, result)
	}
	if spread.keepAffinity("codex", model, bad.ID, []*Auth{bad, good}) {
		t.Fatal("failed stream remained as sticky as a completed stream")
	}
	spread.mu.Lock()
	defer spread.mu.Unlock()
	record := spread.load.records["codex:"+model][routingChannelBaseKey(bad)]
	if record == nil || !record.outcomeObserved || record.successEWMA != 0 || record.inFlight != 0 {
		t.Fatalf("failed stream outcome or lease was lost: %+v", record)
	}
}
