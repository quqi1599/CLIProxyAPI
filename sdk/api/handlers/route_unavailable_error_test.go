package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRouteUnavailableErrorResponsePreservesCodeAndRetryAfter(t *testing.T) {
	wait := 1500 * time.Millisecond
	err := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: 503, OuterStatus: 503, ProviderCode: "auth_unavailable", SemanticCode: "auth_unavailable",
		SemanticType: "server_error", StreamPhase: failurecontract.StreamPhaseBeforeOutput,
		RetryAfter: &wait, Retryable: true, PublicMessage: "auth_unavailable: no auth available",
		Cause: &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: 503},
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	NewBaseAPIHandlers(nil, nil).WriteErrorResponse(c, executionErrorMessage(enrichAuthSelectionError(err, []string{"codex"}, "gpt-5.6-sol")))
	if recorder.Code != 503 || recorder.Header().Get("Retry-After") != "2" {
		t.Fatalf("status=%d Retry-After=%q, want 503/2", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "auth_unavailable" || payload.Error.Type != "server_error" {
		t.Fatalf("response=%s, want structured auth_unavailable", recorder.Body.String())
	}
}

func TestRouteUnavailableBodyDoesNotTrustUpstreamText(t *testing.T) {
	for _, err := range []error{
		errors.New("auth_unavailable: no auth available"),
		&failurecontract.Failure{HTTPStatus: 503, PublicMessage: "auth_unavailable: no auth available"},
		&failurecontract.Failure{
			Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider, HTTPStatus: 503,
			SemanticCode: "auth_unavailable", StreamPhase: failurecontract.StreamPhaseBeforeOutput,
			PublicMessage: "auth_unavailable: no auth available", Cause: errors.New("provider body"),
		},
	} {
		if _, ok := buildRouteUnavailableErrorBody(http.StatusServiceUnavailable, err); ok {
			t.Fatalf("trusted non-selection error: %T", err)
		}
	}
}
