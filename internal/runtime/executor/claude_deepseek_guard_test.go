package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
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
	workBuddyErr := rejectDeepSeekAnthropicUnsupportedImageInput(context.Background(), body, "deepseek", "https://api.deepseek.com/anthropic", "deepseek-v4-pro", "workbuddy")
	if workBuddyErr == nil {
		t.Fatal("WorkBuddy official DeepSeek Anthropic image guard error = nil")
	}
	for _, want := range []string{"模型设置", "取消勾选“图片输入”", "新建对话", "“工具调用”可以继续勾选", "OpenAI 原生 GPT 模型"} {
		if !strings.Contains(workBuddyErr.Error(), want) {
			t.Fatalf("WorkBuddy message %q missing %q", workBuddyErr.Error(), want)
		}
	}
}

func TestPrepareClaudeRequestDeepSeekDowngradesIncompleteDefaultHistory(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
		"api_key": "test-key", "base_url": "https://api.deepseek.com/anthropic", "compat_kind": "deepseek",
	}}
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]
	}`)
	plan, err := executor.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro", Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, "deepseek-v4-pro", false)
	if err != nil {
		t.Fatalf("prepareClaudeRequest() error = %v", err)
	}
	if got := gjson.GetBytes(plan.bodyForUpstream, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, plan.bodyForUpstream)
	}
	if gjson.GetBytes(plan.bodyForUpstream, "messages.0.content.#(type==\"thinking\")").Exists() {
		t.Fatalf("missing thinking history was synthesized: %s", plan.bodyForUpstream)
	}
}

func TestPrepareClaudeRequestDeepSeekRejectsIncompleteExplicitHistory(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
		"api_key": "test-key", "base_url": "https://api.deepseek.com/anthropic", "compat_kind": "deepseek",
	}}
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"high"},
		"messages":[{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}]
	}`)
	_, err := executor.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro", Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, "deepseek-v4-pro", false)
	if err == nil {
		t.Fatal("prepareClaudeRequest() should reject explicit thinking with incomplete history")
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InvalidThinkingHistory || typed.Scope != failurecontract.ScopeRequest || typed.ProviderCode != deepSeekInvalidThinkingHistoryCode || typed.Retryable {
		t.Fatalf("failure = %+v, want non-retryable request-scoped DeepSeek history error", typed)
	}
	if err.Error() != deepSeekInvalidThinkingHistoryMessage {
		t.Fatalf("error = %q, want customer guidance", err.Error())
	}
}

func TestPrepareClaudeRequestDeepSeekWorkBuddyDowngradesIncompleteExplicitHistory(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
		"api_key": "test-key", "base_url": "https://api.deepseek.com/anthropic", "compat_kind": "deepseek",
	}}
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"high"},
		"messages":[{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}]
	}`)
	plan, err := executor.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro", Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.ClientProfileMetadataKey: "workbuddy",
		},
	}, "deepseek-v4-pro", false)
	if err != nil {
		t.Fatalf("prepareClaudeRequest() error = %v", err)
	}
	if got := gjson.GetBytes(plan.bodyForUpstream, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, plan.bodyForUpstream)
	}
	if gjson.GetBytes(plan.bodyForUpstream, "output_config.effort").Exists() {
		t.Fatalf("output_config.effort should be removed after downgrade: %s", plan.bodyForUpstream)
	}
	if gjson.GetBytes(plan.bodyForUpstream, "messages.0.content.#(type==\"thinking\")").Exists() {
		t.Fatalf("missing thinking history was synthesized: %s", plan.bodyForUpstream)
	}
}
