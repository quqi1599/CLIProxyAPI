package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerGPTChannelBreakerClosedSuccessSkipsChannelFanout(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	selected := gptChannelBreakerTestAuth("selected", "https://healthy.example/v1")
	peer := gptChannelBreakerTestAuth("peer", "https://healthy.example/v1")
	manager.auths[selected.ID] = selected
	manager.auths[peer.ID] = peer

	channelKey := routingChannelBaseKey(selected)
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
	}, now)
	manager.mu.Unlock()

	if len(snapshots) != 0 {
		t.Fatalf("healthy closed success snapshots = %d, want 0", len(snapshots))
	}
	if peer.Health.Observed || !peer.UpdatedAt.IsZero() {
		t.Fatalf("healthy peer was needlessly updated: health=%+v updatedAt=%v", peer.Health, peer.UpdatedAt)
	}
}

func TestManagerGPTChannelBreakerClosedRecoveryPropagates(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	selected := gptChannelBreakerTestAuth("selected-recovery", "https://recovering.example/v1")
	peer := gptChannelBreakerTestAuth("peer-recovery", "https://recovering.example/v1")
	manager.auths[selected.ID] = selected
	manager.auths[peer.ID] = peer

	channelKey := routingChannelBaseKey(selected)
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
	}, now)
	manager.mu.Unlock()

	if len(snapshots) != 2 {
		t.Fatalf("recovering closed success snapshots = %d, want 2", len(snapshots))
	}
	if !peer.Health.Observed || peer.Health.Score <= 60 {
		t.Fatalf("recovering peer health = %+v, want propagated improvement", peer.Health)
	}
}

func TestManagerGPTChannelBreakerTypedCredential429OpensChannel(t *testing.T) {
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

	for _, authID := range []string{auth.ID, peer.ID} {
		health := manager.auths[authID].Health
		if health.BreakerState != HealthBreakerOpen {
			t.Fatalf("auth %s channel breaker = %+v, want open after 8/10 typed credential 429s", authID, health)
		}
	}
}

func TestManagerGPTChannelBreakerHalfOpenRequestErrorReleasesProbe(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := gptChannelBreakerTestAuth("half-open-request-error", "https://half-open-request-error.example/v1")
	manager.auths[auth.ID] = auth

	ctx, trace := ensureRequestAttemptTrace(context.Background())
	key := routingChannelBaseKey(auth)
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
	_, exists := manager.gptChannelBreakers[routingChannelBaseKey(codexAuths[0])]
	manager.mu.RUnlock()
	if exists {
		t.Fatal("mixed non-GPT route created a GPT channel breaker")
	}
}
