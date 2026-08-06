package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	requestFeatureUnsupportedErrorCode = "request_feature_unsupported"
	requestFeatureUnsupportedErrorType = "invalid_request_error"
)

// UserFacingRequestFeatureUnsupportedMessage returns the normalized client-facing message for unsupported request shapes.
func UserFacingRequestFeatureUnsupportedMessage() string {
	return "当前请求的历史工具调用过多、上下文过大，或包含当前模型/路由不支持的工具能力，当前 Claude 兼容路由无法安全承载并转发。请新开会话，或将历史工具调用/MCP 工具结果压缩成普通文本摘要，减少工具/联网/MCP 使用；也可以切换到原生支持该能力的 Claude 路由后重试。原样重复提交不会提高成功率。"
}

func userFacingOpenAICompatToolHistoryMessage() string {
	return "当前 GPT/OpenAI-compatible 路由检测到历史工具调用过多、文件工具结果过多或上下文过大，继续原样转发会显著拖慢或中断请求。请新开会话，或将历史工具调用/文件结果压缩成普通文本摘要，减少重复文件提交；也可以切换到更适合长文件上下文的模型后重试。原样重复提交不会提高成功率。"
}

func userFacingDeepSeekChatJSONSchemaMessage() string {
	return "当前选择的 DeepSeek Chat 接口不支持本次 Codex 请求使用的“结构化 JSON 输出”。这是 DeepSeek 对该输出方式的兼容性限制，不是账号或余额问题。请在 Codex 的模型选择器中切换到原生 GPT 模型后重试；如果继续使用 DeepSeek，请改用普通 JSON 输出。原样重试不会成功。"
}

func userFacingDeepSeekOfficialImageInputMessage() string {
	return "当前选择的 DeepSeek 模型不支持本次 Codex 请求中的图片输入（包括对话历史里的图片）。这是模型能力兼容限制，不是账号或余额问题。请在 Codex 的模型选择器中切换到支持图片的原生 GPT 模型后重试；如果继续使用 DeepSeek，请移除当前和历史消息中的图片，仅保留文字。原样重试不会成功。"
}

func userFacingDeepSeekResponsesNonFunctionToolsMessage(errText string) string {
	toolNames := deepSeekUnsupportedToolChineseNames(errText)
	return "当前选择的 DeepSeek 模型无法完整支持 Codex 正在使用的工具：" + strings.Join(toolNames, "、") + "。这是 DeepSeek 对 Codex 扩展工具协议的兼容性限制，不是账号、余额或网络问题。请在 Codex 的模型选择器中切换到原生 GPT 模型后重试；如果必须继续使用 DeepSeek，请关闭这些扩展工具，仅保留普通函数调用。原样重试不会成功。"
}

func deepSeekUnsupportedToolChineseNames(errText string) []string {
	const marker = "工具类型："
	seen := make(map[string]struct{})
	toolNames := make([]string, 0, 4)
	for _, candidate := range requestFeatureUnsupportedErrorCandidates(errText) {
		start := strings.Index(candidate, marker)
		if start < 0 {
			continue
		}
		typeSummary := candidate[start+len(marker):]
		if end := strings.IndexAny(typeSummary, "。;\n\r"); end >= 0 {
			typeSummary = typeSummary[:end]
		}
		for _, toolType := range strings.Split(typeSummary, ",") {
			name := deepSeekToolChineseName(toolType)
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			toolNames = append(toolNames, name)
		}
	}
	if len(toolNames) == 0 {
		return []string{"联网搜索、工具分组等扩展工具"}
	}
	return toolNames
}

func deepSeekToolChineseName(toolType string) string {
	switch strings.ToLower(strings.TrimSpace(toolType)) {
	case "namespace":
		return "工具分组"
	case "web_search", "web_search_preview":
		return "联网搜索"
	case "file_search":
		return "文件搜索"
	case "computer", "computer_use", "computer_use_preview":
		return "电脑操作"
	case "code_interpreter":
		return "代码执行"
	case "mcp":
		return "MCP 外部工具"
	case "image_generation":
		return "图片生成"
	case "local_shell":
		return "本地命令"
	case "custom":
		return "自定义工具"
	case "missing":
		return "未标注类型的工具"
	default:
		return "其他扩展工具"
	}
}

func userFacingMiMoV25ProImageInputMessage() string {
	return "mimo-v2.5-pro 不支持图片输入，请将请求中的 model 明确改为 mimo-v2.5 后重试。系统不会自动替换模型，也不会重试或切换其他渠道。"
}

// NormalizeRequestFeatureUnsupportedStatus converts deterministic request-shape rejections to client errors.
func NormalizeRequestFeatureUnsupportedStatus(status int, errText string) int {
	if _, ok := requestFeatureUnsupportedErrorDetail(status, errText); !ok {
		return status
	}
	return http.StatusBadRequest
}

// BuildRequestFeatureUnsupportedErrorBody builds a normalized OpenAI-style error body for unsupported request shapes.
func BuildRequestFeatureUnsupportedErrorBody(status int, errText string) ([]byte, bool) {
	detail, ok := requestFeatureUnsupportedErrorDetail(status, errText)
	if !ok {
		return nil, false
	}
	payload, err := json.Marshal(ErrorResponse{Error: detail})
	if err != nil {
		return []byte(`{"error":{"message":"request feature unsupported","type":"invalid_request_error","code":"request_feature_unsupported"}}`), true
	}
	return payload, true
}

func requestFeatureUnsupportedErrorDetail(status int, errText string) (ErrorDetail, bool) {
	if !IsRequestFeatureUnsupportedError(status, errText) {
		return ErrorDetail{}, false
	}
	message := UserFacingRequestFeatureUnsupportedMessage()
	for _, candidate := range requestFeatureUnsupportedErrorCandidates(errText) {
		switch {
		case hasOpenAICompatToolHistorySignal(candidate):
			message = userFacingOpenAICompatToolHistoryMessage()
		case hasDeepSeekChatJSONSchemaSignal(candidate):
			message = userFacingDeepSeekChatJSONSchemaMessage()
		case hasDeepSeekOfficialImageInputSignal(candidate):
			message = userFacingDeepSeekOfficialImageInputMessage()
		case hasDeepSeekResponsesNonFunctionToolsSignal(candidate):
			message = userFacingDeepSeekResponsesNonFunctionToolsMessage(errText)
		case hasMiMoV25ProImageInputSignal(candidate):
			message = userFacingMiMoV25ProImageInputMessage()
		}
	}
	return ErrorDetail{
		Message: message,
		Type:    requestFeatureUnsupportedErrorType,
		Code:    requestFeatureUnsupportedErrorCode,
	}, true
}

// IsRequestFeatureUnsupportedError reports whether the upstream error matches an unsupported request-shape rejection.
func IsRequestFeatureUnsupportedError(status int, errText string) bool {
	if status > 0 && status < http.StatusBadRequest {
		return false
	}
	for _, candidate := range requestFeatureUnsupportedErrorCandidates(errText) {
		if hasRequestFeatureUnsupportedSignal(candidate) {
			return true
		}
	}
	return false
}

func requestFeatureUnsupportedErrorCandidates(errText string) []string {
	trimmed := strings.TrimSpace(errText)
	if trimmed == "" {
		return nil
	}

	candidates := []string{trimmed}
	if !json.Valid([]byte(trimmed)) {
		return candidates
	}

	for _, path := range []string{
		"error.message",
		"error.code",
		"error.type",
		"message",
		"detail",
		"code",
		"type",
	} {
		value := strings.TrimSpace(gjson.Get(trimmed, path).String())
		if value != "" {
			candidates = append(candidates, value)
		}
	}
	return candidates
}

func hasRequestFeatureUnsupportedSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}

	if strings.Contains(lower, requestFeatureUnsupportedErrorCode) {
		return true
	}
	if strings.Contains(lower, "large_claude_tool_history") {
		return true
	}
	if hasOpenAICompatToolHistorySignal(lower) {
		return true
	}
	if hasDeepSeekOfficialImageInputSignal(lower) {
		return true
	}
	if hasMiMoV25ProImageInputSignal(lower) {
		return true
	}
	if strings.Contains(lower, "does not support") &&
		(strings.Contains(lower, "anthropic compatibility") ||
			strings.Contains(lower, "server tool type") ||
			strings.Contains(lower, "output_config.format")) {
		return true
	}
	return false
}

func hasOpenAICompatToolHistorySignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "large_openai_tool_history")
}

func hasDeepSeekChatJSONSchemaSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_chat_json_schema")
}

func hasDeepSeekOfficialImageInputSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_official_image_input")
}

func hasDeepSeekResponsesNonFunctionToolsSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_responses_non_function_tools")
}

func hasMiMoV25ProImageInputSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "mimo_v2_5_pro_image_input")
}
