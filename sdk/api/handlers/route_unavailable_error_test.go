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
	"github.com/tidwall/gjson"
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

func TestForbiddenResponsesDoNotInferQuotaFromStatus(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		cause      error
	}{
		{"generic", "permission_error", nil},
		{"confirmed", "insufficient_quota", &failurecontract.Failure{Kind: failurecontract.QuotaExceeded, HTTPStatus: 403}},
	} {
		for _, body := range [][]byte{BuildErrorResponseBodyWithCause(403, "request denied", tc.cause), BuildOpenAIResponsesStreamErrorChunkWithCause(403, "request denied", 1, tc.cause)} {
			if got := gjson.GetBytes(body, "error.code").String(); got != tc.want && gjson.GetBytes(body, "code").String() != tc.want {
				t.Fatalf("%s: %s", tc.name, body)
			}
		}
	}
}
