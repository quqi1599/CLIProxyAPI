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

func userFacingDeepSeekOfficialFileInputMessage() string {
	return "当前选择的 DeepSeek 模型不支持本次 Codex 请求中的文件输入。这是模型能力兼容限制，不是账号、余额或网络问题。请在 Codex 中切换到支持文件的原生 GPT 模型；如果继续使用 DeepSeek，请先把文件内容转成文本摘要。"
}

func userFacingWorkBuddyDeepSeekToolHistoryMessage() string {
	return "当前 WorkBuddy 对话已累积大量工具调用记录，DeepSeek 兼容接口无法继续完整接收。请点击“新建对话”后重新发送；“工具调用”可以继续勾选。若仍失败，请打开“模型设置”关闭“推理模式”，或切换到 OpenAI 原生 GPT 模型。这不是余额或网络问题。"
}

func userFacingWorkBuddyDeepSeekComplexToolsMessage() string {
	return "当前 DeepSeek 兼容通道无法接收 WorkBuddy 一次发送的整套复杂工具定义，系统会尝试其他可用通道。如果仍失败，请点击“新建对话”后重新发送；“工具调用”可以继续勾选，或切换到 OpenAI 原生 GPT 模型。这不是余额或网络问题。"
}

func userFacingWorkBuddyDeepSeekAttachmentMessage() string {
	return "当前 WorkBuddy 对话包含图片或文件内容，DeepSeek 兼容接口无法接收。请打开“模型设置”→取消勾选“图片输入”→点击“新建对话”后重新发送；“工具调用”可以继续勾选。如果必须处理图片或文件，请切换到 OpenAI 原生 GPT 模型。这不是余额或网络问题。"
}

func userFacingWorkBuddyDeepSeekContentFormatMessage() string {
	return "当前 WorkBuddy 对话包含 DeepSeek 无法识别的分段内容。请点击“新建对话”后重试；“工具调用”可以继续勾选。若仍失败，请打开“模型设置”关闭“推理模式”，或切换到 OpenAI 原生 GPT 模型。这不是余额或网络问题。"
}

func userFacingClaudeCodeDeepSeekToolHistoryMessage() string {
	return "当前 Claude Code 会话已累积大量工具调用记录，DeepSeek-艾俊 Flash 暂时无法可靠接收，系统将自动尝试其他 DeepSeek 通道。如果仍失败，请在 Claude Code 中新建会话，或切换到 OpenAI 原生 GPT 模型。这不是 API Key、余额或网络问题。"
}

func userFacingClaudeCodeDeepSeekComplexToolsMessage() string {
	return "当前 Claude Code 请求包含较多工具定义，DeepSeek-艾俊 Flash 暂时无法可靠接收，系统将自动尝试其他 DeepSeek 通道。如果仍失败，请在 Claude Code 中新建会话，或切换到 OpenAI 原生 GPT 模型。这不是 API Key、余额或网络问题。"
}

func userFacingClaudeCodeDeepSeekAttachmentMessage() string {
	return "当前 Claude Code 请求包含图片或文件内容，DeepSeek-艾俊 Flash 无法可靠接收，系统将自动尝试其他 DeepSeek 通道。如果仍失败，请移除图片或文件后新建 Claude Code 会话，或切换到 OpenAI 原生 GPT 模型。"
}

func userFacingClaudeCodeDeepSeekContentFormatMessage() string {
	return "当前 Claude Code 请求包含 DeepSeek-艾俊 Flash 无法可靠识别的分段内容，系统将自动尝试其他 DeepSeek 通道。如果仍失败，请在 Claude Code 中新建会话，或切换到 OpenAI 原生 GPT 模型。"
}

func userFacingDeepSeekResponsesNonFunctionToolsMessage(errText string) string {
	toolNames := deepSeekUnsupportedToolChineseNames(errText)
	return "DeepSeek V4 Pro 已支持函数调用、联网搜索和补丁应用，但当前 Codex 请求还使用了 DeepSeek 不支持的工具：" + strings.Join(toolNames, "、") + "。这是工具协议的兼容性限制，不是账号、余额或网络问题。请移除这些工具，或在 Codex 的模型选择器中切换到原生 GPT 模型后重试。"
}

func userFacingDeepSeekResponsesStateMessage() string {
	return "DeepSeek Responses API 当前不保存服务端会话状态，因此不能使用上一次响应 ID、服务端会话或存储响应。请让客户端重传必要的对话内容；如果 Codex 必须依赖这些状态能力，请切换到原生 GPT 模型。"
}

func userFacingDeepSeekFIMMessage(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "deepseek_fim_requires_openai_compat"):
		return "DeepSeek FIM 中间补全只能通过 OpenAI 兼容的 /v1/completions 调用，不能走 Anthropic API。系统会自动尝试其他可用的 DeepSeek OpenAI 兼容通道；如果仍看到此错误，说明当前没有可用的兼容通道，请改用普通对话请求或联系管理员配置 DeepSeek 官方 OpenAI 接口。"
	case strings.Contains(lower, "deepseek_fim_non_thinking_only"):
		return "DeepSeek FIM 中间补全仅支持非思考模式。请关闭思考并移除 reasoning / reasoning_effort 后重试。"
	case strings.Contains(lower, "deepseek_fim_max_tokens"):
		return "DeepSeek FIM 中间补全最多输出 4096 tokens，请降低 max_tokens 后重试。"
	default:
		return "DeepSeek FIM 中间补全需要在 /v1/completions 请求中提供 prompt，suffix 可选。"
	}
}

func deepSeekUnsupportedToolChineseNames(errText string) []string {
	markers := []string{"当前请求还包含不支持的：", "工具类型："}
	seen := make(map[string]struct{})
	toolNames := make([]string, 0, 4)
	for _, candidate := range requestFeatureUnsupportedErrorCandidates(errText) {
		start := -1
		marker := ""
		for _, current := range markers {
			if index := strings.Index(candidate, current); index >= 0 {
				start = index
				marker = current
				break
			}
		}
		if start < 0 || marker == "" {
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
	normalized := strings.ToLower(strings.TrimSpace(toolType))
	switch {
	case strings.Contains(normalized, "namespace"):
		return "工具分组"
	case strings.Contains(normalized, "file_search"):
		return "文件搜索"
	case strings.Contains(normalized, "code_interpreter"):
		return "代码执行"
	case strings.Contains(normalized, "computer"):
		return "电脑操作"
	case strings.Contains(normalized, "mcp"):
		return "MCP 外部工具"
	case strings.Contains(normalized, "custom"):
		return "自定义工具"
	}
	switch normalized {
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
		case hasClaudeCodeDeepSeekComplexToolsSignal(candidate):
			message = userFacingClaudeCodeDeepSeekComplexToolsMessage()
		case hasClaudeCodeDeepSeekToolHistorySignal(candidate):
			message = userFacingClaudeCodeDeepSeekToolHistoryMessage()
		case hasClaudeCodeDeepSeekAttachmentSignal(candidate):
			message = userFacingClaudeCodeDeepSeekAttachmentMessage()
		case hasClaudeCodeDeepSeekContentFormatSignal(candidate):
			message = userFacingClaudeCodeDeepSeekContentFormatMessage()
		case hasWorkBuddyDeepSeekComplexToolsSignal(candidate):
			message = userFacingWorkBuddyDeepSeekComplexToolsMessage()
		case hasWorkBuddyDeepSeekToolHistorySignal(candidate):
			message = userFacingWorkBuddyDeepSeekToolHistoryMessage()
		case hasWorkBuddyDeepSeekAttachmentSignal(candidate):
			message = userFacingWorkBuddyDeepSeekAttachmentMessage()
		case hasWorkBuddyDeepSeekContentFormatSignal(candidate):
			message = userFacingWorkBuddyDeepSeekContentFormatMessage()
		case hasOpenAICompatToolHistorySignal(candidate):
			message = userFacingOpenAICompatToolHistoryMessage()
		case hasDeepSeekChatJSONSchemaSignal(candidate):
			message = userFacingDeepSeekChatJSONSchemaMessage()
		case hasDeepSeekOfficialImageInputSignal(candidate):
			message = userFacingDeepSeekOfficialImageInputMessage()
		case hasDeepSeekOfficialFileInputSignal(candidate):
			message = userFacingDeepSeekOfficialFileInputMessage()
		case hasDeepSeekResponsesNonFunctionToolsSignal(candidate):
			message = userFacingDeepSeekResponsesNonFunctionToolsMessage(errText)
		case hasDeepSeekResponsesStateSignal(candidate):
			message = userFacingDeepSeekResponsesStateMessage()
		case hasDeepSeekFIMSignal(candidate):
			message = userFacingDeepSeekFIMMessage(candidate)
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
	if hasDeepSeekOfficialFileInputSignal(lower) || hasDeepSeekResponsesStateSignal(lower) || hasDeepSeekFIMSignal(lower) {
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

func hasDeepSeekOfficialFileInputSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_official_file_input")
}

func hasWorkBuddyDeepSeekToolHistorySignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "workbuddy_deepseek_tool_history_too_large")
}

func hasWorkBuddyDeepSeekComplexToolsSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "workbuddy_deepseek_akool_complex_tools")
}

func hasWorkBuddyDeepSeekAttachmentSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "workbuddy_deepseek_attachment_input")
}

func hasWorkBuddyDeepSeekContentFormatSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "workbuddy_deepseek_content_format")
}

func hasClaudeCodeDeepSeekToolHistorySignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "claude_code_deepseek_akool_tool_history")
}

func hasClaudeCodeDeepSeekComplexToolsSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "claude_code_deepseek_akool_complex_tools")
}

func hasClaudeCodeDeepSeekAttachmentSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "claude_code_deepseek_akool_attachment_input")
}

func hasClaudeCodeDeepSeekContentFormatSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "claude_code_deepseek_akool_content_format")
}

func hasDeepSeekResponsesNonFunctionToolsSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_responses_non_function_tools") ||
		strings.Contains(lower, "deepseek_responses_unsupported_tools")
}

func hasDeepSeekResponsesStateSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_responses_state")
}

func hasDeepSeekFIMSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_fim_")
}

func hasMiMoV25ProImageInputSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "mimo_v2_5_pro_image_input")
}
