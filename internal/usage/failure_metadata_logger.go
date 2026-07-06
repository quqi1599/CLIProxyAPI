package usage

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const failureMetadataMaxStringLen = 160

func init() {
	coreusage.RegisterPlugin(&FailureMetadataLogger{})
}

// FailureMetadataLogger emits a safe structured log for failed upstream attempts.
// It never logs request bodies, response bodies, auth IDs, API keys, headers, or raw error text.
type FailureMetadataLogger struct{}

// HandleUsage implements coreusage.Plugin.
func (p *FailureMetadataLogger) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !record.Failed {
		return
	}

	attempt := coreusage.RequestAttemptFromContext(ctx)
	shape := coreusage.RequestShapeFromContext(ctx)
	messageCount := record.MessageCount
	if messageCount <= 0 {
		messageCount = shape.MessageCount
	}
	toolCount := record.ToolCount
	if toolCount <= 0 {
		toolCount = shape.ToolCount
	}
	attemptCount := record.AttemptNo
	if attemptCount <= 0 {
		attemptCount = attempt.AttemptNo
	}

	status := failureMetadataStatus(record)
	errorCode := safeFailureMetadataString(record.ErrorCode)
	if errorCode == "" {
		errorCode = safeFailureMetadataString(record.Fail.ErrorCode)
	}
	reasoningEffort := safeFailureMetadataString(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = safeFailureMetadataString(coreusage.ReasoningEffortFromContext(ctx))
	}

	model := safeFailureMetadataString(record.Alias)
	if model == "" {
		model = safeFailureMetadataString(record.Model)
	}

	normalizedStatus, normalizedErrorType, normalizedErrorCode := normalizeFailureMetadataError(status, errorCode)
	fields := log.Fields{
		"event":            "failure_metadata",
		"failure_class":    classifyFailureMetadata(status, errorCode),
		"model":            model,
		"endpoint_method":  safeFailureMetadataString(internallogging.GetEndpointMethod(ctx)),
		"endpoint_path":    safeFailureMetadataString(internallogging.GetEndpointPath(ctx)),
		"message_count":    messageCount,
		"tool_count":       toolCount,
		"reasoning_effort": reasoningEffort,
		"attempt_count":    attemptCount,
		"duration_ms":      durationMilliseconds(record.Latency),
	}
	if normalizedStatus > 0 {
		fields["normalized_status"] = normalizedStatus
	}
	if normalizedErrorType != "" {
		fields["error_type"] = normalizedErrorType
	}
	if normalizedErrorCode != "" {
		fields["error_code"] = normalizedErrorCode
	}
	if status > 0 {
		fields["upstream_status"] = status
		fields["status_code"] = status
	}
	if errorCode != "" {
		fields["upstream_error_code"] = errorCode
	}
	if requestID := safeFailureMetadataString(resolveFailureMetadataRequestID(ctx, record, attempt)); requestID != "" {
		fields["request_id"] = requestID
	}
	if authIndex := safeFailureMetadataString(record.AuthIndex); authIndex != "" {
		fields["auth_index"] = authIndex
	}
	if routingGroup := safeFailureMetadataString(coreusage.RoutingGroupFromContext(ctx)); routingGroup != "" {
		fields["routing_group"] = routingGroup
	}
	addFailureToolShapeFields(fields, coreusage.ToolShapeFromContext(ctx))
	addFailureDiagnosticFields(fields, coreusage.FailureDiagnosticFromContext(ctx))

	log.WithFields(fields).Warn("failure_metadata")
}

func normalizeFailureMetadataError(status int, errorCode string) (int, string, string) {
	code := strings.Trim(strings.ToLower(strings.TrimSpace(errorCode)), `"'(),:;[]{}<>`)
	if isFailureMetadataContentSafetyCode(code) {
		return http.StatusBadRequest, "invalid_request_error", "content_policy_violation"
	}
	if status <= 0 {
		return 0, "", ""
	}
	switch status {
	case http.StatusUnauthorized:
		return status, "authentication_error", "invalid_api_key"
	case http.StatusForbidden:
		return status, "permission_error", "insufficient_quota"
	case http.StatusTooManyRequests:
		return status, "rate_limit_error", "rate_limit_exceeded"
	case http.StatusNotFound:
		return status, "invalid_request_error", "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			return status, "server_error", "internal_server_error"
		}
		if status >= http.StatusBadRequest {
			if code != "" {
				return status, "invalid_request_error", code
			}
			return status, "invalid_request_error", "invalid_request_error"
		}
	}
	return status, "", code
}

func isFailureMetadataContentSafetyCode(code string) bool {
	switch code {
	case "1026", "1027", "1301", "content_policy_violation":
		return true
	default:
		return false
	}
}

func addFailureToolShapeFields(fields log.Fields, shape coreusage.ToolShape) {
	if len(fields) == 0 || !shape.HasData() {
		return
	}
	if shape.DeclaredToolCount > 0 {
		fields["declared_tool_count"] = shape.DeclaredToolCount
	}
	if shape.InteractionCount > 0 {
		fields["tool_interaction_count"] = shape.InteractionCount
	}
	if shape.MCPToolCount > 0 {
		fields["mcp_tool_count"] = shape.MCPToolCount
	}
	if shape.BuiltinToolCount > 0 {
		fields["builtin_tool_count"] = shape.BuiltinToolCount
	}
	if shape.ToolTypes != "" {
		fields["tool_types"] = safeFailureMetadataString(shape.ToolTypes)
	}
	if shape.ToolNameHashes != "" {
		fields["tool_name_hashes"] = safeFailureMetadataString(shape.ToolNameHashes)
	}
}

func addFailureDiagnosticFields(fields log.Fields, diag coreusage.FailureDiagnostic) {
	if len(fields) == 0 || !diag.HasData() {
		return
	}
	if diag.CompatKind != "" {
		fields["compat_kind"] = safeFailureMetadataString(diag.CompatKind)
	}
	if diag.CompatMapping != "" {
		fields["compat_mapping"] = safeFailureMetadataString(diag.CompatMapping)
	}
	if diag.MessageRoleSequence != "" {
		fields["message_role_sequence"] = safeFailureMetadataString(diag.MessageRoleSequence)
	}
	if diag.MessageContentKinds != "" {
		fields["message_content_kinds"] = safeFailureMetadataString(diag.MessageContentKinds)
	}
	if diag.InputItemTypes != "" {
		fields["input_item_types"] = safeFailureMetadataString(diag.InputItemTypes)
	}
	if diag.ToolChoiceType != "" {
		fields["tool_choice_type"] = safeFailureMetadataString(diag.ToolChoiceType)
	}
	if diag.ThinkingType != "" {
		fields["thinking_type"] = safeFailureMetadataString(diag.ThinkingType)
	}
	if diag.ResponseFormatType != "" {
		fields["response_format_type"] = safeFailureMetadataString(diag.ResponseFormatType)
	}
	if diag.ParallelToolCalls != "" {
		fields["parallel_tool_calls"] = safeFailureMetadataString(diag.ParallelToolCalls)
	}
	if diag.AssistantToolCalls > 0 {
		fields["assistant_tool_call_messages"] = diag.AssistantToolCalls
	}
	if diag.ToolResultMessages > 0 {
		fields["tool_result_messages"] = diag.ToolResultMessages
	}
	if diag.ReasoningMessages > 0 {
		fields["reasoning_messages"] = diag.ReasoningMessages
	}
	if diag.MaxContentParts > 0 {
		fields["max_content_parts"] = diag.MaxContentParts
	}
}

func failureMetadataStatus(record coreusage.Record) int {
	if record.ProviderStatusCode > 0 {
		return record.ProviderStatusCode
	}
	if record.Fail.StatusCode > 0 {
		return record.Fail.StatusCode
	}
	return 0
}

func resolveFailureMetadataRequestID(ctx context.Context, record coreusage.Record, attempt coreusage.RequestAttempt) string {
	if requestID := strings.TrimSpace(record.RequestID); requestID != "" {
		return requestID
	}
	if requestID := strings.TrimSpace(attempt.RequestID); requestID != "" {
		return requestID
	}
	return internallogging.GetRequestID(ctx)
}

func durationMilliseconds(latency time.Duration) int64 {
	if latency <= 0 {
		return 0
	}
	return latency.Milliseconds()
}

func classifyFailureMetadata(status int, code string) string {
	normalizedCode := strings.ToLower(strings.TrimSpace(code))
	switch {
	case strings.Contains(normalizedCode, "empty"):
		return "empty_response"
	case strings.Contains(normalizedCode, "transient_routing"):
		return "transient_routing"
	case strings.Contains(normalizedCode, "timeout") || strings.Contains(normalizedCode, "deadline"):
		return "timeout"
	case strings.Contains(normalizedCode, "rate_limit"):
		return "rate_limit"
	case strings.Contains(normalizedCode, "quota") || strings.Contains(normalizedCode, "insufficient_balance"):
		return "quota"
	case strings.Contains(normalizedCode, "unauthor") || strings.Contains(normalizedCode, "invalid_api_key"):
		return "auth"
	case strings.Contains(normalizedCode, "permission") || strings.Contains(normalizedCode, "forbidden"):
		return "permission"
	case strings.Contains(normalizedCode, "model_not") || strings.Contains(normalizedCode, "model_unsupported"):
		return "model_unavailable"
	case strings.Contains(normalizedCode, "api_error") || strings.Contains(normalizedCode, "internal_server_error"):
		return "upstream_api_error"
	}

	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status == http.StatusUnauthorized:
		return "auth"
	case status == http.StatusPaymentRequired:
		return "quota"
	case status == http.StatusForbidden:
		return "permission"
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return "timeout"
	case status == http.StatusNotFound:
		return "model_unavailable"
	case status >= http.StatusInternalServerError:
		return "upstream_5xx"
	case status >= http.StatusBadRequest:
		return "request_4xx"
	default:
		return "unknown"
	}
}

func safeFailureMetadataString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if looksLikeFailureMetadataSecret(value) {
		return "[redacted]"
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if len(value) <= failureMetadataMaxStringLen {
		return value
	}
	return value[:failureMetadataMaxStringLen] + "...[truncated " + strconv.Itoa(len(value)-failureMetadataMaxStringLen) + " bytes]"
}

func looksLikeFailureMetadataSecret(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "bearer ") ||
		strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(lower, "sk_") ||
		strings.Contains(lower, "api_key=") ||
		strings.Contains(lower, "apikey=") ||
		strings.Contains(lower, "authorization:")
}
