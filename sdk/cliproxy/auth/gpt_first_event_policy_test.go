package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestGPTFirstEventObserverRequiresMinimumSamples(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for i := 0; i < gptFirstEventPolicyMinSamples-1; i++ {
		observer.observe("gpt-5.6-sol", policySample(now, i < 80, 20*time.Second))
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now)
	if snapshot.PolicyState != gptFirstEventPolicyStateNormal {
		t.Fatalf("policy state = %q, want normal", snapshot.PolicyState)
	}
	if snapshot.EnforcedTimeoutMs != (25 * time.Second).Milliseconds() {
		t.Fatalf("enforced timeout = %dms, want 25000ms", snapshot.EnforcedTimeoutMs)
	}
}

func TestGPTFirstEventObserverEntersSlow30AfterThreeWindows(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 80, 0, 0, 0, 20)
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy state = %q, want %q", snapshot.PolicyState, gptFirstEventPolicyStateSlow30)
	}
	if snapshot.EnforcedTimeoutMs != (30 * time.Second).Milliseconds() {
		t.Fatalf("enforced timeout = %dms, want 30000ms", snapshot.EnforcedTimeoutMs)
	}
	if snapshot.FirstEventSuccessRate25 != 0.8 {
		t.Fatalf("25s success rate = %v, want 0.8", snapshot.FirstEventSuccessRate25)
	}
	if snapshot.MaxChannels != 6 || snapshot.MaxRounds != 2 {
		t.Fatalf("retry limits = %d channels x %d rounds, want 6 x 2", snapshot.MaxChannels, snapshot.MaxRounds)
	}
}

func TestGPTFirstEventObserverEscalatesOnlyWithLateSuccessEvidence(t *testing.T) {
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	withLateSuccess := newGPTFirstEventObserver()
	withLateSuccess.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}
	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(withLateSuccess, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 39, 10, 0, 0, 51)
	}
	snapshot := withLateSuccess.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow40 {
		t.Fatalf("policy state with late success = %q, want %q", snapshot.PolicyState, gptFirstEventPolicyStateSlow40)
	}

	withoutLateSuccess := newGPTFirstEventObserver()
	withoutLateSuccess.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}
	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(withoutLateSuccess, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 49, 0, 0, 0, 51)
	}
	snapshot = withoutLateSuccess.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy state without late success = %q, want %q", snapshot.PolicyState, gptFirstEventPolicyStateSlow30)
	}
}

func TestGPTFirstEventObserverEscalatesFromSlow40ToSlow50(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow40,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 39, 0, 10, 0, 51)
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow50 {
		t.Fatalf("policy state = %q, want %q", snapshot.PolicyState, gptFirstEventPolicyStateSlow50)
	}
	if snapshot.EnforcedTimeoutMs != 50000 || snapshot.MaxChannels != 3 || snapshot.MaxRounds != 2 {
		t.Fatalf("slow50 limits = %dms %d channels x %d rounds", snapshot.EnforcedTimeoutMs, snapshot.MaxChannels, snapshot.MaxRounds)
	}
}

func TestGPTFirstEventObserverClassifiesCollectiveOutage(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 5, 0, 0, 0, 95)
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateOutage {
		t.Fatalf("policy state = %q, want outage", snapshot.PolicyState)
	}
	if snapshot.EnforcedTimeoutMs != (25 * time.Second).Milliseconds() {
		t.Fatalf("outage timeout = %dms, want 25000ms", snapshot.EnforcedTimeoutMs)
	}
	if snapshot.MaxChannels != 3 || snapshot.MaxRounds != 1 {
		t.Fatalf("outage retry limits = %d channels x %d rounds, want 3 x 1", snapshot.MaxChannels, snapshot.MaxRounds)
	}
	if snapshot.WaitBudgetMs != gptFirstEventPolicyOutageWaitBudget.Milliseconds() {
		t.Fatalf("outage wait budget = %dms, want %dms", snapshot.WaitBudgetMs, gptFirstEventPolicyOutageWaitBudget.Milliseconds())
	}
}

func TestGPTFirstEventObserverRecoversWithHysteresis(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}

	for window := 0; window < gptFirstEventPolicyRecoveryWindows-1; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 100, 0, 0, 0, 0)
	}
	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(8*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy recovered too early to %q", snapshot.PolicyState)
	}

	observePolicyWindow(observer, "gpt-5.6-sol", now.Add(9*time.Minute), 100, 0, 0, 0, 0)
	snapshot = observer.snapshot("gpt-5.6-sol", now.Add(9*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateNormal {
		t.Fatalf("policy state = %q after sustained recovery, want normal", snapshot.PolicyState)
	}
}

func TestGPTFirstEventObserverUsesGlobalFallback(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	models := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.5", "gpt-5.4"}

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		at := now.Add(time.Duration(window) * time.Minute)
		for i := 0; i < gptFirstEventPolicyMinSamples; i++ {
			observer.observe(models[i%len(models)], policySample(at, i < 80, 20*time.Second))
		}
	}

	snapshot := observer.snapshot("gpt-5.6-terra", now.Add(2*time.Minute))
	if !snapshot.UsedGlobalFallback || snapshot.DecisionSource != gptFirstEventPolicyGlobalModel {
		t.Fatalf("global fallback = %v source=%q, want true and %q", snapshot.UsedGlobalFallback, snapshot.DecisionSource, gptFirstEventPolicyGlobalModel)
	}
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy state = %q, want %q", snapshot.PolicyState, gptFirstEventPolicyStateSlow30)
	}
}

func TestGPTFirstEventObserverExpiresOldSamples(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for i := 0; i < gptFirstEventPolicyMinSamples; i++ {
		observer.observe("gpt-5.6-sol", policySample(now, false, 0))
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(gptFirstEventPolicyWindow+time.Second))
	if snapshot.EligibleFirstAttempts != 0 {
		t.Fatalf("eligible attempts = %d, want 0", snapshot.EligibleFirstAttempts)
	}
}

func TestGPTFirstEventRequestPolicyIsFixedAndBudgeted(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetGPTFirstEventTimeout(25 * time.Second)
	manager.gptFirstEventObserver.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{name: gptFirstEventPolicyStateSlow40}
	trace := &requestAttemptTrace{}
	policy := trace.configureGPTFirstEventPolicy(manager.selectGPTFirstEventPolicy("gpt-5.6-sol"))
	ctx := context.WithValue(context.Background(), requestAttemptTraceContextKey{}, trace)

	if policy.PolicyState != gptFirstEventPolicyStateSlow40 || policy.EnforcedTimeoutMs != 40000 {
		t.Fatalf("selected policy = %q %dms, want slow_40s 40000ms", policy.PolicyState, policy.EnforcedTimeoutMs)
	}
	manager.gptFirstEventObserver.policies["gpt-5.6-sol"].name = gptFirstEventPolicyStateOutage
	if got := manager.firstEventTimeoutForRoute(ctx, []string{"codex"}, "gpt-5.6-sol"); got != 40*time.Second {
		t.Fatalf("fixed request timeout = %v, want 40s", got)
	}
	trace.recordGPTFirstEventAttempt(285*time.Second, true)
	if got := manager.firstEventTimeoutForRoute(ctx, []string{"codex"}, "gpt-5.6-sol"); got != 15*time.Second {
		t.Fatalf("remaining request timeout = %v, want 15s", got)
	}
}

func TestGPTFirstEventPolicyLimitsRetryRounds(t *testing.T) {
	errTimeout := &Error{Code: "gpt_first_event_timeout", HTTPStatus: http.StatusGatewayTimeout, Retryable: true}
	trace := &requestAttemptTrace{
		gptFirstEventPolicySet: true,
		gptFirstEventPolicy: GPTFirstEventPolicySnapshot{
			PolicyState: gptFirstEventPolicyStateOutage,
			MaxChannels: 3,
			MaxRounds:   1,
		},
	}

	if _, retry := shouldRetryGPTRound(errTimeout, 0, []string{"codex"}, "gpt-5.6-sol", trace); retry {
		t.Fatal("outage policy must stop after the first round")
	}
}

func TestClassifyGPTFirstEventObservation(t *testing.T) {
	tests := []struct {
		name        string
		deliverable bool
		timedOut    bool
		err         error
		eligible    bool
		outcome     string
		upstream5xx bool
		network     bool
	}{
		{name: "deliverable", deliverable: true, eligible: true, outcome: gptFirstEventOutcomeDeliverable},
		{name: "timeout", timedOut: true, eligible: true, outcome: gptFirstEventOutcomeTimeout},
		{name: "upstream 503", err: &Error{HTTPStatus: http.StatusServiceUnavailable}, eligible: true, outcome: gptFirstEventOutcomeFailure, upstream5xx: true},
		{name: "request invalid", err: &Error{HTTPStatus: http.StatusBadRequest}, eligible: false},
		{name: "unclassified", err: errors.New("permanent failure"), eligible: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eligible, outcome, upstream5xx, network := classifyGPTFirstEventObservation(tt.deliverable, tt.timedOut, tt.err)
			if eligible != tt.eligible || outcome != tt.outcome || upstream5xx != tt.upstream5xx || network != tt.network {
				t.Fatalf("classification = (%v, %q, %v, %v), want (%v, %q, %v, %v)", eligible, outcome, upstream5xx, network, tt.eligible, tt.outcome, tt.upstream5xx, tt.network)
			}
		})
	}
}

func observePolicyWindow(observer *gptFirstEventObserver, model string, at time.Time, within25, within30, within40, within50, failures int) {
	for i := 0; i < within25; i++ {
		observer.observe(model, policySample(at, true, 20*time.Second))
	}
	for i := 0; i < within30; i++ {
		observer.observe(model, policySample(at, true, 27*time.Second))
	}
	for i := 0; i < within40; i++ {
		observer.observe(model, policySample(at, true, 35*time.Second))
	}
	for i := 0; i < within50; i++ {
		observer.observe(model, policySample(at, true, 45*time.Second))
	}
	for i := 0; i < failures; i++ {
		observer.observe(model, policySample(at, false, 0))
	}
}

func policySample(at time.Time, deliverable bool, delay time.Duration) gptFirstEventSample {
	return gptFirstEventSample{
		at:          at,
		deliverable: deliverable,
		delay:       delay,
		timedOut:    !deliverable,
	}
}
