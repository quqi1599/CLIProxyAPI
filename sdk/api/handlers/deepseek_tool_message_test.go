package handlers

import (
	"strings"
	"testing"
)

func TestDeepSeekCurrentToolMessageUsesChineseNames(t *testing.T) {
	msg := userFacingDeepSeekResponsesNonFunctionToolsMessage("request_feature_unsupported: deepseek_responses_unsupported_tools. 当前 DeepSeek 通道无法执行这些工具：网页搜索(web_search), 文件搜索(file_search), mcp。请切换模型。")
	for _, want := range []string{"联网搜索", "文件搜索", "外部扩展工具"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %s: %s", want, msg)
		}
	}
	for _, bad := range []string{"web_search", "file_search", "mcp", "MCP", "CPA", "Codex"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("internal term %s: %s", bad, msg)
		}
	}
}
