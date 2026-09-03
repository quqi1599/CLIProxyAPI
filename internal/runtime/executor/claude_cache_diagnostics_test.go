package executor

import (
	"bytes"
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/provideridentity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestClaudeCacheDiagnosticsOnlyStartsForOfficialDeepSeekAnthropic(t *testing.T) {
	t.Setenv("CPA_CACHE_DIAG_HMAC_SECRET", "diagnostic-test-secret")
	t.Setenv("CPA_CACHE_DIAG_HMAC_KEY_ID", "test-v1")
	sampleRate := 1.0
	cfg := &config.Config{CacheDiagnostics: config.CacheDiagnosticsConfig{
		DeepSeekAnthropic: config.DeepSeekAnthropicCacheDiagnosticsConfig{
			Enabled:              true,
			SampleRate:           &sampleRate,
			CompareWindowSeconds: 600,
			StableMissThreshold:  3,
			MaxEntries:           100,
		},
	}}
	executor := NewClaudeExecutor(cfg)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"private"}]}`)
	before := append([]byte(nil), body...)
	auth := &cliproxyauth.Auth{ID: "upstream-auth"}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-private",
		},
	}
	officialPlan := claudeRequestPlan{
		baseURL: "https://api.deepseek.com/anthropic",
		providerIdentity: provideridentity.Identity{
			Kind:     "deepseek",
			BaseHost: "api.deepseek.com",
		},
		bodyForUpstream: body,
	}
	if session := executor.beginDeepSeekAnthropicCacheDiagnostic(context.Background(), auth, opts, officialPlan, body, body, "deepseek-v4-flash", false); session == nil {
		t.Fatal("official DeepSeek Anthropic route did not start diagnostics")
	}
	if !bytes.Equal(body, before) {
		t.Fatal("diagnostics mutated the request body")
	}

	nonOfficialPlan := officialPlan
	nonOfficialPlan.baseURL = "https://gateway.example.com/anthropic"
	nonOfficialPlan.providerIdentity.BaseHost = "gateway.example.com"
	if session := executor.beginDeepSeekAnthropicCacheDiagnostic(context.Background(), auth, opts, nonOfficialPlan, body, body, "deepseek-v4-flash", false); session != nil {
		t.Fatal("non-official host started DeepSeek diagnostics")
	}
	if session := executor.beginDeepSeekAnthropicCacheDiagnostic(context.Background(), auth, opts, officialPlan, body, body, "claude-sonnet-4", false); session != nil {
		t.Fatal("non-DeepSeek model started DeepSeek diagnostics")
	}
}

func TestClaudeCacheDiagnosticsFailClosedWithoutSecret(t *testing.T) {
	t.Setenv("CPA_CACHE_DIAG_HMAC_SECRET", "")
	cfg := &config.Config{CacheDiagnostics: config.CacheDiagnosticsConfig{
		DeepSeekAnthropic: config.DeepSeekAnthropicCacheDiagnosticsConfig{Enabled: true},
	}}
	if executor := NewClaudeExecutor(cfg); executor.cacheDiagnostics != nil {
		t.Fatal("diagnostics remained enabled without independent secret")
	}
}
