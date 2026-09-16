package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestGPTHealthyRoutePreferenceRecoversAndNeverEmptiesPool(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "slow", Provider: "codex", Attributes: map[string]string{"base_url": "https://slow.example"}}
	b := &Auth{ID: "healthy", Provider: "codex", Attributes: map[string]string{"base_url": "https://healthy.example"}}
	all := []*Auth{a, b}
	now := time.Now()
	for i := 0; i < 4; i++ {
		m.gptRetryPressure.observe("gpt-5.6-sol", routingChannelBaseKey(a), true, now)
	}
	got, deferred := m.preferHealthyGPTRoutes(all, "gpt-5.6-sol", now)
	if len(got) != 1 || got[0].ID != b.ID || deferred != 1 {
		t.Fatalf("got=%v deferred=%d", got, deferred)
	}
	if got, _ := m.preferHealthyGPTRoutes(all, "gpt-5.6-terra", now); len(got) != 2 {
		t.Fatal("other model was affected")
	}
	if got, _ := m.preferHealthyGPTRoutes(all, "gpt-5.6-sol", now.Add(gptRetryPressureWindow+time.Second)); len(got) != 2 {
		t.Fatal("route failed to recover after sample expiry")
	}
	for _, a := range all {
		for i := 0; i < 4; i++ {
			m.gptRetryPressure.observe("gpt-5.6-sol", routingChannelBaseKey(a), true, now)
		}
	}
	if got, deferred := m.preferHealthyGPTRoutes(all, "gpt-5.6-sol", now); len(got) != 2 || deferred != 0 {
		t.Fatal("all-degraded pool became unavailable")
	}
}

func TestGPTManagerPickPrefersRecentHealthyRoute(t *testing.T) {
	const model = "gpt-5.6-sol"
	m := NewManager(nil, &SpreadSelector{}, nil)
	m.RegisterExecutor(&streamLifecycleExecutor{})
	registerSchedulerModels(t, "codex", model, "degraded-first", "healthy-second")
	a := &Auth{ID: "degraded-first", Provider: "codex", Attributes: map[string]string{"base_url": "https://degraded.example", "priority": "20"}}
	b := &Auth{ID: "healthy-second", Provider: "codex", Attributes: map[string]string{"base_url": "https://healthy.example"}}
	for _, a := range []*Auth{a, b} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		m.gptRetryPressure.observe(model, routingChannelBaseKey(a), true, time.Now())
	}
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.configureGPTRoute(true)
	got, _, _, err := m.pickNextMixed(ctx, []string{"codex"}, model, cliproxyexecutor.Options{}, map[string]struct{}{})
	if err != nil || got == nil || got.ID != b.ID {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestGPTRetryAdmissionDoesNotChangeActiveTimeout(t *testing.T) {
	trace := &requestAttemptTrace{gptRoute: true}
	trace.configureGPTFirstEventPolicy(GPTFirstEventPolicySnapshot{EnforcedTimeoutMs: 50000, MaxChannels: 8, MaxRounds: 3})
	trace.recordGPTFirstEventAttempt(179*time.Second, true)
	if trace.gptRetryAdmissionExhausted() {
		t.Fatal("admission closed early")
	}
	trace.recordGPTFirstEventAttempt(time.Second, true)
	if !trace.gptRetryAdmissionExhausted() {
		t.Fatal("admission budget was ignored")
	}
	policy, _ := trace.gptFirstEventPolicyValue()
	if policy.EnforcedTimeoutMs != 50000 || policy.WaitBudgetMs != 0 {
		t.Fatal("active connection policy was altered")
	}
}

func TestGPTRetryPressureCongestionIsNotThrottling(t *testing.T) {
	trace := &requestAttemptTrace{}
	trace.recordGPTRetryPressure(gptRetryPressureSnapshot{State: gptRetryPressureStateCongested, Acquired: true, FailOpen: true, Wait: 2 * time.Millisecond}, nil)
	if trace.summary().GPTRetryPressureThrottled {
		t.Fatal("congestion or fail-open was counted as throttling")
	}
	trace.recordGPTRetryPressure(gptRetryPressureSnapshot{Queued: true}, nil)
	if !trace.summary().GPTRetryPressureThrottled {
		t.Fatal("real queueing was not recorded")
	}
}
