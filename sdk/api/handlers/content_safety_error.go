package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	contentPolicyViolationErrorCode = "content_policy_violation"
	contentSafetyInputDirection     = "input"
	contentSafetyOutputDirection    = "output"
)

// UserFacingContentSafetyMessage returns a deterministic message for upstream safety rejections.
func UserFacingContentSafetyMessage(direction string) string {
	switch direction {
	case contentSafetyOutputDirection:
		return "生成内容触发安全策略，请勿重复请求。请修改提示词后再请求。"
	default:
		return "有敏感内容，请勿重复请求。请修改内容后再请求。"
	}
}

// NormalizeContentSafetyStatus converts deterministic safety rejections to client errors.
func NormalizeContentSafetyStatus(status int, errText string) int {
	if _, ok := contentSafetyDirection(status, errText); !ok {
		return status
	}
	return http.StatusBadRequest
}

// BuildContentSafetyErrorBody builds a normalized OpenAI-style error body for safety rejections.
func BuildContentSafetyErrorBody(status int, errText string) ([]byte, bool) {
	direction, ok := contentSafetyDirection(status, errText)
	if !ok {
		return nil, false
	}
	payload, err := json.Marshal(ErrorResponse{Error: ErrorDetail{
		Message: UserFacingContentSafetyMessage(direction),
		Type:    "invalid_request_error",
		Code:    contentPolicyViolationErrorCode,
	}})
	if err == nil {
		return payload, true
	}
	return []byte(`{"error":{"message":"content safety rejection","type":"invalid_request_error","code":"content_policy_violation"}}`), true
}

func contentSafetyDirection(status int, errText string) (string, bool) {
	if status > 0 && status < http.StatusBadRequest {
		return "", false
	}
	for _, candidate := range contentSafetyErrorCandidates(errText) {
		if direction, ok := contentSafetySignalDirection(candidate); ok {
			return direction, true
		}
	}
	return "", false
}

func contentSafetyErrorCandidates(errText string) []string {
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

func contentSafetySignalDirection(text string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return "", false
	}

	normalized := strings.Trim(lower, `"'(),:;[]{}<>`)
	switch normalized {
	case "1026":
		return contentSafetyInputDirection, true
	case "1027":
		return contentSafetyOutputDirection, true
	case "1301":
		return contentSafetyInputDirection, true
	}

	if strings.Contains(lower, "input new_sensitive") {
		return contentSafetyInputDirection, true
	}
	if strings.Contains(lower, "output new_sensitive") {
		return contentSafetyOutputDirection, true
	}
	if strings.Contains(lower, "new_sensitive") && strings.Contains(lower, "1026") {
		return contentSafetyInputDirection, true
	}
	if strings.Contains(lower, "new_sensitive") && strings.Contains(lower, "1027") {
		return contentSafetyOutputDirection, true
	}
	if isContentSafety1301Text(lower) {
		return contentSafetyInputDirection, true
	}
	return "", false
}

func isContentSafety1301Text(lower string) bool {
	if !strings.Contains(lower, "1301") {
		return false
	}
	if strings.Contains(lower, "[1301]") || strings.Contains(lower, "(1301)") {
		return true
	}
	for _, marker := range []string{
		"content safety",
		"sensitive",
		"unsafe",
		"policy",
		"blocked",
		"high risk",
		"high-risk",
		"敏感",
		"安全",
		"高风险",
		"不合规",
		"违规",
		"审核",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
