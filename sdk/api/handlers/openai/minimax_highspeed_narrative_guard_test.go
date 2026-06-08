package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestMiniMaxHighspeedNarrativeGuardMatchesAndCaps(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfig(2, 4096)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	decision := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if decision.release == nil {
		t.Fatal("matching request should acquire a guard slot")
	}
	defer decision.release()
	if !decision.cappedOutput {
		t.Fatal("matching request should cap output tokens")
	}
	if got := gjson.GetBytes(decision.rawJSON, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", got)
	}
	if decision.structuralHits < miniMaxHighspeedNarrativeMinStructuralHits {
		t.Fatalf("structural hits = %d, want >= %d", decision.structuralHits, miniMaxHighspeedNarrativeMinStructuralHits)
	}
	if decision.narrativeHits < miniMaxHighspeedNarrativeMinNarrativeHits {
		t.Fatalf("narrative hits = %d, want >= %d", decision.narrativeHits, miniMaxHighspeedNarrativeMinNarrativeHits)
	}
}

func TestMiniMaxHighspeedNarrativeGuardQueuesAboveConcurrency(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfig(2, 4096)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	first := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if first.release == nil {
		t.Fatal("first matching request should be admitted")
	}
	defer first.release()

	second := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if second.release == nil {
		t.Fatal("second matching request should be admitted")
	}
	defer second.release()

	thirdDone := make(chan miniMaxHighspeedNarrativeGuardDecision, 1)
	go func() {
		thirdDone <- prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	}()

	select {
	case third := <-thirdDone:
		if third.release != nil {
			third.release()
		}
		t.Fatal("third concurrent matching request should wait for a slot")
	case <-time.After(50 * time.Millisecond):
	}

	first.release()

	select {
	case third := <-thirdDone:
		if third.waitErr != nil {
			t.Fatalf("queued request wait error: %v", third.waitErr)
		}
		if third.release == nil {
			t.Fatal("queued request should acquire a slot after release")
		}
		third.release()
	case <-time.After(time.Second):
		t.Fatal("queued request did not acquire slot after release")
	}
}

func TestMiniMaxHighspeedNarrativeGuardQueueCancelDoesNotLeakSlot(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfig(1, 4096)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	first := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if first.release == nil {
		t.Fatal("first matching request should be admitted")
	}
	defer first.release()

	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan miniMaxHighspeedNarrativeGuardDecision, 1)
	go func() {
		waitDone <- prepareMiniMaxHighspeedNarrativeGuard(ctx, payload, cfg, limiter)
	}()

	cancel()
	cancelled := <-waitDone
	if cancelled.waitErr == nil {
		t.Fatal("queued request should observe context cancellation")
	}
	if cancelled.release != nil {
		t.Fatal("cancelled queued request must not hold a slot")
	}

	first.release()
	next := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if next.waitErr != nil {
		t.Fatalf("next request wait error: %v", next.waitErr)
	}
	if next.release == nil {
		t.Fatal("slot should be available after cancellation and release")
	}
	next.release()
}

func TestMiniMaxHighspeedNarrativeGuardQueueDepthLimit(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfigWithQueue(1, 1, 4096)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	first := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if first.release == nil {
		t.Fatal("first matching request should be admitted")
	}
	defer first.release()

	waitDone := make(chan miniMaxHighspeedNarrativeGuardDecision, 1)
	go func() {
		waitDone <- prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	}()
	waitForMiniMaxHighspeedNarrativeQueueDepth(t, limiter, 1)

	third := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if !third.queueFull {
		t.Fatal("third request should trip the queue depth limit")
	}
	if third.release != nil {
		t.Fatal("queue-full request must not hold a slot")
	}
	if third.waitErr != nil {
		t.Fatalf("queue-full request should not report wait error: %v", third.waitErr)
	}
	if third.queued != 1 {
		t.Fatalf("queued = %d, want 1", third.queued)
	}
	if third.maxQueue != 1 {
		t.Fatalf("maxQueue = %d, want 1", third.maxQueue)
	}

	first.release()
	select {
	case second := <-waitDone:
		if second.queueFull {
			t.Fatal("queued request should acquire a slot after release, not trip queue limit")
		}
		if second.waitErr != nil {
			t.Fatalf("queued request wait error: %v", second.waitErr)
		}
		if second.release == nil {
			t.Fatal("queued request should acquire a slot")
		}
		second.release()
	case <-time.After(time.Second):
		t.Fatal("queued request did not acquire slot after release")
	}
}

func TestMiniMaxHighspeedNarrativeGuardQueueWaitTimeoutDoesNotLeakSlot(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfigWithQueueAndWait(1, 2, 1, 4096)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	first := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if first.release == nil {
		t.Fatal("first matching request should be admitted")
	}

	waitDone := make(chan miniMaxHighspeedNarrativeGuardDecision, 1)
	go func() {
		waitDone <- prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	}()
	waitForMiniMaxHighspeedNarrativeQueueDepth(t, limiter, 1)

	select {
	case timedOut := <-waitDone:
		if !timedOut.waitTimedOut {
			t.Fatal("queued request should report queue wait timeout")
		}
		if timedOut.release != nil {
			t.Fatal("timed-out request must not hold a slot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not time out")
	}

	first.release()
	next := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if next.waitErr != nil {
		t.Fatalf("next request wait error: %v", next.waitErr)
	}
	if next.release == nil {
		t.Fatal("slot should be available after queue wait timeout and release")
	}
	next.release()
}

func TestMiniMaxHighspeedNarrativeGuardIgnoresLongCodeContext(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfig(2, 4096)
	codePrompt := strings.Repeat("package main\nfunc handler() error { return nil }\n", 3000)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, codePrompt)
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	decision := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, cfg, limiter)
	if decision.release != nil {
		t.Fatal("long code context should not acquire a guard slot")
	}
	if got := gjson.GetBytes(decision.rawJSON, "max_tokens").Int(); got != 12000 {
		t.Fatalf("max_tokens = %d, want unchanged 12000", got)
	}
}

func TestMiniMaxHighspeedNarrativeGuardDisabledByDefault(t *testing.T) {
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	decision := prepareMiniMaxHighspeedNarrativeGuard(context.Background(), payload, &sdkconfig.SDKConfig{}, &miniMaxHighspeedNarrativeLimiter{})
	if decision.release != nil {
		t.Fatal("disabled guard should not acquire slots")
	}
	if got := gjson.GetBytes(decision.rawJSON, "max_tokens").Int(); got != 12000 {
		t.Fatalf("max_tokens = %d, want unchanged 12000", got)
	}
}

func miniMaxHighspeedNarrativeTestConfig(maxConcurrent, maxOutputTokens int) *sdkconfig.SDKConfig {
	return miniMaxHighspeedNarrativeTestConfigWithQueue(maxConcurrent, 0, maxOutputTokens)
}

func miniMaxHighspeedNarrativeTestConfigWithQueue(maxConcurrent, maxQueue, maxOutputTokens int) *sdkconfig.SDKConfig {
	return miniMaxHighspeedNarrativeTestConfigWithQueueAndWait(maxConcurrent, maxQueue, 0, maxOutputTokens)
}

func miniMaxHighspeedNarrativeTestConfigWithQueueAndWait(maxConcurrent, maxQueue, maxWaitSeconds, maxOutputTokens int) *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{
		RequestGuards: sdkconfig.RequestGuardsConfig{
			MiniMaxHighspeedNarrative: sdkconfig.MiniMaxHighspeedNarrativeGuardConfig{
				Enabled:           true,
				MaxConcurrent:     maxConcurrent,
				MaxQueue:          maxQueue,
				MaxWaitSeconds:    maxWaitSeconds,
				MaxOutputTokens:   maxOutputTokens,
				RetryAfterSeconds: 30,
			},
		},
	}
}

func waitForMiniMaxHighspeedNarrativeQueueDepth(t *testing.T, limiter *miniMaxHighspeedNarrativeLimiter, depth int) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		limiter.mu.Lock()
		got := len(limiter.waiters)
		limiter.mu.Unlock()
		if got == depth {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("queue depth did not become %d", depth)
		case <-ticker.C:
		}
	}
}

func miniMaxHighspeedNarrativeTestPayload(maxTokens int, prompt string) []byte {
	payload := map[string]any{
		"model":      miniMaxHighspeedNarrativeModel,
		"stream":     true,
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

func miniMaxHighspeedNarrativeTestPrompt() string {
	return "lastRules\n" +
		"FICTIONAL CONTEXT LOCK\n" +
		"OUTPUT FORMAT\n" +
		"statebar\n" +
		"plot_choices\n" +
		"content_key_summary\n" +
		"Mandatory language requirement\n" +
		"<scenario>\n" +
		"<narration>\n" +
		"<action>\n" +
		"<message>\n" +
		"novel-style storytelling\n" +
		"roleplay\n" +
		strings.Repeat("story scene continuation ", 6000)
}
