package auth

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

const (
	gptRetryPressureStateNormal    = "normal"
	gptRetryPressureStateCongested = "congested"

	gptRetryPressureWindow             = 2 * time.Minute
	gptRetryPressureRecoveryHold       = time.Minute
	gptRetryPressureQueueWait          = 5 * time.Second
	gptRetryPressureRecheckInterval    = 250 * time.Millisecond
	gptRetryPressureRetryAfter         = time.Second
	gptRetryPressureRouteMinSamples    = 4
	gptRetryPressureRouteFailureRate   = 0.50
	gptRetryPressureRouteFailureStreak = 2
	gptRetryPressureMinDegradedRoutes  = 3
	gptRetryPressureMaxWaitersPerModel = 64
	gptRetryPressureMaxSamplesPerRoute = 256
)

type gptRetryPressureSample struct {
	at     time.Time
	failed bool
}

type gptRetryPressureModelState struct {
	state        string
	reason       string
	previous     string
	transitioned bool
	enteredAt    time.Time
	healthySince time.Time
	routes       map[string][]gptRetryPressureSample
	inFlight     int
	waiters      int
	changed      chan struct{}
}

type gptRetryPressureController struct {
	mu     sync.Mutex
	models map[string]*gptRetryPressureModelState
}

type gptRetryPressureSnapshot struct {
	Model           string
	State           string
	Reason          string
	PreviousState   string
	Transitioned    bool
	CandidateRoutes int
	EligibleRoutes  int
	BlockedRoutes   int
	DegradedRoutes  int
	PermitLimit     int
	InFlightRetries int
	WaitingRetries  int
	Wait            time.Duration
	Acquired        bool
	Rejected        bool
}

type gptRouteAvailabilitySnapshot struct {
	candidateRoutes  map[string]struct{}
	eligibleRoutes   map[string]struct{}
	blockedRoutes    map[string]string
	breakerRoutes    map[string]int
	earliestRecovery time.Time
}

func newGPTRetryPressureController() *gptRetryPressureController {
	return &gptRetryPressureController{models: make(map[string]*gptRetryPressureModelState)}
}

func (c *gptRetryPressureController) modelStateLocked(model string) *gptRetryPressureModelState {
	state := c.models[model]
	if state == nil {
		state = &gptRetryPressureModelState{
			state:   gptRetryPressureStateNormal,
			routes:  make(map[string][]gptRetryPressureSample),
			changed: make(chan struct{}),
		}
		c.models[model] = state
	}
	if state.changed == nil {
		state.changed = make(chan struct{})
	}
	return state
}

func notifyGPTRetryPressureWaitersLocked(state *gptRetryPressureModelState) {
	if state == nil {
		return
	}
	if state.changed != nil {
		close(state.changed)
	}
	state.changed = make(chan struct{})
}

func (c *gptRetryPressureController) observe(model, routeKey string, failed bool, now time.Time) {
	if c == nil {
		return
	}
	model = canonicalModelKey(model)
	routeKey = strings.TrimSpace(routeKey)
	if model == "" || routeKey == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	c.mu.Lock()
	state := c.modelStateLocked(model)
	samples := append(state.routes[routeKey], gptRetryPressureSample{at: now, failed: failed})
	if len(samples) > gptRetryPressureMaxSamplesPerRoute {
		samples = append([]gptRetryPressureSample(nil), samples[len(samples)-gptRetryPressureMaxSamplesPerRoute:]...)
	}
	state.routes[routeKey] = samples
	notifyGPTRetryPressureWaitersLocked(state)
	c.mu.Unlock()
}

func (c *gptRetryPressureController) degradedRoutesLocked(state *gptRetryPressureModelState, now time.Time) map[string]struct{} {
	degraded := make(map[string]struct{})
	if state == nil {
		return degraded
	}
	cutoff := now.Add(-gptRetryPressureWindow)
	for routeKey, samples := range state.routes {
		first := 0
		for first < len(samples) && samples[first].at.Before(cutoff) {
			first++
		}
		if first > 0 {
			samples = append([]gptRetryPressureSample(nil), samples[first:]...)
		}
		if len(samples) == 0 {
			delete(state.routes, routeKey)
			continue
		}
		state.routes[routeKey] = samples
		failures := 0
		failureStreak := 0
		for i, sample := range samples {
			if sample.failed {
				failures++
			}
			if i >= len(samples)-gptRetryPressureRouteFailureStreak {
				if sample.failed {
					failureStreak++
				} else {
					failureStreak = 0
				}
			}
		}
		failureRate := float64(failures) / float64(len(samples))
		if (len(samples) >= gptRetryPressureRouteMinSamples && failureRate >= gptRetryPressureRouteFailureRate) ||
			failureStreak >= gptRetryPressureRouteFailureStreak {
			degraded[routeKey] = struct{}{}
		}
	}
	return degraded
}

func (c *gptRetryPressureController) evaluateLocked(model string, availability gptRouteAvailabilitySnapshot, now time.Time) gptRetryPressureSnapshot {
	state := c.modelStateLocked(model)
	observedDegraded := c.degradedRoutesLocked(state, now)
	degraded := make(map[string]struct{}, len(observedDegraded)+len(availability.blockedRoutes))
	for routeKey := range observedDegraded {
		if _, candidate := availability.candidateRoutes[routeKey]; candidate {
			degraded[routeKey] = struct{}{}
		}
	}
	for routeKey := range availability.blockedRoutes {
		degraded[routeKey] = struct{}{}
	}

	candidateCount := len(availability.candidateRoutes)
	eligibleCount := len(availability.eligibleRoutes)
	blockedCount := len(availability.blockedRoutes)
	degradedCount := len(degraded)
	reason := ""
	congested := candidateCount >= gptRetryPressureMinDegradedRoutes && degradedCount >= gptRetryPressureMinDegradedRoutes
	if congested {
		reason = "multi_route_degradation"
	}
	// A retry reaches this evaluation only after the request's unrestricted
	// first attempt failed. Two blocked routes plus one remaining eligible route
	// therefore represent three independently degraded routes for that request,
	// even before the remaining route has enough window samples.
	if candidateCount >= gptRetryPressureMinDegradedRoutes && eligibleCount <= 1 && blockedCount >= candidateCount-1 {
		congested = true
		reason = "route_pool_exhausted"
	}

	state.transitioned = false
	state.previous = ""
	if state.state == "" {
		state.state = gptRetryPressureStateNormal
	}
	if congested {
		state.healthySince = time.Time{}
		state.reason = reason
		if state.state != gptRetryPressureStateCongested {
			state.previous = state.state
			state.state = gptRetryPressureStateCongested
			state.enteredAt = now
			state.transitioned = true
			notifyGPTRetryPressureWaitersLocked(state)
		}
	} else if state.state == gptRetryPressureStateCongested {
		if state.healthySince.IsZero() {
			state.healthySince = now
		}
		if now.Sub(state.healthySince) >= gptRetryPressureRecoveryHold {
			state.previous = state.state
			state.state = gptRetryPressureStateNormal
			state.reason = "routes_recovered"
			state.enteredAt = time.Time{}
			state.healthySince = time.Time{}
			state.transitioned = true
			notifyGPTRetryPressureWaitersLocked(state)
		}
	} else {
		state.reason = ""
		state.healthySince = time.Time{}
	}

	return gptRetryPressureSnapshot{
		Model:           model,
		State:           state.state,
		Reason:          state.reason,
		PreviousState:   state.previous,
		Transitioned:    state.transitioned,
		CandidateRoutes: candidateCount,
		EligibleRoutes:  eligibleCount,
		BlockedRoutes:   blockedCount,
		DegradedRoutes:  degradedCount,
		PermitLimit:     eligibleCount,
		InFlightRetries: state.inFlight,
		WaitingRetries:  state.waiters,
	}
}

func (c *gptRetryPressureController) acquire(ctx context.Context, model string, availability func(time.Time) gptRouteAvailabilitySnapshot) (func(), gptRetryPressureSnapshot, error) {
	if c == nil || availability == nil {
		return func() {}, gptRetryPressureSnapshot{State: gptRetryPressureStateNormal}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	model = canonicalModelKey(model)
	startedAt := time.Now()
	deadline := startedAt.Add(gptRetryPressureQueueWait)
	waiterRegistered := false
	var transition gptRetryPressureSnapshot
	defer func() {
		if waiterRegistered {
			c.unregisterWaiter(model)
		}
	}()

	for {
		now := time.Now()
		currentAvailability := availability(now)
		c.mu.Lock()
		snapshot := c.evaluateLocked(model, currentAvailability, now)
		state := c.modelStateLocked(model)
		if snapshot.Transitioned {
			transition = snapshot
		}
		if snapshot.CandidateRoutes == 0 {
			snapshot.Rejected = true
			snapshot.Wait = time.Since(startedAt)
			c.mu.Unlock()
			return nil, snapshot, newGPTRetryPressureLimitedError()
		}
		if snapshot.PermitLimit > 0 && state.inFlight < snapshot.PermitLimit {
			state.inFlight++
			snapshot.InFlightRetries = state.inFlight
			snapshot.Acquired = true
			snapshot.Wait = time.Since(startedAt)
			if transition.Transitioned {
				snapshot.Transitioned = true
				snapshot.PreviousState = transition.PreviousState
			}
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() { c.release(model) })
			}, snapshot, nil
		}
		if !waiterRegistered {
			if state.waiters >= gptRetryPressureMaxWaitersPerModel {
				snapshot.Rejected = true
				snapshot.Wait = time.Since(startedAt)
				c.mu.Unlock()
				return nil, snapshot, newGPTRetryPressureLimitedError()
			}
			state.waiters++
			waiterRegistered = true
			snapshot.WaitingRetries = state.waiters
		}
		changed := state.changed
		c.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			snapshot.Rejected = true
			snapshot.Wait = time.Since(startedAt)
			return nil, snapshot, newGPTRetryPressureLimitedError()
		}
		waitFor := remaining
		if waitFor > gptRetryPressureRecheckInterval {
			waitFor = gptRetryPressureRecheckInterval
		}
		timer := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, snapshot, ctx.Err()
		case <-changed:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			if time.Now().Before(deadline) {
				continue
			}
			snapshot.Rejected = true
			snapshot.Wait = time.Since(startedAt)
			return nil, snapshot, newGPTRetryPressureLimitedError()
		}
	}
}

func (c *gptRetryPressureController) unregisterWaiter(model string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	state := c.models[canonicalModelKey(model)]
	if state != nil && state.waiters > 0 {
		state.waiters--
	}
	c.mu.Unlock()
}

func (c *gptRetryPressureController) release(model string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	state := c.models[canonicalModelKey(model)]
	if state != nil && state.inFlight > 0 {
		state.inFlight--
		notifyGPTRetryPressureWaitersLocked(state)
	}
	c.mu.Unlock()
}

func newGPTRetryPressureLimitedError() error {
	retryAfter := gptRetryPressureRetryAfter
	return &failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeModel,
		HTTPStatus:    http.StatusServiceUnavailable,
		OuterStatus:   http.StatusServiceUnavailable,
		ProviderCode:  "gpt_retry_pressure_limited",
		SemanticCode:  "gpt_retry_pressure_limited",
		SemanticType:  "server_error",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		Retryable:     true,
		RetryAfter:    &retryAfter,
		PublicMessage: "GPT model routes are congested; retry shortly",
	}
}

func classifyGPTRetryPressureObservation(deliverable, timedOut bool, err error) (eligible, failed bool) {
	if deliverable {
		return true, false
	}
	if timedOut {
		return true, true
	}
	if err == nil || isRequestInvalidError(err) {
		return false, false
	}
	if failure, ok := failurecontract.As(err); ok && failure.Scope == failurecontract.ScopeRequest {
		return false, false
	}
	status := statusCodeFromError(err)
	switch {
	case status == http.StatusUnauthorized,
		status == http.StatusForbidden,
		status == http.StatusRequestTimeout,
		status == http.StatusTooManyRequests,
		status >= 500 && status <= 599,
		isTransientNetworkError(err):
		return true, true
	default:
		return false, false
	}
}

func (m *Manager) recordGPTRetryPressureAttempt(ctx context.Context, model, routeKey string, deliverable bool, err error) {
	if m == nil || m.gptRetryPressure == nil || !isGPTRequestRoute(ctx, nil, model) {
		return
	}
	timedOut := strings.EqualFold(strings.TrimSpace(errorCodeFromError(err)), "gpt_first_event_timeout")
	eligible, failed := classifyGPTRetryPressureObservation(deliverable, timedOut, err)
	if !eligible {
		return
	}
	m.gptRetryPressure.observe(model, routeKey, failed, time.Now())
}

func shouldAcquireGPTRetryPermit(trace *requestAttemptTrace) bool {
	return trace != nil && trace.attemptCount() > 0
}

func (m *Manager) acquireGPTRetryPermit(ctx context.Context, providers []string, model string) (func(), gptRetryPressureSnapshot, error) {
	if m == nil || m.gptRetryPressure == nil {
		return func() {}, gptRetryPressureSnapshot{State: gptRetryPressureStateNormal}, nil
	}
	release, snapshot, err := m.gptRetryPressure.acquire(ctx, model, func(now time.Time) gptRouteAvailabilitySnapshot {
		return m.gptRouteAvailabilitySnapshot(providers, model, now)
	})
	fields := log.Fields{
		"event":                 "gpt_retry_pressure",
		"model":                 canonicalModelKey(model),
		"retry_pressure_state":  snapshot.State,
		"retry_pressure_reason": snapshot.Reason,
		"candidate_route_count": snapshot.CandidateRoutes,
		"eligible_route_count":  snapshot.EligibleRoutes,
		"blocked_route_count":   snapshot.BlockedRoutes,
		"degraded_route_count":  snapshot.DegradedRoutes,
		"retry_permit_limit":    snapshot.PermitLimit,
		"retry_in_flight":       snapshot.InFlightRetries,
		"retry_waiters":         snapshot.WaitingRetries,
		"retry_permit_wait_ms":  snapshot.Wait.Milliseconds(),
		"retry_permit_acquired": snapshot.Acquired,
		"retry_permit_rejected": snapshot.Rejected,
	}
	addRequestAttemptLogFields(ctx, fields)
	entry := logEntryWithRequestID(ctx).WithFields(fields)
	if err != nil {
		entry.Warn("gpt_retry_pressure")
	} else {
		entry.Info("gpt_retry_pressure")
	}
	if snapshot.Transitioned {
		transitionFields := log.Fields{
			"event":                 "gpt_retry_pressure_transition",
			"model":                 canonicalModelKey(model),
			"previous_state":        snapshot.PreviousState,
			"retry_pressure_state":  snapshot.State,
			"retry_pressure_reason": snapshot.Reason,
			"candidate_route_count": snapshot.CandidateRoutes,
			"eligible_route_count":  snapshot.EligibleRoutes,
			"blocked_route_count":   snapshot.BlockedRoutes,
			"degraded_route_count":  snapshot.DegradedRoutes,
		}
		logEntryWithRequestID(ctx).WithFields(transitionFields).Warn("gpt_retry_pressure_transition")
	}
	return release, snapshot, err
}

func (m *Manager) gptRouteAvailabilitySnapshot(providers []string, model string, now time.Time) gptRouteAvailabilitySnapshot {
	snapshot := gptRouteAvailabilitySnapshot{
		candidateRoutes: make(map[string]struct{}),
		eligibleRoutes:  make(map[string]struct{}),
		blockedRoutes:   make(map[string]string),
		breakerRoutes:   make(map[string]int),
	}
	if m == nil {
		return snapshot
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range normalizeProviderKeys(providers) {
		providerSet[provider] = struct{}{}
	}
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || executorKeyForProviderSet(auth, providerSet, m.executors) == "" {
			continue
		}
		if strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		routeKey := routingChannelBaseKey(auth)
		if routeKey == "" {
			continue
		}
		snapshot.candidateRoutes[routeKey] = struct{}{}
		checkModel := m.selectionModelForAuth(auth, model)
		blocked, reason, next := isAuthBlockedForModelRoute(auth, checkModel, now, true)
		if !blocked {
			snapshot.eligibleRoutes[routeKey] = struct{}{}
			continue
		}
		if _, exists := snapshot.blockedRoutes[routeKey]; !exists {
			snapshot.blockedRoutes[routeKey] = blockReasonLabel(reason)
		}
		if !next.IsZero() && next.After(now) && (snapshot.earliestRecovery.IsZero() || next.Before(snapshot.earliestRecovery)) {
			snapshot.earliestRecovery = next
		}
		health := resolveHealthState(auth, checkModel)
		if health.BreakerState == HealthBreakerOpen && health.OpenUntil.After(now) {
			snapshot.breakerRoutes[routeKey] = health.LastStatusCode
			if snapshot.earliestRecovery.IsZero() || health.OpenUntil.Before(snapshot.earliestRecovery) {
				snapshot.earliestRecovery = health.OpenUntil
			}
		}
	}
	m.mu.RUnlock()
	for routeKey := range snapshot.eligibleRoutes {
		delete(snapshot.blockedRoutes, routeKey)
		delete(snapshot.breakerRoutes, routeKey)
	}
	return snapshot
}

func (snapshot gptRouteAvailabilitySnapshot) logFields(now time.Time) log.Fields {
	fields := log.Fields{
		"candidate_route_count": len(snapshot.candidateRoutes),
		"eligible_route_count":  len(snapshot.eligibleRoutes),
		"blocked_route_count":   len(snapshot.blockedRoutes),
		"breaker_open_count":    len(snapshot.breakerRoutes),
	}
	if len(snapshot.blockedRoutes) > 0 {
		counts := make(map[string]int)
		for _, reason := range snapshot.blockedRoutes {
			counts[reason]++
		}
		fields["blocked_reasons"] = formatReasonCounts(counts)
	}
	if len(snapshot.breakerRoutes) > 0 {
		statuses := make(map[int]struct{}, len(snapshot.breakerRoutes))
		counts := make(map[string]int)
		for _, status := range snapshot.breakerRoutes {
			if status > 0 {
				statuses[status] = struct{}{}
			}
			reason := "status_unknown"
			if status > 0 {
				reason = "status_" + strconv.Itoa(status)
			}
			counts[reason]++
		}
		ordered := make([]int, 0, len(statuses))
		for status := range statuses {
			ordered = append(ordered, status)
		}
		sort.Ints(ordered)
		parts := make([]string, 0, len(ordered))
		for _, status := range ordered {
			parts = append(parts, strconv.Itoa(status))
		}
		fields["breaker_statuses"] = strings.Join(parts, ",")
		fields["breaker_reasons"] = formatReasonCounts(counts)
	}
	if !snapshot.earliestRecovery.IsZero() && snapshot.earliestRecovery.After(now) {
		fields["earliest_recovery_ms"] = snapshot.earliestRecovery.Sub(now).Milliseconds()
	}
	return fields
}
