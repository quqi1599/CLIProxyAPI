package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	contentPolicyViolationErrorCode  = "content_policy_violation"
	contentSafetyInputDirection      = "input"
	contentSafetyInputImageDirection = "input_image"
	contentSafetyOutputDirection     = "output"
)

// UserFacingContentSafetyMessage returns a deterministic message for upstream safety rejections.
func UserFacingContentSafetyMessage(direction string) string {
	switch direction {
	case contentSafetyInputImageDirection:
		return "有敏感内容，请勿重复尝试。系统已拦截本次请求，检测类型：输入图片或多模态内容触发内容安全策略。请替换、移除或改写相关内容后再提交；重复提交相同内容不会提高成功率，只会继续被拦截。"
	case contentSafetyOutputDirection:
		return "有敏感内容，请勿重复尝试。系统已拦截本次请求，检测类型：模型输出触发内容安全策略。请调整提示词或降低敏感表达后重新提交；重复提交相同内容不会提高成功率，只会继续被拦截。"
	default:
		return "有敏感内容，请勿重复尝试。系统已拦截本次请求，检测类型：输入内容触发内容安全策略。请删除或改写相关敏感内容后再提交；重复提交相同内容不会提高成功率，只会继续被拦截。"
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

	if strings.Contains(lower, "output data may contain inappropriate content") {
		return contentSafetyOutputDirection, true
	}
	if strings.Contains(lower, "data_inspection_failed") ||
		strings.Contains(lower, "datainspectionfailed") ||
		strings.Contains(lower, "input data may contain inappropriate content") {
		return contentSafetyInputDirection, true
	}

	if strings.Contains(lower, "input new_sensitive") {
		if isInputImageContentSafetyText(lower) {
			return contentSafetyInputImageDirection, true
		}
		return contentSafetyInputDirection, true
	}
	if strings.Contains(lower, "output new_sensitive") {
		return contentSafetyOutputDirection, true
	}
	if strings.Contains(lower, "new_sensitive") && strings.Contains(lower, "1026") {
		if isInputImageContentSafetyText(lower) {
			return contentSafetyInputImageDirection, true
		}
		return contentSafetyInputDirection, true
	}
	if strings.Contains(lower, "new_sensitive") && strings.Contains(lower, "1027") {
		return contentSafetyOutputDirection, true
	}
	if isContentSafety1301Text(lower) {
		return contentSafetyInputDirection, true
	}
	if isImageGenerationSafetyRefusalText(lower) {
		return contentSafetyInputDirection, true
	}
	if isGenericContentSafetyText(lower) {
		return contentSafetyInputDirection, true
	}
	return "", false
}

func isInputImageContentSafetyText(lower string) bool {
	return strings.Contains(lower, "image is sensitive") ||
		strings.Contains(lower, "image content is sensitive") ||
		(strings.Contains(lower, "content[") && strings.Contains(lower, "image") && strings.Contains(lower, "sensitive")) ||
		(strings.Contains(lower, "image") && strings.Contains(lower, "new_sensitive") && strings.Contains(lower, "1026"))
}

func isGenericContentSafetyText(lower string) bool {
	if strings.Contains(lower, "content_policy_violation") {
		return true
	}
	if strings.Contains(lower, "有敏感内容") ||
		strings.Contains(lower, "敏感内容，请勿重复") ||
		(strings.Contains(lower, "敏感内容") && strings.Contains(lower, "请勿重复")) ||
		(strings.Contains(lower, "敏感") && strings.Contains(lower, "请勿重复请求")) ||
		(strings.Contains(lower, "敏感") && strings.Contains(lower, "请勿重复尝试")) {
		return true
	}
	if strings.Contains(lower, "内容安全") ||
		(strings.Contains(lower, "安全策略") && strings.Contains(lower, "触发")) ||
		(strings.Contains(lower, "安全策略") && strings.Contains(lower, "拦截")) {
		return true
	}
	return false
}

func isImageGenerationSafetyRefusalText(lower string) bool {
	if !strings.Contains(lower, "upstream returned text instead of image output") {
		return false
	}
	if strings.Contains(lower, "不能帮助生成") ||
		strings.Contains(lower, "无法帮助生成") ||
		strings.Contains(lower, "不能协助生成") ||
		strings.Contains(lower, "无法协助生成") ||
		strings.Contains(lower, "不能生成") ||
		strings.Contains(lower, "无法生成") ||
		strings.Contains(lower, "can't help generate") ||
		strings.Contains(lower, "cannot help generate") ||
		strings.Contains(lower, "can't assist with generating") ||
		strings.Contains(lower, "cannot assist with generating") {
		return true
	}
	if strings.Contains(lower, "安全版本") ||
		strings.Contains(lower, "safe version") ||
		strings.Contains(lower, "policy") ||
		strings.Contains(lower, "safety") {
		return true
	}
	return false
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
