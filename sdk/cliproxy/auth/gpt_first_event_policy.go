package auth

import (
	"context"
	"math"
	"net/http"
	"sort"
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
	// Protective escalation intentionally needs fewer samples than recovery.
	// This lets an individual model react to a latency burst without allowing a
	// short healthy interval to lower the timeout just as quickly.
	gptFirstEventPolicyEscalationMinSamples = 40
	gptFirstEventPolicyRecoveryMinSamples   = 100
	gptFirstEventPolicyLowSampleMinSamples  = 10
	gptFirstEventPolicyLowSampleTimeoutRate = 0.80
	gptFirstEventPolicyMinSamples           = gptFirstEventPolicyEscalationMinSamples
	gptFirstEventPolicyEnterWindows         = 3
	gptFirstEventPolicyRecoveryWindows      = 10
	gptFirstEventPolicySlowRate             = 0.90
	gptFirstEventPolicySlowTimeoutRate      = 0.10
	gptFirstEventPolicyEscalationRate       = 0.50
	gptFirstEventPolicyRecoveryRate         = 0.95
	gptFirstEventPolicyLateSuccessGain      = 0.05
	gptFirstEventPolicyOutageSuccessRate    = 0.10
	gptFirstEventPolicyOutageFailureRate    = 0.90
	gptFirstEventPolicyWaitBudget           = 300 * time.Second
	gptFirstEventPolicyOutageWaitBudget     = 75 * time.Second
	gptFirstEventPolicyPersistenceTTL       = 24 * time.Hour
	gptFirstEventPolicyCheckpointInterval   = time.Hour
	gptFirstEventPolicyPersistTimeout       = 2 * time.Second
	gptFirstEventPolicyDailyRetention       = 31
	gptFirstEventPolicyGlobalModel          = "*"
	gptFirstEventPolicyStateNormal          = "normal"
	gptFirstEventPolicyStateSlow30          = "slow_30s"
	gptFirstEventPolicyStateSlow40          = "slow_40s"
	gptFirstEventPolicyStateSlow50          = "slow_50s"
	gptFirstEventPolicyStateOutage          = "outage"
	gptFirstEventOutcomeDeliverable         = "deliverable"
	gptFirstEventOutcomeTimeout             = "timeout"
	gptFirstEventOutcomeFailure             = "upstream_failure"
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

func (w gptFirstEventWindow) hardFailureRate() float64 {
	if w.Eligible == 0 {
		return 0
	}
	return float64(w.Upstream5xx+w.NetworkFailure) / float64(w.Eligible)
}

func (w gptFirstEventWindow) timeoutRate() float64 {
	if w.Eligible == 0 {
		return 0
	}
	return float64(w.Timeouts) / float64(w.Eligible)
}

type gptFirstEventPolicyState struct {
	name                    string
	previousState           string
	decisionReason          string
	candidateTarget         string
	candidateReason         string
	lastEvaluatedAt         time.Time
	lastEvaluatedSequence   uint64
	lastTransitionAt        time.Time
	updatedAt               time.Time
	lastTransitionSequence  uint64
	enterWindows            int
	recoveryWindows         int
	lowSampleTimeoutWindows int
	lastEvaluationLowSample bool
}

// GPTFirstEventPolicyStateRecord is the durable state for one exact model.
// Short-window samples and in-progress candidate counters are intentionally
// excluded so a restart cannot replay stale evidence as a fresh transition.
type GPTFirstEventPolicyStateRecord struct {
	Model            string    `json:"model"`
	PolicyState      string    `json:"policy_state"`
	PreviousState    string    `json:"previous_state,omitempty"`
	DecisionReason   string    `json:"decision_reason,omitempty"`
	LastTransitionAt time.Time `json:"last_transition_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type gptFirstEventDailyBucket struct {
	Date           string
	Eligible       int
	Within25       int
	Within30       int
	Within40       int
	Within50       int
	Timeouts       int
	Upstream5xx    int
	NetworkFailure int
	Transitions    map[string]int
}

type GPTFirstEventDailySnapshot struct {
	Date                    string         `json:"date"`
	EligibleFirstAttempts   int            `json:"eligible_first_attempts"`
	DeliverableWithin25     int            `json:"deliverable_within_25"`
	DeliverableWithin30     int            `json:"deliverable_within_30"`
	DeliverableWithin40     int            `json:"deliverable_within_40"`
	DeliverableWithin50     int            `json:"deliverable_within_50"`
	FirstEventSuccessRate25 float64        `json:"first_event_success_rate_25"`
	FirstEventSuccessRate30 float64        `json:"first_event_success_rate_30"`
	FirstEventSuccessRate40 float64        `json:"first_event_success_rate_40"`
	FirstEventSuccessRate50 float64        `json:"first_event_success_rate_50"`
	FailureRate             float64        `json:"failure_rate"`
	Timeouts                int            `json:"timeouts"`
	Upstream5xx             int            `json:"upstream_5xx"`
	NetworkFailures         int            `json:"network_failures"`
	Transitions             map[string]int `json:"transitions"`
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
	HardFailureRate          float64   `json:"hard_failure_rate"`
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
	RecoveryMinimumSamples   int       `json:"recovery_minimum_samples"`
	ObservationWindowSeconds int64     `json:"observation_window_seconds"`
	stateCheckpointed        bool
}

type gptFirstEventObserver struct {
	mu       sync.Mutex
	samples  map[string][]gptFirstEventSample
	policies map[string]*gptFirstEventPolicyState
	daily    map[string]map[string]*gptFirstEventDailyBucket
	sequence uint64
	now      func() time.Time
}

func newGPTFirstEventObserver() *gptFirstEventObserver {
	return &gptFirstEventObserver{
		samples:  make(map[string][]gptFirstEventSample),
		policies: make(map[string]*gptFirstEventPolicyState),
		daily:    make(map[string]map[string]*gptFirstEventDailyBucket),
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
	checkpointed := o.checkpointPolicyStateLocked(model, sample.at)
	o.recordDailyLocked(model, sample)
	if model != gptFirstEventPolicyGlobalModel {
		o.appendLocked(gptFirstEventPolicyGlobalModel, sample)
		o.evaluateLocked(gptFirstEventPolicyGlobalModel, sample.at)
		o.recordDailyLocked(gptFirstEventPolicyGlobalModel, sample)
	}
	snapshot := o.snapshotLocked(model, sample.at, sample.sequence)
	snapshot.stateCheckpointed = checkpointed
	return snapshot
}

func (o *gptFirstEventObserver) dailySnapshot(model string, days int, now time.Time) []GPTFirstEventDailySnapshot {
	if o == nil {
		return []GPTFirstEventDailySnapshot{}
	}
	model = canonicalModelKey(model)
	if model == "" {
		model = gptFirstEventPolicyGlobalModel
	}
	if days <= 0 {
		days = 7
	}
	if days > gptFirstEventPolicyDailyRetention {
		days = gptFirstEventPolicyDailyRetention
	}
	if now.IsZero() {
		now = o.now()
	}
	cutoff := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	o.mu.Lock()
	defer o.mu.Unlock()
	buckets := o.daily[model]
	dates := make([]string, 0, len(buckets))
	for date := range buckets {
		if date >= cutoff {
			dates = append(dates, date)
		}
	}
	sort.Strings(dates)
	out := make([]GPTFirstEventDailySnapshot, 0, len(dates))
	for _, date := range dates {
		bucket := buckets[date]
		if bucket == nil {
			continue
		}
		window := gptFirstEventWindow{
			Eligible:       bucket.Eligible,
			Within25:       bucket.Within25,
			Within30:       bucket.Within30,
			Within40:       bucket.Within40,
			Within50:       bucket.Within50,
			Timeouts:       bucket.Timeouts,
			Upstream5xx:    bucket.Upstream5xx,
			NetworkFailure: bucket.NetworkFailure,
		}
		transitions := make(map[string]int, len(bucket.Transitions))
		for state, count := range bucket.Transitions {
			transitions[state] = count
		}
		out = append(out, GPTFirstEventDailySnapshot{
			Date:                    date,
			EligibleFirstAttempts:   bucket.Eligible,
			DeliverableWithin25:     bucket.Within25,
			DeliverableWithin30:     bucket.Within30,
			DeliverableWithin40:     bucket.Within40,
			DeliverableWithin50:     bucket.Within50,
			FirstEventSuccessRate25: window.successRate(25 * time.Second),
			FirstEventSuccessRate30: window.successRate(30 * time.Second),
			FirstEventSuccessRate40: window.successRate(40 * time.Second),
			FirstEventSuccessRate50: window.successRate(50 * time.Second),
			FailureRate:             window.failureRate(),
			Timeouts:                bucket.Timeouts,
			Upstream5xx:             bucket.Upstream5xx,
			NetworkFailures:         bucket.NetworkFailure,
			Transitions:             transitions,
		})
	}
	return out
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
	transitioned := o.relaxStaleOutageLocked(model, now)
	snapshot := o.snapshotLocked(model, now, 0)
	snapshot.Transitioned = transitioned
	return snapshot
}

func (o *gptFirstEventObserver) relaxStaleOutageLocked(model string, now time.Time) bool {
	if model == "" || model == gptFirstEventPolicyGlobalModel {
		return false
	}
	policy := o.policies[model]
	if policy == nil || policy.name != gptFirstEventPolicyStateOutage || policy.lastTransitionAt.IsZero() || now.Sub(policy.lastTransitionAt) < gptFirstEventPolicyMinStateHold {
		return false
	}
	window := o.windowLocked(model, now)
	if window.Eligible >= gptFirstEventPolicyEscalationMinSamples && window.hardFailureRate() >= gptFirstEventPolicyOutageFailureRate {
		return false
	}
	o.transitionLocked(policy, gptFirstEventPolicyStateSlow30, "outage_evidence_expired_to_slow", now, 0)
	return true
}

func (o *gptFirstEventObserver) checkpointPolicyStateLocked(model string, now time.Time) bool {
	if model == "" || model == gptFirstEventPolicyGlobalModel {
		return false
	}
	policy := o.policies[model]
	if policy == nil || !gptFirstEventSlowStateSupportedByWindow(policy.name, o.windowLocked(model, now)) {
		return false
	}
	updatedAt := policy.updatedAt
	if updatedAt.IsZero() {
		updatedAt = policy.lastTransitionAt
	}
	if !updatedAt.IsZero() && now.Sub(updatedAt) < gptFirstEventPolicyCheckpointInterval {
		return false
	}
	policy.updatedAt = now
	return true
}

func gptFirstEventSlowStateSupportedByWindow(state string, window gptFirstEventWindow) bool {
	if window.Eligible < gptFirstEventPolicyLowSampleMinSamples {
		return false
	}
	if window.timeoutRate() >= gptFirstEventPolicySlowTimeoutRate {
		return true
	}
	_, lowerTimeout, ok := gptFirstEventLowerState(state)
	if !ok {
		return false
	}
	return window.lateSuccessRate(lowerTimeout, gptFirstEventTimeoutForState(state)) >= gptFirstEventPolicyLateSuccessGain
}

func (o *gptFirstEventObserver) exportPolicyStates(now time.Time) []GPTFirstEventPolicyStateRecord {
	if o == nil {
		return nil
	}
	if now.IsZero() {
		now = o.now()
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	records := make([]GPTFirstEventPolicyStateRecord, 0, len(o.policies))
	for model, policy := range o.policies {
		model = canonicalModelKey(model)
		if model == "" || model == gptFirstEventPolicyGlobalModel || policy == nil || !validGPTFirstEventPolicyState(policy.name) {
			continue
		}
		updatedAt := policy.updatedAt
		if updatedAt.IsZero() {
			updatedAt = policy.lastTransitionAt
		}
		if updatedAt.IsZero() {
			updatedAt = now
		}
		records = append(records, GPTFirstEventPolicyStateRecord{
			Model:            model,
			PolicyState:      policy.name,
			PreviousState:    policy.previousState,
			DecisionReason:   policy.decisionReason,
			LastTransitionAt: policy.lastTransitionAt,
			UpdatedAt:        updatedAt,
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Model < records[j].Model })
	return records
}

func (o *gptFirstEventObserver) restorePolicyStates(records []GPTFirstEventPolicyStateRecord) {
	if o == nil || len(records) == 0 {
		return
	}
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()

	for _, record := range records {
		model := canonicalModelKey(record.Model)
		state := strings.TrimSpace(record.PolicyState)
		updatedAt := record.UpdatedAt
		if model == "" || model == gptFirstEventPolicyGlobalModel || !validGPTFirstEventPolicyState(state) || updatedAt.IsZero() {
			continue
		}
		if now.Sub(updatedAt) > gptFirstEventPolicyPersistenceTTL {
			continue
		}
		previous := strings.TrimSpace(record.PreviousState)
		if previous != "" && !validGPTFirstEventPolicyState(previous) {
			previous = ""
		}
		lastTransitionAt := record.LastTransitionAt
		decisionReason := strings.TrimSpace(record.DecisionReason)
		// Persisted outage is a hard-failure snapshot, not a safe startup mode:
		// without the prior observation window it would otherwise pin a model to
		// 25 seconds and one round indefinitely. Resume conservatively at slow30
		// and require fresh samples to classify a new outage.
		if state == gptFirstEventPolicyStateOutage {
			previous = gptFirstEventPolicyStateOutage
			state = gptFirstEventPolicyStateSlow30
			decisionReason = "restored_outage_as_slow_30s"
			lastTransitionAt = now
			updatedAt = now
		}
		if existing := o.policies[model]; existing != nil && existing.updatedAt.After(updatedAt) {
			continue
		}
		o.policies[model] = &gptFirstEventPolicyState{
			name:             state,
			previousState:    previous,
			decisionReason:   decisionReason,
			lastTransitionAt: lastTransitionAt,
			updatedAt:        updatedAt,
		}
	}
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

func (o *gptFirstEventObserver) recordDailyLocked(model string, sample gptFirstEventSample) {
	if o.daily[model] == nil {
		o.daily[model] = make(map[string]*gptFirstEventDailyBucket)
	}
	date := sample.at.Format("2006-01-02")
	bucket := o.daily[model][date]
	if bucket == nil {
		bucket = &gptFirstEventDailyBucket{Date: date, Transitions: make(map[string]int)}
		o.daily[model][date] = bucket
	}
	bucket.Eligible++
	if sample.deliverable {
		if sample.delay <= 25*time.Second {
			bucket.Within25++
		}
		if sample.delay <= 30*time.Second {
			bucket.Within30++
		}
		if sample.delay <= 40*time.Second {
			bucket.Within40++
		}
		if sample.delay <= 50*time.Second {
			bucket.Within50++
		}
	}
	if sample.timedOut {
		bucket.Timeouts++
	}
	if sample.upstream5xx {
		bucket.Upstream5xx++
	}
	if sample.network {
		bucket.NetworkFailure++
	}
	if policy := o.policies[model]; policy != nil && policy.lastTransitionSequence == sample.sequence {
		bucket.Transitions[policy.name]++
	}
	cutoff := sample.at.AddDate(0, 0, -gptFirstEventPolicyDailyRetention).Format("2006-01-02")
	for storedDate := range o.daily[model] {
		if storedDate < cutoff {
			delete(o.daily[model], storedDate)
		}
	}
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
	if window.Eligible < gptFirstEventPolicyLowSampleMinSamples {
		policy.decisionReason = "insufficient_samples"
		o.clearCandidateLocked(policy)
		return
	}
	if window.latestSequence == policy.lastEvaluatedSequence {
		return
	}
	crossedFullSampleBoundary := policy.lastEvaluationLowSample && window.Eligible >= gptFirstEventPolicyEscalationMinSamples
	if !crossedFullSampleBoundary && !policy.lastEvaluatedAt.IsZero() && now.Sub(policy.lastEvaluatedAt) < gptFirstEventPolicyEvaluationInterval {
		return
	}
	policy.lastEvaluatedAt = now
	policy.lastEvaluatedSequence = window.latestSequence
	if window.Eligible < gptFirstEventPolicyEscalationMinSamples {
		policy.lastEvaluationLowSample = true
		if policy.name == gptFirstEventPolicyStateNormal && window.timeoutRate() >= gptFirstEventPolicyLowSampleTimeoutRate {
			o.prepareCandidateLocked(policy, gptFirstEventPolicyStateSlow30, "low_sample_timeout_protection")
			policy.lowSampleTimeoutWindows++
			policy.decisionReason = "low_sample_timeout_candidate"
			if policy.lowSampleTimeoutWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
				o.transitionLocked(policy, gptFirstEventPolicyStateSlow30, "low_sample_timeout_protection", now, window.latestSequence)
			}
			return
		}
		o.clearCandidateLocked(policy)
		policy.decisionReason = "insufficient_samples"
		return
	}
	policy.lastEvaluationLowSample = false
	if policy.lowSampleTimeoutWindows > 0 {
		o.clearCandidateLocked(policy)
	}

	currentTimeout := gptFirstEventTimeoutForState(policy.name)
	currentRate := window.successRate(currentTimeout)
	hardFailureRate := window.hardFailureRate()
	// A local first-event deadline is a latency signal, not proof of a provider
	// outage. Only explicit upstream 5xx and network failures can enter outage.
	if currentRate < gptFirstEventPolicyOutageSuccessRate && hardFailureRate >= gptFirstEventPolicyOutageFailureRate {
		o.prepareCandidateLocked(policy, gptFirstEventPolicyStateOutage, "collective_outage")
		policy.enterWindows++
		policy.recoveryWindows = 0
		policy.decisionReason = "collective_outage_candidate"
		if policy.enterWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
			o.transitionLocked(policy, gptFirstEventPolicyStateOutage, "collective_outage", now, window.latestSequence)
		}
		return
	}

	if policy.name == gptFirstEventPolicyStateOutage {
		if window.Eligible < gptFirstEventPolicyRecoveryMinSamples {
			o.clearCandidateLocked(policy)
			policy.decisionReason = "insufficient_recovery_samples"
			return
		}
		if hardFailureRate < gptFirstEventPolicyOutageFailureRate && window.timeoutRate() >= gptFirstEventPolicyEscalationRate {
			o.prepareCandidateLocked(policy, gptFirstEventPolicyStateSlow30, "hard_outage_cleared_to_slow")
			policy.recoveryWindows++
			policy.enterWindows = 0
			policy.decisionReason = "hard_outage_cleared_to_slow_candidate"
			if policy.recoveryWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
				o.transitionLocked(policy, gptFirstEventPolicyStateSlow30, "hard_outage_cleared_to_slow", now, window.latestSequence)
			}
			return
		}
		if window.successRate(25*time.Second) >= gptFirstEventPolicyEscalationRate {
			next := gptFirstEventPolicyStateSlow30
			if window.successRate(25*time.Second) >= gptFirstEventPolicyRecoveryRate {
				next = gptFirstEventPolicyStateNormal
			}
			o.prepareCandidateLocked(policy, next, "outage_recovered")
			policy.recoveryWindows++
			policy.enterWindows = 0
			policy.decisionReason = "outage_recovery_candidate"
			if policy.recoveryWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
				o.transitionLocked(policy, next, "outage_recovered", now, window.latestSequence)
			}
			return
		}
		o.clearCandidateLocked(policy)
		policy.decisionReason = "collective_outage"
		return
	}

	if nextState, lowerTimeout, upperTimeout, ok := gptFirstEventHigherState(policy.name); ok {
		rate := window.successRate(upperTimeout)
		lateGain := window.lateSuccessRate(lowerTimeout, upperTimeout)
		shouldEscalate := false
		reason := "stable"
		if policy.name == gptFirstEventPolicyStateNormal {
			shouldEscalate = rate < gptFirstEventPolicySlowRate && window.timeoutRate() >= gptFirstEventPolicySlowTimeoutRate
			reason = "first_event_rate_below_90_percent"
		} else {
			shouldEscalate = rate < gptFirstEventPolicyEscalationRate &&
				(lateGain >= gptFirstEventPolicyLateSuccessGain || window.timeoutRate() >= gptFirstEventPolicyEscalationRate)
			if window.timeoutRate() >= gptFirstEventPolicyEscalationRate {
				reason = "local_timeout_pressure"
			} else {
				reason = "late_success_proves_more_wait_helps"
			}
		}
		if shouldEscalate {
			o.prepareCandidateLocked(policy, nextState, reason)
			policy.enterWindows++
			policy.recoveryWindows = 0
			policy.decisionReason = reason
			if policy.enterWindows >= gptFirstEventPolicyEnterWindows && o.canTransitionLocked(policy, now) {
				o.transitionLocked(policy, nextState, reason, now, window.latestSequence)
			}
			return
		}
	}

	if lowerState, lowerTimeout, ok := gptFirstEventLowerState(policy.name); ok {
		if window.Eligible < gptFirstEventPolicyRecoveryMinSamples {
			o.clearCandidateLocked(policy)
			policy.decisionReason = "insufficient_recovery_samples"
			return
		}
		if window.successRate(lowerTimeout) >= gptFirstEventPolicyRecoveryRate {
			o.prepareCandidateLocked(policy, lowerState, "sustained_recovery")
			policy.recoveryWindows++
			policy.enterWindows = 0
			policy.decisionReason = "recovery_candidate"
			if policy.recoveryWindows >= gptFirstEventPolicyRecoveryWindows && o.canTransitionLocked(policy, now) {
				o.transitionLocked(policy, lowerState, "sustained_recovery", now, window.latestSequence)
			}
			return
		}
	}

	o.clearCandidateLocked(policy)
	policy.decisionReason = "stable"
}

func (o *gptFirstEventObserver) prepareCandidateLocked(policy *gptFirstEventPolicyState, target, reason string) {
	if policy == nil || (policy.candidateTarget == target && policy.candidateReason == reason) {
		return
	}
	o.clearCandidateLocked(policy)
	policy.candidateTarget = target
	policy.candidateReason = reason
}

func (o *gptFirstEventObserver) clearCandidateLocked(policy *gptFirstEventPolicyState) {
	if policy == nil {
		return
	}
	policy.candidateTarget = ""
	policy.candidateReason = ""
	policy.enterWindows = 0
	policy.recoveryWindows = 0
	policy.lowSampleTimeoutWindows = 0
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
	policy.updatedAt = now
	policy.lastTransitionSequence = sequence
	o.clearCandidateLocked(policy)
	policy.lastEvaluationLowSample = false
}

func validGPTFirstEventPolicyState(state string) bool {
	switch strings.TrimSpace(state) {
	case gptFirstEventPolicyStateNormal,
		gptFirstEventPolicyStateSlow30,
		gptFirstEventPolicyStateSlow40,
		gptFirstEventPolicyStateSlow50,
		gptFirstEventPolicyStateOutage:
		return true
	default:
		return false
	}
}

func (o *gptFirstEventObserver) snapshotLocked(model string, now time.Time, observationSequence uint64) GPTFirstEventPolicySnapshot {
	window := o.windowLocked(model, now)
	// Never apply the aggregate "*" state to a concrete model. Models have
	// materially different latency distributions, and a hot/slow model must not
	// change the deadline or retry shape of an unrelated cold-start model.
	decisionSource := model
	policy := o.policyLocked(model)
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
		HardFailureRate:          window.hardFailureRate(),
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
		UsedGlobalFallback:       false,
		MinimumSamples:           gptFirstEventPolicyEscalationMinSamples,
		RecoveryMinimumSamples:   gptFirstEventPolicyRecoveryMinSamples,
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
		RecoveryMinimumSamples:   gptFirstEventPolicyRecoveryMinSamples,
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
	snapshot := m.gptFirstEventObserver.snapshot(model, time.Now())
	if snapshot.Transitioned {
		logGPTFirstEventPolicyTransition(context.Background(), snapshot)
		m.persistGPTFirstEventPolicyUpdate(context.Background(), snapshot)
	}
	return snapshot
}

func (m *Manager) GPTFirstEventDailySnapshots(model string, days int) []GPTFirstEventDailySnapshot {
	if m == nil || m.gptFirstEventObserver == nil {
		return []GPTFirstEventDailySnapshot{}
	}
	return m.gptFirstEventObserver.dailySnapshot(model, days, time.Now())
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
		"hard_failure_rate":           roundPolicyRate(snapshot.HardFailureRate),
		"timeout_count":               snapshot.Timeouts,
		"upstream_5xx_count":          snapshot.Upstream5xx,
		"network_failure_count":       snapshot.NetworkFailures,
		"used_global_fallback":        snapshot.UsedGlobalFallback,
	}
	addRequestAttemptLogFields(ctx, fields)
	logEntryWithRequestID(ctx).WithFields(fields).Info("gpt_first_event_observation")
	if snapshot.Transitioned {
		logGPTFirstEventPolicyTransition(ctx, snapshot)
	}
	if snapshot.stateCheckpointed {
		checkpointFields := log.Fields{
			"event":           "gpt_first_event_policy_checkpoint",
			"model":           canonicalModelKey(model),
			"policy_state":    snapshot.PolicyState,
			"decision_reason": snapshot.DecisionReason,
		}
		logEntryWithRequestID(ctx).WithFields(checkpointFields).Info("gpt_first_event_policy_checkpoint")
	}
	if snapshot.Transitioned || snapshot.stateCheckpointed {
		m.persistGPTFirstEventPolicyUpdate(ctx, snapshot)
	}
}

func logGPTFirstEventPolicyTransition(ctx context.Context, snapshot GPTFirstEventPolicySnapshot) {
	transitionFields := log.Fields{
		"event":                       "gpt_first_event_policy_transition",
		"model":                       canonicalModelKey(snapshot.Model),
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
		"hard_failure_rate":           roundPolicyRate(snapshot.HardFailureRate),
	}
	logEntryWithRequestID(ctx).WithFields(transitionFields).Warn("gpt_first_event_policy_transition")
}

func (m *Manager) persistGPTFirstEventPolicyUpdate(ctx context.Context, snapshot GPTFirstEventPolicySnapshot) {
	if m == nil || (!snapshot.Transitioned && !snapshot.stateCheckpointed) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	// Policy updates are infrequent, but persistence must not add disk or custom
	// store latency to the customer request. The bounded background write still
	// survives request cancellation while preventing an unbounded store call.
	go func() {
		persistCtx, cancel := context.WithTimeout(ctx, gptFirstEventPolicyPersistTimeout)
		defer cancel()
		m.persistGPTFirstEventPolicyStates(persistCtx)
	}()
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
	network = !upstream5xx && isTransientNetworkError(err)
	if !upstream5xx && !network {
		return false, "", false, false
	}
	return true, gptFirstEventOutcomeFailure, upstream5xx, network
}

func roundPolicyRate(value float64) float64 {
	return math.Round(value*10000) / 10000
}
