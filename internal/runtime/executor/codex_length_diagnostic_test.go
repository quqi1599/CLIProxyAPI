package executor

import (
	"net/http"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestCodexLengthFailureIsAlwaysRequestScoped(t *testing.T) {
	for _, outer := range []int{http.StatusBadRequest, http.StatusOK, http.StatusSwitchingProtocols} {
		for _, errorType := range []string{"invalid_request_error", "server_error"} {
			for _, committed := range []bool{false, true} {
				failure, _ := canonicalCodexFailure(codexFailureInput{
					outerStatus:     outer,
					body:            []byte(`{"error":{"code":"string_above_max_length","type":"` + errorType + `","param":"input[2].call_id"}}`),
					outputCommitted: committed,
				})
				if failure.HTTPStatus != http.StatusBadRequest || failure.OuterStatus != outer ||
					failure.Kind != failurecontract.InvalidRequest || failure.Scope != failurecontract.ScopeRequest ||
					failure.Retryable || failure.OutputCommitted != committed ||
					failure.SemanticCode != "string_above_max_length" || failure.SemanticType != "invalid_request_error" {
					t.Errorf("outer=%d type=%s committed=%v: %+v", outer, errorType, committed, failure)
				}
			}
		}
	}
}

func TestCodexLengthDiagnosticStaysWithSelectedEnvelope(t *testing.T) {
	body := []byte(`{"error":{"code":"invalid_request_error","param":"instructions","message":"private outer value"},"body":{"error":{"code":"string_above_max_length","type":"server_error","param":"input[2].call_id","message":"Expected a string with maximum length 64, but got a string with length 85 instead."}}}`)
	err := newCodexStatusErr(http.StatusBadRequest, body)
	f := failurecontract.Classify(err)
	if f == nil || f.ErrorDiagnostic.Param != "input[].call_id" ||
		f.ErrorDiagnostic.ReportedMaxLength == nil || *f.ErrorDiagnostic.ReportedMaxLength != 64 ||
		f.ErrorDiagnostic.ReportedActualLength == nil || *f.ErrorDiagnostic.ReportedActualLength != 85 {
		t.Fatalf("diagnostic selected a different error envelope: %+v", f)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "call_id") || strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("public error exposed diagnostic source text: %s", err.Error())
	}
	body = []byte(`{"error":{"code":"invalid_request_error","param":"instructions","message":"Expected a string with maximum length 64, but got a string with length 85 instead."},"body":{"error":{"code":"string_above_max_length"}}}`)
	f = failurecontract.Classify(newCodexStatusErr(http.StatusBadRequest, body))
	if f.ErrorDiagnostic != (failurecontract.UpstreamErrorDiagnostic{}) {
		t.Fatalf("diagnostic fell back to a different envelope: %+v", f.ErrorDiagnostic)
	}
}

func TestGenericLengthDiagnosticIsRequestScopedAndSafe(t *testing.T) {
	body := []byte(`{"error":{"code":"string_above_max_length","type":"server_error","param":"input[2].call_id","message":"Expected a string with maximum length 64, but got a string with length 85 instead."}}`)
	for _, outer := range []int{http.StatusBadRequest, http.StatusBadGateway} {
		for _, err := range []statusErr{
			newUpstreamStatusErr(outer, nil, "application/json", body),
			newOpenAICompatStatusErr(openAICompatProfile{}, nil, "fixture", outer, http.Header{}, "application/json", body),
		} {
			f := failurecontract.Classify(err)
			if f == nil || f.HTTPStatus != http.StatusBadRequest || f.OuterStatus != outer ||
				f.Kind != failurecontract.InvalidRequest || f.Scope != failurecontract.ScopeRequest || f.Retryable ||
				f.SemanticCode != "string_above_max_length" || f.ErrorDiagnostic.Param != "input[].call_id" ||
				f.ErrorDiagnostic.ReportedMaxLength == nil || *f.ErrorDiagnostic.ReportedMaxLength != 64 {
				t.Fatalf("generic length failure lost its safe semantics: %+v", f)
			}
			if strings.Contains(err.Error(), "call_id") || strings.Contains(err.Error(), "maximum length") {
				t.Fatalf("generic public error exposed source text: %s", err.Error())
			}
		}
	}
}

func TestCodexLengthDiagnosticRejectsAmbiguousTerminalEnvelopes(t *testing.T) {
	const body = `{"type":"error","status":400,"error":{"code":"string_above_max_length","param":"input[2].call_id"},"error":{"code":"server_error","param":"instructions"}}`
	streamErr, _, ok := codexTerminalStreamErrWithMetadata([]byte(body), nil, false)
	if !ok || streamErr.failure.ErrorDiagnostic != (failurecontract.UpstreamErrorDiagnostic{}) {
		t.Fatalf("SSE flattening hid duplicate envelope keys: %+v", streamErr.failure)
	}
	wsErr, ok := parseCodexWebsocketErrorWithMetadata([]byte(body), nil, false)
	if !ok || failurecontract.Classify(wsErr).ErrorDiagnostic != (failurecontract.UpstreamErrorDiagnostic{}) {
		t.Fatalf("WebSocket flattening hid duplicate envelope keys: %v", wsErr)
	}
}

func TestLengthDiagnosticPreservesExistingHTTP413Classification(t *testing.T) {
	const body = `{"error":{"code":"string_above_max_length","type":"invalid_request_error","param":"input[2].call_id"}}`
	for _, err := range []statusErr{
		newUpstreamStatusErr(http.StatusRequestEntityTooLarge, nil, "application/json", []byte(body)),
		newOpenAICompatStatusErr(openAICompatProfile{}, nil, "fixture", http.StatusRequestEntityTooLarge, http.Header{}, "application/json", []byte(body)),
	} {
		f := failurecontract.Classify(err)
		if f.HTTPStatus != http.StatusRequestEntityTooLarge || f.OuterStatus != http.StatusRequestEntityTooLarge ||
			f.Kind != failurecontract.RequestTooLarge || f.Scope != failurecontract.ScopeRequest || f.Retryable ||
			f.ErrorDiagnostic.Param != "input[].call_id" {
			t.Fatalf("length diagnostic replaced HTTP 413 semantics: %+v", f)
		}
	}
	f := failurecontract.Classify(newCodexStatusErr(http.StatusRequestEntityTooLarge, []byte(body)))
	if f.HTTPStatus != http.StatusRequestEntityTooLarge || f.OuterStatus != http.StatusRequestEntityTooLarge ||
		f.Kind != failurecontract.ContextLengthExceeded || f.Retryable {
		t.Fatalf("length diagnostic replaced existing Codex HTTP 413 semantics: %+v", f)
	}
}
