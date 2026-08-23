package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/tidwall/gjson"
)

const testAkoolDeepSeekBaseURL = "https://akool.com/interface/maas-backend/api/v1/llm/v1"

func TestNormalizeWorkBuddyAkoolDeepSeekChatPayloadFlattensTextPartsAndKeepsTools(t *testing.T) {
	payload := []byte(`{
		"model":"DeepSeek-V4-Flash",
		"messages":[
			{"role":"system","content":"Be concise"},
			{"role":"user","content":[{"type":"text","text":"first"},{"type":"input_text","text":"second"}]}
		],
		"tools":[{"type":"function","function":{"name":"echo","parameters":{"type":"object","properties":{"text":{"type":"string"}}}}}]
	}`)
	got, err := normalizeWorkBuddyAkoolDeepSeekChatPayload(
		context.Background(), payload, testAkoolDeepSeekBaseURL, "DeepSeek-V4-Flash", "/chat/completions", "workbuddy",
	)
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	if content := gjson.GetBytes(got, "messages.1.content").String(); content != "first\nsecond" {
		t.Fatalf("content = %q, want flattened text; body=%s", content, got)
	}
	if toolName := gjson.GetBytes(got, "tools.0.function.name").String(); toolName != "echo" {
		t.Fatalf("tool name = %q, want preserved; body=%s", toolName, got)
	}
}

func TestNormalizeWorkBuddyAkoolDeepSeekChatPayloadLeavesOtherClientsUnchanged(t *testing.T) {
	payload := []byte(`{"model":"DeepSeek-V4-Flash","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	got, err := normalizeWorkBuddyAkoolDeepSeekChatPayload(
		context.Background(), payload, testAkoolDeepSeekBaseURL, "DeepSeek-V4-Flash", "/chat/completions", "other-client",
	)
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("non-WorkBuddy payload changed: %s", got)
	}
}

func TestNormalizeWorkBuddyAkoolDeepSeekChatPayloadFallsBackForComplexTools(t *testing.T) {
	tools := make([]any, 0, workBuddyAkoolComplexToolDefinitions)
	for i := 0; i < workBuddyAkoolComplexToolDefinitions; i++ {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       fmt.Sprintf("tool_%d", i),
				"parameters": map[string]any{"type": "object"},
			},
		})
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"model":    "DeepSeek-V4-Flash",
		"messages": []any{map[string]any{"role": "user", "content": "help"}},
		"tools":    tools,
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	_, err := normalizeWorkBuddyAkoolDeepSeekChatPayload(
		context.Background(), payload, testAkoolDeepSeekBaseURL, "DeepSeek-V4-Flash", "/chat/completions", "workbuddy",
	)
	if err == nil {
		t.Fatal("normalize error = nil, want provider fallback")
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Scope != failurecontract.ScopeModel || !typed.Retryable || typed.StreamPhase != failurecontract.StreamPhaseBeforeOutput {
		t.Fatalf("failure = %#v, want retryable model-scoped pre-output failure", typed)
	}
	for _, want := range []string{"系统会尝试其他可用通道", "新建对话", "“工具调用”可以继续勾选", "OpenAI 原生 GPT 模型"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q missing %q", err.Error(), want)
		}
	}
}

func TestNormalizeWorkBuddyAkoolDeepSeekChatPayloadRejectsAttachmentsWithTutorial(t *testing.T) {
	payload := []byte(`{"model":"DeepSeek-V4-Flash","messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	_, err := normalizeWorkBuddyAkoolDeepSeekChatPayload(
		context.Background(), payload, testAkoolDeepSeekBaseURL, "DeepSeek-V4-Flash", "/chat/completions", "workbuddy",
	)
	if err == nil {
		t.Fatal("normalize error = nil, want attachment tutorial")
	}
	message := err.Error()
	for _, want := range []string{"模型设置", "取消勾选“图片输入”", "新建对话", "“工具调用”可以继续勾选", "OpenAI 原生 GPT 模型"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q missing %q", message, want)
		}
	}
	for _, internal := range []string{"CPA", "reasoning_content", "tool_result"} {
		if strings.Contains(message, internal) {
			t.Fatalf("message exposes internal term %q: %s", internal, message)
		}
	}
}

func TestNormalizeWorkBuddyAkoolDeepSeekChatPayloadRejectsOversizedToolHistoryWithTutorial(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": "start"}}
	for i := 0; i < workBuddyAkoolToolOutputMessages; i++ {
		callID := fmt.Sprintf("call_%d", i)
		messages = append(messages,
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id": callID, "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": strings.Repeat("x", 6*1024)},
		)
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"model":    "DeepSeek-V4-Flash",
		"messages": messages,
		"tools": []any{map[string]any{
			"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}},
		}},
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	if len(payload) < workBuddyAkoolToolHistoryPayloadBytes {
		t.Fatalf("payload bytes = %d, want at least %d", len(payload), workBuddyAkoolToolHistoryPayloadBytes)
	}
	_, err := normalizeWorkBuddyAkoolDeepSeekChatPayload(
		context.Background(), payload, testAkoolDeepSeekBaseURL, "DeepSeek-V4-Flash", "/chat/completions", "workbuddy",
	)
	if err == nil {
		t.Fatal("normalize error = nil, want tool-history tutorial")
	}
	message := err.Error()
	for _, want := range []string{"大量工具调用记录", "新建对话", "“工具调用”可以继续勾选", "关闭“推理模式”", "OpenAI 原生 GPT 模型"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q missing %q", message, want)
		}
	}
}

func TestIsWorkBuddyAkoolDeepSeekChatRoute(t *testing.T) {
	if !isWorkBuddyAkoolDeepSeekChatRoute(testAkoolDeepSeekBaseURL, "deepseek-v4-flash", "/chat/completions", "WorkBuddy") {
		t.Fatal("expected Akool WorkBuddy DeepSeek Chat route")
	}
	for _, tc := range []struct {
		baseURL, model, endpoint, profile string
	}{
		{"https://api.deepseek.com/v1", "deepseek-v4-flash", "/chat/completions", "workbuddy"},
		{testAkoolDeepSeekBaseURL, "gpt-5.6-sol", "/chat/completions", "workbuddy"},
		{testAkoolDeepSeekBaseURL, "deepseek-v4-flash", "/responses", "workbuddy"},
		{testAkoolDeepSeekBaseURL, "deepseek-v4-flash", "/chat/completions", "other"},
	} {
		if isWorkBuddyAkoolDeepSeekChatRoute(tc.baseURL, tc.model, tc.endpoint, tc.profile) {
			t.Fatalf("unexpected route match: %+v", tc)
		}
	}
}
