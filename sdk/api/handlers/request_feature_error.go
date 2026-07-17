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

func userFacingDeepSeekOfficialImageInputMessage() string {
	return "当前 DeepSeek 官方 OpenAI Chat 路由不支持 image_url 图片内容，包括历史消息里的 image_url / input_image。请移除图片输入与图片历史，仅保留文本内容，或切换到原生支持图像输入的模型/路由后重试。原样重复提交不会提高成功率。"
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
		case hasDeepSeekOfficialImageInputSignal(candidate):
			message = userFacingDeepSeekOfficialImageInputMessage()
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

func hasDeepSeekOfficialImageInputSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "deepseek_official_image_input")
}

func hasMiMoV25ProImageInputSignal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "mimo_v2_5_pro_image_input")
}
