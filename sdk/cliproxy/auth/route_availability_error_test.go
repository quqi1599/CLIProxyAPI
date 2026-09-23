package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRouteAvailabilityErrorDistinguishesRateLimitFromOtherBlockers(t *testing.T) {
	t.Parallel()
	now := time.Now()
	type fixture struct {
		kind   string
		status int
	}
	for _, provider := range []string{"codex", "claude"} {
		model := "gpt-5.6-sol"
		if provider == "claude" {
			model = "claude-sonnet-4-6"
		}
		for _, spread := range []bool{false, true} {
			for _, tc := range []struct {
				name   string
				items  []fixture
				status int
			}{
				{"auth401", []fixture{{"auth", 401}}, 503},
				{"auth403", []fixture{{"auth", 403}}, 503},
				{"model401", []fixture{{"model", 401}}, 503},
				{"health401", []fixture{{"health", 401}}, 503},
				{"health403", []fixture{{"health", 403}}, 503},
				{"health502", []fixture{{"health", 502}}, 503},
				{"unknown_health", []fixture{{"health", 0}}, 503},
				{"quota429", []fixture{{"auth", 429}, {"model", 429}}, 429},
				{"health429", []fixture{{"health", 429}}, 429},
				{"mixed_auth_and_quota", []fixture{{"auth", 401}, {"model", 429}}, 503},
				{"mixed_transport_and_quota", []fixture{{"health", 502}, {"model", 429}}, 503},
				{"mixed_disabled_and_quota", []fixture{{"disabled", 0}, {"model", 429}}, 503},
			} {
				t.Run(fmt.Sprintf("%s/spread=%t/%s", provider, spread, tc.name), func(t *testing.T) {
					var selector Selector = &RoundRobinSelector{}
					if spread {
						selector = &SpreadSelector{channelAware: true}
					}
					manager := NewManager(nil, selector, nil)
					auths := make([]*Auth, 0, len(tc.items))
					for i, item := range tc.items {
						auth := &Auth{ID: fmt.Sprintf("route-%d", i), Provider: provider, Status: StatusActive,
							Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.invalid"}}
						switch item.kind {
						case "auth":
							auth.Unavailable = true
							auth.NextRetryAfter = now.Add(2 * time.Minute)
							auth.Quota.Exceeded = item.status == 429
							auth.LastError = &Error{HTTPStatus: item.status}
						case "model":
							auth.ModelStates = map[string]*ModelState{model: {
								Unavailable: true, NextRetryAfter: now.Add(2 * time.Minute),
								Quota: QuotaState{Exceeded: item.status == 429}, LastError: &Error{HTTPStatus: item.status},
							}}
						case "health":
							auth.Health = HealthState{Observed: true, BreakerState: HealthBreakerOpen,
								OpenUntil: now.Add(2 * time.Minute), LastStatusCode: item.status}
						case "disabled":
							auth.Disabled = true
						}
						// Exhaust the probe lease without making a probe active. This
						// observes the terminal selection error rather than a new probe.
						manager.halfOpenProbeNext[halfOpenProbeKey(auth.ID, model)] = now.Add(2 * time.Minute)
						auths = append(auths, auth)
					}
					ctx, trace := ensureRequestAttemptTrace(context.Background())
					available, err := manager.availableAuthsForRouteModelContext(ctx, auths, provider, model, now)
					if err == nil || len(available) != 0 {
						t.Fatalf("available=%v err=%v, want no candidates and error", available, err)
					}
					if got := statusCodeFromError(err); got != tc.status {
						t.Fatalf("status=%d, want %d; err=%v", got, tc.status, err)
					}
					if trace.attemptCount() != 0 {
						t.Fatal("selection-only failure recorded an upstream attempt")
					}
					if tc.status == 503 {
						if got := errorCodeFromError(err); got != "auth_unavailable" {
							t.Fatalf("code=%q, want auth_unavailable", got)
						}
						var cooldown *modelCooldownError
						if errors.As(err, &cooldown) {
							t.Fatal("non-rate unavailable error must not unwrap to model_cooldown")
						}
					}
					if tc.name != "mixed_disabled_and_quota" {
						if wait := retryAfterFromError(err); wait == nil || *wait != 2*time.Minute {
							t.Fatalf("retryAfter=%v, want 2m", wait)
						}
					}
				})
			}
		}
	}
}

func TestRouteTemporalRateClassificationRejectsContradictoryState(t *testing.T) {
	t.Parallel()
	now := time.Now()
	const model = "gpt-5.6-sol"
	for _, tc := range []struct {
		name string
		auth *Auth
	}{
		{"stale_auth_quota_after401", &Auth{Unavailable: true, NextRetryAfter: now.Add(time.Minute),
			Quota: QuotaState{Exceeded: true}, LastError: &Error{HTTPStatus: 401}}},
		{"stale_model_quota_after403", &Auth{ModelStates: map[string]*ModelState{model: {
			Unavailable: true, NextRetryAfter: now.Add(time.Minute), Quota: QuotaState{Exceeded: true}, LastError: &Error{HTTPStatus: 403},
		}}}},
		{"rate_breaker_with_auth401", &Auth{Unavailable: true, NextRetryAfter: now.Add(time.Minute), LastError: &Error{HTTPStatus: 401}}},
		{"legacy_unauthorized_with_stale_quota", &Auth{Unavailable: true, NextRetryAfter: now.Add(time.Minute),
			Quota: QuotaState{Exceeded: true}, LastError: &Error{Code: "unauthorized"}}},
		{"typed_auth_failure_with_stale_quota", &Auth{Unavailable: true, NextRetryAfter: now.Add(time.Minute),
			Quota: QuotaState{Exceeded: true}, LastError: &Error{Kind: string(failurecontract.AuthenticationFailed)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.auth.Health = HealthState{Observed: true, LastStatusCode: 429, BreakerState: HealthBreakerOpen}
			if routeTemporalBlockIsRateLimited(tc.auth, model, now, true) {
				t.Fatal("authentication block must not be masked by quota/rate state")
			}
		})
	}
}

func TestRouteUnavailableErrorPreservesTerminalContractAndRetryBoundary(t *testing.T) {
	t.Parallel()
	wait := 1500 * time.Millisecond
	err := newRouteUnavailableError(&wait)
	terminal := normalizeTerminalManagerError(context.Background(), normalizeExhaustedGPTChannelError(err, []string{"codex"}, "gpt-5.6-sol"))
	if terminal != err || errorCodeFromError(terminal) != "auth_unavailable" || statusCodeFromError(terminal) != 503 {
		t.Fatalf("terminal=%T %v, want original local auth_unavailable 503", terminal, terminal)
	}
	if !isGPTZeroEligibleSelectionError(terminal) {
		t.Fatal("selection error no longer supports the zero-eligible recovery lease")
	}
	if got := err.Headers().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After=%q, want 2", got)
	}
	if _, retry := preserveCanonicalGPTRoundRetryAfter(terminal, 0, true, time.Second); retry {
		t.Fatal("must not immediately retry a selection failure beyond the wait budget")
	}
	if got, retry := preserveCanonicalGPTRoundRetryAfter(terminal, 0, true, 2*time.Second); !retry || got != wait {
		t.Fatalf("wait=%v retry=%t, want preserved 1.5s retry", got, retry)
	}
	// This marker belongs only to local selection; a real upstream 503 must
	// continue through the existing exhausted-channel normalization.
	upstream := &failurecontract.Failure{HTTPStatus: 503, OuterStatus: 503, PublicMessage: "auth_unavailable: provider unavailable"}
	if normalized := normalizeExhaustedGPTChannelError(upstream, []string{"codex"}, "gpt-5.6-sol"); errorCodeFromError(normalized) != gptChannelsUnavailableErrorCode {
		t.Fatalf("upstream normalization=%v, want gpt_channels_unavailable", normalized)
	}
}

func TestRouteUnavailableErrorNoAttemptRunnerStopsBeforeRecoveryTime(t *testing.T) {
	t.Parallel()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.configureGPTRoute(true)
	wait := 5 * time.Minute
	runs := 0
	runner := managerAttemptRunner[int]{manager: manager, runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (int, error) {
		runs++
		return 0, newRouteUnavailableError(&wait)
	}}
	outcome := runner.run(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{}, 5, time.Second)
	if runs != 1 || errorCodeFromError(outcome.returnErr) != "auth_unavailable" || trace.attemptCount() != 0 {
		t.Fatalf("runs=%d err=%v attempts=%d, want one selection-only run", runs, outcome.returnErr, trace.attemptCount())
	}
}
