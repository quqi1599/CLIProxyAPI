package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestCanonicalCodexFailureServerErrorPlaceholderCode(t *testing.T) {
	failure, _ := canonicalCodexFailure(codexFailureInput{
		outerStatus: http.StatusBadRequest,
		body:        []byte(`{"error":{"code":0,"type":"server_error","message":"internal failure"}}`),
	})
	if failure.OuterStatus != http.StatusBadRequest || failure.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("statuses = outer:%d normalized:%d, want 400/502", failure.OuterStatus, failure.HTTPStatus)
	}
	if failure.SemanticCode != "server_error" || failure.SemanticType != "server_error" {
		t.Fatalf("semantic = %q/%q, want server_error/server_error", failure.SemanticCode, failure.SemanticType)
	}
	if failure.Kind != failurecontract.ProviderUnavailable || failure.Scope != failurecontract.ScopeProvider || !failure.Retryable {
		t.Fatalf("classification = kind:%q scope:%q retryable:%v", failure.Kind, failure.Scope, failure.Retryable)
	}
	if failure.StreamPhase != failurecontract.StreamPhaseBeforeOutput || failure.OutputCommitted {
		t.Fatalf("stream state = %q/%v", failure.StreamPhase, failure.OutputCommitted)
	}
}

func TestCanonicalCodexFailureMetadataOnlyIsNotGhostServerError(t *testing.T) {
	body := []byte(`upstream request failed [BODY METADATA v1] {"bytes":163,"sha256":"abcdef","truncated":false}`)
	err := newCodexStatusErr(http.StatusBadRequest, body)
	failure, ok := failurecontract.As(err)
	if !ok {
		t.Fatal("expected canonical failure")
	}
	if failure.HTTPStatus != http.StatusBadRequest || failure.Kind != failurecontract.InvalidRequest || failure.Scope != failurecontract.ScopeRequest || failure.Retryable {
		t.Fatalf("classification = status:%d kind:%q scope:%q retryable:%v", failure.HTTPStatus, failure.Kind, failure.Scope, failure.Retryable)
	}
	if failure.SemanticCode != "" || failure.SemanticType != "" || strings.Contains(err.Error(), "server_error") {
		t.Fatalf("metadata-only body invented semantics: failure=%+v public=%s", failure, err.Error())
	}
}

func TestCanonicalCodexFailureDeterministicOuter400NeverRetries(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		scope failurecontract.Scope
	}{
		{"context", `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"context too long"}}`, failurecontract.ScopeRequest},
		{"tool schema", `{"error":{"type":"invalid_request_error","code":"tool_schema_error","message":"invalid tool schema"}}`, failurecontract.ScopeRequest},
		{"auth", `{"error":{"type":"authentication_error","code":"invalid_api_key","message":"bad key"}}`, failurecontract.ScopeCredential},
		{"balance", `{"error":{"type":"insufficient_balance","message":"insufficient balance"}}`, failurecontract.ScopeCredential},
		{"quota", `{"error":{"type":"usage_limit_reached","message":"usage limit"}}`, failurecontract.ScopeCredential},
		{"billing", `{"error":{"type":"server_error","code":"billing_config_error","message":"billing config"}}`, failurecontract.ScopeRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			failure, _ := canonicalCodexFailure(codexFailureInput{outerStatus: http.StatusBadRequest, body: []byte(tc.body)})
			if failure.Scope != tc.scope || failure.Retryable {
				t.Fatalf("classification = scope:%q retryable:%v, want %q/false", failure.Scope, failure.Retryable, tc.scope)
			}
		})
	}
}

func TestCanonicalCodexFailureCredentialScopeRequiresAccountLevelEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		wantKind  failurecontract.Kind
		wantScope failurecontract.Scope
		wantRetry bool
	}{
		{
			name:      "invalid api key",
			status:    http.StatusForbidden,
			body:      `{"error":{"type":"authentication_error","code":"invalid_api_key","message":"invalid key"}}`,
			wantKind:  failurecontract.AuthenticationFailed,
			wantScope: failurecontract.ScopeCredential,
		},
		{
			name:      "model permission denied",
			status:    http.StatusForbidden,
			body:      `{"error":{"type":"permission_error","code":"permission_denied","message":"model access denied"}}`,
			wantKind:  failurecontract.ModelUnavailable,
			wantScope: failurecontract.ScopeModel,
			wantRetry: true,
		},
		{
			name:   "generic forbidden",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"forbidden by upstream policy"}}`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			failure, _ := canonicalCodexFailure(codexFailureInput{outerStatus: tc.status, body: []byte(tc.body)})
			if failure.Kind != tc.wantKind || failure.Scope != tc.wantScope || failure.Retryable != tc.wantRetry {
				t.Fatalf("classification = %q/%q/%t, want %q/%q/%t", failure.Kind, failure.Scope, failure.Retryable, tc.wantKind, tc.wantScope, tc.wantRetry)
			}
		})
	}
}

func TestCanonicalCodexFailureNestedEnvelopes(t *testing.T) {
	for _, pathBody := range []string{
		`{"body":{"error":{"type":"server_error","code":"server_error","message":"boom"}}}`,
		`{"response":{"error":{"type":"server_error","code":"server_error","message":"boom"}}}`,
		`{"data":{"error":{"type":"server_error","code":"server_error","message":"boom"}}}`,
	} {
		failure, _ := canonicalCodexFailure(codexFailureInput{outerStatus: http.StatusBadRequest, body: []byte(pathBody)})
		if failure.SemanticCode != "server_error" || failure.SemanticType != "server_error" || failure.HTTPStatus != http.StatusBadGateway {
			t.Fatalf("body %s classified as %+v", pathBody, failure)
		}
	}
}

func TestCanonicalCodexFailureNestedWrapperPlaceholderDoesNotMaskInnerError(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantCode  string
		wantKind  failurecontract.Kind
		wantScope failurecontract.Scope
		wantRetry bool
	}{
		{
			name:      "inner provider failure",
			body:      `{"error":{"code":0,"type":"invalid_request_error"},"body":{"error":{"code":"server_error","type":"server_error","message":"boom"}}}`,
			wantCode:  "server_error",
			wantKind:  failurecontract.ProviderUnavailable,
			wantScope: failurecontract.ScopeProvider,
			wantRetry: true,
		},
		{
			name:      "inner context failure",
			body:      `{"error":{"code":"0","type":"server_error"},"response":{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"context too long"}}}`,
			wantCode:  "context_too_large",
			wantKind:  failurecontract.ContextLengthExceeded,
			wantScope: failurecontract.ScopeRequest,
			wantRetry: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			failure, _ := canonicalCodexFailure(codexFailureInput{outerStatus: http.StatusBadRequest, body: []byte(tc.body)})
			if failure.SemanticCode != tc.wantCode || failure.Kind != tc.wantKind || failure.Scope != tc.wantScope || failure.Retryable != tc.wantRetry {
				t.Fatalf("failure = %+v", failure)
			}
		})
	}
}

func TestCodexTransportFailureCanonicalBeforeAndAfterOutput(t *testing.T) {
	cause := errors.New("upstream connection reset")
	headers := http.Header{"X-Request-Id": []string{"transport-req"}, "Retry-After": []string{"7"}}
	before := newCodexTransportStatusErr(http.StatusOK, headers, cause, false)
	beforeFailure, ok := failurecontract.As(before)
	if !ok {
		t.Fatal("missing before-output canonical failure")
	}
	if beforeFailure.HTTPStatus != http.StatusBadGateway || beforeFailure.OuterStatus != http.StatusOK ||
		beforeFailure.Scope != failurecontract.ScopeProvider || beforeFailure.Kind != failurecontract.TransportError || !beforeFailure.Retryable ||
		beforeFailure.StreamPhase != failurecontract.StreamPhaseBeforeOutput || beforeFailure.OutputCommitted ||
		beforeFailure.UpstreamRequestID != "transport-req" || beforeFailure.RetryAfter == nil || *beforeFailure.RetryAfter != 7*time.Second ||
		!errors.Is(before, cause) {
		t.Fatalf("before-output failure = %+v", beforeFailure)
	}
	after := newCodexTransportStatusErr(http.StatusOK, headers, cause, true)
	afterFailure, ok := failurecontract.As(after)
	if !ok || afterFailure.Retryable || !afterFailure.OutputCommitted || afterFailure.StreamPhase != failurecontract.StreamPhaseAfterOutput {
		t.Fatalf("after-output failure = %+v", afterFailure)
	}
}

func TestCodexTransportFailurePreservesCancellation(t *testing.T) {
	err := newCodexTransportStatusErr(http.StatusOK, nil, context.Canceled, false)
	failure, ok := failurecontract.As(err)
	if !ok || failure.Kind != failurecontract.Cancelled || failure.Scope != failurecontract.ScopeRequest || failure.Retryable || failure.HTTPStatus != 499 {
		t.Fatalf("cancel failure = %+v", failure)
	}
}

func TestCodexHTTPDoFailureNormalizesTransportAndTimeout(t *testing.T) {
	t.Run("provider transport", func(t *testing.T) {
		cause := errors.New("proxy dial failed")
		err := newCodexHTTPDoStatusErr(context.Background(), cause, false)
		failure, ok := failurecontract.As(err)
		if !ok {
			t.Fatal("expected canonical transport failure")
		}
		if failure.Kind != failurecontract.TransportError || failure.Scope != failurecontract.ScopeProvider ||
			failure.HTTPStatus != http.StatusBadGateway || failure.OuterStatus != http.StatusBadGateway || !failure.Retryable ||
			failure.StreamPhase != failurecontract.StreamPhaseBeforeOutput || failure.OutputCommitted || !errors.Is(err, cause) {
			t.Fatalf("transport failure = %+v", failure)
		}
	})

	t.Run("provider timeout", func(t *testing.T) {
		cause := context.DeadlineExceeded
		err := newCodexHTTPDoStatusErr(context.Background(), cause, false)
		failure, ok := failurecontract.As(err)
		if !ok {
			t.Fatal("expected canonical timeout failure")
		}
		if failure.HTTPStatus != http.StatusGatewayTimeout || failure.OuterStatus != http.StatusGatewayTimeout ||
			failure.SemanticCode != "upstream_timeout" || !failure.Retryable || !errors.Is(err, cause) {
			t.Fatalf("timeout failure = %+v", failure)
		}
	})

	t.Run("live request upstream canceled", func(t *testing.T) {
		cause := context.Canceled
		err := newCodexHTTPDoStatusErr(context.Background(), cause, false)
		failure, ok := failurecontract.As(err)
		if !ok {
			t.Fatal("expected canonical transport failure")
		}
		if failure.HTTPStatus != http.StatusBadGateway || failure.Kind != failurecontract.TransportError ||
			failure.Scope != failurecontract.ScopeProvider || !failure.Retryable || !errors.Is(err, cause) {
			t.Fatalf("live-context canceled failure = %+v", failure)
		}
	})
}

func TestCodexHTTPDoFailurePreservesEstablishedCanonicalFailure(t *testing.T) {
	cause := &failurecontract.Failure{
		Kind:          failurecontract.InvalidRequest,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusBadRequest,
		SemanticCode:  "invalid_request_error",
		Retryable:     false,
		PublicMessage: "invalid request",
	}
	if got := newCodexHTTPDoStatusErr(context.Background(), cause, false); got != cause {
		t.Fatalf("established canonical failure was replaced: got %T %v", got, got)
	}
}

func TestCodexHTTPDoFailurePreservesCallerDeadlineScope(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := newCodexHTTPDoStatusErr(ctx, context.DeadlineExceeded, false)
	failure, ok := failurecontract.As(err)
	if !ok || failure == nil {
		t.Fatalf("error = %T, want canonical caller cancellation", err)
	}
	if failure.Kind != failurecontract.Cancelled || failure.Scope != failurecontract.ScopeRequest ||
		failure.HTTPStatus != 499 || failure.Retryable {
		t.Fatalf("caller deadline failure = %+v", failure)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller deadline cause was not preserved")
	}
}

func TestCodexHTTPDoFailureCallerCancellationOverridesTypedProviderFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusServiceUnavailable,
		Retryable:     true,
		PublicMessage: "provider unavailable",
	}
	err := newCodexHTTPDoStatusErr(ctx, provider, false)
	failure, ok := failurecontract.As(err)
	if !ok || failure == nil {
		t.Fatalf("error = %T, want canonical caller cancellation", err)
	}
	if failure.HTTPStatus != 499 || failure.Kind != failurecontract.Cancelled || failure.Scope != failurecontract.ScopeRequest || failure.Retryable {
		t.Fatalf("caller cancellation = %+v", failure)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation cause was not preserved")
	}
}

func TestCanonicalCodexFailurePreservesProviderCodeAndQuotaSemantics(t *testing.T) {
	provider, _ := canonicalCodexFailure(codexFailureInput{
		outerStatus: http.StatusServiceUnavailable,
		body:        []byte(`{"error":{"type":"server_error","code":"upstream_unavailable","message":"boom"}}`),
	})
	if provider.SemanticCode != "upstream_unavailable" || provider.SemanticType != "server_error" {
		t.Fatalf("provider semantic = %q/%q", provider.SemanticCode, provider.SemanticType)
	}
	quota, _ := canonicalCodexFailure(codexFailureInput{
		outerStatus: http.StatusForbidden,
		body:        []byte(`{"error":{"type":"insufficient_balance","message":"balance exhausted"}}`),
	})
	if quota.Kind != failurecontract.QuotaExceeded || quota.SemanticCode != "insufficient_balance" {
		t.Fatalf("quota failure = %+v", quota)
	}
}

func TestCanonicalCodexFailurePreservesHeadersAndPrefersBodyReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	headers := http.Header{"X-Request-Id": []string{" upstream-123 "}, "Retry-After": []string{"99"}}
	body := []byte(`{"response":{"error":{"type":"usage_limit_reached","resets_in_seconds":17}}}`)
	failure, _ := canonicalCodexFailure(codexFailureInput{outerStatus: http.StatusBadRequest, headers: headers, body: body, now: now})
	if failure.UpstreamRequestID != "upstream-123" {
		t.Fatalf("request id = %q", failure.UpstreamRequestID)
	}
	if failure.RetryAfter == nil || *failure.RetryAfter != 17*time.Second {
		t.Fatalf("retry after = %v, want body reset 17s", failure.RetryAfter)
	}
}

func TestCodexSSEFailureParsingSupportsCRLFMultiEventAndMultilineData(t *testing.T) {
	body := []byte("event: response.in_progress\r\ndata: {\"type\":\"response.in_progress\"}\r\n\r\n" +
		"event: response.failed\r\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":\r\n" +
		"data: {\"type\":\"server_error\",\"code\":\"0\",\"message\":\"boom\"}}}\r\n\r\n")
	err := newCodexHTTPStatusErr(http.StatusBadRequest, http.Header{"X-Request-Id": []string{"sse-req"}}, body)
	failure, ok := failurecontract.As(err)
	if !ok {
		t.Fatal("expected canonical SSE failure")
	}
	if failure.SemanticCode != "server_error" || failure.HTTPStatus != http.StatusBadGateway || failure.UpstreamRequestID != "sse-req" {
		t.Fatalf("SSE failure = %+v", failure)
	}
}

func TestCodexResponseFailedHonorsOutputCommitBoundary(t *testing.T) {
	event := []byte(`{"type":"response.failed","response":{"error":{"type":"server_error","code":0,"message":"boom"}}}`)
	before, _, ok := codexTerminalStreamErrWithMetadata(event, nil, false)
	if !ok || before.failure == nil || before.failure.OuterStatus != http.StatusOK || before.failure.HTTPStatus != http.StatusBadGateway || !before.failure.Retryable {
		t.Fatalf("before-output failure = %+v, ok=%v", before.failure, ok)
	}
	after, _, ok := codexTerminalStreamErrWithMetadata(event, nil, true)
	if !ok || after.failure == nil || after.failure.Retryable || !after.failure.OutputCommitted || after.failure.StreamPhase != failurecontract.StreamPhaseAfterOutput {
		t.Fatalf("after-output failure = %+v, ok=%v", after.failure, ok)
	}
}

func TestCodexBootstrapAndReasoningDoNotCommitOutput(t *testing.T) {
	events := []struct {
		typeID string
		body   string
	}{
		{"response.created", `{"type":"response.created","response":{"id":"resp-1"}}`},
		{"response.in_progress", `{"type":"response.in_progress"}`},
		{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}}`},
	}
	outputCommitted := false
	for _, event := range events {
		outputCommitted = outputCommitted || codexEventCommitsOutput(event.typeID, []byte(event.body))
	}
	if outputCommitted {
		t.Fatal("bootstrap/reasoning-only events must not commit downstream output")
	}
	failed := []byte(`{"type":"response.failed","response":{"error":{"type":"server_error","code":0,"message":"boom"}}}`)
	err, _, ok := codexTerminalStreamErrWithMetadata(failed, nil, outputCommitted)
	if !ok || err.failure == nil || !err.failure.Retryable || err.failure.StreamPhase != failurecontract.StreamPhaseBeforeOutput {
		t.Fatalf("failure after bootstrap/reasoning = %+v, ok=%v", err.failure, ok)
	}
	if !codexEventCommitsOutput("response.output_text.delta", []byte(`{"delta":"answer"}`)) {
		t.Fatal("text output must commit")
	}
	if !codexEventCommitsOutput("response.future_output.delta", []byte(`{"delta":"answer"}`)) {
		t.Fatal("unknown non-error output events must conservatively commit")
	}
}

func TestCodexWebsocketCanonicalFailureParity(t *testing.T) {
	payload := []byte(`{"type":"error","status":400,"headers":{"x-request-id":"ws-req","retry-after":"7"},"error":{"type":"server_error","code":0,"message":"boom"}}`)
	err, ok := parseCodexWebsocketErrorWithMetadata(payload, nil, true)
	if !ok {
		t.Fatal("expected websocket failure")
	}
	failure, ok := failurecontract.As(err)
	if !ok {
		t.Fatal("expected canonical websocket failure")
	}
	if failure.OuterStatus != http.StatusBadRequest || failure.HTTPStatus != http.StatusBadGateway || failure.UpstreamRequestID != "ws-req" || failure.Retryable || !failure.OutputCommitted {
		t.Fatalf("websocket failure = %+v", failure)
	}
	if failure.RetryAfter == nil || *failure.RetryAfter != 7*time.Second {
		t.Fatalf("websocket retry-after = %v", failure.RetryAfter)
	}
}
