package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// BuildErrorResponseBodyWithCause preserves typed local error contracts while
// retaining the caller's normalized error text for all other error families.
func BuildErrorResponseBodyWithCause(status int, errText string, cause error) []byte {
	if body, ok := buildRouteUnavailableErrorBody(status, cause); ok {
		return body
	}
	return BuildErrorResponseBody(status, errText)
}

// buildRouteUnavailableErrorBody preserves the local selection contract across
// the error writer's legacy string-only normalization. Plain upstream messages
// and unrelated provider failures must not acquire this code by text matching.
func buildRouteUnavailableErrorBody(status int, err error) ([]byte, bool) {
	if status != http.StatusServiceUnavailable {
		return nil, false
	}
	failure, ok := failurecontract.As(err)
	if !ok || failure.HTTPStatus != http.StatusServiceUnavailable || failure.SemanticCode != "auth_unavailable" ||
		failure.Kind != failurecontract.ProviderUnavailable || failure.Scope != failurecontract.ScopeProvider ||
		failure.StreamPhase != failurecontract.StreamPhaseBeforeOutput || failure.OutputCommitted {
		return nil, false
	}
	var cause *coreauth.Error
	if !errors.As(failure.Cause, &cause) || cause == nil || cause.Code != "auth_unavailable" || cause.HTTPStatus != http.StatusServiceUnavailable {
		return nil, false
	}
	body, errMarshal := json.Marshal(ErrorResponse{Error: ErrorDetail{
		Message: "auth_unavailable: requested route is temporarily unavailable",
		Type:    "server_error",
		Code:    "auth_unavailable",
	}})
	return body, errMarshal == nil
}
