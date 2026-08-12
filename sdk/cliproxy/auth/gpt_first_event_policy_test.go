package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	logtest "github.com/sirupsen/logrus/hooks/test"
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

func TestGPTFirstEventObserverLowSampleTimeoutProtection(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 0, 0, 0, 0, gptFirstEventPolicyLowSampleMinSamples)
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.EligibleFirstAttempts < gptFirstEventPolicyLowSampleMinSamples || snapshot.EligibleFirstAttempts >= gptFirstEventPolicyEscalationMinSamples {
		t.Fatalf("eligible attempts = %d, want low-sample range [%d,%d)", snapshot.EligibleFirstAttempts, gptFirstEventPolicyLowSampleMinSamples, gptFirstEventPolicyEscalationMinSamples)
	}
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 || snapshot.EnforcedTimeoutMs != 30000 {
		t.Fatalf("policy = %q %dms, want slow_30s/30000ms", snapshot.PolicyState, snapshot.EnforcedTimeoutMs)
	}
	if snapshot.DecisionReason != "low_sample_timeout_protection" {
		t.Fatalf("decision reason = %q, want low_sample_timeout_protection", snapshot.DecisionReason)
	}
}

func TestGPTFirstEventObserverOccasionalLowSampleTimeoutDoesNotEscalate(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	observePolicyWindow(observer, "gpt-5.6-sol", now, 2, 0, 0, 0, 8)
	observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Minute), 10, 0, 0, 0, 0)
	observePolicyWindow(observer, "gpt-5.6-sol", now.Add(2*time.Minute), 2, 0, 0, 0, 8)

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateNormal {
		t.Fatalf("policy state = %q, want normal after non-consecutive timeout pressure", snapshot.PolicyState)
	}
}

func TestGPTFirstEventObserverLowSamplesDoNotEscalateLearnedSlowState(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 0, 0, 0, 0, gptFirstEventPolicyLowSampleMinSamples)
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy state = %q, want retained slow_30s", snapshot.PolicyState)
	}
}

func TestGPTFirstEventObserverEntersSlow30AfterThreeWindows(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 32, 0, 0, 0, 8)
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
		observePolicyWindow(withLateSuccess, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 15, 4, 0, 0, 21)
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
		observeHardFailurePolicyWindow(withoutLateSuccess, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 19, 21)
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
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 15, 0, 4, 0, 21)
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
		observeHardFailurePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 2, 38)
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

func TestGPTFirstEventObserverHardFailuresDoNotMasqueradeAsLatency(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observeHardFailurePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 20, 20)
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateNormal {
		t.Fatalf("policy state = %q, want normal because more waiting cannot fix hard failures", snapshot.PolicyState)
	}
}

func TestGPTFirstEventObserverOutageMovesToSlowWhenHardFailuresClear(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateOutage,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}

	for window := 0; window <= gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 0, 0, 0, 0, gptFirstEventPolicyRecoveryMinSamples)
	}
	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(time.Duration(gptFirstEventPolicyEnterWindows)*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy state = %q, want slow_30s after hard failures cleared into local timeouts", snapshot.PolicyState)
	}
	if snapshot.DecisionReason != "hard_outage_cleared_to_slow" {
		t.Fatalf("decision reason = %q, want hard_outage_cleared_to_slow", snapshot.DecisionReason)
	}
}

func TestGPTFirstEventObserverLocalTimeoutBurstEscalatesWithoutOutage(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for window := 0; window <= 12; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 0, 0, 0, 0, gptFirstEventPolicyEscalationMinSamples)
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(12*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow50 {
		t.Fatalf("policy state = %q, want %q", snapshot.PolicyState, gptFirstEventPolicyStateSlow50)
	}
	if snapshot.HardFailureRate != 0 || snapshot.FailureRate != 1 {
		t.Fatalf("failure rates = total:%v hard:%v, want 1/0", snapshot.FailureRate, snapshot.HardFailureRate)
	}
	if snapshot.DecisionReason != "local_timeout_pressure" {
		t.Fatalf("decision reason = %q, want local_timeout_pressure", snapshot.DecisionReason)
	}
}

func TestGPTFirstEventObserverRelaxesStaleOutageWithoutTraffic(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateOutage,
		lastTransitionAt: now,
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(6*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 || snapshot.MaxRounds != 2 {
		t.Fatalf("aged outage policy = %q with %d rounds, want slow_30s with two rounds", snapshot.PolicyState, snapshot.MaxRounds)
	}
	if !snapshot.Transitioned || snapshot.DecisionReason != "outage_evidence_expired_to_slow" {
		t.Fatalf("aged outage transition = %v reason=%q", snapshot.Transitioned, snapshot.DecisionReason)
	}
}

func TestGPTFirstEventObserverRelaxesStaleOutageWithLowTraffic(t *testing.T) {
	observer := newGPTFirstEventObserver()
	model := "gpt-5.6-sol"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	observer.policies[model] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateOutage,
		lastTransitionAt: now,
	}
	for i := 0; i < gptFirstEventPolicyEscalationMinSamples-1; i++ {
		observer.sequence++
		observer.samples[model] = append(observer.samples[model], gptFirstEventSample{
			sequence:    observer.sequence,
			at:          now.Add(5*time.Minute + 30*time.Second),
			upstream5xx: true,
		})
	}

	snapshot := observer.snapshot(model, now.Add(6*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 || snapshot.MaxRounds != 2 {
		t.Fatalf("low-traffic outage policy = %q with %d rounds, want slow_30s with two rounds", snapshot.PolicyState, snapshot.MaxRounds)
	}
}

func TestGPTFirstEventObserverKeepsOutageWithSustainedHardFailureEvidence(t *testing.T) {
	observer := newGPTFirstEventObserver()
	model := "gpt-5.6-sol"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	observer.policies[model] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateOutage,
		lastTransitionAt: now,
	}
	for i := 0; i < gptFirstEventPolicyEscalationMinSamples; i++ {
		observer.sequence++
		observer.samples[model] = append(observer.samples[model], gptFirstEventSample{
			sequence:    observer.sequence,
			at:          now.Add(5*time.Minute + 30*time.Second),
			upstream5xx: true,
		})
	}

	snapshot := observer.snapshot(model, now.Add(6*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateOutage || snapshot.MaxRounds != 1 || snapshot.Transitioned {
		t.Fatalf("sustained hard-failure policy = %q rounds=%d transitioned=%v, want outage/1/false", snapshot.PolicyState, snapshot.MaxRounds, snapshot.Transitioned)
	}
}

func TestManagerNextRequestRelaxesStaleOutage(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	manager := NewManager(nil, nil, nil)
	manager.gptFirstEventObserver.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateOutage,
		lastTransitionAt: time.Now().Add(-6 * time.Minute),
	}

	snapshot := manager.selectGPTFirstEventPolicy("gpt-5.6-sol")
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 || snapshot.MaxRounds != 2 || snapshot.EnforcedTimeoutMs != 30000 {
		t.Fatalf("next-request policy = %q %dms %d rounds, want slow_30s/30000ms/2", snapshot.PolicyState, snapshot.EnforcedTimeoutMs, snapshot.MaxRounds)
	}
	foundTransition := false
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "gpt_first_event_policy_transition" && entry.Data["decision_reason"] == "outage_evidence_expired_to_slow" {
			foundTransition = true
			break
		}
	}
	if !foundTransition {
		t.Fatal("stale outage relaxation did not emit a policy transition log")
	}
}

func TestGPTFirstEventObserverRecoversWithHysteresis(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}

	for window := 0; window < gptFirstEventPolicyRecoveryWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 100, 0, 0, 0, 0)
	}
	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(9*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy recovered too early to %q", snapshot.PolicyState)
	}

	observePolicyWindow(observer, "gpt-5.6-sol", now.Add(10*time.Minute), 100, 0, 0, 0, 0)
	snapshot = observer.snapshot("gpt-5.6-sol", now.Add(10*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateNormal {
		t.Fatalf("policy state = %q after sustained recovery, want normal", snapshot.PolicyState)
	}
}

func TestGPTFirstEventObserverKeepsModelsIsolated(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 32, 0, 0, 0, 8)
	}

	sol := observer.snapshot("gpt-5.6-sol", now.Add(2*time.Minute))
	if sol.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("Sol policy state = %q, want %q", sol.PolicyState, gptFirstEventPolicyStateSlow30)
	}
	terra := observer.snapshot("gpt-5.6-terra", now.Add(2*time.Minute))
	if terra.UsedGlobalFallback || terra.DecisionSource != "gpt-5.6-terra" {
		t.Fatalf("Terra fallback = %v source=%q, want false and exact model", terra.UsedGlobalFallback, terra.DecisionSource)
	}
	if terra.PolicyState != gptFirstEventPolicyStateNormal || terra.EnforcedTimeoutMs != 25000 {
		t.Fatalf("Terra policy = %q %dms, want independent normal/25000ms", terra.PolicyState, terra.EnforcedTimeoutMs)
	}
}

func TestGPTFirstEventObserverRetainsLearnedStateWithInsufficientSamples(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	for window := 0; window < gptFirstEventPolicyEnterWindows; window++ {
		observePolicyWindow(observer, "gpt-5.6-sol", now.Add(time.Duration(window)*time.Minute), 32, 0, 0, 0, 8)
	}

	later := now.Add(gptFirstEventPolicyWindow + 3*time.Minute)
	for i := 0; i < gptFirstEventPolicyEscalationMinSamples-1; i++ {
		observer.observe("gpt-5.6-sol", policySample(later, true, 10*time.Second))
	}
	snapshot := observer.snapshot("gpt-5.6-sol", later)
	if snapshot.EligibleFirstAttempts != gptFirstEventPolicyEscalationMinSamples-1 {
		t.Fatalf("eligible attempts = %d, want %d", snapshot.EligibleFirstAttempts, gptFirstEventPolicyEscalationMinSamples-1)
	}
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 || snapshot.EnforcedTimeoutMs != 30000 {
		t.Fatalf("retained policy = %q %dms, want slow_30s/30000ms", snapshot.PolicyState, snapshot.EnforcedTimeoutMs)
	}
	if snapshot.DecisionReason != "insufficient_samples" {
		t.Fatalf("decision reason = %q, want insufficient_samples", snapshot.DecisionReason)
	}
}

func TestGPTFirstEventObserverCheckpointsSupportedSlowStateHourly(t *testing.T) {
	observer := newGPTFirstEventObserver()
	model := "gpt-5.6-sol"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	observer.policies[model] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now,
		updatedAt:        now,
	}

	var snapshot GPTFirstEventPolicySnapshot
	for i := 0; i < gptFirstEventPolicyLowSampleMinSamples; i++ {
		snapshot = observer.observe(model, policySample(now.Add(59*time.Minute), false, 0))
	}
	if snapshot.stateCheckpointed {
		t.Fatal("slow state checkpointed before the one-hour interval")
	}

	snapshot = observer.observe(model, policySample(now.Add(time.Hour), false, 0))
	if !snapshot.stateCheckpointed {
		t.Fatal("supported slow state did not checkpoint after one hour")
	}
	if got := observer.policies[model].updatedAt; !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("checkpoint updated_at = %v, want %v", got, now.Add(time.Hour))
	}

	snapshot = observer.observe(model, policySample(now.Add(time.Hour+time.Minute), false, 0))
	if snapshot.stateCheckpointed {
		t.Fatal("slow state checkpointed more than once within an hour")
	}
}

func TestGPTFirstEventObserverDoesNotCheckpointSlowStateOnFastSuccesses(t *testing.T) {
	observer := newGPTFirstEventObserver()
	model := "gpt-5.6-sol"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	observer.policies[model] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now,
		updatedAt:        now,
	}

	var snapshot GPTFirstEventPolicySnapshot
	for i := 0; i < gptFirstEventPolicyLowSampleMinSamples; i++ {
		snapshot = observer.observe(model, policySample(now.Add(2*time.Hour), true, 20*time.Second))
	}
	if snapshot.stateCheckpointed {
		t.Fatal("fast successes must not extend a learned slow state checkpoint")
	}
	if got := observer.policies[model].updatedAt; !got.Equal(now) {
		t.Fatalf("updated_at = %v, want unchanged %v", got, now)
	}
}

func TestGPTFirstEventObserverExportsAndRestoresExactModelStateWithoutSamples(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	observer := newGPTFirstEventObserver()
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow40,
		previousState:    gptFirstEventPolicyStateSlow30,
		decisionReason:   "local_timeout_pressure",
		lastTransitionAt: now.Add(-time.Minute),
		updatedAt:        now,
		candidateTarget:  gptFirstEventPolicyStateSlow50,
		candidateReason:  "local_timeout_pressure",
		enterWindows:     2,
	}
	observer.policies[gptFirstEventPolicyGlobalModel] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateOutage,
		lastTransitionAt: now,
		updatedAt:        now,
	}
	observer.samples["gpt-5.6-sol"] = []gptFirstEventSample{policySample(now, true, 20*time.Second)}

	records := observer.exportPolicyStates(now)
	if len(records) != 1 {
		t.Fatalf("exported records = %d, want one exact model and no global state", len(records))
	}
	if records[0].Model != "gpt-5.6-sol" || records[0].PolicyState != gptFirstEventPolicyStateSlow40 || records[0].DecisionReason != "local_timeout_pressure" {
		t.Fatalf("exported policy = %+v", records[0])
	}

	restored := newGPTFirstEventObserver()
	restored.now = func() time.Time { return now }
	restored.restorePolicyStates(records)
	snapshot := restored.snapshot("gpt-5.6-sol", now)
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow40 || snapshot.PreviousState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("restored policy = %+v", snapshot)
	}
	if snapshot.EligibleFirstAttempts != 0 {
		t.Fatalf("restored samples = %d, want none", snapshot.EligibleFirstAttempts)
	}
	restoredPolicy := restored.policies["gpt-5.6-sol"]
	if restoredPolicy == nil || restoredPolicy.candidateTarget != "" || restoredPolicy.enterWindows != 0 || restoredPolicy.recoveryWindows != 0 {
		t.Fatalf("restored transient candidate state = %+v, want empty", restoredPolicy)
	}
}

func TestGPTFirstEventObserverRestorePolicyStateAging(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		record       GPTFirstEventPolicyStateRecord
		wantState    string
		wantPrevious string
		wantReason   string
	}{
		{
			name: "recent slow state",
			record: GPTFirstEventPolicyStateRecord{
				Model:            "gpt-5.6-sol",
				PolicyState:      gptFirstEventPolicyStateSlow40,
				PreviousState:    gptFirstEventPolicyStateSlow30,
				DecisionReason:   "local_timeout_pressure",
				LastTransitionAt: now.Add(-time.Hour),
				UpdatedAt:        now.Add(-time.Minute),
			},
			wantState:    gptFirstEventPolicyStateSlow40,
			wantPrevious: gptFirstEventPolicyStateSlow30,
			wantReason:   "local_timeout_pressure",
		},
		{
			name: "recent outage is downgraded",
			record: GPTFirstEventPolicyStateRecord{
				Model:            "gpt-5.6-sol",
				PolicyState:      gptFirstEventPolicyStateOutage,
				PreviousState:    gptFirstEventPolicyStateSlow50,
				DecisionReason:   "collective_outage",
				LastTransitionAt: now.Add(-time.Minute),
				UpdatedAt:        now.Add(-time.Minute),
			},
			wantState:    gptFirstEventPolicyStateSlow30,
			wantPrevious: gptFirstEventPolicyStateOutage,
			wantReason:   "restored_outage_as_slow_30s",
		},
		{
			name: "old slow state expires",
			record: GPTFirstEventPolicyStateRecord{
				Model:       "gpt-5.6-sol",
				PolicyState: gptFirstEventPolicyStateSlow50,
				UpdatedAt:   now.Add(-gptFirstEventPolicyPersistenceTTL - time.Second),
			},
			wantState:  gptFirstEventPolicyStateNormal,
			wantReason: "insufficient_samples",
		},
		{
			name: "old outage expires",
			record: GPTFirstEventPolicyStateRecord{
				Model:       "gpt-5.6-sol",
				PolicyState: gptFirstEventPolicyStateOutage,
				UpdatedAt:   now.Add(-gptFirstEventPolicyPersistenceTTL - time.Second),
			},
			wantState:  gptFirstEventPolicyStateNormal,
			wantReason: "insufficient_samples",
		},
		{
			name: "zero updated at is ignored",
			record: GPTFirstEventPolicyStateRecord{
				Model:       "gpt-5.6-sol",
				PolicyState: gptFirstEventPolicyStateSlow30,
			},
			wantState:  gptFirstEventPolicyStateNormal,
			wantReason: "insufficient_samples",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observer := newGPTFirstEventObserver()
			observer.now = func() time.Time { return now }
			observer.restorePolicyStates([]GPTFirstEventPolicyStateRecord{tt.record})
			snapshot := observer.snapshot("gpt-5.6-sol", now)
			if snapshot.PolicyState != tt.wantState || snapshot.PreviousState != tt.wantPrevious || snapshot.DecisionReason != tt.wantReason {
				t.Fatalf("restored policy = state:%q previous:%q reason:%q, want %q/%q/%q", snapshot.PolicyState, snapshot.PreviousState, snapshot.DecisionReason, tt.wantState, tt.wantPrevious, tt.wantReason)
			}
		})
	}
}

func TestGPTFirstEventObserverDoesNotCombineSlowAndOutageCandidates(t *testing.T) {
	observer := newGPTFirstEventObserver()
	model := "gpt-5.6-sol"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	evaluatePolicySamples(observer, model, now, policyEvaluationSamples(now, 32, 8, 0))
	evaluatePolicySamples(observer, model, now.Add(time.Minute), policyEvaluationSamples(now.Add(time.Minute), 32, 8, 0))
	evaluatePolicySamples(observer, model, now.Add(2*time.Minute), policyEvaluationSamples(now.Add(2*time.Minute), 2, 0, 38))

	snapshot := observer.snapshot(model, now.Add(2*time.Minute))
	if snapshot.PolicyState != gptFirstEventPolicyStateNormal {
		t.Fatalf("policy state = %q, want normal after candidate target changed", snapshot.PolicyState)
	}
	policy := observer.policies[model]
	if policy == nil || policy.candidateTarget != gptFirstEventPolicyStateOutage || policy.enterWindows != 1 {
		t.Fatalf("outage candidate state = %+v, want a fresh one-window candidate", policy)
	}
}

func TestGPTFirstEventObserverDoesNotCombineAlternatingOutageRecoveryTargets(t *testing.T) {
	observer := newGPTFirstEventObserver()
	model := "gpt-5.6-sol"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	observer.policies[model] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateOutage,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}

	evaluatePolicySamples(observer, model, now, policyEvaluationSamples(now, 0, 100, 0))
	evaluatePolicySamples(observer, model, now.Add(time.Minute), policyEvaluationSamples(now.Add(time.Minute), 100, 0, 0))
	evaluatePolicySamples(observer, model, now.Add(2*time.Minute), policyEvaluationSamples(now.Add(2*time.Minute), 0, 100, 0))

	observer.mu.Lock()
	snapshot := observer.snapshotLocked(model, now.Add(2*time.Minute), 0)
	policy := observer.policies[model]
	observer.mu.Unlock()
	if snapshot.PolicyState != gptFirstEventPolicyStateOutage {
		t.Fatalf("policy state = %q, want outage until one recovery target is sustained", snapshot.PolicyState)
	}
	if policy == nil || policy.candidateTarget != gptFirstEventPolicyStateSlow30 || policy.recoveryWindows != 1 {
		t.Fatalf("recovery candidate state = %+v, want a fresh slow_30s candidate", policy)
	}
}

func TestGPTFirstEventObserverRequiresMoreSamplesToRecover(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	observer.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:             gptFirstEventPolicyStateSlow30,
		lastTransitionAt: now.Add(-10 * time.Minute),
	}

	for i := 0; i < gptFirstEventPolicyRecoveryMinSamples-1; i++ {
		observer.observe("gpt-5.6-sol", policySample(now, true, 10*time.Second))
	}
	snapshot := observer.snapshot("gpt-5.6-sol", now)
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("policy recovered with %d samples: %q", snapshot.EligibleFirstAttempts, snapshot.PolicyState)
	}
	if snapshot.DecisionReason != "insufficient_recovery_samples" {
		t.Fatalf("decision reason = %q, want insufficient_recovery_samples", snapshot.DecisionReason)
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

func TestGPTFirstEventObserverBuildsDailySnapshots(t *testing.T) {
	observer := newGPTFirstEventObserver()
	dayOne := time.Date(2026, 8, 5, 0, 0, 0, 0, time.Local)
	dayTwo := dayOne.AddDate(0, 0, 1)

	observePolicyWindow(observer, "gpt-5.6-sol", dayOne, 80, 0, 0, 0, 20)
	observePolicyWindow(observer, "gpt-5.6-sol", dayTwo, 90, 5, 0, 0, 5)
	daily := observer.dailySnapshot("gpt-5.6-sol", 2, dayTwo)
	if len(daily) != 2 {
		t.Fatalf("daily snapshots = %d, want 2", len(daily))
	}
	if daily[0].Date != "2026-08-05" || daily[0].FirstEventSuccessRate25 != 0.8 || daily[0].Timeouts != 20 {
		t.Fatalf("unexpected first day: %+v", daily[0])
	}
	if daily[1].Date != "2026-08-06" || daily[1].FirstEventSuccessRate25 != 0.9 || daily[1].FirstEventSuccessRate30 != 0.95 {
		t.Fatalf("unexpected second day: %+v", daily[1])
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
		{name: "upstream 503", err: &Error{Code: "upstream_unavailable", HTTPStatus: http.StatusServiceUnavailable}, eligible: true, outcome: gptFirstEventOutcomeFailure, upstream5xx: true},
		{name: "canonical provider 503", err: &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider, HTTPStatus: http.StatusServiceUnavailable, Retryable: true}, eligible: true, outcome: gptFirstEventOutcomeFailure, upstream5xx: true},
		{name: "legacy provider 500", err: &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal execution error"}, eligible: true, outcome: gptFirstEventOutcomeFailure, upstream5xx: true},
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

func observeHardFailurePolicyWindow(observer *gptFirstEventObserver, model string, at time.Time, within25, failures int) {
	for i := 0; i < within25; i++ {
		observer.observe(model, policySample(at, true, 20*time.Second))
	}
	for i := 0; i < failures; i++ {
		observer.observe(model, gptFirstEventSample{at: at, upstream5xx: true})
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

func evaluatePolicySamples(observer *gptFirstEventObserver, model string, at time.Time, samples []gptFirstEventSample) {
	observer.mu.Lock()
	for i := range samples {
		observer.sequence++
		samples[i].sequence = observer.sequence
	}
	observer.samples[model] = append([]gptFirstEventSample(nil), samples...)
	observer.evaluateLocked(model, at)
	observer.mu.Unlock()
}

func policyEvaluationSamples(at time.Time, within25, timeouts, upstreamFailures int) []gptFirstEventSample {
	samples := make([]gptFirstEventSample, 0, within25+timeouts+upstreamFailures)
	for i := 0; i < within25; i++ {
		samples = append(samples, gptFirstEventSample{at: at, deliverable: true, delay: 20 * time.Second})
	}
	for i := 0; i < timeouts; i++ {
		samples = append(samples, gptFirstEventSample{at: at, timedOut: true})
	}
	for i := 0; i < upstreamFailures; i++ {
		samples = append(samples, gptFirstEventSample{at: at, upstream5xx: true})
	}
	return samples
}
