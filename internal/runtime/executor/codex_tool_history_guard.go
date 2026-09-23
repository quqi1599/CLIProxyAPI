package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func rejectLargeCodexToolHistory(ctx context.Context, body []byte, metadata map[string]any, auth *cliproxyauth.Auth) error {
	if !cliproxyauth.RequiresNativeResponsesToolHistory(metadata, body) || cliproxyauth.SupportsNativeResponses(auth) {
		return nil
	}

	fields := map[string]any{
		"event":                  "codex_tool_history_guard",
		"client_profile":         metadataString(metadata, cliproxyexecutor.ClientProfileMetadataKey),
		"message_count":          metadataInt(metadata, cliproxyexecutor.MessageCountMetadataKey),
		"tool_count":             metadataInt(metadata, cliproxyexecutor.ToolCountMetadataKey),
		"declared_tool_count":    metadataInt(metadata, cliproxyexecutor.DeclaredToolCountMetadataKey),
		"tool_interaction_count": metadataInt(metadata, cliproxyexecutor.ToolInteractionCountMetadataKey),
		"payload_bytes":          len(body),
		"native_responses":       false,
	}
	helpers := helps.LogWithRequestID(ctx)
	helpers.WithFields(fields).Warn("codex request rejected before upstream: route has no verified or declared native Responses support for long tool history")
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: "request_feature_unsupported",
		msg:       codexLargeToolHistoryUserMessage(metadataString(metadata, cliproxyexecutor.ClientProfileMetadataKey)),
	}
}

func metadataInt(metadata map[string]any, key string) int {
	raw := metadata[key]
	switch value := raw.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

func codexLargeToolHistoryUserMessage(profile string) string {
	profile = strings.ToLower(strings.TrimSpace(profile))
	client := "当前 WorkBuddy/Codex"
	if profile == "codex" || profile == "codex_cli" || profile == "codex_tui" {
		client = "当前 Codex"
	}
	return "request_feature_unsupported: codex_tool_history_too_large. " + client + " 对话已累积较长的工具调用历史，当前路由未确认或未声明支持原生 Responses/tool calls，已停止向上游发送且不会原样轮换重试。请新建会话，或先把历史工具调用、MCP/文件工具结果压缩成普通文本摘要；也可以切换到已验证支持该能力的渠道。管理员应先验证渠道兼容性，再显式声明其原生 Responses 能力。"
}
