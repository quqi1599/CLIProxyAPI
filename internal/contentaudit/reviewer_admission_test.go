package contentaudit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelReviewAdmissionChargesOnceForSharedFlightAndCache(t *testing.T) {
	var admitted, providerCalls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		providerCalls.Add(1)
		close(entered)
		<-release
		return ModelReviewResult{Decision: ModelReviewAllow, Confidence: .9}, nil
	}))
	controller.admit = func(context.Context) (bool, string, error) {
		admitted.Add(1)
		return true, "", nil
	}
	request := ModelReviewRequest{Text: "same complete task", TenantScope: "tenant", PolicyVersion: "v1"}
	outcomes := make(chan modelReviewOutcome, 2)
	go func() { outcomes <- controller.review(t.Context(), request) }()
	<-entered
	go func() { outcomes <- controller.review(t.Context(), request) }()
	waitReviewFlight(t, controller, 1, 2)
	close(release)
	for range 2 {
		outcome := <-outcomes
		if outcome.Decision != ModelReviewAllow || outcome.Fallback != "" {
			t.Fatalf("shared outcome = %#v", outcome)
		}
		if _, exists := outcome.StageLatenciesMS["admission"]; !exists {
			t.Fatal("admission stage timing missing")
		}
	}
	if outcome := controller.review(t.Context(), request); !outcome.CacheHit {
		t.Fatal("expected cache hit after shared result")
	}
	if admitted.Load() != 1 || providerCalls.Load() != 1 {
		t.Fatalf("admissions=%d provider=%d, want one each", admitted.Load(), providerCalls.Load())
	}
}

func TestModelReviewAdmissionDeniedCannotCallProviderOrTripCircuit(t *testing.T) {
	for _, test := range []struct {
		name, reason, fallback string
		err                    error
	}{
		{name: "daily", reason: "daily_budget_exhausted", fallback: "daily_budget_exhausted"},
		{name: "minute", reason: "minute_rate_limited", fallback: "minute_rate_limited"},
		{name: "storage", err: errors.New("private database connection information"), fallback: "budget_storage_error"},
		{name: "unknown_reason", reason: "private unexpected text", fallback: "budget_storage_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var admitted, calls atomic.Int32
			controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
				calls.Add(1)
				return ModelReviewResult{Decision: ModelReviewAllow}, nil
			}))
			controller.openUntil = time.Now().Add(-time.Second)
			controller.failures = 1
			controller.admit = func(context.Context) (bool, string, error) {
				admitted.Add(1)
				return false, test.reason, test.err
			}
			outcome := controller.review(t.Context(), ModelReviewRequest{Text: "synthetic"})
			if outcome.Decision != ModelReviewUncertain || outcome.Fallback != test.fallback || calls.Load() != 0 || admitted.Load() != 1 {
				t.Fatalf("outcome=%#v admissions=%d provider=%d", outcome, admitted.Load(), calls.Load())
			}
			controller.circuitMu.Lock()
			defer controller.circuitMu.Unlock()
			if controller.failures != 1 || controller.halfOpen {
				t.Fatal("quota denial changed circuit failure count or retained half-open permit")
			}
		})
	}
}

func TestModelReviewAdmissionSkipsCanceledQueuedAndSaturatedWork(t *testing.T) {
	var admissions, calls atomic.Int32
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		return ModelReviewResult{Decision: ModelReviewAllow}, nil
	}))
	controller.admit = func(context.Context) (bool, string, error) {
		admissions.Add(1)
		return true, "", nil
	}
	controller.semaphore <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outcomes := make(chan modelReviewOutcome, 2)
	go func() { outcomes <- controller.review(ctx, ModelReviewRequest{Text: "one"}) }()
	go func() { outcomes <- controller.review(ctx, ModelReviewRequest{Text: "two"}) }()
	waitReviewFlight(t, controller, 2, 1)
	if outcome := controller.review(t.Context(), ModelReviewRequest{Text: "three"}); outcome.Fallback != "saturated" {
		t.Fatalf("capacity fallback = %#v", outcome)
	}
	cancel()
	for range 2 {
		if outcome := <-outcomes; outcome.Fallback != "canceled" {
			t.Fatalf("cancellation fallback = %#v", outcome)
		}
	}
	waitReviewFlight(t, controller, 0, 0)
	<-controller.semaphore
	if admissions.Load() != 0 || calls.Load() != 0 {
		t.Fatalf("queued canceled work consumed quota: admissions=%d provider=%d", admissions.Load(), calls.Load())
	}
	controller.circuitMu.Lock()
	defer controller.circuitMu.Unlock()
	if controller.failures != 0 {
		t.Fatal("queued cancellation counted as provider failure")
	}
}

func TestModelReviewAdmissionSkipsOpenCircuit(t *testing.T) {
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		t.Error("open circuit reached provider")
		return ModelReviewResult{}, nil
	}))
	controller.openUntil = time.Now().Add(time.Minute)
	controller.admit = func(context.Context) (bool, string, error) {
		t.Error("open circuit consumed quota")
		return true, "", nil
	}
	if outcome := controller.review(t.Context(), ModelReviewRequest{Text: "synthetic"}); outcome.Fallback != "circuit_open" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestModelReviewAdmissionChargesProviderFailures(t *testing.T) {
	var admissions, calls atomic.Int32
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		return ModelReviewResult{}, errors.New("provider failed after external attempt")
	}))
	controller.admit = func(context.Context) (bool, string, error) {
		admissions.Add(1)
		return true, "", nil
	}
	for range 2 {
		if outcome := controller.review(t.Context(), ModelReviewRequest{Text: "retry same task"}); outcome.Fallback != "review_error" {
			t.Fatalf("outcome = %#v", outcome)
		}
	}
	if admissions.Load() != 2 || calls.Load() != 2 {
		t.Fatalf("failed attempts not charged: admissions=%d provider=%d", admissions.Load(), calls.Load())
	}
}

func TestModelReviewAdmissionWaitConsumesTotalBudgetWithoutCircuitPenalty(t *testing.T) {
	cfg := reviewControllerConfig()
	cfg.TimeoutMilliseconds = 50
	controller := newModelReviewController(cfg, modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		t.Error("timed-out reservation reached provider")
		return ModelReviewResult{}, nil
	}))
	controller.admit = func(ctx context.Context) (bool, string, error) {
		<-ctx.Done()
		return false, "", ctx.Err()
	}
	outcome := controller.review(t.Context(), ModelReviewRequest{Text: "synthetic"})
	if outcome.Fallback != "timeout" || outcome.StageLatenciesMS["admission"] <= 0 {
		t.Fatalf("admission timeout = %#v", outcome)
	}
	waitReviewFlight(t, controller, 0, 0)
	controller.circuitMu.Lock()
	defer controller.circuitMu.Unlock()
	if controller.failures != 0 || controller.halfOpen {
		t.Fatal("admission timeout poisoned provider circuit")
	}
}
