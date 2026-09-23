package handlers

import (
	"errors"
	"net/http"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestTypedSelectionErrorContractAcrossBodyAndResponsesStream(t *testing.T) {
	typed := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: 503, OuterStatus: 503, SemanticCode: "auth_unavailable",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		PublicMessage: "auth_unavailable: no auth available",
		Cause:         &coreauth.Error{Code: "auth_unavailable", HTTPStatus: 503},
	}
	for _, test := range []struct {
		name     string
		cause    error
		wantCode string
	}{
		{name: "typed local selection", cause: typed, wantCode: "auth_unavailable"},
		{name: "upstream text is not a local selection", cause: errors.New(typed.Error()), wantCode: "internal_server_error"},
		{name: "no cause keeps legacy behavior", wantCode: "internal_server_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := BuildErrorResponseBodyWithCause(http.StatusServiceUnavailable, typed.Error(), test.cause)
			stream := BuildOpenAIResponsesStreamErrorChunkWithCause(http.StatusServiceUnavailable, typed.Error(), 7, test.cause)
			for _, payload := range [][]byte{body, stream} {
				if got := gjson.GetBytes(payload, "error.code").String(); got != test.wantCode {
					t.Fatalf("error.code=%q want %q; payload=%s", got, test.wantCode, payload)
				}
				if got := gjson.GetBytes(payload, "error.type").String(); got != "server_error" {
					t.Fatalf("error.type=%q, want server_error", got)
				}
			}
			if gjson.GetBytes(stream, "type").String() != "error" || gjson.GetBytes(stream, "sequence_number").Int() != 7 {
				t.Fatalf("Responses event framing changed: %s", stream)
			}
		})
	}
}

func TestTypedErrorBodyKeepsNormalizedFallbackText(t *testing.T) {
	const normalized = "normalized user-facing explanation"
	body := BuildErrorResponseBodyWithCause(503, normalized, errors.New("raw upstream details"))
	if got := gjson.GetBytes(body, "error.message").String(); got != normalized {
		t.Fatalf("error.message=%q, want normalized caller text", got)
	}
}
