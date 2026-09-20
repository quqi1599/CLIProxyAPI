package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	codexWorkBuddyToolHistoryMessageLimit      = 240
	codexWorkBuddyToolHistoryInteractionLimit  = 8
	codexWorkBuddyToolHistoryDeclaredToolLimit = 8
	codexWorkBuddyToolHistoryPayloadLimit      = 2 << 20
)

type codexToolHistoryStats struct {
	messageCount       int
	toolCount          int
	declaredToolCount  int
	interactionCount   int
	complexToolSurface bool
}

func rejectLargeCodexToolHistory(ctx context.Context, body []byte, metadata map[string]any, auth *cliproxyauth.Auth) error {
	if !codexToolHistoryClient(metadata) || codexNativeResponsesAuth(auth) {
		return nil
	}
	stats := codexToolHistoryStatsFromRequest(body, metadata)
	if !largeCodexToolHistory(stats, len(body)) {
		return nil
	}

	fields := map[string]any{
		"event":                  "codex_tool_history_guard",
		"client_profile":         metadataString(metadata, cliproxyexecutor.ClientProfileMetadataKey),
		"message_count":          stats.messageCount,
		"tool_count":             stats.toolCount,
		"declared_tool_count":    stats.declaredToolCount,
		"tool_interaction_count": stats.interactionCount,
		"payload_bytes":          len(body),
		"native_responses":       false,
	}
	if stats.complexToolSurface {
		fields["complex_tool_surface"] = true
	}
	helpers := helps.LogWithRequestID(ctx)
	helpers.WithFields(fields).Warn("codex request rejected before upstream: non-native route cannot safely carry long tool history")
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: "request_feature_unsupported",
		msg:       codexLargeToolHistoryUserMessage(metadataString(metadata, cliproxyexecutor.ClientProfileMetadataKey)),
	}
}

func codexToolHistoryClient(metadata map[string]any) bool {
	switch strings.ToLower(strings.TrimSpace(metadataString(metadata, cliproxyexecutor.ClientProfileMetadataKey))) {
	case "workbuddy", "codex", "codex_cli", "codex_tui":
		return true
	default:
		return false
	}
}

func codexNativeResponsesAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		switch strings.ToLower(strings.TrimSpace(auth.Attributes["native_responses"])) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	baseURL := ""
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if baseURL == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Hostname())) {
	case "api.openai.com", "chatgpt.com", "chat.openai.com":
		return true
	default:
		return false
	}
}

func codexToolHistoryStatsFromRequest(body []byte, metadata map[string]any) codexToolHistoryStats {
	stats := codexToolHistoryStats{
		messageCount:      metadataInt(metadata, cliproxyexecutor.MessageCountMetadataKey),
		toolCount:         metadataInt(metadata, cliproxyexecutor.ToolCountMetadataKey),
		declaredToolCount: metadataInt(metadata, cliproxyexecutor.DeclaredToolCountMetadataKey),
		interactionCount:  metadataInt(metadata, cliproxyexecutor.ToolInteractionCountMetadataKey),
	}
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		if stats.messageCount == 0 {
			stats.messageCount = len(input.Array())
		}
		if stats.interactionCount == 0 {
			for _, item := range input.Array() {
				itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
				if strings.Contains(itemType, "tool") || strings.Contains(itemType, "function_call") || strings.Contains(itemType, "custom_tool") {
					stats.interactionCount++
				}
			}
		}
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		if stats.declaredToolCount == 0 {
			stats.declaredToolCount = len(tools.Array())
		}
		if stats.toolCount == 0 {
			stats.toolCount = len(tools.Array())
		}
		for _, tool := range tools.Array() {
			typ := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
			if strings.Contains(typ, "namespace") || strings.Contains(typ, "custom") || strings.Contains(typ, "mcp") || strings.Contains(typ, "function") {
				stats.complexToolSurface = true
			}
		}
	}
	toolTypes := strings.ToLower(metadataString(metadata, cliproxyexecutor.ToolShapeTypesMetadataKey))
	if strings.Contains(toolTypes, "namespace") || strings.Contains(toolTypes, "custom_tool") || strings.Contains(toolTypes, "mcp") || strings.Contains(toolTypes, "function_call") {
		stats.complexToolSurface = true
	}
	return stats
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

func largeCodexToolHistory(stats codexToolHistoryStats, payloadBytes int) bool {
	if stats.messageCount < codexWorkBuddyToolHistoryMessageLimit {
		return false
	}
	if stats.interactionCount >= codexWorkBuddyToolHistoryInteractionLimit || stats.declaredToolCount >= codexWorkBuddyToolHistoryDeclaredToolLimit || stats.toolCount >= codexWorkBuddyToolHistoryInteractionLimit {
		return true
	}
	return stats.complexToolSurface && payloadBytes >= codexWorkBuddyToolHistoryPayloadLimit
}

func codexLargeToolHistoryUserMessage(profile string) string {
	profile = strings.ToLower(strings.TrimSpace(profile))
	client := "当前 WorkBuddy/Codex"
	if profile == "codex" || profile == "codex_cli" || profile == "codex_tui" {
		client = "当前 Codex"
	}
	return "request_feature_unsupported: codex_tool_history_too_large. " + client + " 对话已累积较长的工具调用历史，当前非原生 Responses/tool calls 路由无法安全承载，已停止向上游发送并不会继续轮换重试。请新建会话，或先把历史工具调用、MCP/文件工具结果压缩成普通文本摘要；也可以切换到原生支持 Responses 和 tool calls 的渠道后重试。"
}
