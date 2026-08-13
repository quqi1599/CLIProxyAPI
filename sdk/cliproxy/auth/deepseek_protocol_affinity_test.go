package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestDeepSeekProtocolAffinityPrefersMatchingOfficialCredential(t *testing.T) {
	openAI := deepSeekProtocolAffinityAuth("openai", "openai-compatibility", "https://api.deepseek.com/v1")
	claude := deepSeekProtocolAffinityAuth("claude", "claude", "https://api.deepseek.com/anthropic")
	relay := deepSeekProtocolAffinityAuth("relay", "openai-compatibility", "https://relay.example.com/v1")
	auths := []*Auth{relay, claude, openAI}

	for _, test := range []struct {
		name   string
		format sdktranslator.Format
		wantID string
	}{
		{name: "OpenAI Chat", format: sdktranslator.FormatOpenAI, wantID: "openai"},
		{name: "Claude Messages", format: sdktranslator.FormatClaude, wantID: "claude"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := deepSeekProtocolAffinityOptions(test.format, deepSeekProtocolAffinityMinTools, 1024)
			got := preferDeepSeekProtocolAffinityAuths(auths, "deepseek-v4-pro", opts)
			if len(got) != 1 || got[0].ID != test.wantID {
				t.Fatalf("preferred auths = %v, want %q", authIDs(got), test.wantID)
			}
		})
	}
}

func TestDeepSeekProtocolAffinityRequiresLongToolHistoryAndChatOrClaudeProtocol(t *testing.T) {
	auths := []*Auth{
		deepSeekProtocolAffinityAuth("openai", "openai-compatibility", "https://api.deepseek.com/v1"),
		deepSeekProtocolAffinityAuth("claude", "claude", "https://api.deepseek.com/anthropic"),
	}

	tests := []struct {
		name   string
		model  string
		format sdktranslator.Format
		tools  int
		bytes  int
	}{
		{name: "short history", model: "deepseek-v4-pro", format: sdktranslator.FormatOpenAI, tools: 2, bytes: 1024},
		{name: "Responses stays on existing routing", model: "deepseek-v4-flash", format: sdktranslator.FormatOpenAIResponse, tools: 200, bytes: deepSeekProtocolAffinityMinBytes},
		{name: "other model", model: "gpt-5", format: sdktranslator.FormatOpenAI, tools: 200, bytes: deepSeekProtocolAffinityMinBytes},
		{name: "large body without tools", model: "deepseek-v4-pro", format: sdktranslator.FormatClaude, tools: 0, bytes: deepSeekProtocolAffinityMinBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := deepSeekProtocolAffinityOptions(test.format, test.tools, test.bytes)
			if shouldPreferDeepSeekProtocolAffinity(test.model, opts) {
				t.Fatal("protocol affinity enabled unexpectedly")
			}
			got := preferDeepSeekProtocolAffinityAuths(auths, test.model, opts)
			if len(got) != len(auths) {
				t.Fatalf("auth count = %d, want %d", len(got), len(auths))
			}
		})
	}
}

func TestDeepSeekProtocolAffinityUsesBodyThresholdAndFallsBackWhenPreferredCredentialUnavailable(t *testing.T) {
	opts := deepSeekProtocolAffinityOptions(sdktranslator.FormatOpenAI, 1, deepSeekProtocolAffinityMinBytes)
	if !shouldPreferDeepSeekProtocolAffinity("deepseek-v4-flash", opts) {
		t.Fatal("large DeepSeek tool history should enable protocol affinity")
	}
	remaining := []*Auth{
		deepSeekProtocolAffinityAuth("relay", "openai-compatibility", "https://relay.example.com/v1"),
		deepSeekProtocolAffinityAuth("claude", "claude", "https://api.deepseek.com/anthropic"),
	}
	got := preferDeepSeekProtocolAffinityAuths(remaining, "deepseek-v4-flash", opts)
	if len(got) != len(remaining) {
		t.Fatalf("fallback auths = %v, want all remaining auths", authIDs(got))
	}
}

func TestManagerPickNextMixedDeepSeekProtocolAffinityAcrossCredentials(t *testing.T) {
	const model = "deepseek-v4-pro"
	openAI := deepSeekProtocolAffinityAuth("affinity-openai", "openai-compatibility", "https://api.deepseek.com/v1")
	claude := deepSeekProtocolAffinityAuth("affinity-claude", "claude", "https://api.deepseek.com/anthropic")
	relay := deepSeekProtocolAffinityAuth("affinity-relay", "openai-compatibility", "https://relay.example.com/v1")
	registerSchedulerModels(t, "deepseek", model, openAI.ID, claude.ID, relay.ID)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["openai-compatibility"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	for _, candidate := range []*Auth{relay, claude, openAI} {
		if _, err := manager.Register(context.Background(), candidate); err != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, err)
		}
	}

	for _, test := range []struct {
		name   string
		format sdktranslator.Format
		wantID string
	}{
		{name: "OpenAI Chat", format: sdktranslator.FormatOpenAI, wantID: openAI.ID},
		{name: "Claude Messages", format: sdktranslator.FormatClaude, wantID: claude.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := deepSeekProtocolAffinityOptions(test.format, deepSeekProtocolAffinityMinTools, 1024)
			got, _, _, err := manager.pickNextMixed(context.Background(), []string{"openai-compatibility", "claude"}, model, opts, map[string]struct{}{})
			if err != nil {
				t.Fatalf("pickNextMixed() error = %v", err)
			}
			if got == nil || got.ID != test.wantID {
				t.Fatalf("pickNextMixed() auth = %#v, want %q", got, test.wantID)
			}
		})
	}

	opts := deepSeekProtocolAffinityOptions(sdktranslator.FormatOpenAI, deepSeekProtocolAffinityMinTools, 1024)
	got, _, _, err := manager.pickNextMixed(context.Background(), []string{"openai-compatibility", "claude"}, model, opts, map[string]struct{}{openAI.ID: struct{}{}})
	if err != nil {
		t.Fatalf("pickNextMixed() fallback error = %v", err)
	}
	if got == nil || got.ID == openAI.ID {
		t.Fatalf("pickNextMixed() fallback auth = %#v, want an untried credential", got)
	}
}

func deepSeekProtocolAffinityAuth(id, provider, baseURL string) *Auth {
	attributes := map[string]string{
		"api_key":  "test-key",
		"base_url": baseURL,
	}
	if provider == "openai-compatibility" {
		attributes["compat_name"] = id
	} else if provider == "claude" {
		attributes["compat_name"] = "deepseek-anthropic"
	}
	return &Auth{ID: id, Provider: provider, Attributes: attributes}
}

func deepSeekProtocolAffinityOptions(format sdktranslator.Format, toolInteractions, requestBytes int) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		SourceFormat: format,
		Metadata: map[string]any{
			cliproxyexecutor.ToolInteractionCountMetadataKey: toolInteractions,
			cliproxyexecutor.RequestBodyBytesMetadataKey:     requestBytes,
		},
	}
}

func authIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth != nil {
			ids = append(ids, auth.ID)
		}
	}
	return ids
}
