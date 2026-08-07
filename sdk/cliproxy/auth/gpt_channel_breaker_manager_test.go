package auth

import (
	"context"
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
