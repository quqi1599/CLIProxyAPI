package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestManagerGPTChannelBreaker_Provider5xxIsolatedByModel(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	failing := gptChannelBreakerTestAuth("failing", "https://unstable.example/v1")
	sameChannel := gptChannelBreakerTestAuth("same-channel", "https://unstable.example/v1")
	backup := gptChannelBreakerTestAuth("backup", "https://stable.example/v1")
	manager.auths[failing.ID] = failing
	manager.auths[sameChannel.ID] = sameChannel
	manager.auths[backup.ID] = backup

	for range codexChannelBreakerOpen5xxFailures {
		failure := gptTypedChannelBreakerFailure(failurecontract.ScopeProvider, http.StatusServiceUnavailable)
		manager.MarkResult(context.Background(), Result{
			AuthID:   failing.ID,
			Provider: "codex",
			Model:    "gpt-5.6-terra",
			Success:  false,
			Error:    failure.Error,
			Cause:    failure.Cause,
		})
	}

	for _, authID := range []string{failing.ID, sameChannel.ID} {
		health := manager.auths[authID].ModelStates["gpt-5.6-terra"].Health
		if health.BreakerState != HealthBreakerOpen {
			t.Fatalf("auth %s Terra breaker = %+v, want open", authID, health)
		}
		if health.OpenUntil.IsZero() || !health.OpenUntil.After(time.Now()) {
			t.Fatalf("auth %s Terra OpenUntil = %v, want future time", authID, health.OpenUntil)
		}
		if state := manager.auths[authID].ModelStates["gpt-5.4"]; state != nil && state.Health.BreakerState == HealthBreakerOpen {
			t.Fatalf("auth %s Terra breaker leaked into gpt-5.4: %+v", authID, state.Health)
		}
	}
	if state := manager.auths[backup.ID].ModelStates["gpt-5.6-terra"]; state != nil && state.Health.Observed {
		t.Fatalf("backup route Terra health = %+v, want unaffected", state.Health)
	}
}

func TestGPTChannelBreaker_ThirtySecondWindowRequiresTenSamplesAtEightyPercent(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_750_000_000, 0)
	withinWindow := codexChannelBreakerState{}
	results := make([]Result, 0, 10)
	for i := 0; i < 2; i++ {
		results = append(results, Result{Success: true})
	}
	for i := 0; i < 8; i++ {
		results = append(results, gptChannelBreakerFailure(http.StatusTooManyRequests))
	}
	for i, result := range results {
		applyCodexChannelBreakerResult(&withinWindow, result, start.Add(time.Duration(i)*3*time.Second), "")
	}
	if withinWindow.Health.BreakerState != HealthBreakerOpen {
		t.Fatalf("30-second 8/10 breaker = %+v, want open", withinWindow.Health)
	}

	outsideWindow := codexChannelBreakerState{}
	for i, result := range results {
		applyCodexChannelBreakerResult(&outsideWindow, result, start.Add(time.Duration(i)*4*time.Second), "")
	}
	if outsideWindow.Health.BreakerState == HealthBreakerOpen {
		t.Fatalf("36-second 8/10 breaker = %+v, want closed because fewer than 10 samples remain in-window", outsideWindow.Health)
	}
}

func TestGPTChannelBreaker_TypedFailuresRespectScope(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_750_000_000, 0)
	ignoredState := codexChannelBreakerState{}
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		applyCodexChannelBreakerResult(&ignoredState, gptTypedChannelBreakerFailure(failurecontract.ScopeCredential, http.StatusTooManyRequests), at, "")
	}
	applyCodexChannelBreakerResult(&ignoredState, gptTypedChannelBreakerFailure(failurecontract.ScopeRequest, http.StatusTooManyRequests), start.Add(9*time.Second), "")
	if ignoredState.Health.Observed || ignoredState.recentCount != 0 {
		t.Fatalf("request/credential failures affected channel breaker: %+v count=%d", ignoredState.Health, ignoredState.recentCount)
	}

	modelState := codexChannelBreakerState{}
	applyCodexChannelBreakerResult(&modelState, Result{Success: true}, start, "")
	applyCodexChannelBreakerResult(&modelState, Result{Success: true}, start.Add(time.Second), "")
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i+2) * time.Second)
		applyCodexChannelBreakerResult(&modelState, gptTypedChannelBreakerFailure(failurecontract.ScopeModel, http.StatusTooManyRequests), at, "")
	}
	if modelState.Health.BreakerState != HealthBreakerOpen {
		t.Fatalf("typed model 429 window health = %+v, want open at 8/10 failures", modelState.Health)
	}

	providerState := codexChannelBreakerState{}
	for i := 0; i < codexChannelBreakerOpen5xxFailures; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		applyCodexChannelBreakerResult(&providerState, gptTypedChannelBreakerFailure(failurecontract.ScopeProvider, http.StatusServiceUnavailable), at, "")
	}
	if providerState.Health.BreakerState != HealthBreakerOpen {
		t.Fatalf("typed provider failures health = %+v, want existing provider breaker behavior", providerState.Health)
	}
}

func TestGPTChannelBreaker_SingleHalfOpenProbeAndEscalatingCooldown(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_750_000_000, 0)
	state := codexChannelBreakerState{}
	failure := gptChannelBreakerFailure(http.StatusServiceUnavailable)
	for i := 0; i < codexChannelBreakerOpen5xxFailures; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		applyCodexChannelBreakerResult(&state, failure, at, "")
	}
	assertGPTChannelCooldown(t, state, start.Add(time.Duration(codexChannelBreakerOpen5xxFailures-1)*time.Second), 30*time.Second, 1)

	firstProbeAt := state.Health.OpenUntil
	if !reserveCodexChannelProbe(&state, "request-a", firstProbeAt) {
		t.Fatal("first channel probe was rejected after cooldown")
	}
	if state.Health.BreakerState != HealthBreakerHalfOpen {
		t.Fatalf("breaker state after probe reservation = %q, want half-open", state.Health.BreakerState)
	}
	if reserveCodexChannelProbe(&state, "request-b", firstProbeAt) {
		t.Fatal("second concurrent probe for the same channel was allowed")
	}
	if state.ProbeRequestID != "request-a" {
		t.Fatalf("ProbeRequestID = %q, want request-a", state.ProbeRequestID)
	}

	secondOpenAt := firstProbeAt.Add(time.Second)
	applyCodexChannelBreakerResult(&state, failure, secondOpenAt, "request-a")
	assertGPTChannelCooldown(t, state, secondOpenAt, 60*time.Second, 2)
	if state.ProbeRequestID != "" {
		t.Fatalf("ProbeRequestID after failed probe = %q, want empty", state.ProbeRequestID)
	}

	secondProbeAt := state.Health.OpenUntil
	if !reserveCodexChannelProbe(&state, "request-c", secondProbeAt) {
		t.Fatal("second channel probe was rejected after cooldown")
	}
	thirdOpenAt := secondProbeAt.Add(time.Second)
	applyCodexChannelBreakerResult(&state, failure, thirdOpenAt, "request-c")
	assertGPTChannelCooldown(t, state, thirdOpenAt, 120*time.Second, 3)

	thirdProbeAt := state.Health.OpenUntil
	if !reserveCodexChannelProbe(&state, "request-d", thirdProbeAt) {
		t.Fatal("third channel probe was rejected after cooldown")
	}
	cappedOpenAt := thirdProbeAt.Add(time.Second)
	applyCodexChannelBreakerResult(&state, failure, cappedOpenAt, "request-d")
	assertGPTChannelCooldown(t, state, cappedOpenAt, 120*time.Second, 3)

	recoveryProbeAt := state.Health.OpenUntil
	if !reserveCodexChannelProbe(&state, "request-e", recoveryProbeAt) {
		t.Fatal("first recovery probe was rejected after cooldown")
	}
	applyCodexChannelBreakerResult(&state, Result{Success: true}, recoveryProbeAt.Add(time.Second), "request-e")
	if state.Health.BreakerState != HealthBreakerHalfOpen || state.Health.HalfOpenSuccesses != 1 {
		t.Fatalf("first recovery success health = %+v, want half-open with one success", state.Health)
	}
	if !reserveCodexChannelProbe(&state, "request-f", recoveryProbeAt.Add(2*time.Second)) {
		t.Fatal("second recovery probe was rejected")
	}
	applyCodexChannelBreakerResult(&state, Result{Success: true}, recoveryProbeAt.Add(3*time.Second), "request-f")
	if state.Health.BreakerState != HealthBreakerClosed {
		t.Fatalf("second recovery success health = %+v, want closed", state.Health)
	}
	if state.BackoffLevel != 0 || state.ProbeRequestID != "" {
		t.Fatalf("recovered channel state = backoff:%d probe:%q, want reset", state.BackoffLevel, state.ProbeRequestID)
	}
}

func gptChannelBreakerTestAuth(id, baseURL string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":  id + "-key",
			"base_url": baseURL,
			"priority": "10",
		},
	}
}

func gptChannelBreakerFailure(status int) Result {
	return Result{
		Success: false,
		Error: &Error{
			HTTPStatus: status,
			Code:       "upstream_unavailable",
			Message:    "upstream unavailable",
			Retryable:  true,
		},
	}
}

func gptTypedChannelBreakerFailure(scope failurecontract.Scope, status int) Result {
	kind := failurecontract.RateLimited
	if status != http.StatusTooManyRequests {
		kind = failurecontract.ProviderUnavailable
	}
	cause := &failurecontract.Failure{
		Kind:          kind,
		Scope:         scope,
		HTTPStatus:    status,
		Retryable:     true,
		PublicMessage: "upstream unavailable",
	}
	return Result{
		Success: false,
		Error:   resultErrorFromCause(cause),
		Cause:   cause,
	}
}

func assertGPTChannelCooldown(t *testing.T, state codexChannelBreakerState, openedAt time.Time, want time.Duration, wantLevel int) {
	t.Helper()
	if state.Health.BreakerState != HealthBreakerOpen {
		t.Fatalf("breaker state = %q, want open", state.Health.BreakerState)
	}
	if got := state.Health.OpenUntil.Sub(openedAt); got != want {
		t.Fatalf("cooldown = %v, want %v", got, want)
	}
	if got := int(state.BackoffLevel); got != wantLevel {
		t.Fatalf("BackoffLevel = %d, want %d", got, wantLevel)
	}
}
