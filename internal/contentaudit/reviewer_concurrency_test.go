package contentaudit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func reviewControllerConfig() config.ContentAuditModelReviewConfig {
	return config.ContentAuditModelReviewConfig{
		Mode: ModelReviewModeShadow, Model: "synthetic-reviewer", PromptVersion: "test-v2",
		TimeoutMilliseconds: 2000, QueueTimeoutMilliseconds: 1000,
		MaxConcurrent: 1, CacheSeconds: 60, MaxInputBytes: 4096,
	}
}

func waitReviewFlight(t *testing.T, c *modelReviewController, count, waiters int) *modelReviewFlight {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		c.flightsMu.Lock()
		if len(c.flights) == count {
			for _, flight := range c.flights {
				if flight.waiters == waiters {
					c.flightsMu.Unlock()
					return flight
				}
			}
			if count == 0 {
				c.flightsMu.Unlock()
				return nil
			}
		}
		c.flightsMu.Unlock()
		select {
		case <-timeout.C:
			t.Fatal("review flights did not reach expected state")
		case <-poll.C:
		}
	}
}

func TestModelReviewControllerWaitersCancelIndependently(t *testing.T) {
	for _, cancelLeader := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel_leader_%t", cancelLeader), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var providerCanceled atomic.Bool
			var calls atomic.Int32
			controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(ctx context.Context, _ ModelReviewRequest) (ModelReviewResult, error) {
				calls.Add(1)
				close(entered)
				select {
				case <-ctx.Done():
					providerCanceled.Store(true)
					return ModelReviewResult{}, ctx.Err()
				case <-release:
					return ModelReviewResult{Decision: ModelReviewAllow, Confidence: .9}, nil
				}
			}))
			leaderCtx, cancelFirst := context.WithCancel(t.Context())
			defer cancelFirst()
			followerCtx, cancelSecond := context.WithCancel(t.Context())
			defer cancelSecond()
			first, second := make(chan modelReviewOutcome, 1), make(chan modelReviewOutcome, 1)
			request := ModelReviewRequest{Text: "same full task", RuleID: "rule"}
			go func() { first <- controller.review(leaderCtx, request) }()
			<-entered
			go func() { second <- controller.review(followerCtx, request) }()
			waitReviewFlight(t, controller, 1, 2)
			canceled, remaining := first, second
			if cancelLeader {
				cancelFirst()
			} else {
				cancelSecond()
				canceled, remaining = second, first
			}
			if outcome := <-canceled; outcome.Fallback != "canceled" {
				t.Fatalf("canceled waiter = %#v", outcome)
			}
			if providerCanceled.Load() {
				t.Fatal("canceling one waiter canceled shared provider")
			}
			close(release)
			if outcome := <-remaining; outcome.Decision != ModelReviewAllow || outcome.Fallback != "" {
				t.Fatalf("remaining waiter = %#v", outcome)
			}
			if calls.Load() != 1 {
				t.Fatalf("provider calls = %d", calls.Load())
			}
		})
	}
}

func TestModelReviewControllerFinalWaiterCancelsProviderWithoutTrippingCircuit(t *testing.T) {
	entered, stopped := make(chan struct{}), make(chan struct{})
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(ctx context.Context, _ ModelReviewRequest) (ModelReviewResult, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return ModelReviewResult{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan modelReviewOutcome, 1)
	go func() { done <- controller.review(ctx, ModelReviewRequest{Text: "request"}) }()
	<-entered
	cancel()
	if outcome := <-done; outcome.Fallback != "canceled" {
		t.Fatalf("outcome = %#v", outcome)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("provider was not canceled after final waiter left")
	}
	waitReviewFlight(t, controller, 0, 0)
	controller.circuitMu.Lock()
	defer controller.circuitMu.Unlock()
	if controller.failures != 0 || !controller.openUntil.IsZero() {
		t.Fatal("client cancellation was counted as provider failure")
	}
}

func TestModelReviewControllerUniqueFloodHasBoundedWork(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		// Deliberately ignore cancellation to verify capacity remains bounded.
		<-release
		return ModelReviewResult{Decision: ModelReviewAllow, Confidence: .9}, nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan modelReviewOutcome, 2)
	go func() { done <- controller.review(ctx, ModelReviewRequest{Text: "one"}) }()
	<-entered
	go func() { done <- controller.review(ctx, ModelReviewRequest{Text: "two"}) }()
	waitReviewFlight(t, controller, 2, 1)
	for index := range 128 {
		outcome := controller.review(t.Context(), ModelReviewRequest{Text: fmt.Sprintf("unique-%d", index)})
		if outcome.Fallback != "saturated" {
			t.Fatalf("flood outcome = %#v", outcome)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("active provider calls = %d, want 1", calls.Load())
	}
	cancel()
	<-done
	<-done
	// The abandoned provider is still admitted; an identical new request cannot
	// start a replacement while that uncooperative provider remains alive.
	if outcome := controller.review(t.Context(), ModelReviewRequest{Text: "one"}); outcome.Fallback != "saturated" {
		t.Fatalf("abandoned flight replacement = %#v", outcome)
	}
	close(release)
	waitReviewFlight(t, controller, 0, 0)
}

func TestModelReviewControllerBudgetStartsBeforeAdmission(t *testing.T) {
	cfg := reviewControllerConfig()
	cfg.TimeoutMilliseconds = 150
	cfg.QueueTimeoutMilliseconds = 1000
	providerDeadline := make(chan time.Time, 1)
	controller := newModelReviewController(cfg, modelReviewerFunc(func(ctx context.Context, _ ModelReviewRequest) (ModelReviewResult, error) {
		deadline, _ := ctx.Deadline()
		providerDeadline <- deadline
		<-ctx.Done()
		return ModelReviewResult{}, ctx.Err()
	}))
	controller.semaphore <- struct{}{}
	done := make(chan modelReviewOutcome, 1)
	go func() { done <- controller.review(t.Context(), ModelReviewRequest{Text: "queued task"}) }()
	flight := waitReviewFlight(t, controller, 1, 1)
	expected := flight.started.Add(150 * time.Millisecond)
	<-controller.semaphore
	if deadline := <-providerDeadline; !deadline.Equal(expected) {
		t.Fatalf("provider deadline = %v, want admission deadline %v", deadline, expected)
	}
	if outcome := <-done; outcome.Fallback != "timeout" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestModelReviewControllerTimeoutSnapshotsCompletedProviderStages(t *testing.T) {
	cfg := reviewControllerConfig()
	cfg.TimeoutMilliseconds = 50
	controller := newModelReviewController(cfg, modelReviewerFunc(func(ctx context.Context, _ ModelReviewRequest) (ModelReviewResult, error) {
		trace := coreexecutor.ContentAuditReviewTraceFromContext(ctx)
		if trace == nil {
			t.Error("shared flight lacks live stage trace")
			return ModelReviewResult{}, errors.New("no trace")
		}
		trace.Record("auth_select", 7*time.Millisecond)
		trace.Record("request_write", time.Millisecond)
		<-ctx.Done()
		return ModelReviewResult{}, ctx.Err()
	}))
	outcome := controller.review(t.Context(), ModelReviewRequest{Text: "synthetic"})
	if outcome.Fallback != "timeout" || outcome.StageLatenciesMS["auth_select"] != 7 || outcome.StageLatenciesMS["request_write"] != 1 {
		t.Fatalf("timeout stage trace = %#v", outcome)
	}
	if _, exists := outcome.StageLatenciesMS["ttfb"]; exists {
		t.Fatal("unobserved first response byte was reported as completed")
	}
}

func TestModelReviewControllerFingerprintUsesFullTaskAndDecisionContext(t *testing.T) {
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		return ModelReviewResult{Decision: ModelReviewAllow}, nil
	}))
	base := ModelReviewRequest{TenantScope: "tenant", PolicyVersion: "policy-v1", PromptVersion: "prompt-v1", Model: "model-v1", RuleID: "rule", Category: "sexual", Severity: "high", MatchedTerm: "match", Text: "match " + strings.Repeat("common ", 100) + "tail-one", ReferenceText: "reference"}
	variants := []struct {
		name   string
		change func(*ModelReviewRequest)
	}{
		{"tail_outside_excerpt", func(r *ModelReviewRequest) { r.Text = strings.TrimSuffix(r.Text, "tail-one") + "tail-two" }},
		{"tenant", func(r *ModelReviewRequest) { r.TenantScope += "-two" }},
		{"policy", func(r *ModelReviewRequest) { r.PolicyVersion += "-two" }},
		{"prompt", func(r *ModelReviewRequest) { r.PromptVersion += "-two" }},
		{"model", func(r *ModelReviewRequest) { r.Model += "-two" }},
		{"rule", func(r *ModelReviewRequest) { r.RuleID += "-two" }},
		{"category", func(r *ModelReviewRequest) { r.Category = "cyber" }},
		{"severity", func(r *ModelReviewRequest) { r.Severity = "critical" }},
		{"reference", func(r *ModelReviewRequest) { r.ReferenceText += " changed" }},
		{"coverage", func(r *ModelReviewRequest) { r.ContextIncomplete = true }},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			changed := base
			variant.change(&changed)
			if controller.fingerprint(base) == controller.fingerprint(changed) {
				t.Fatal("distinct decision context shares fingerprint")
			}
			if variant.name == "tail_outside_excerpt" && compactReviewText(base.Text, "match", 100) != compactReviewText(changed.Text, "match", 100) {
				t.Fatal("test did not produce identical excerpts")
			}
		})
	}
	left, right := base, base
	left.TenantScope, left.PolicyVersion = "tenant\x00policy", "v1"
	right.TenantScope, right.PolicyVersion = "tenant", "policy\x00v1"
	if controller.fingerprint(left) == controller.fingerprint(right) {
		t.Fatal("field separator collision")
	}
}

func TestModelReviewControllerCacheScopeAndMutableResults(t *testing.T) {
	var calls atomic.Int32
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(_ context.Context, request ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		if request.TenantScope != "" || request.PolicyVersion != "" {
			t.Error("local cache identity leaked to provider")
		}
		return ModelReviewResult{Decision: ModelReviewAllow, Confidence: .9, ReasonCodes: []string{"SAFE_CONTEXT"}, StageLatenciesMS: map[string]int64{"parse": 1}}, nil
	}))
	request := ModelReviewRequest{TenantScope: "one", PolicyVersion: "v1", Text: "same task"}
	first := controller.review(t.Context(), request)
	first.ReasonCodes[0] = "MUTATED"
	first.StageLatenciesMS["parse"] = 999
	second := controller.review(t.Context(), request)
	if !second.CacheHit || second.ReasonCodes[0] != "SAFE_CONTEXT" {
		t.Fatalf("cached result = %#v", second)
	}
	if _, retained := second.StageLatenciesMS["parse"]; retained {
		t.Fatal("cache hit attributed old provider parse time to current request")
	}
	request.TenantScope = "two"
	if outcome := controller.review(t.Context(), request); outcome.CacheHit {
		t.Fatal("cache crossed tenant boundary")
	}
	request.PolicyVersion = "v2"
	if outcome := controller.review(t.Context(), request); outcome.CacheHit {
		t.Fatal("cache crossed policy boundary")
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestModelReviewControllerIncompleteContextCannotCertifyDecision(t *testing.T) {
	for _, decision := range []string{ModelReviewAllow, ModelReviewBlock} {
		t.Run(decision, func(t *testing.T) {
			cfg := reviewControllerConfig()
			cfg.MaxInputBytes = 100
			controller := newModelReviewController(cfg, modelReviewerFunc(func(_ context.Context, request ModelReviewRequest) (ModelReviewResult, error) {
				t.Error("incomplete context consumed a supplier call")
				return ModelReviewResult{Decision: decision, Category: "sexual", Confidence: .99}, nil
			}))
			controller.admit = func(context.Context) (bool, string, error) {
				t.Error("incomplete context consumed quota")
				return true, "", nil
			}
			request := ModelReviewRequest{Text: strings.Repeat("current ", 100), ReferenceText: strings.Repeat("reference ", 100)}
			compacted := compactModelReviewRequest(request, 100)
			if len(compacted.Text)+len(compacted.ReferenceText) > 100 || !compacted.ContextIncomplete {
				t.Fatal("combined scope was not bounded with an incomplete marker")
			}
			for range 2 {
				outcome := controller.review(t.Context(), request)
				if outcome.Decision != ModelReviewUncertain || outcome.Fallback != "context_incomplete" || outcome.CacheHit {
					t.Fatalf("incomplete outcome = %#v", outcome)
				}
			}
		})
	}
}

func TestNormalizeModelReviewResultRejectsInvalidValues(t *testing.T) {
	for _, confidence := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -.01, 1.01} {
		if result := normalizeModelReviewResult(ModelReviewResult{Decision: ModelReviewBlock, Category: "sexual", Confidence: confidence}); result.Decision != "" {
			t.Fatalf("accepted invalid confidence %v", confidence)
		}
	}
	for _, result := range []ModelReviewResult{
		{Decision: "refusal", Category: "sexual", Confidence: .9},
		{Decision: ModelReviewBlock, Category: "provider prose", Confidence: .9},
		{Decision: ModelReviewBlock, Category: "none", Confidence: .9},
		{Decision: ModelReviewAllow, Confidence: .9, ReasonCodes: []string{"unbounded customer prose"}},
		{Decision: ModelReviewAllow, Confidence: .9, ReasonCodes: make([]string, 9)},
	} {
		if normalized := normalizeModelReviewResult(result); normalized.Decision != "" {
			t.Fatalf("accepted invalid result %#v", result)
		}
	}
}

type syntheticReviewFailure struct{ code string }

func (e syntheticReviewFailure) Error() string {
	return "sensitive provider diagnostic must not escape"
}
func (e syntheticReviewFailure) AuditReviewFailureCode() string { return e.code }
func (e syntheticReviewFailure) AuditReviewStageLatenciesMS() map[string]int64 {
	return map[string]int64{"parse": 3, "customer-text": 1, "read": -1}
}

func TestModelReviewControllerTypedFailuresRemainSanitized(t *testing.T) {
	controller := newModelReviewController(reviewControllerConfig(), modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		return ModelReviewResult{}, fmt.Errorf("private wrapper: %w", syntheticReviewFailure{code: "review_response_json_invalid"})
	}))
	outcome := controller.review(t.Context(), ModelReviewRequest{Text: "synthetic"})
	if outcome.Fallback != "review_response_json_invalid" || outcome.StageLatenciesMS["parse"] != 3 {
		t.Fatalf("typed failure = %#v", outcome)
	}
	if _, exists := outcome.StageLatenciesMS["customer-text"]; exists {
		t.Fatal("unapproved diagnostic stage escaped")
	}
	for _, err := range []error{errors.New("secret raw error"), syntheticReviewFailure{code: "secret_code"}} {
		if reason := modelReviewFallbackReason(err); reason != "review_error" {
			t.Fatalf("unknown error exposed as %q", reason)
		}
	}
}
