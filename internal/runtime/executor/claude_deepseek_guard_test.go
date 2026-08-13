package executor

import (
	"context"
	"strings"
	"testing"
)

func TestClaudeDeepSeekOfficialLongContextToolPileLimits(t *testing.T) {
	for _, model := range []string{"deepseek-v4-pro", "deepseek-v4-flash(1m)"} {
		meta := compatRepairLogMeta{
			requestedModel: model,
			upstreamModel:  model,
			compatKind:     "deepseek",
			baseHost:       "api.deepseek.com",
		}
		payloadBytes, toolResults := claudeCompatToolResultPileLimits(meta)
		if payloadBytes != largeClaudeDeepSeekToolResultPilePayloadBytes || toolResults != largeClaudeDeepSeekToolResultOnlyMessages {
			t.Fatalf("model %q limits = %d/%d, want %d/%d", model, payloadBytes, toolResults, largeClaudeDeepSeekToolResultPilePayloadBytes, largeClaudeDeepSeekToolResultOnlyMessages)
		}
	}
}

func TestClaudeDeepSeekToolPileLimitsStayDefaultOutsideOfficialV4AnthropicRoute(t *testing.T) {
	for _, meta := range []compatRepairLogMeta{
		{requestedModel: "deepseek-v4-pro", compatKind: "deepseek", baseHost: "relay.example.com"},
		{requestedModel: "deepseek-v3.2", compatKind: "deepseek", baseHost: "api.deepseek.com"},
		{requestedModel: "deepseek-v4-pro", compatKind: "doubao", baseHost: "api.deepseek.com"},
	} {
		payloadBytes, toolResults := claudeCompatToolResultPileLimits(meta)
		if payloadBytes != largeClaudeCompatToolResultPilePayloadBytes || toolResults != largeClaudeCompatToolResultOnlyMessages {
			t.Fatalf("meta %+v limits = %d/%d, want defaults %d/%d", meta, payloadBytes, toolResults, largeClaudeCompatToolResultPilePayloadBytes, largeClaudeCompatToolResultOnlyMessages)
		}
	}
}

func TestDeepSeekAnthropicImageGuardOnlyAppliesToOfficialHost(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}]}`)
	if got := countClaudeImageParts(body); got != 1 {
		t.Fatalf("image parts = %d, want 1", got)
	}
	if err := rejectDeepSeekAnthropicUnsupportedImageInput(context.Background(), body, "deepseek", "https://relay.example.com/anthropic", "deepseek-v4-pro"); err != nil {
		t.Fatalf("custom relay should retain its capability policy: %v", err)
	}
	err := rejectDeepSeekAnthropicUnsupportedImageInput(context.Background(), body, "deepseek", "https://api.deepseek.com/anthropic", "deepseek-v4-pro")
	if err == nil || !strings.Contains(err.Error(), "不支持图片内容块") {
		t.Fatalf("official DeepSeek Anthropic image guard error = %v", err)
	}
}
