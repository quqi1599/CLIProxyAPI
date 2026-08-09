package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerGPTChannelBreakerClosedSuccessSkipsChannelFanout(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	selected := gptChannelBreakerTestAuth("selected", "https://healthy.example/v1")
	peer := gptChannelBreakerTestAuth("peer", "https://healthy.example/v1")
	manager.auths[selected.ID] = selected
	manager.auths[peer.ID] = peer

	channelKey := gptChannelBreakerKey(selected, "gpt-5.5")
	now := time.Now()
	manager.gptChannelBreakers[channelKey] = &codexChannelBreakerState{
		Health: HealthState{
			Observed:      true,
			Score:         healthScoreDefault,
			BreakerState:  HealthBreakerClosed,
			LastUpdatedAt: now.Add(-time.Second),
		},
	}

	manager.mu.Lock()
	snapshots := manager.applyGPTChannelBreakerResultLocked(context.Background(), selected, Result{
		AuthID:   selected.ID,
		Provider: "codex",
		Model:    "gpt-5.5",
		Success:  true,
	}, "gpt-5.5", now)
	manager.mu.Unlock()

	if len(snapshots) != 0 {
		t.Fatalf("healthy closed success snapshots = %d, want 0", len(snapshots))
	}
	if state := peer.ModelStates["gpt-5.5"]; state != nil && state.Health.Observed {
		t.Fatalf("healthy peer model was needlessly updated: health=%+v", state.Health)
	}
}

func TestManagerGPTChannelBreakerClosedRecoveryPropagates(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	selected := gptChannelBreakerTestAuth("selected-recovery", "https://recovering.example/v1")
	peer := gptChannelBreakerTestAuth("peer-recovery", "https://recovering.example/v1")
	manager.auths[selected.ID] = selected
	manager.auths[peer.ID] = peer

	channelKey := gptChannelBreakerKey(selected, "gpt-5.5")
	now := time.Now()
	manager.gptChannelBreakers[channelKey] = &codexChannelBreakerState{
		Health: HealthState{
			Observed:      true,
			Score:         60,
			BreakerState:  HealthBreakerClosed,
			LastUpdatedAt: now,
		},
	}

	manager.mu.Lock()
	snapshots := manager.applyGPTChannelBreakerResultLocked(context.Background(), selected, Result{
		AuthID:   selected.ID,
		Provider: "codex",
		Model:    "gpt-5.5",
		Success:  true,
	}, "gpt-5.5", now)
	manager.mu.Unlock()

	if len(snapshots) != 2 {
		t.Fatalf("recovering closed success snapshots = %d, want 2", len(snapshots))
	}
	peerState := peer.ModelStates["gpt-5.5"]
	if peerState == nil || !peerState.Health.Observed || peerState.Health.Score <= 60 {
		t.Fatalf("recovering peer model health = %+v, want propagated improvement", peerState)
	}
}

func TestManagerGPTChannelBreakerTypedCredential429DoesNotOpenChannel(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("typed-429", "https://typed-429.example/v1")
	peer := gptChannelBreakerTestAuth("typed-429-peer", "https://typed-429.example/v1")
	manager.auths[auth.ID] = auth
	manager.auths[peer.ID] = peer

	for i := 0; i < 2; i++ {
		manager.MarkResult(context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5.5",
			Success:  true,
		})
	}
	for i := 0; i < 8; i++ {
		cause := &failurecontract.Failure{
			Kind:          failurecontract.RateLimited,
			Scope:         failurecontract.ScopeCredential,
			HTTPStatus:    http.StatusTooManyRequests,
			Retryable:     true,
			PublicMessage: "upstream rate limited",
		}
		manager.MarkResult(context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    "gpt-5.5",
			Success:  false,
			Error:    resultErrorFromCause(cause),
			Cause:    cause,
		})
	}

	manager.mu.RLock()
	state := manager.gptChannelBreakers[gptChannelBreakerKey(auth, "gpt-5.5")]
	manager.mu.RUnlock()
	if state == nil || state.Health.BreakerState == HealthBreakerOpen || state.consecutive5xx != 0 || state.recentCount != 2 {
		t.Fatalf("typed credential 429 affected route/model breaker: %+v", state)
	}
	if !manager.auths[auth.ID].Unavailable {
		t.Fatal("typed credential 429 did not cool the affected credential")
	}
	if manager.auths[peer.ID].Unavailable {
		t.Fatal("typed credential 429 cooled a peer credential")
	}
}

func TestManagerGPTFirstEventTimeoutDoesNotCoolCodexAPIKeyRoute(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("local-first-event-timeout", "https://slow.example/v1")
	manager.auths[auth.ID] = auth
	model := "gpt-5.6-sol"
	timeoutErr := &Error{
		Code:       "gpt_first_event_timeout",
		Message:    "local first-event deadline exceeded",
		Retryable:  true,
		HTTPStatus: http.StatusGatewayTimeout,
	}

	for i := 0; i < codexChannelBreakerSampleLimit*2; i++ {
		manager.MarkResult(context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "codex",
			Model:    model,
			Success:  false,
			Error:    resultErrorFromCause(timeoutErr),
			Cause:    timeoutErr,
		})
	}

	manager.mu.RLock()
	breaker := manager.gptChannelBreakers[gptChannelBreakerKey(auth, model)]
	state := manager.auths[auth.ID].ModelStates[model]
	manager.mu.RUnlock()
	if breaker != nil && (breaker.Health.Observed || breaker.recentCount > 0 || breaker.Health.BreakerState == HealthBreakerOpen) {
		t.Fatalf("local timeout affected GPT route breaker: %+v", breaker)
	}
	if state != nil && (state.Health.Observed || state.Health.BreakerState == HealthBreakerOpen || state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("local timeout cooled Codex API-key route: %+v", state)
	}
}

func TestManagerLegacyGPT500BecomesProviderHealthEvidence(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("opaque-500", "https://opaque.example/v1")
	manager.auths[auth.ID] = auth
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.configureGPTRoute(true)
	cause := &Error{HTTPStatus: http.StatusInternalServerError, Message: "opaque failure"}

	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.6-sol",
		Error:    cause,
		Cause:    cause,
	})

	breaker := manager.gptChannelBreakers[gptChannelBreakerKey(auth, "gpt-5.6-sol")]
	if breaker == nil || breaker.recentCount != 1 || breaker.Health.BreakerState == HealthBreakerOpen {
		t.Fatalf("provider 500 breaker = %+v, want one recorded failure without opening", breaker)
	}
	state := auth.ModelStates["gpt-5.6-sol"]
	if state == nil || !state.Health.Observed || state.Unavailable || state.Health.BreakerState == HealthBreakerOpen {
		t.Fatalf("provider 500 model state = %+v, want observed soft health without cooldown", state)
	}
}

func TestManagerMarkResultClearsActiveHalfOpenBeforeReleasingWaiters(t *testing.T) {
	const model = "gpt-5.6-sol"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("half-open-owner", "https://half-open.example/v1")
	manager.auths[auth.ID] = auth

	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.requestID = "req-half-open-owner"
	trace.configureGPTRoute(true)
	now := time.Now()
	if ok, _ := manager.reserveHalfOpenProbe(auth.ID, model, now); !ok {
		t.Fatal("reserveHalfOpenProbe() = false, want owner")
	}
	if ok, _ := manager.reserveZeroEligibleProbe(ctx, model, now); !ok {
		t.Fatal("reserveZeroEligibleProbe() = false, want owner")
	}

	failure := &failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		SemanticCode:  "upstream_unavailable",
		Retryable:     true,
		PublicMessage: "upstream unavailable",
	}
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    model,
		Error:    resultErrorFromCause(failure),
		Cause:    failure,
	})

	if manager.halfOpenProbeActive(auth.ID, model, time.Now()) {
		t.Fatal("completed probe remained active and could bypass cooldown for waiters")
	}
	key := halfOpenProbeKey(auth.ID, model)
	manager.halfOpenProbeMu.Lock()
	next := manager.halfOpenProbeNext[key]
	manager.halfOpenProbeMu.Unlock()
	if next.IsZero() || !next.After(now) {
		t.Fatalf("next probe interval = %v, want preserved future gate", next)
	}

	zeroKey := zeroEligibleProbeKey(model)
	manager.zeroEligibleProbeMu.Lock()
	lease := manager.zeroEligibleProbes[zeroKey]
	manager.zeroEligibleProbeMu.Unlock()
	if lease.done != nil || lease.requestID != "" {
		t.Fatalf("zero-route lease = %+v, want released after active bypass cleared", lease)
	}
}

func TestManagerCanceledProbeOwnerClearsActiveHalfOpenBeforeWake(t *testing.T) {
	const model = "gpt-5.6-sol"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("canceled-half-open-owner", "https://canceled-half-open.example/v1")
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.requestID = "req-canceled-half-open-owner"
	trace.configureGPTRoute(true)

	now := time.Now()
	if ok, _ := manager.reserveHalfOpenProbe(auth.ID, model, now); !ok {
		t.Fatal("reserveHalfOpenProbe() = false, want owner")
	}
	if ok, _ := manager.reserveZeroEligibleProbe(ctx, model, now); !ok {
		t.Fatal("reserveZeroEligibleProbe() = false, want owner")
	}
	manager.bindZeroEligibleProbeRoute(ctx, model, auth.ID, model)
	key := zeroEligibleProbeKey(model)
	manager.zeroEligibleProbeMu.Lock()
	done := manager.zeroEligibleProbes[key].done
	manager.zeroEligibleProbeMu.Unlock()
	if done == nil {
		t.Fatal("zero-route owner has no completion channel")
	}

	// This is the common cleanup path when the owner returns on caller cancel
	// before producing a Result/MarkResult call.
	manager.releaseZeroEligibleProbe(ctx, model)
	if manager.halfOpenProbeActive(auth.ID, model, time.Now()) {
		t.Fatal("canceled owner left half-open bypass active")
	}
	select {
	case <-done:
	default:
		t.Fatal("waiters were not released after the active bypass was cleared")
	}
	probeKey := halfOpenProbeKey(auth.ID, model)
	manager.halfOpenProbeMu.Lock()
	next := manager.halfOpenProbeNext[probeKey]
	manager.halfOpenProbeMu.Unlock()
	if next.IsZero() || !next.After(now) {
		t.Fatalf("next probe interval = %v, want preserved future gate", next)
	}
}

func TestManagerStatuslessGPTInternalFailureDoesNotPolluteRouteHealth(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("statusless-internal", "https://internal.example/v1")
	manager.auths[auth.ID] = auth
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.configureGPTRoute(true)
	cause := errors.New("opaque executor failure")

	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.6-sol",
		Error:    resultErrorFromCause(cause),
		Cause:    cause,
	})

	if len(manager.gptChannelBreakers) != 0 {
		t.Fatalf("statusless internal failure created GPT breaker state: %+v", manager.gptChannelBreakers)
	}
	state := auth.ModelStates["gpt-5.6-sol"]
	if state != nil && (state.Unavailable || state.Health.Observed) {
		t.Fatalf("statusless internal failure polluted model health: %+v", state)
	}
}

func TestManagerGPTChannelBreakerIsolatedByModel(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("model-isolation", "https://model-isolation.example/v1")
	manager.auths[auth.ID] = auth
	now := time.Now()
	terraKey := gptChannelBreakerKey(auth, "gpt-5.6-terra")
	manager.gptChannelBreakers[terraKey] = &codexChannelBreakerState{Health: HealthState{
		BreakerState: HealthBreakerOpen,
		OpenUntil:    now.Add(time.Minute),
	}}

	if !manager.gptChannelBreakerOpen(auth, "gpt-5.6-terra", now) {
		t.Fatal("Terra breaker is not open")
	}
	if manager.gptChannelBreakerOpen(auth, "gpt-5.4", now) {
		t.Fatal("Terra breaker leaked into gpt-5.4")
	}
}

func TestManagerZeroEligibleGPTProbeIsSingleFlightPerModel(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	now := time.Unix(1_750_000_000, 0)
	ctxA, _ := ensureRequestAttemptTrace(context.Background())
	ctxB, _ := ensureRequestAttemptTrace(context.Background())
	candidates := []cooldownFallbackCandidate{
		{auth: gptChannelBreakerTestAuth("probe-a", "https://probe-a.example/v1"), model: "gpt-5.6-terra", next: now.Add(time.Minute)},
		{auth: gptChannelBreakerTestAuth("probe-b", "https://probe-b.example/v1"), model: "gpt-5.6-terra", next: now.Add(time.Minute)},
	}

	first, _ := manager.zeroEligibleFallbackProbe(ctxA, "gpt-5.6-terra", candidates, now, true)
	if first == nil || first.auth == nil {
		t.Fatal("first zero-eligible request did not acquire a probe")
	}
	if !manager.zeroEligibleProbeBlocksRequest(ctxB, "gpt-5.6-terra", now) {
		t.Fatal("second request was not blocked by the active model probe")
	}
	if manager.zeroEligibleProbeBlocksRequest(ctxA, "gpt-5.6-terra", now) {
		t.Fatal("probe owner was blocked from its own active lease")
	}
	second, next := manager.zeroEligibleFallbackProbe(ctxB, "gpt-5.6-terra", candidates, now, true)
	if second != nil || !next.After(now) {
		t.Fatalf("second concurrent probe = %+v next=%v, want blocked with recovery time", second, next)
	}

	manager.releaseZeroEligibleProbe(ctxA, "gpt-5.6-terra")
	second, next = manager.zeroEligibleFallbackProbe(ctxB, "gpt-5.6-terra", candidates, now.Add(time.Second), true)
	if second != nil || !next.After(now.Add(time.Second)) {
		t.Fatalf("probe interval was bypassed after release: probe=%+v next=%v", second, next)
	}
	second, _ = manager.zeroEligibleFallbackProbe(ctxB, "gpt-5.6-terra", candidates, now.Add(healthHalfOpenInterval+time.Second), true)
	if second == nil || second.auth == nil {
		t.Fatal("new probe was not allowed after the single-flight interval")
	}
}

func TestManagerZeroEligibleGPTProbeIsolatedByCandidateRoutes(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	model := "gpt-5.6-sol"
	now := time.Now()
	ctxA, _ := ensureRequestAttemptTrace(context.Background())
	ctxB, _ := ensureRequestAttemptTrace(context.Background())
	routeA := gptChannelBreakerTestAuth("route-a", "https://route-a.example/v1")
	routeB := gptChannelBreakerTestAuth("route-b", "https://route-b.example/v1")

	keyA := zeroEligibleProbeScopeKey(model, []*Auth{routeA})
	keyB := zeroEligibleProbeScopeKey(model, []*Auth{routeB})
	if keyA == keyB {
		t.Fatalf("independent candidate sets produced the same probe key %q", keyA)
	}
	manager.configureZeroEligibleProbeScope(ctxA, keyA)
	manager.configureZeroEligibleProbeScope(ctxB, keyB)
	if ok, _ := manager.reserveZeroEligibleProbe(ctxA, model, now); !ok {
		t.Fatal("route A did not acquire its probe")
	}
	if ok, _ := manager.reserveZeroEligibleProbe(ctxB, model, now); !ok {
		t.Fatal("route B was incorrectly blocked by route A's probe")
	}

	manager.releaseZeroEligibleProbe(ctxA, model)
	manager.releaseZeroEligibleProbe(ctxB, model)
}

func TestManagerZeroEligibleGPTProbeWaitersAreBoundedAndReleased(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	model := "gpt-5.6-sol"
	ownerCtx, _ := ensureRequestAttemptTrace(context.Background())
	waiterCtx, _ := ensureRequestAttemptTrace(context.Background())

	if ok, _ := manager.reserveZeroEligibleProbe(ownerCtx, model, time.Now()); !ok {
		t.Fatal("probe owner did not acquire the zero-eligible lease")
	}

	type waitResult struct {
		state zeroEligibleProbeWaitState
		err   error
	}
	resultCh := make(chan waitResult, 1)
	go func() {
		state, errWait := manager.waitForZeroEligibleProbe(waiterCtx, model, time.Second)
		resultCh <- waitResult{state: state, err: errWait}
	}()

	time.Sleep(20 * time.Millisecond)
	manager.releaseZeroEligibleProbe(ownerCtx, model)

	select {
	case result := <-resultCh:
		if result.err != nil || result.state != zeroEligibleProbeWaitCompleted {
			t.Fatalf("wait result = %+v, want a completed bounded wait", result)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not released when the single probe completed")
	}

	startedAt := time.Now()
	waitState, errWait := manager.waitForZeroEligibleProbe(waiterCtx, model, 50*time.Millisecond)
	if errWait != nil || waitState != zeroEligibleProbeWaitNone {
		t.Fatalf("wait without an active probe = (%v, %v), want no wait", waitState, errWait)
	}
	if elapsed := time.Since(startedAt); elapsed > 200*time.Millisecond {
		t.Fatalf("inactive probe wait took %v, want immediate return", elapsed)
	}

	if ok, _ := manager.reserveZeroEligibleProbe(ownerCtx, model, time.Now().Add(healthHalfOpenInterval)); !ok {
		t.Fatal("probe owner did not reacquire the lease for waiter-cap validation")
	}
	key := zeroEligibleProbeKey(model)
	manager.zeroEligibleProbeMu.Lock()
	lease := manager.zeroEligibleProbes[key]
	lease.waiters = gptZeroEligibleProbeMaxWaiters
	manager.zeroEligibleProbes[key] = lease
	manager.zeroEligibleProbeMu.Unlock()
	startedAt = time.Now()
	waitState, errWait = manager.waitForZeroEligibleProbe(waiterCtx, model, 50*time.Millisecond)
	if errWait != nil || waitState != zeroEligibleProbeWaitRejected {
		t.Fatalf("capped waiter = (%v, %v), want immediate rejection", waitState, errWait)
	}
	if elapsed := time.Since(startedAt); elapsed > 200*time.Millisecond {
		t.Fatalf("capped waiter took %v, want immediate return", elapsed)
	}
	manager.releaseZeroEligibleProbe(ownerCtx, model)
}

func TestManagerZeroEligibleGPTProbeWaitCancellationReleasesCapacity(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	model := "gpt-5.6-sol"
	ownerCtx, _ := ensureRequestAttemptTrace(context.Background())
	waiterBase, _ := ensureRequestAttemptTrace(context.Background())
	if ok, _ := manager.reserveZeroEligibleProbe(ownerCtx, model, time.Now()); !ok {
		t.Fatal("probe owner did not acquire the zero-eligible lease")
	}
	waiterCtx, cancel := context.WithTimeout(waiterBase, 20*time.Millisecond)
	defer cancel()
	waitState, errWait := manager.waitForZeroEligibleProbe(waiterCtx, model, time.Second)
	if waitState != zeroEligibleProbeWaitTimedOut || !errors.Is(errWait, context.DeadlineExceeded) {
		t.Fatalf("cancelled wait = (%v, %v), want timed-out caller deadline", waitState, errWait)
	}

	key := zeroEligibleProbeKey(model)
	manager.zeroEligibleProbeMu.Lock()
	waiters := manager.zeroEligibleProbes[key].waiters
	manager.zeroEligibleProbeMu.Unlock()
	if waiters != 0 {
		t.Fatalf("waiters after cancellation = %d, want 0", waiters)
	}
	manager.releaseZeroEligibleProbe(ownerCtx, model)
}

func TestManagerAuthAvailabilityMetricFieldsReportBreakerRecovery(t *testing.T) {
	const model = "gpt-5.6-terra"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	now := time.Now()
	blocked := gptChannelBreakerTestAuth("metric-blocked", "https://metric-blocked.example/v1")
	eligible := gptChannelBreakerTestAuth("metric-eligible", "https://metric-eligible.example/v1")
	blocked.ModelStates = map[string]*ModelState{
		model: {
			Unavailable:    true,
			Status:         StatusError,
			NextRetryAfter: now.Add(time.Minute),
			Health: HealthState{
				Observed:       true,
				BreakerState:   HealthBreakerOpen,
				OpenUntil:      now.Add(time.Minute),
				LastStatusCode: http.StatusServiceUnavailable,
			},
		},
	}
	manager.auths[blocked.ID] = blocked
	manager.auths[eligible.ID] = eligible

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(blocked.ID, "codex", []*registry.ModelInfo{{ID: model}})
	modelRegistry.RegisterClient(eligible.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(blocked.ID)
		modelRegistry.UnregisterClient(eligible.ID)
	})

	fields := manager.authAvailabilityMetricFields([]string{"codex"}, model, now)
	for key, want := range map[string]any{
		"candidate_route_count": 2,
		"eligible_route_count":  1,
		"blocked_route_count":   1,
		"breaker_open_count":    1,
		"breaker_statuses":      "503",
		"breaker_reasons":       "status_503:1",
	} {
		if got := fields[key]; got != want {
			t.Fatalf("%s = %v, want %v; fields=%v", key, got, want, fields)
		}
	}
	if recoveryMS, ok := fields["earliest_recovery_ms"].(int64); !ok || recoveryMS <= 0 {
		t.Fatalf("earliest_recovery_ms = %v, want positive int64", fields["earliest_recovery_ms"])
	}
}

func TestManagerGPTChannelBreakerHalfOpenRequestErrorReleasesProbe(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("half-open-request-error", "https://half-open-request-error.example/v1")
	manager.auths[auth.ID] = auth

	ctx, trace := ensureRequestAttemptTrace(context.Background())
	key := gptChannelBreakerKey(auth, "gpt-5.5")
	manager.gptChannelBreakers[key] = &codexChannelBreakerState{
		Health: HealthState{
			Observed:     true,
			BreakerState: HealthBreakerHalfOpen,
		},
		ProbeRequestID:  trace.requestIDValue(),
		ProbeLeaseUntil: time.Now().Add(time.Minute),
	}
	cause := &failurecontract.Failure{
		Kind:          failurecontract.InvalidRequest,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusBadRequest,
		PublicMessage: "invalid request",
	}
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.5",
		Error:    resultErrorFromCause(cause),
		Cause:    cause,
	})

	manager.mu.Lock()
	state := manager.gptChannelBreakers[key]
	if state.ProbeRequestID != "" || !state.ProbeLeaseUntil.IsZero() {
		manager.mu.Unlock()
		t.Fatalf("probe lease after request-scoped failure = %q / %v, want released", state.ProbeRequestID, state.ProbeLeaseUntil)
	}
	if !reserveCodexChannelProbe(state, "next-request", time.Now()) {
		manager.mu.Unlock()
		t.Fatal("next half-open probe was rejected after non-counted request error")
	}
	manager.mu.Unlock()
}

func TestManagerMixedProviderCodexFailuresDoNotCreateGPTBreaker(t *testing.T) {
	const model = "claude-sonnet-mixed-breaker"
	failure := retryableGPTChannelFailure(http.StatusServiceUnavailable)
	codexExecutor := &authFallbackExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"aa-mixed-codex": failure,
			"ab-mixed-codex": failure,
			"ac-mixed-codex": failure,
		},
	}
	claudeExecutor := &authFallbackExecutor{id: "claude"}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(10, 30*time.Second, 10)
	manager.RegisterExecutor(codexExecutor)
	manager.RegisterExecutor(claudeExecutor)

	codexAuths := []*Auth{
		gptChannelBreakerTestAuth("aa-mixed-codex", "https://mixed-non-gpt.example/v1"),
		gptChannelBreakerTestAuth("ab-mixed-codex", "https://mixed-non-gpt.example/v1"),
		gptChannelBreakerTestAuth("ac-mixed-codex", "https://mixed-non-gpt.example/v1"),
	}
	for _, auth := range codexAuths {
		auth.Attributes["priority"] = "10"
	}
	claudeAuth := &Auth{
		ID:       "ba-mixed-claude",
		Provider: "claude",
		Status:   StatusActive,
		Attributes: map[string]string{
			"priority": "1",
		},
	}
	registerGPTChannelFailoverAuths(t, manager, "codex", model, codexAuths)
	registerGPTChannelFailoverAuths(t, manager, "claude", model, []*Auth{claudeAuth})

	if _, errExecute := manager.Execute(context.Background(), []string{"codex", "claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("mixed execute: %v", errExecute)
	}
	if got := len(codexExecutor.ExecuteCalls()); got != 3 {
		t.Fatalf("Codex execute calls = %d, want 3 consecutive failures", got)
	}

	manager.mu.RLock()
	_, exists := manager.gptChannelBreakers[gptChannelBreakerKey(codexAuths[0], model)]
	manager.mu.RUnlock()
	if exists {
		t.Fatal("mixed non-GPT route created a GPT channel breaker")
	}
}
