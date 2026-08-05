package auth

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestGPTFirstEventObserverRequiresMinimumSamples(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for i := 0; i < gptFirstEventShadowMinSamples-1; i++ {
		observer.observe("gpt-5.6-sol", gptFirstEventSample{
			at:          now.Add(time.Duration(i) * time.Millisecond),
			deliverable: i < 80,
			delay:       20 * time.Second,
			timedOut:    i >= 80,
		})
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(time.Second))
	if snapshot.ShadowState != "normal" {
		t.Fatalf("shadow state = %q, want normal", snapshot.ShadowState)
	}
	if snapshot.SuggestedTimeoutMs != gptFirstEventShadowBaseTimeout.Milliseconds() {
		t.Fatalf("suggested timeout = %dms, want %dms", snapshot.SuggestedTimeoutMs, gptFirstEventShadowBaseTimeout.Milliseconds())
	}
}

func TestGPTFirstEventObserverSuggestsSlowTimeout(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for i := 0; i < gptFirstEventShadowMinSamples; i++ {
		observer.observe("gpt-5.6-sol", gptFirstEventSample{
			at:          now.Add(time.Duration(i) * time.Millisecond),
			deliverable: i < 80,
			delay:       20 * time.Second,
			timedOut:    i >= 80,
		})
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(time.Second))
	if snapshot.ShadowState != "slow" {
		t.Fatalf("shadow state = %q, want slow", snapshot.ShadowState)
	}
	if snapshot.SuggestedTimeoutMs != gptFirstEventShadowSlowTimeout.Milliseconds() {
		t.Fatalf("suggested timeout = %dms, want %dms", snapshot.SuggestedTimeoutMs, gptFirstEventShadowSlowTimeout.Milliseconds())
	}
	if snapshot.FirstEventSuccessRate25 != 0.8 {
		t.Fatalf("25s success rate = %v, want 0.8", snapshot.FirstEventSuccessRate25)
	}
	if snapshot.Timeouts != 20 {
		t.Fatalf("timeouts = %d, want 20", snapshot.Timeouts)
	}
}

func TestGPTFirstEventObserverUsesGlobalFallback(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for i := 0; i < gptFirstEventShadowMinSamples; i++ {
		model := "gpt-5.6-sol"
		if i%2 == 1 {
			model = "gpt-5.6-terra"
		}
		observer.observe(model, gptFirstEventSample{
			at:          now.Add(time.Duration(i) * time.Millisecond),
			deliverable: i < 80,
			delay:       20 * time.Second,
			timedOut:    i >= 80,
		})
	}

	snapshot := observer.snapshot("gpt-5.6-terra", now.Add(time.Second))
	if !snapshot.UsedGlobalFallback {
		t.Fatal("expected global fallback")
	}
	if snapshot.EligibleFirstAttempts != gptFirstEventShadowMinSamples {
		t.Fatalf("eligible attempts = %d, want %d", snapshot.EligibleFirstAttempts, gptFirstEventShadowMinSamples)
	}
	if snapshot.ShadowState != "slow" {
		t.Fatalf("shadow state = %q, want slow", snapshot.ShadowState)
	}
}

func TestGPTFirstEventObserverExpiresOldSamples(t *testing.T) {
	observer := newGPTFirstEventObserver()
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	for i := 0; i < gptFirstEventShadowMinSamples; i++ {
		observer.observe("gpt-5.6-sol", gptFirstEventSample{
			at:       now.Add(time.Duration(i) * time.Millisecond),
			timedOut: true,
		})
	}

	snapshot := observer.snapshot("gpt-5.6-sol", now.Add(gptFirstEventShadowWindow+time.Second))
	if snapshot.EligibleFirstAttempts != 0 {
		t.Fatalf("eligible attempts = %d, want 0", snapshot.EligibleFirstAttempts)
	}
	if snapshot.ShadowState != "normal" {
		t.Fatalf("shadow state = %q, want normal", snapshot.ShadowState)
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
