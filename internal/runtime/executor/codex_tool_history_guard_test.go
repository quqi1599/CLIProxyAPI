package executor

import (
	"context"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRejectLargeCodexToolHistoryBeforeUpstream(t *testing.T) {
	metadata := map[string]any{
		cliproxyexecutor.ClientProfileMetadataKey:        "workbuddy",
		cliproxyexecutor.MessageCountMetadataKey:         272,
		cliproxyexecutor.ToolCountMetadataKey:            12,
		cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey: "test-key",
			"base_url":                   "https://compat.example.com/v1",
		},
	}
	err := rejectLargeCodexToolHistory(context.Background(), []byte(`{"input":[]}`), metadata, auth)
	if err == nil {
		t.Fatal("expected non-native long tool history to be rejected")
	}
	if !strings.Contains(err.Error(), "request_feature_unsupported: codex_tool_history_too_large") {
		t.Fatalf("error = %q, want actionable request_feature_unsupported marker", err)
	}
}

func TestRejectLargeCodexToolHistoryAllowsNativeAuth(t *testing.T) {
	metadata := map[string]any{
		cliproxyexecutor.ClientProfileMetadataKey:        "workbuddy",
		cliproxyexecutor.MessageCountMetadataKey:         272,
		cliproxyexecutor.ToolCountMetadataKey:            12,
		cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey: "test-key",
			"base_url":                   "https://api.openai.com/v1",
		},
	}
	if err := rejectLargeCodexToolHistory(context.Background(), nil, metadata, auth); err != nil {
		t.Fatalf("native Responses auth should be allowed, got %v", err)
	}
}

func TestLargeCodexToolHistoryRequiresToolHistory(t *testing.T) {
	stats := codexToolHistoryStats{messageCount: codexWorkBuddyToolHistoryMessageLimit, interactionCount: 0}
	if largeCodexToolHistory(stats, 0) {
		t.Fatal("plain long conversation without tool history should not trigger the tool-history guard")
	}
}
