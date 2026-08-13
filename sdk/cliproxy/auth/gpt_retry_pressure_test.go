package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func testGPTRouteAvailability(candidate, eligible, blocked []string) gptRouteAvailabilitySnapshot {
	snapshot := gptRouteAvailabilitySnapshot{
		candidateRoutes: make(map[string]struct{}, len(candidate)),
		eligibleRoutes:  make(map[string]struct{}, len(eligible)),
		blockedRoutes:   make(map[string]string, len(blocked)),
		breakerRoutes:   make(map[string]int),
	}
	for _, route := range candidate {
		snapshot.candidateRoutes[route] = struct{}{}
	}
	for _, route := range eligible {
		snapshot.eligibleRoutes[route] = struct{}{}
	}
	for _, route := range blocked {
		snapshot.blockedRoutes[route] = "other"
	}
	return snapshot
}

func evaluateGPTRetryPressure(t *testing.T, controller *gptRetryPressureController, model string, availability gptRouteAvailabilitySnapshot, now time.Time) gptRetryPressureSnapshot {
	t.Helper()
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.evaluateLocked(model, availability, now)
}

func TestGPTRetryPressureRequiresThreeIndependentDegradedRoutes(t *testing.T) {
	controller := newGPTRetryPressureController()
	now := time.Now()
	availability := testGPTRouteAvailability(
		[]string{"route-a", "route-b", "route-c"},
		[]string{"route-a", "route-b", "route-c"},
		nil,
	)

	for _, route := range []string{"route-a", "route-b"} {
		controller.observe("gpt-5.6-sol", route, true, now)
		controller.observe("gpt-5.6-sol", route, true, now.Add(time.Millisecond))
	}
	if snapshot := evaluateGPTRetryPressure(t, controller, "gpt-5.6-sol", availability, now.Add(time.Second)); snapshot.State != gptRetryPressureStateNormal {
		t.Fatalf("two degraded routes must not enter congestion, got %+v", snapshot)
	}

	controller.observe("gpt-5.6-sol", "route-c", true, now)
	controller.observe("gpt-5.6-sol", "route-c", true, now.Add(time.Millisecond))
	snapshot := evaluateGPTRetryPressure(t, controller, "gpt-5.6-sol", availability, now.Add(time.Second))
	if snapshot.State != gptRetryPressureStateCongested {
		t.Fatalf("three degraded routes must enter congestion, got %+v", snapshot)
	}
	if snapshot.Reason != "multi_route_degradation" || snapshot.DegradedRoutes != 3 {
		t.Fatalf("unexpected congestion evidence: %+v", snapshot)
	}
}

func TestGPTRetryPressureRoutePoolExhaustionAndRecoveryHysteresis(t *testing.T) {
	controller := newGPTRetryPressureController()
	now := time.Now()
	congestedAvailability := testGPTRouteAvailability(
		[]string{"route-a", "route-b", "route-c"},
		[]string{"route-c"},
		[]string{"route-a", "route-b"},
	)
	snapshot := evaluateGPTRetryPressure(t, controller, "gpt-5.6-sol", congestedAvailability, now)
	if snapshot.State != gptRetryPressureStateCongested || snapshot.Reason != "route_pool_exhausted" {
		t.Fatalf("near-empty route pool must enter congestion, got %+v", snapshot)
	}

	healthyAvailability := testGPTRouteAvailability(
		[]string{"route-a", "route-b", "route-c"},
		[]string{"route-a", "route-b", "route-c"},
		nil,
	)
	snapshot = evaluateGPTRetryPressure(t, controller, "gpt-5.6-sol", healthyAvailability, now.Add(time.Second))
	if snapshot.State != gptRetryPressureStateCongested {
		t.Fatalf("congestion must not clear immediately, got %+v", snapshot)
	}
	snapshot = evaluateGPTRetryPressure(t, controller, "gpt-5.6-sol", healthyAvailability, now.Add(time.Second+gptRetryPressureRecoveryHold))
	if snapshot.State != gptRetryPressureStateNormal || snapshot.Reason != "routes_recovered" {
		t.Fatalf("healthy hold must clear congestion, got %+v", snapshot)
	}
}

func TestGPTRetryPressurePermitLimitTracksEligibleRoutes(t *testing.T) {
	controller := newGPTRetryPressureController()
	availability := testGPTRouteAvailability(
		[]string{"route-a", "route-b", "route-c"},
		[]string{"route-c"},
		[]string{"route-a", "route-b"},
	)
	availabilityFn := func(time.Time) gptRouteAvailabilitySnapshot { return availability }

	releaseFirst, first, err := controller.acquire(context.Background(), "gpt-5.6-luna", availabilityFn)
	if err != nil {
		t.Fatalf("first retry permit failed: %v", err)
	}
	if !first.Acquired || first.PermitLimit != 1 || first.InFlightRetries != 1 {
		t.Fatalf("one eligible route must allow one retry, got %+v", first)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		release, _, errAcquire := controller.acquire(ctx, "gpt-5.6-luna", availabilityFn)
		if release != nil {
			release()
		}
		result <- errAcquire
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if errAcquire := <-result; !errors.Is(errAcquire, context.Canceled) {
		t.Fatalf("second concurrent retry must wait for the only permit, got %v", errAcquire)
	}
	releaseFirst()

	controller.mu.Lock()
	state := controller.models[canonicalModelKey("gpt-5.6-luna")]
	inFlight, waiters := state.inFlight, state.waiters
	controller.mu.Unlock()
	if inFlight != 0 || waiters != 0 {
		t.Fatalf("cancelled waiter or release leaked capacity: in_flight=%d waiters=%d", inFlight, waiters)
	}
}

func TestGPTRetryPressureRejectsMissingRouteWithoutQueueing(t *testing.T) {
	controller := newGPTRetryPressureController()
	startedAt := time.Now()
	release, snapshot, err := controller.acquire(context.Background(), "gpt-5.6-sol", func(time.Time) gptRouteAvailabilitySnapshot {
		return testGPTRouteAvailability(nil, nil, nil)
	})
	if release != nil {
		release()
	}
	if err == nil || !snapshot.Rejected {
		t.Fatalf("missing route must be rejected: snapshot=%+v err=%v", snapshot, err)
	}
	if elapsed := time.Since(startedAt); elapsed >= gptRetryPressureRecheckInterval {
		t.Fatalf("missing route must not consume the congestion queue, elapsed=%s", elapsed)
	}
}

func TestGPTRetryPressureAllowsTwoSolRetriesAndWakesWaiter(t *testing.T) {
	controller := newGPTRetryPressureController()
	now := time.Now()
	for _, route := range []string{"route-a", "route-b", "route-c"} {
		controller.observe("gpt-5.6-sol", route, true, now)
		controller.observe("gpt-5.6-sol", route, true, now.Add(time.Millisecond))
	}
	availability := testGPTRouteAvailability(
		[]string{"route-a", "route-b", "route-c"},
		[]string{"route-a", "route-b"},
		[]string{"route-c"},
	)
	availabilityFn := func(time.Time) gptRouteAvailabilitySnapshot { return availability }

	releaseFirst, first, err := controller.acquire(context.Background(), "gpt-5.6-sol", availabilityFn)
	if err != nil {
		t.Fatalf("first permit failed: %v", err)
	}
	releaseSecond, second, err := controller.acquire(context.Background(), "gpt-5.6-sol", availabilityFn)
	if err != nil {
		t.Fatalf("second permit failed: %v", err)
	}
	if first.PermitLimit != 2 || second.InFlightRetries != 2 {
		t.Fatalf("two eligible routes must allow two retries: first=%+v second=%+v", first, second)
	}

	woken := make(chan error, 1)
	go func() {
		release, snapshot, errAcquire := controller.acquire(context.Background(), "gpt-5.6-sol", availabilityFn)
		if errAcquire == nil {
			if !snapshot.Acquired {
				errAcquire = errors.New("waiter woke without a permit")
			}
			release()
		}
		woken <- errAcquire
	}()
	time.Sleep(20 * time.Millisecond)
	releaseFirst()
	select {
	case errAcquire := <-woken:
		if errAcquire != nil {
			t.Fatalf("waiting retry did not acquire released permit: %v", errAcquire)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting retry was not woken by permit release")
	}
	releaseSecond()
}

func TestGPTRetryPressureModelIsolationAndFirstAttemptBypass(t *testing.T) {
	controller := newGPTRetryPressureController()
	now := time.Now()
	availability := testGPTRouteAvailability(
		[]string{"route-a", "route-b", "route-c"},
		[]string{"route-a", "route-b", "route-c"},
		nil,
	)
	for _, route := range []string{"route-a", "route-b", "route-c"} {
		controller.observe("gpt-5.6-sol", route, true, now)
		controller.observe("gpt-5.6-sol", route, true, now.Add(time.Millisecond))
	}
	if snapshot := evaluateGPTRetryPressure(t, controller, "gpt-5.6-luna", availability, now.Add(time.Second)); snapshot.State != gptRetryPressureStateNormal {
		t.Fatalf("Sol degradation must not congest Luna: %+v", snapshot)
	}
	release, snapshot, err := controller.acquire(context.Background(), "gpt-5.6-luna", func(time.Time) gptRouteAvailabilitySnapshot {
		return availability
	})
	if err != nil {
		t.Fatalf("normal-state retry permit failed: %v", err)
	}
	if snapshot.State != gptRetryPressureStateNormal || !snapshot.Acquired || snapshot.PermitLimit != 3 {
		t.Fatalf("normal-state retries must still acquire a model permit: %+v", snapshot)
	}
	release()
	traceWithNormalPressure := &requestAttemptTrace{}
	traceWithNormalPressure.configureGPTFirstEventPolicy(GPTFirstEventPolicySnapshot{MaxChannels: 8, MaxRounds: 3})
	traceWithNormalPressure.recordGPTRetryPressure(snapshot, nil)
	_, normalMaxRounds := traceWithNormalPressure.gptFirstEventRetryLimits()
	if normalMaxRounds != 2 {
		t.Fatalf("a permitted retry must cap the request at two rounds even before congestion, got %d", normalMaxRounds)
	}

	trace := &requestAttemptTrace{}
	if shouldAcquireGPTRetryPermit(trace) {
		t.Fatal("first attempt must bypass retry pressure permits")
	}
	trace.nextAttempt("")
	if !shouldAcquireGPTRetryPermit(trace) {
		t.Fatal("subsequent attempts must require retry pressure evaluation")
	}
	trace.configureGPTFirstEventPolicy(GPTFirstEventPolicySnapshot{MaxChannels: 8, MaxRounds: 3})
	trace.recordGPTRetryPressure(gptRetryPressureSnapshot{State: gptRetryPressureStateCongested}, nil)
	maxChannels, maxRounds := trace.gptFirstEventRetryLimits()
	if maxChannels != 8 || maxRounds != 2 {
		t.Fatalf("congestion must retain channels but cap the request at two rounds, got channels=%d rounds=%d", maxChannels, maxRounds)
	}
}

func TestClassifyGPTRetryPressureObservation(t *testing.T) {
	requestFailure := &failurecontract.Failure{
		Kind:       failurecontract.InvalidRequest,
		Scope:      failurecontract.ScopeRequest,
		HTTPStatus: http.StatusBadRequest,
	}
	providerFailure := &failurecontract.Failure{
		Kind:       failurecontract.ProviderUnavailable,
		Scope:      failurecontract.ScopeProvider,
		HTTPStatus: http.StatusServiceUnavailable,
	}
	tests := []struct {
		name        string
		deliverable bool
		timedOut    bool
		err         error
		eligible    bool
		failed      bool
	}{
		{name: "deliverable", deliverable: true, eligible: true},
		{name: "first event timeout", timedOut: true, eligible: true, failed: true},
		{name: "provider 503", err: providerFailure, eligible: true, failed: true},
		{name: "request 400", err: requestFailure},
		{name: "model 429", err: &Error{HTTPStatus: http.StatusTooManyRequests}, eligible: true, failed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eligible, failed := classifyGPTRetryPressureObservation(tc.deliverable, tc.timedOut, tc.err)
			if eligible != tc.eligible || failed != tc.failed {
				t.Fatalf("got eligible=%v failed=%v, want eligible=%v failed=%v", eligible, failed, tc.eligible, tc.failed)
			}
		})
	}
}
