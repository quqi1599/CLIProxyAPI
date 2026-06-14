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
	return "当前请求的上下文过长，或包含当前模型/路由不支持的工具调用能力，无法安全转发。请清理或压缩对话历史，减少历史工具调用/MCP 工具结果，或切换到原生支持该能力的模型后重试；原样重复提交不会提高成功率。"
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
	return ErrorDetail{
		Message: UserFacingRequestFeatureUnsupportedMessage(),
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
	if strings.Contains(lower, "does not support") &&
		(strings.Contains(lower, "anthropic compatibility") ||
			strings.Contains(lower, "server tool type") ||
			strings.Contains(lower, "output_config.format")) {
		return true
	}
	return false
}
