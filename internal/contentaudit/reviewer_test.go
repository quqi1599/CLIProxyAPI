package contentaudit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type modelReviewerFunc func(context.Context, ModelReviewRequest) (ModelReviewResult, error)

func (f modelReviewerFunc) Review(ctx context.Context, request ModelReviewRequest) (ModelReviewResult, error) {
	return f(ctx, request)
}

func TestModelReviewControllerCachesIdenticalContent(t *testing.T) {
	var calls atomic.Int32
	controller := newModelReviewController(config.ContentAuditModelReviewConfig{
		Mode:                     ModelReviewModeShadow,
		Model:                    "synthetic-reviewer",
		PromptVersion:            "test-v1",
		TimeoutMilliseconds:      500,
		QueueTimeoutMilliseconds: 50,
		MaxConcurrent:            2,
		CacheSeconds:             60,
		MaxInputBytes:            4096,
	}, modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		return ModelReviewResult{Decision: ModelReviewAllow, Category: "jailbreak", Confidence: 0.99}, nil
	}))
	request := ModelReviewRequest{Text: "synthetic request", RuleID: "rule", Category: "jailbreak"}
	first := controller.review(t.Context(), request)
	second := controller.review(t.Context(), request)
	if first.Decision != ModelReviewAllow || second.Decision != ModelReviewAllow || !second.CacheHit {
		t.Fatalf("outcomes = %#v %#v", first, second)
	}
	if calls.Load() != 1 {
		t.Fatalf("reviewer calls = %d, want 1", calls.Load())
	}
}

func TestModelReviewControllerSingleflightAndTimeout(t *testing.T) {
	var calls atomic.Int32
	controller := newModelReviewController(config.ContentAuditModelReviewConfig{
		Mode:                     ModelReviewModeShadow,
		Model:                    "synthetic-reviewer",
		PromptVersion:            "test-v1",
		TimeoutMilliseconds:      30,
		QueueTimeoutMilliseconds: 10,
		MaxConcurrent:            1,
		CacheSeconds:             60,
		MaxInputBytes:            4096,
	}, modelReviewerFunc(func(ctx context.Context, _ ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		<-ctx.Done()
		return ModelReviewResult{}, ctx.Err()
	}))

	var wait sync.WaitGroup
	wait.Add(2)
	outcomes := make(chan modelReviewOutcome, 2)
	for range 2 {
		go func() {
			defer wait.Done()
			outcomes <- controller.review(t.Context(), ModelReviewRequest{Text: "same", RuleID: "rule"})
		}()
	}
	wait.Wait()
	close(outcomes)
	for outcome := range outcomes {
		if outcome.Decision != ModelReviewUncertain || outcome.Fallback != "timeout" {
			t.Fatalf("outcome = %#v", outcome)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("reviewer calls = %d, want 1", calls.Load())
	}
}

func TestTruncateReviewTextPreservesUTF8(t *testing.T) {
	text := truncateReviewText("合规审计内容", 5)
	if text != "合" {
		t.Fatalf("truncateReviewText() = %q", text)
	}
}

func TestModelReviewControllerCircuitBreakerStopsRepeatedFailures(t *testing.T) {
	var calls atomic.Int32
	controller := newModelReviewController(config.ContentAuditModelReviewConfig{
		Mode:                     ModelReviewModeShadow,
		Model:                    "synthetic-reviewer",
		PromptVersion:            "test-v1",
		TimeoutMilliseconds:      100,
		QueueTimeoutMilliseconds: 10,
		MaxConcurrent:            1,
		MaxInputBytes:            4096,
		CircuitFailureThreshold:  1,
		CircuitOpenSeconds:       30,
	}, modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		return ModelReviewResult{}, errors.New("synthetic failure")
	}))
	first := controller.review(t.Context(), ModelReviewRequest{Text: "one", RuleID: "rule"})
	second := controller.review(t.Context(), ModelReviewRequest{Text: "two", RuleID: "rule"})
	if first.Fallback != "review_error" || second.Fallback != "circuit_open" {
		t.Fatalf("outcomes=%#v %#v", first, second)
	}
	if calls.Load() != 1 {
		t.Fatalf("reviewer calls=%d, want 1", calls.Load())
	}
}
