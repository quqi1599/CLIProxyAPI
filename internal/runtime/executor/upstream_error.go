package executor

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

var safeUpstreamErrorIdentifiers = map[string]struct{}{
	"1000": {}, "1026": {}, "1027": {}, "1301": {},
	"authentication_error": {}, "auth_unavailable": {}, "billing_cycle_quota": {},
	"content_policy_violation": {}, "context_length_exceeded": {}, "context_too_large": {},
	"data_inspection_failed": {}, "datainspectionfailed": {}, "deadline_exceeded": {},
	"empty_upstream_response": {}, "failed_precondition": {}, "insufficient_balance": {},
	"insufficient_quota": {}, "invalid_api_key": {}, "invalid_function_arguments": {},
	"invalid_grant": {}, "invalid_request": {}, "invalid_request_error": {},
	"invalidargument": {}, "invalid_parameter": {}, "model_not_found": {},
	"model_not_supported": {}, "not_found": {}, "overloaded": {}, "overloaded_error": {},
	"permission_denied": {}, "permission_error": {}, "policy_denied": {},
	"previous_response_not_found": {}, "rate_limit": {}, "rate_limit_error": {},
	"rate_limit_exceeded": {}, "request_feature_unsupported": {}, "resource_exhausted": {},
	"server_error": {}, "thinking_signature_invalid": {}, "timeout": {}, "unauthenticated": {},
	"unknown": {}, "unavailable": {}, "upstream_response_too_large": {},
	"upstream_timeout": {}, "usage_limit_reached": {}, "websocket_connection_limit_reached": {},
}

func newUpstreamStatusErr(statusCode int, headers http.Header, contentType string, body []byte) statusErr {
	message, errorCode := safeUpstreamFailureMessage(contentType, body)
	var clonedHeaders http.Header
	if headers != nil {
		clonedHeaders = headers.Clone()
	}
	return statusErr{
		code:               statusCode,
		providerStatusCode: statusCode,
		msg:                message,
		errorCode:          errorCode,
		headers:            clonedHeaders,
	}
}

func safeUpstreamFailureMessage(contentType string, body []byte) (string, string) {
	errorCode, errorType, errorStatus := safeUpstreamIdentifiers(body)
	reasons := safeUpstreamFailureReasons(body, errorCode, errorType, errorStatus)
	parts := []string{"upstream request failed"}
	if len(reasons) > 0 {
		parts = append(parts, "reason="+strings.Join(reasons, ","))
	}
	if errorCode != "" {
		parts = append(parts, "error_code="+errorCode)
	}
	if errorType != "" && errorType != errorCode {
		parts = append(parts, "error_type="+errorType)
	}
	if errorStatus != "" && errorStatus != errorCode && errorStatus != errorType {
		parts = append(parts, "error_status="+errorStatus)
	}
	parts = append(parts, helps.SummarizeErrorBody(contentType, body))
	if errorCode == "" {
		errorCode = errorType
	}
	return strings.Join(parts, " "), errorCode
}

func safeUpstreamIdentifiers(body []byte) (string, string, string) {
	jsonBody := upstreamJSONErrorBody(body)
	return firstSafeUpstreamIdentifier(jsonBody,
			"error.code", "body.error.code", "code", "error.err_code", "error_code"),
		firstSafeUpstreamIdentifier(jsonBody,
			"error.type", "body.error.type", "type"),
		firstSafeUpstreamIdentifier(jsonBody,
			"error.status", "body.error.status", "status")
}

func upstreamJSONErrorBody(body []byte) []byte {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || gjson.Valid(trimmed) {
		return []byte(trimmed)
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < len("data:") || !strings.EqualFold(line[:len("data:")], "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload != "" && payload != "[DONE]" && gjson.Valid(payload) {
			return []byte(payload)
		}
	}
	return body
}

func firstSafeUpstreamIdentifier(body []byte, paths ...string) string {
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() {
			continue
		}
		candidate := strings.ToLower(strings.TrimSpace(value.String()))
		if candidate == "" {
			candidate = strings.ToLower(strings.TrimSpace(value.Raw))
		}
		if _, ok := safeUpstreamErrorIdentifiers[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func safeUpstreamFailureReasons(body []byte, errorCode, errorType, errorStatus string) []string {
	lower := strings.ToLower(strings.TrimSpace(string(body)))
	reasons := make([]string, 0, 8)
	add := func(reason string) {
		if reason == "" {
			return
		}
		for _, existing := range reasons {
			if existing == reason {
				return
			}
		}
		reasons = append(reasons, reason)
	}
	contains := func(patterns ...string) bool {
		for _, pattern := range patterns {
			if pattern != "" && strings.Contains(lower, pattern) {
				return true
			}
		}
		return false
	}
	identifierIs := func(candidates ...string) bool {
		for _, candidate := range candidates {
			if errorCode == candidate || errorType == candidate || errorStatus == candidate {
				return true
			}
		}
		return false
	}

	if len(body) == 0 {
		add("empty upstream response")
	}
	if identifierIs("model_not_found", "model_not_supported") || contains(
		"requested model is not supported", "requested model is unsupported", "requested model is unavailable",
		"requested model does not exist", "requested model is not available", "model is not supported",
		"model not supported", "model does not exist", "model not found", "unsupported model",
		"model unavailable", "not available for your plan", "not available for your account",
		"not available for this account", "not enabled for your account", "not enabled for this account",
		"does not have access to model", "model has been disabled", "模型不存在", "模型未开通", "模型不可用", "没有该模型权限",
	) {
		add("model_not_supported model unavailable")
	}
	if identifierIs("billing_cycle_quota", "insufficient_balance", "insufficient_quota", "usage_limit_reached") || contains(
		"usage limit", "billing cycle", "quota will be refreshed", "refreshed in the next cycle",
		"monthly quota", "insufficient balance", "balance insufficient", "quota exhausted", "quota_exhausted",
		"用量上限", "账期", "额度已用尽", "额度不足", "余额不足", "账户余额不足", "帐户余额不足",
	) {
		add("usage limit billing cycle quota will be refreshed")
	}
	if identifierIs("rate_limit", "rate_limit_error", "rate_limit_exceeded", "resource_exhausted") || contains(
		"rate limit", "rate_limit", "too many requests", "resource exhausted", "频率限制",
	) {
		add("rate limit")
	}
	if identifierIs("invalid_grant") || contains(`"error":"invalid_grant"`, `"code":"invalid_grant"`, `"error_code":"invalid_grant"`) {
		add("invalid_grant")
	}
	if contains("challenge-platform", "cf-mitigated", "cloudflare challenge") || strings.Contains(lower, "cloudflare") && strings.Contains(lower, "<html") {
		add("cloudflare challenge")
	}
	if identifierIs("context_length_exceeded", "context_too_large") || contains(
		"context window exceeds limit", "context window exceeded", "context length exceeded", "context length exceeds",
		"context_length_exceeded", "maximum context", "context too long", "too many tokens",
	) {
		add("context_length_exceeded")
	}
	if identifierIs("request_feature_unsupported") || contains(
		"request_feature_unsupported", "minimax anthropic compatibility does not support output_config.format",
	) {
		add("request_feature_unsupported")
	}
	if identifierIs("invalidargument", "invalid_parameter") || contains("invalidparameter", "invalid parameter") {
		add("invalidparameter")
	}
	if contains("out of supported range") {
		for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			if strings.Contains(lower, field) {
				add(field + " out of supported range")
				break
			}
		}
	}
	if identifierIs("previous_response_not_found") || contains("items are not persisted", "previous_response_not_found") {
		add("item with id not found items are not persisted when `store` is set to false")
	}
	if identifierIs("content_policy_violation", "data_inspection_failed", "datainspectionfailed", "1301") || contains(
		"request was rejected", "content blocked", "content_policy_violation", "data_inspection_failed",
		"data may contain inappropriate content", "有敏感内容", "敏感内容", "内容安全", "安全策略",
	) {
		add("content_policy_violation content blocked")
	}
	if identifierIs("1026") || contains("input new_sensitive") {
		add("input new_sensitive 1026")
	}
	if identifierIs("1027") || contains("output new_sensitive") {
		add("output new_sensitive 1027")
	}
	if identifierIs("1000") && contains("unknown error") {
		add("unknown error 1000")
	}
	if contains("deepseek_official_image_input") {
		add("deepseek_official_image_input")
	}
	if contains("large_claude_tool_history") {
		add("large_claude_tool_history")
	}
	if contains("thinking mode does not support this tool_choice") {
		add("thinking mode does not support this tool_choice")
	}
	if contains("invalid schema for function") && contains("null is not of type") && contains("array") {
		add("invalid schema for function null is not of type array")
	}
	if contains("no available key", "no available api key", "no available channel", "channel unavailable", "upstream unavailable", "provider unavailable", "no healthy upstream", "无可用 key", "无可用key", "无可用渠道", "渠道不可用", "上游不可用") {
		add("upstream unavailable")
	}
	if identifierIs("overloaded", "overloaded_error") || contains("upstream overloaded", "service overloaded") {
		add("upstream overloaded")
	}
	if identifierIs("unknown") {
		add(`{"status":"UNKNOWN"}`)
	}
	for _, pattern := range []string{
		"connection reset by peer", "broken pipe", "unexpected eof", "read: eof", "write: eof",
		"server closed idle connection", "use of closed network connection", "i/o timeout", "io timeout",
		"tls handshake timeout", "timeout awaiting response headers", "client timeout exceeded",
		"context deadline exceeded", "connection refused", "connection aborted",
	} {
		if strings.Contains(lower, pattern) {
			add(pattern)
			break
		}
	}
	return reasons
}
