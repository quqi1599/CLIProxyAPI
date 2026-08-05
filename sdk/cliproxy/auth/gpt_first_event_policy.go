package auth

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	log "github.com/sirupsen/logrus"
)

const (
	gptFirstEventShadowWindow       = 5 * time.Minute
	gptFirstEventShadowMinSamples   = 100
	gptFirstEventShadowSlowRate     = 0.90
	gptFirstEventShadowBaseTimeout  = 25 * time.Second
	gptFirstEventShadowSlowTimeout  = 30 * time.Second
	gptFirstEventPolicyGlobalModel  = "*"
	gptFirstEventOutcomeDeliverable = "deliverable"
	gptFirstEventOutcomeTimeout     = "timeout"
	gptFirstEventOutcomeFailure     = "upstream_failure"
)

type gptFirstEventSample struct {
	at          time.Time
	deliverable bool
	delay       time.Duration
	timedOut    bool
	upstream5xx bool
	network     bool
}

type gptFirstEventWindow struct {
	windowStart    time.Time
	windowEnd      time.Time
	Eligible       int
	Within25       int
	Within30       int
	Within40       int
	Within50       int
	Timeouts       int
	Upstream5xx    int
	NetworkFailure int
}

func (w gptFirstEventWindow) successRateWithin25() float64 {
	if w.Eligible == 0 {
		return 0
	}
	return float64(w.Within25) / float64(w.Eligible)
}

type GPTFirstEventPolicySnapshot struct {
	Model                    string    `json:"model"`
	WindowStart              time.Time `json:"window_start"`
	WindowEnd                time.Time `json:"window_end"`
	WindowSeconds            int64     `json:"window_seconds"`
	EligibleFirstAttempts    int       `json:"eligible_first_attempts"`
	DeliverableWithin25      int       `json:"deliverable_within_25"`
	DeliverableWithin30      int       `json:"deliverable_within_30"`
	DeliverableWithin40      int       `json:"deliverable_within_40"`
	DeliverableWithin50      int       `json:"deliverable_within_50"`
	FirstEventSuccessRate25  float64   `json:"first_event_success_rate_25"`
	Timeouts                 int       `json:"timeouts"`
	Upstream5xx              int       `json:"upstream_5xx"`
	NetworkFailures          int       `json:"network_failures"`
	SuggestedTimeoutMs       int64     `json:"suggested_timeout_ms"`
	ShadowState              string    `json:"shadow_state"`
	UsedGlobalFallback       bool      `json:"used_global_fallback"`
	MinimumSamples           int       `json:"minimum_samples"`
	ObservationWindowSeconds int64     `json:"observation_window_seconds"`
}

type gptFirstEventObserver struct {
	mu      sync.Mutex
	samples map[string][]gptFirstEventSample
	now     func() time.Time
}

func newGPTFirstEventObserver() *gptFirstEventObserver {
	return &gptFirstEventObserver{
		samples: make(map[string][]gptFirstEventSample),
		now:     time.Now,
	}
}

func (o *gptFirstEventObserver) observe(model string, sample gptFirstEventSample) GPTFirstEventPolicySnapshot {
	if o == nil {
		return GPTFirstEventPolicySnapshot{}
	}
	model = canonicalModelKey(model)
	if model == "" {
		model = gptFirstEventPolicyGlobalModel
	}
	if sample.at.IsZero() {
		sample.at = o.now()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.appendLocked(model, sample)
	if model != gptFirstEventPolicyGlobalModel {
		o.appendLocked(gptFirstEventPolicyGlobalModel, sample)
	}
	return o.snapshotLocked(model, sample.at)
}

func (o *gptFirstEventObserver) snapshot(model string, now time.Time) GPTFirstEventPolicySnapshot {
	if o == nil {
		return GPTFirstEventPolicySnapshot{}
	}
	model = canonicalModelKey(model)
	if model == "" {
		model = gptFirstEventPolicyGlobalModel
	}
	if now.IsZero() {
		now = o.now()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.snapshotLocked(model, now)
}

func (o *gptFirstEventObserver) appendLocked(model string, sample gptFirstEventSample) {
	cutoff := sample.at.Add(-gptFirstEventShadowWindow)
	current := o.samples[model]
	first := 0
	for first < len(current) && current[first].at.Before(cutoff) {
		first++
	}
	if first > 0 {
		current = append([]gptFirstEventSample(nil), current[first:]...)
	}
	current = append(current, sample)
	o.samples[model] = current
}

func (o *gptFirstEventObserver) snapshotLocked(model string, now time.Time) GPTFirstEventPolicySnapshot {
	window := o.windowLocked(model, now)
	usedGlobal := false
	if window.Eligible < gptFirstEventShadowMinSamples && model != gptFirstEventPolicyGlobalModel {
		globalWindow := o.windowLocked(gptFirstEventPolicyGlobalModel, now)
		if globalWindow.Eligible >= gptFirstEventShadowMinSamples {
			window = globalWindow
			usedGlobal = true
		}
	}
	rate25 := window.successRateWithin25()
	suggested := gptFirstEventShadowBaseTimeout
	state := "normal"
	if window.Eligible >= gptFirstEventShadowMinSamples && rate25 < gptFirstEventShadowSlowRate {
		suggested = gptFirstEventShadowSlowTimeout
		state = "slow"
	}
	return GPTFirstEventPolicySnapshot{
		Model:                    model,
		WindowStart:              window.windowStart,
		WindowEnd:                window.windowEnd,
		WindowSeconds:            int64(gptFirstEventShadowWindow / time.Second),
		EligibleFirstAttempts:    window.Eligible,
		DeliverableWithin25:      window.Within25,
		DeliverableWithin30:      window.Within30,
		DeliverableWithin40:      window.Within40,
		DeliverableWithin50:      window.Within50,
		FirstEventSuccessRate25:  rate25,
		Timeouts:                 window.Timeouts,
		Upstream5xx:              window.Upstream5xx,
		NetworkFailures:          window.NetworkFailure,
		SuggestedTimeoutMs:       suggested.Milliseconds(),
		ShadowState:              state,
		UsedGlobalFallback:       usedGlobal,
		MinimumSamples:           gptFirstEventShadowMinSamples,
		ObservationWindowSeconds: int64(gptFirstEventShadowWindow / time.Second),
	}
}

func (o *gptFirstEventObserver) windowLocked(model string, now time.Time) gptFirstEventWindow {
	cutoff := now.Add(-gptFirstEventShadowWindow)
	window := gptFirstEventWindow{windowStart: cutoff, windowEnd: now}
	current := o.samples[model]
	first := 0
	for first < len(current) && current[first].at.Before(cutoff) {
		first++
	}
	if first > 0 {
		current = append([]gptFirstEventSample(nil), current[first:]...)
		o.samples[model] = current
	}
	for _, sample := range current {
		if sample.at.After(now) {
			continue
		}
		window.Eligible++
		if sample.deliverable {
			if sample.delay <= 25*time.Second {
				window.Within25++
			}
			if sample.delay <= 30*time.Second {
				window.Within30++
			}
			if sample.delay <= 40*time.Second {
				window.Within40++
			}
			if sample.delay <= 50*time.Second {
				window.Within50++
			}
		}
		if sample.timedOut {
			window.Timeouts++
		}
		if sample.upstream5xx {
			window.Upstream5xx++
		}
		if sample.network {
			window.NetworkFailure++
		}
	}
	return window
}

func (m *Manager) GPTFirstEventPolicySnapshot(model string) GPTFirstEventPolicySnapshot {
	if m == nil || m.gptFirstEventObserver == nil {
		return GPTFirstEventPolicySnapshot{}
	}
	return m.gptFirstEventObserver.snapshot(model, time.Now())
}

func (m *Manager) recordGPTFirstEventAttempt(ctx context.Context, model string, enforcedTimeout, delay time.Duration, deliverable bool, err error) {
	if m == nil || !isGPTRequestRoute(ctx, nil, model) {
		return
	}
	trace := requestAttemptTraceFromContext(ctx)
	if trace == nil {
		return
	}
	timedOut := strings.EqualFold(strings.TrimSpace(errorCodeFromError(err)), "gpt_first_event_timeout")
	trace.recordGPTFirstEventAttempt(delay, timedOut)
	if !trace.claimGPTFirstEventObservation() {
		return
	}
	eligible, outcome, upstream5xx, network := classifyGPTFirstEventObservation(deliverable, timedOut, err)
	if !eligible {
		return
	}
	snapshot := m.gptFirstEventObserver.observe(model, gptFirstEventSample{
		at:          time.Now(),
		deliverable: deliverable,
		delay:       delay,
		timedOut:    timedOut,
		upstream5xx: upstream5xx,
		network:     network,
	})
	trace.recordGPTFirstEventShadow(snapshot)
	fields := log.Fields{
		"event":                       "gpt_first_event_observation",
		"model":                       canonicalModelKey(model),
		"outcome":                     outcome,
		"eligible":                    true,
		"delay_ms":                    delay.Milliseconds(),
		"enforced_timeout_ms":         enforcedTimeout.Milliseconds(),
		"shadow_only":                 true,
		"shadow_state":                snapshot.ShadowState,
		"shadow_timeout_ms":           snapshot.SuggestedTimeoutMs,
		"window_seconds":              snapshot.WindowSeconds,
		"eligible_first_attempts":     snapshot.EligibleFirstAttempts,
		"deliverable_within_25":       snapshot.DeliverableWithin25,
		"first_event_success_rate_25": roundPolicyRate(snapshot.FirstEventSuccessRate25),
		"timeout_count":               snapshot.Timeouts,
		"upstream_5xx_count":          snapshot.Upstream5xx,
		"network_failure_count":       snapshot.NetworkFailures,
		"used_global_fallback":        snapshot.UsedGlobalFallback,
	}
	addRequestAttemptLogFields(ctx, fields)
	logEntryWithRequestID(ctx).WithFields(fields).Info("gpt_first_event_observation")
}

func classifyGPTFirstEventObservation(deliverable, timedOut bool, err error) (eligible bool, outcome string, upstream5xx, network bool) {
	if deliverable {
		return true, gptFirstEventOutcomeDeliverable, false, false
	}
	if timedOut {
		return true, gptFirstEventOutcomeTimeout, false, false
	}
	if err == nil || isRequestInvalidError(err) {
		return false, "", false, false
	}
	if failure, ok := failurecontract.As(err); ok && failure.Scope == failurecontract.ScopeRequest {
		return false, "", false, false
	}
	status := statusCodeFromError(err)
	upstream5xx = status >= 500 && status <= 599
	network = isTransientNetworkError(err)
	if !upstream5xx && !network {
		return false, "", false, false
	}
	return true, gptFirstEventOutcomeFailure, upstream5xx, network
}

func roundPolicyRate(value float64) float64 {
	return math.Round(value*10000) / 10000
}
