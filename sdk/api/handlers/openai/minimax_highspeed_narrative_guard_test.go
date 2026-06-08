package openai

import (
	"encoding/json"
	"strings"
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestMiniMaxHighspeedNarrativeGuardMatchesAndCaps(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfig(2, 4096)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	decision := prepareMiniMaxHighspeedNarrativeGuard(payload, cfg, limiter)
	if decision.blocked {
		t.Fatal("first matching request should be admitted")
	}
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

func TestMiniMaxHighspeedNarrativeGuardRejectsAboveConcurrency(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfig(2, 4096)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	first := prepareMiniMaxHighspeedNarrativeGuard(payload, cfg, limiter)
	if first.blocked || first.release == nil {
		t.Fatal("first matching request should be admitted")
	}
	defer first.release()

	second := prepareMiniMaxHighspeedNarrativeGuard(payload, cfg, limiter)
	if second.blocked || second.release == nil {
		t.Fatal("second matching request should be admitted")
	}
	defer second.release()

	third := prepareMiniMaxHighspeedNarrativeGuard(payload, cfg, limiter)
	if !third.blocked {
		t.Fatal("third concurrent matching request should be rejected")
	}
	if third.release != nil {
		t.Fatal("rejected request must not hold a guard slot")
	}

	first.release()
	fourth := prepareMiniMaxHighspeedNarrativeGuard(payload, cfg, limiter)
	if fourth.blocked || fourth.release == nil {
		t.Fatal("request should be admitted after a slot is released")
	}
	fourth.release()
}

func TestMiniMaxHighspeedNarrativeGuardIgnoresLongCodeContext(t *testing.T) {
	cfg := miniMaxHighspeedNarrativeTestConfig(2, 4096)
	codePrompt := strings.Repeat("package main\nfunc handler() error { return nil }\n", 3000)
	payload := miniMaxHighspeedNarrativeTestPayload(12000, codePrompt)
	limiter := &miniMaxHighspeedNarrativeLimiter{}

	decision := prepareMiniMaxHighspeedNarrativeGuard(payload, cfg, limiter)
	if decision.blocked {
		t.Fatal("long code context should not be blocked")
	}
	if decision.release != nil {
		t.Fatal("long code context should not acquire a guard slot")
	}
	if got := gjson.GetBytes(decision.rawJSON, "max_tokens").Int(); got != 12000 {
		t.Fatalf("max_tokens = %d, want unchanged 12000", got)
	}
}

func TestMiniMaxHighspeedNarrativeGuardDisabledByDefault(t *testing.T) {
	payload := miniMaxHighspeedNarrativeTestPayload(12000, miniMaxHighspeedNarrativeTestPrompt())
	decision := prepareMiniMaxHighspeedNarrativeGuard(payload, &sdkconfig.SDKConfig{}, &miniMaxHighspeedNarrativeLimiter{})
	if decision.blocked {
		t.Fatal("disabled guard should not reject requests")
	}
	if decision.release != nil {
		t.Fatal("disabled guard should not acquire slots")
	}
	if got := gjson.GetBytes(decision.rawJSON, "max_tokens").Int(); got != 12000 {
		t.Fatalf("max_tokens = %d, want unchanged 12000", got)
	}
}

func miniMaxHighspeedNarrativeTestConfig(maxConcurrent, maxOutputTokens int) *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{
		RequestGuards: sdkconfig.RequestGuardsConfig{
			MiniMaxHighspeedNarrative: sdkconfig.MiniMaxHighspeedNarrativeGuardConfig{
				Enabled:           true,
				MaxConcurrent:     maxConcurrent,
				MaxOutputTokens:   maxOutputTokens,
				RetryAfterSeconds: 30,
			},
		},
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
