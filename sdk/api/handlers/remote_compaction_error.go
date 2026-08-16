package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
)

var remoteCompactionErrorCodes = []string{
	"remote_compaction_context_management_unsupported",
	"remote_compaction_trigger_unsupported",
	"remote_compaction_unsupported",
	"compaction_context_unavailable",
	"compaction_route_unavailable",
	"compaction_upstream_error",
	"invalid_compaction_response",
	"invalid_compaction_stream",
}

func remoteCompactionErrorDetail(status int, errText string) (ErrorDetail, int, bool) {
	lower := strings.ToLower(errText)
	for _, code := range remoteCompactionErrorCodes {
		index := strings.Index(lower, code)
		if index < 0 {
			continue
		}
		message := strings.TrimSpace(errText)
		remainder := strings.TrimSpace(errText[index+len(code):])
		remainder = strings.TrimSpace(strings.TrimPrefix(remainder, ":"))
		if remainder != "" && !strings.HasPrefix(remainder, `"`) {
			message = remainder
		}
		normalizedStatus := remoteCompactionErrorStatus(code, status)
		errorType := "invalid_request_error"
		if normalizedStatus >= http.StatusInternalServerError {
			errorType = "server_error"
		}
		return ErrorDetail{Message: message, Type: errorType, Code: code}, normalizedStatus, true
	}
	return ErrorDetail{}, status, false
}

func remoteCompactionErrorStatus(code string, fallback int) int {
	switch code {
	case "remote_compaction_unsupported", "remote_compaction_trigger_unsupported",
		"remote_compaction_context_management_unsupported", "compaction_context_unavailable":
		return http.StatusBadRequest
	case "compaction_route_unavailable":
		return http.StatusServiceUnavailable
	case "compaction_upstream_error", "invalid_compaction_response", "invalid_compaction_stream":
		return http.StatusBadGateway
	default:
		return fallback
	}
}

// BuildRemoteCompactionErrorBody builds a stable OpenAI-style error body for
// the dedicated remote-compaction protocol family.
func BuildRemoteCompactionErrorBody(status int, errText string) ([]byte, bool) {
	detail, _, ok := remoteCompactionErrorDetail(status, errText)
	if !ok {
		return nil, false
	}
	payload, err := json.Marshal(ErrorResponse{Error: detail})
	if err != nil {
		return nil, false
	}
	return payload, true
}
