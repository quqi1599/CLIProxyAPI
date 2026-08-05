package auth

import (
	"context"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	log "github.com/sirupsen/logrus"
)

const (
	gptFirstEventPolicyWindow             = 5 * time.Minute
	gptFirstEventPolicyEvaluationInterval = time.Minute
	gptFirstEventPolicyMinStateHold       = 5 * time.Minute
	gptFirstEventPolicyMinSamples         = 100
	gptFirstEventPolicyEnterWindows       = 3
	gptFirstEventPolicyRecoveryWindows    = 10
	gptFirstEventPolicySlowRate           = 0.90
	gptFirstEventPolicyEscalationRate     = 0.50
	gptFirstEventPolicyRecoveryRate       = 0.95
	gptFirstEventPolicyLateSuccessGain    = 0.05
	gptFirstEventPolicyOutageSuccessRate  = 0.10
	gptFirstEventPolicyOutageFailureRate  = 0.90
	gptFirstEventPolicyWaitBudget         = 300 * time.Second
	gptFirstEventPolicyOutageWaitBudget   = 75 * time.Second
	gptFirstEventPolicyGlobalModel        = "*"
	gptFirstEventPolicyStateNormal        = "normal"
	gptFirstEventPolicyStateSlow30        = "slow_30s"
	gptFirstEventPolicyStateSlow40        = "slow_40s"
	gptFirstEventPolicyStateSlow50        = "slow_50s"
	gptFirstEventPolicyStateOutage        = "outage"
	gptFirstEventOutcomeDeliverable       = "deliverable"
	gptFirstEventOutcomeTimeout           = "timeout"
	gptFirstEventOutcomeFailure           = "upstream_failure"
)

type gptFirstEventSample struct {
	sequence    uint64
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
	latestSequence uint64
	Eligible       int
	Within25       int
	Within30       int
	Within40       int
	Within50       int
	Timeouts       int
	Upstream5xx    int
	NetworkFailure int
}

func (w gptFirstEventWindow) successRate(timeout time.Duration) float64 {
	if w.Eligible == 0 {
		return 0
	}
	var successes int
	switch {
	case timeout <= 25*time.Second:
		successes = w.Within25
	case timeout <= 30*time.Second:
		successes = w.Within30
	case timeout <= 40*time.Second:
		successes = w.Within40
	default:
		successes = w.Within50
	}
	return float64(successes) / float64(w.Eligible)
}

func (w gptFirstEventWindow) lateSuccessRate(lower, upper time.Duration) float64 {
	if w.Eligible == 0 {
		return 0
	}
	return math.Max(0, w.successRate(upper)-w.successRate(lower))
}

func (w gptFirstEventWindow) failureRate() float64 {
	if w.Eligible == 0 {
		return 0
	}
	return float64(w.Timeouts+w.Upstream5xx+w.NetworkFailure) / float64(w.Eligible)
}

type gptFirstEventPolicyState struct {
	name                   string
	previousState          string
	decisionReason         string
	lastEvaluatedAt        time.Time
	lastEvaluatedSequence  uint64
	lastTransitionAt       time.Time
	lastTransitionSequence uint64
	enterWindows           int
	recoveryWindows        int
}

type GPTFirstEventPolicySnapshot struct {
	Model                    string    `json:"model"`
	DecisionSource           string    `json:"decision_source"`
	WindowStart              time.Time `json:"window_start"`
	WindowEnd                time.Time `json:"window_end"`
	WindowSeconds            int64     `json:"window_seconds"`
	EligibleFirstAttempts    int       `json:"eligible_first_attempts"`
	DeliverableWithin25      int       `json:"deliverable_within_25"`
	DeliverableWithin30      int       `json:"deliverable_within_30"`
	DeliverableWithin40      int       `json:"deliverable_within_40"`
	DeliverableWithin50      int       `json:"deliverable_within_50"`
	FirstEventSuccessRate25  float64   `json:"first_event_success_rate_25"`
	FirstEventSuccessRate30  float64   `json:"first_event_success_rate_30"`
	FirstEventSuccessRate40  float64   `json:"first_event_success_rate_40"`
	FirstEventSuccessRate50  float64   `json:"first_event_success_rate_50"`
	FailureRate              float64   `json:"failure_rate"`
	Timeouts                 int       `json:"timeouts"`
	Upstream5xx              int       `json:"upstream_5xx"`
	NetworkFailures          int       `json:"network_failures"`
	PolicyState              string    `json:"policy_state"`
	PreviousState            string    `json:"previous_state,omitempty"`
	DecisionReason           string    `json:"decision_reason"`
	EnforcedTimeoutMs        int64     `json:"enforced_timeout_ms"`
	MaxChannels              int       `json:"max_channels"`
	MaxRounds                int       `json:"max_rounds"`
	WaitBudgetMs             int64     `json:"wait_budget_ms"`
	Transitioned             bool      `json:"transitioned"`
	LastTransitionAt         time.Time `json:"last_transition_at,omitempty"`
	UsedGlobalFallback       bool      `json:"used_global_fallback"`
	MinimumSamples           int       `json:"minimum_samples"`
	ObservationWindowSeconds int64     `json:"observation_window_seconds"`
}

type gptFirstEventObserver struct {
	mu       sync.Mutex
	samples  map[string][]gptFirstEventSample
	policies map[string]*gptFirstEventPolicyState
	sequence uint64
	now      func() time.Time
}

func newGPTFirstEventObserver() *gptFirstEventObserver {
	return &gptFirstEventObserver{
		samples:  make(map[string][]gptFirstEventSample),
		policies: make(map[string]*gptFirstEventPolicyState),
		now:      time.Now,
	}
}

func (o *gptFirstEventObserver) observe(model string, sample gptFirstEventSample) GPTFirstEventPolicySnapshot {
	if o == nil {
		return defaultGPTFirstEventPolicySnapshot(model)
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
	o.sequence++
	sample.sequence = o.sequence
	o.appendLocked(model, sample)
	o.evaluateLocked(model, sample.at)
	if model != gptFirstEventPolicyGlobalModel {
		o.appendLocked(gptFirstEventPolicyGlobalModel, sample)
		o.evaluateLocked(gptFirstEventPolicyGlobalModel, sample.at)
	}
	return o.snapshotLocked(model, sample.at, sample.sequence)
}

func (o *gptFirstEventObserver) snapshot(model string, now time.Time) GPTFirstEventPolicySnapshot {
	if o == nil {
		return defaultGPTFirstEventPolicySnapshot(model)
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
	return o.snapshotLocked(model, now, 0)
}

func (o *gptFirstEventObserver) appendLocked(model string, sample gptFirstEventSample) {
	cutoff := sample.at.Add(-gptFirstEventPolicyWindow)
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

func (o *gptFirstEventObserver) policyLocked(model string) *gptFirstEventPolicyState {
	policy := o.policies[model]
	if policy == nil {
		policy = &gptFirstEventPolicyState{
			name:           gptFirstEventPolicyStateNormal,
			decisionReason: "insufficient_samples",
		}
		o.policies[model] = policy
	}
	return policy
}

func (o *gptFirstEventObserver) evaluateLocked(model string, now time.Time) {
	window := o.windowLocked(model, now)
	policy := o.policyLocked(model)
	if window.Eligible < gptFirstEventPolicyMinSamples || window.latestSequence == policy.lastEvaluatedSequence {
		policy.decisionReason = "insufficient_samples"
		return
	}
	if !policy.lastEvaluatedAt.IsZero() && now.Sub(policy.lastEvaluatedAt) < gptFirstEventPolicyEvaluationInterval {
		return
	}
	policy.lastEvaluatedAt = now
	policy.lastEvaluatedSequence = window.latestSequence

	currentTimeout := gptFirstEventTimeoutForState(policy.name)
	currentRate := window.successRate(currentTimeout)
	failureRate := window.failureRate()
	if currentRate < gptFirstEventPolicyOutageSuccessRate && failureRate >= gptFirstEventPolicyOutageFailureRate {
		policy.enterWindows++
		policy.recoveryWindows = 0
		policy.decisionReason = "collective_outage_candidate"
		if policy.enterWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
			o.transitionLocked(policy, gptFirstEventPolicyStateOutage, "collective_outage", now, window.latestSequence)
		}
		return
	}

	if policy.name == gptFirstEventPolicyStateOutage {
		if window.successRate(25*time.Second) >= gptFirstEventPolicyEscalationRate {
			policy.recoveryWindows++
			policy.enterWindows = 0
			policy.decisionReason = "outage_recovery_candidate"
			if policy.recoveryWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
				next := gptFirstEventPolicyStateSlow30
				if window.successRate(25*time.Second) >= gptFirstEventPolicyRecoveryRate {
					next = gptFirstEventPolicyStateNormal
				}
				o.transitionLocked(policy, next, "outage_recovered", now, window.latestSequence)
			}
			return
		}
		policy.enterWindows = 0
		policy.recoveryWindows = 0
		policy.decisionReason = "collective_outage"
		return
	}

	if lowerState, lowerTimeout, ok := gptFirstEventLowerState(policy.name); ok && window.successRate(lowerTimeout) >= gptFirstEventPolicyRecoveryRate {
		policy.recoveryWindows++
		policy.enterWindows = 0
		policy.decisionReason = "recovery_candidate"
		if policy.recoveryWindows >= gptFirstEventPolicyRecoveryWindows && o.canTransitionLocked(policy, now) {
			o.transitionLocked(policy, lowerState, "sustained_recovery", now, window.latestSequence)
		}
		return
	}

	if nextState, lowerTimeout, upperTimeout, ok := gptFirstEventHigherState(policy.name); ok {
		rate := window.successRate(upperTimeout)
		lateGain := window.lateSuccessRate(lowerTimeout, upperTimeout)
		shouldEscalate := false
		reason := "stable"
		if policy.name == gptFirstEventPolicyStateNormal {
			shouldEscalate = rate < gptFirstEventPolicySlowRate
			reason = "first_event_rate_below_90_percent"
		} else {
			shouldEscalate = rate < gptFirstEventPolicyEscalationRate && lateGain >= gptFirstEventPolicyLateSuccessGain
			reason = "late_success_proves_more_wait_helps"
		}
		if shouldEscalate {
			policy.enterWindows++
			policy.recoveryWindows = 0
			policy.decisionReason = reason
			if policy.enterWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
				o.transitionLocked(policy, nextState, reason, now, window.latestSequence)
			}
			return
		}
	}

	policy.enterWindows = 0
	policy.recoveryWindows = 0
	policy.decisionReason = "stable"
}

func (o *gptFirstEventObserver) canTransitionLocked(policy *gptFirstEventPolicyState, now time.Time) bool {
	return policy.lastTransitionAt.IsZero() || now.Sub(policy.lastTransitionAt) >= gptFirstEventPolicyMinStateHold
}

func (o *gptFirstEventObserver) transitionLocked(policy *gptFirstEventPolicyState, next, reason string, now time.Time, sequence uint64) {
	if policy == nil || next == "" || policy.name == next {
		return
	}
	policy.previousState = policy.name
	policy.name = next
	policy.decisionReason = reason
	policy.lastTransitionAt = now
	policy.lastTransitionSequence = sequence
	policy.enterWindows = 0
	policy.recoveryWindows = 0
}

func (o *gptFirstEventObserver) snapshotLocked(model string, now time.Time, observationSequence uint64) GPTFirstEventPolicySnapshot {
	window := o.windowLocked(model, now)
	decisionSource := model
	usedGlobal := false
	if window.Eligible < gptFirstEventPolicyMinSamples && model != gptFirstEventPolicyGlobalModel {
		globalWindow := o.windowLocked(gptFirstEventPolicyGlobalModel, now)
		if globalWindow.Eligible >= gptFirstEventPolicyMinSamples {
			window = globalWindow
			decisionSource = gptFirstEventPolicyGlobalModel
			usedGlobal = true
		}
	}
	policy := o.policyLocked(decisionSource)
	timeout, maxChannels, maxRounds, waitBudget := gptFirstEventPolicyLimits(policy.name)
	return GPTFirstEventPolicySnapshot{
		Model:                    model,
		DecisionSource:           decisionSource,
		WindowStart:              window.windowStart,
		WindowEnd:                window.windowEnd,
		WindowSeconds:            int64(gptFirstEventPolicyWindow / time.Second),
		EligibleFirstAttempts:    window.Eligible,
		DeliverableWithin25:      window.Within25,
		DeliverableWithin30:      window.Within30,
		DeliverableWithin40:      window.Within40,
		DeliverableWithin50:      window.Within50,
		FirstEventSuccessRate25:  window.successRate(25 * time.Second),
		FirstEventSuccessRate30:  window.successRate(30 * time.Second),
		FirstEventSuccessRate40:  window.successRate(40 * time.Second),
		FirstEventSuccessRate50:  window.successRate(50 * time.Second),
		FailureRate:              window.failureRate(),
		Timeouts:                 window.Timeouts,
		Upstream5xx:              window.Upstream5xx,
		NetworkFailures:          window.NetworkFailure,
		PolicyState:              policy.name,
		PreviousState:            policy.previousState,
		DecisionReason:           policy.decisionReason,
		EnforcedTimeoutMs:        timeout.Milliseconds(),
		MaxChannels:              maxChannels,
		MaxRounds:                maxRounds,
		WaitBudgetMs:             waitBudget.Milliseconds(),
		Transitioned:             observationSequence > 0 && policy.lastTransitionSequence == observationSequence,
		LastTransitionAt:         policy.lastTransitionAt,
		UsedGlobalFallback:       usedGlobal,
		MinimumSamples:           gptFirstEventPolicyMinSamples,
		ObservationWindowSeconds: int64(gptFirstEventPolicyWindow / time.Second),
	}
}

func (o *gptFirstEventObserver) windowLocked(model string, now time.Time) gptFirstEventWindow {
	cutoff := now.Add(-gptFirstEventPolicyWindow)
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
		window.latestSequence = sample.sequence
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

func defaultGPTFirstEventPolicySnapshot(model string) GPTFirstEventPolicySnapshot {
	timeout, maxChannels, maxRounds, waitBudget := gptFirstEventPolicyLimits(gptFirstEventPolicyStateNormal)
	return GPTFirstEventPolicySnapshot{
		Model:                    canonicalModelKey(model),
		DecisionSource:           canonicalModelKey(model),
		WindowSeconds:            int64(gptFirstEventPolicyWindow / time.Second),
		PolicyState:              gptFirstEventPolicyStateNormal,
		DecisionReason:           "insufficient_samples",
		EnforcedTimeoutMs:        timeout.Milliseconds(),
		MaxChannels:              maxChannels,
		MaxRounds:                maxRounds,
		WaitBudgetMs:             waitBudget.Milliseconds(),
		MinimumSamples:           gptFirstEventPolicyMinSamples,
		ObservationWindowSeconds: int64(gptFirstEventPolicyWindow / time.Second),
	}
}

func gptFirstEventTimeoutForState(state string) time.Duration {
	timeout, _, _, _ := gptFirstEventPolicyLimits(state)
	return timeout
}

func gptFirstEventPolicyLimits(state string) (timeout time.Duration, maxChannels, maxRounds int, waitBudget time.Duration) {
	switch state {
	case gptFirstEventPolicyStateSlow30:
		return 30 * time.Second, 6, 2, gptFirstEventPolicyWaitBudget
	case gptFirstEventPolicyStateSlow40:
		return 40 * time.Second, 4, 2, gptFirstEventPolicyWaitBudget
	case gptFirstEventPolicyStateSlow50:
		return 50 * time.Second, 3, 2, gptFirstEventPolicyWaitBudget
	case gptFirstEventPolicyStateOutage:
		return 25 * time.Second, 3, 1, gptFirstEventPolicyOutageWaitBudget
	default:
		return 25 * time.Second, gptImmediateFailoverMaxChannels, gptImmediateFailoverMaxRounds, gptFirstEventPolicyWaitBudget
	}
}

func gptFirstEventHigherState(state string) (next string, lowerTimeout, upperTimeout time.Duration, ok bool) {
	switch state {
	case gptFirstEventPolicyStateNormal:
		return gptFirstEventPolicyStateSlow30, 0, 25 * time.Second, true
	case gptFirstEventPolicyStateSlow30:
		return gptFirstEventPolicyStateSlow40, 25 * time.Second, 30 * time.Second, true
	case gptFirstEventPolicyStateSlow40:
		return gptFirstEventPolicyStateSlow50, 30 * time.Second, 40 * time.Second, true
	default:
		return "", 0, 0, false
	}
}

func gptFirstEventLowerState(state string) (next string, timeout time.Duration, ok bool) {
	switch state {
	case gptFirstEventPolicyStateSlow30:
		return gptFirstEventPolicyStateNormal, 25 * time.Second, true
	case gptFirstEventPolicyStateSlow40:
		return gptFirstEventPolicyStateSlow30, 30 * time.Second, true
	case gptFirstEventPolicyStateSlow50:
		return gptFirstEventPolicyStateSlow40, 40 * time.Second, true
	default:
		return "", 0, false
	}
}

func newGPTFirstEventWaitBudgetError() error {
	return &Error{
		Code:       "gpt_first_event_wait_budget_exhausted",
		Message:    "GPT first-event wait budget exhausted before a downstream-deliverable event",
		Retryable:  true,
		HTTPStatus: http.StatusGatewayTimeout,
	}
}

func (m *Manager) GPTFirstEventPolicySnapshot(model string) GPTFirstEventPolicySnapshot {
	if m == nil || m.gptFirstEventObserver == nil {
		return defaultGPTFirstEventPolicySnapshot(model)
	}
	return m.gptFirstEventObserver.snapshot(model, time.Now())
}

func (m *Manager) selectGPTFirstEventPolicy(model string) GPTFirstEventPolicySnapshot {
	snapshot := m.GPTFirstEventPolicySnapshot(model)
	configured := time.Duration(m.gptFirstEventTimeout.Load())
	if configured <= 0 {
		snapshot.EnforcedTimeoutMs = 0
		snapshot.WaitBudgetMs = 0
		snapshot.MaxChannels = gptImmediateFailoverMaxChannels
		snapshot.MaxRounds = gptImmediateFailoverMaxRounds
		snapshot.DecisionReason = "disabled_by_configuration"
		return snapshot
	}
	if configured != defaultGPTFirstEventTimeout {
		snapshot.EnforcedTimeoutMs = configured.Milliseconds()
		snapshot.WaitBudgetMs = 0
		snapshot.MaxChannels = gptImmediateFailoverMaxChannels
		snapshot.MaxRounds = gptImmediateFailoverMaxRounds
		snapshot.DecisionReason = "manual_timeout_override"
	}
	return snapshot
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
	fields := log.Fields{
		"event":                       "gpt_first_event_observation",
		"model":                       canonicalModelKey(model),
		"outcome":                     outcome,
		"eligible":                    true,
		"delay_ms":                    delay.Milliseconds(),
		"enforced_timeout_ms":         enforcedTimeout.Milliseconds(),
		"policy_state":                snapshot.PolicyState,
		"decision_source":             snapshot.DecisionSource,
		"decision_reason":             snapshot.DecisionReason,
		"window_seconds":              snapshot.WindowSeconds,
		"eligible_first_attempts":     snapshot.EligibleFirstAttempts,
		"deliverable_within_25":       snapshot.DeliverableWithin25,
		"deliverable_within_30":       snapshot.DeliverableWithin30,
		"deliverable_within_40":       snapshot.DeliverableWithin40,
		"deliverable_within_50":       snapshot.DeliverableWithin50,
		"first_event_success_rate_25": roundPolicyRate(snapshot.FirstEventSuccessRate25),
		"first_event_success_rate_30": roundPolicyRate(snapshot.FirstEventSuccessRate30),
		"first_event_success_rate_40": roundPolicyRate(snapshot.FirstEventSuccessRate40),
		"first_event_success_rate_50": roundPolicyRate(snapshot.FirstEventSuccessRate50),
		"failure_rate":                roundPolicyRate(snapshot.FailureRate),
		"timeout_count":               snapshot.Timeouts,
		"upstream_5xx_count":          snapshot.Upstream5xx,
		"network_failure_count":       snapshot.NetworkFailures,
		"used_global_fallback":        snapshot.UsedGlobalFallback,
	}
	addRequestAttemptLogFields(ctx, fields)
	logEntryWithRequestID(ctx).WithFields(fields).Info("gpt_first_event_observation")
	if snapshot.Transitioned {
		transitionFields := log.Fields{
			"event":                       "gpt_first_event_policy_transition",
			"model":                       canonicalModelKey(model),
			"decision_source":             snapshot.DecisionSource,
			"previous_state":              snapshot.PreviousState,
			"policy_state":                snapshot.PolicyState,
			"decision_reason":             snapshot.DecisionReason,
			"enforced_timeout_ms":         snapshot.EnforcedTimeoutMs,
			"max_channels":                snapshot.MaxChannels,
			"max_rounds":                  snapshot.MaxRounds,
			"wait_budget_ms":              snapshot.WaitBudgetMs,
			"eligible_first_attempts":     snapshot.EligibleFirstAttempts,
			"first_event_success_rate_25": roundPolicyRate(snapshot.FirstEventSuccessRate25),
			"failure_rate":                roundPolicyRate(snapshot.FailureRate),
		}
		logEntryWithRequestID(ctx).WithFields(transitionFields).Warn("gpt_first_event_policy_transition")
	}
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
