package executor

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestNewOpenAICompatStatusErrTypedFailureParity(t *testing.T) {
	t.Parallel()

	billingMessage := "You've reached your usage limit for this billing cycle. Your quota will be refreshed in the next cycle."
	tests := []struct {
		name      string
		status    int
		headers   http.Header
		body      string
		wantKind  failurecontract.Kind
		wantScope failurecontract.Scope
		wantRetry bool
	}{
		{
			name:      "400 request",
			status:    http.StatusBadRequest,
			body:      `{"error":{"message":"malformed payload","type":"invalid_request_error","code":"invalid_request"}}`,
			wantKind:  failurecontract.InvalidRequest,
			wantScope: failurecontract.ScopeRequest,
		},
		{
			name:      "401 credential",
			status:    http.StatusUnauthorized,
			body:      `{"error":{"message":"invalid API key","code":"invalid_api_key"}}`,
			wantKind:  failurecontract.AuthenticationFailed,
			wantScope: failurecontract.ScopeCredential,
		},
		{
			name:      "403 content safety is request scoped",
			status:    http.StatusForbidden,
			body:      `{"error":{"message":"request blocked by content policy","code":"content_policy_violation"}}`,
			wantKind:  failurecontract.ContentSafetyBlocked,
			wantScope: failurecontract.ScopeRequest,
		},
		{
			name:      "403 without auth evidence remains unclassified",
			status:    http.StatusForbidden,
			body:      `{"error":{"message":"forbidden by upstream policy","code":"policy_denied"}}`,
			wantKind:  "",
			wantScope: "",
		},
		{
			name:      "402 balance",
			status:    http.StatusPaymentRequired,
			body:      `{"error":{"message":"insufficient balance","code":"insufficient_balance"}}`,
			wantKind:  failurecontract.QuotaExceeded,
			wantScope: failurecontract.ScopeCredential,
			wantRetry: true,
		},
		{
			name:      "403 billing quota",
			status:    http.StatusForbidden,
			body:      `{"error":{"message":"` + billingMessage + `","code":"billing_cycle_quota"}}`,
			wantKind:  failurecontract.QuotaExceeded,
			wantScope: failurecontract.ScopeCredential,
			wantRetry: true,
		},
		{
			name:      "429 rate limit",
			status:    http.StatusTooManyRequests,
			headers:   http.Header{"Retry-After": {"12"}},
			body:      `{"error":{"message":"too many requests","code":"rate_limit"}}`,
			wantKind:  failurecontract.RateLimited,
			wantScope: failurecontract.ScopeCredential,
			wantRetry: true,
		},
		{
			name:      "404 store false item miss is request scoped",
			status:    http.StatusNotFound,
			body:      `{"error":{"message":"Item with id 'rs_123' not found. Items are not persisted when ` + "`store`" + ` is set to false. Try again without this item.","type":"invalid_request_error","code":null}}`,
			wantKind:  failurecontract.InvalidRequest,
			wantScope: failurecontract.ScopeRequest,
		},
		{
			name:      "404 unknown object remains unclassified",
			status:    http.StatusNotFound,
			body:      `{"error":{"message":"resource not found","code":"not_found"}}`,
			wantKind:  "",
			wantScope: "",
		},
		{
			name:      "404 explicit model miss",
			status:    http.StatusNotFound,
			body:      `{"error":{"message":"requested model was not found","code":"model_not_found"}}`,
			wantKind:  failurecontract.ModelUnavailable,
			wantScope: failurecontract.ScopeModel,
		},
		{
			name:      "503 provider unavailable",
			status:    http.StatusServiceUnavailable,
			body:      `{"error":{"message":"service unavailable","code":"overloaded"}}`,
			wantKind:  failurecontract.ProviderUnavailable,
			wantScope: failurecontract.ScopeProvider,
			wantRetry: true,
		},
		{
			name:      "524 transport timeout",
			status:    524,
			body:      `{"error":{"message":"upstream timeout","code":"timeout"}}`,
			wantKind:  failurecontract.TransportError,
			wantScope: failurecontract.ScopeProvider,
			wantRetry: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			profile := openAICompatProfileForKind("generic")
			got := newOpenAICompatStatusErr(profile, nil, "test-model", tc.status, tc.headers, "application/json", []byte(tc.body))
			legacy := legacyOpenAICompatStatusErrForParity(tc.status, tc.headers, []byte(tc.body))

			assertOpenAICompatStatusErrParity(t, got, legacy)
			legacyView := failurecontract.Classify(legacy)
			if legacyView.Kind != "" || legacyView.Scope != "" ||
				legacyView.HTTPStatus != legacy.StatusCode() ||
				legacyView.ProviderCode != legacy.ErrorCode() ||
				!equalRetryAfter(legacyView.RetryAfter, legacy.RetryAfter()) {
				t.Fatalf("legacy fallback drifted: %#v", legacyView)
			}

			var typed *failurecontract.Failure
			if !errors.As(got, &typed) || typed == nil {
				t.Fatalf("errors.As(%T) did not find *failure.Failure", got)
			}
			if typed.Kind != tc.wantKind || typed.Scope != tc.wantScope || typed.Retryable != tc.wantRetry {
				t.Fatalf("typed semantics = %q/%q/%t, want %q/%q/%t", typed.Kind, typed.Scope, typed.Retryable, tc.wantKind, tc.wantScope, tc.wantRetry)
			}
			if typed.HTTPStatus != got.StatusCode() || typed.ProviderCode != got.ErrorCode() || typed.PublicMessage != got.Error() {
				t.Fatalf("typed metadata = status:%d code:%q message:%q; outer = status:%d code:%q message:%q", typed.HTTPStatus, typed.ProviderCode, typed.PublicMessage, got.StatusCode(), got.ErrorCode(), got.Error())
			}
			if !equalRetryAfter(typed.RetryAfter, got.RetryAfter()) {
				t.Fatalf("typed RetryAfter = %v, outer = %v", typed.RetryAfter, got.RetryAfter())
			}
		})
	}
}

func legacyOpenAICompatStatusErrForParity(statusCode int, headers http.Header, body []byte) statusErr {
	retryAfter := openAICompatRetryAfter(headers, body)
	jsonBody := openAICompatJSONErrorBody(body)
	message := summarizeOpenAICompatError(body)
	errorCode := firstNonEmptyJSONValue(jsonBody, "error.code", "code", "error.type", "type", "error.err_code")
	if errorCode == "" && strings.TrimSpace(string(body)) == "" {
		errorCode = openAICompatEmptyUpstreamResponseCode
	}
	return statusErr{
		code:               normalizeOpenAICompatStatus(statusCode, message),
		providerStatusCode: statusCode,
		msg:                message,
		errorCode:          errorCode,
		retryAfter:         retryAfter,
	}
}

func assertOpenAICompatStatusErrParity(t *testing.T, got, legacy statusErr) {
	t.Helper()
	if got.StatusCode() != legacy.StatusCode() ||
		got.ProviderStatusCode() != legacy.ProviderStatusCode() ||
		got.ErrorCode() != legacy.ErrorCode() ||
		!equalRetryAfter(got.RetryAfter(), legacy.RetryAfter()) {
		t.Fatalf("statusErr parity mismatch:\n got=%s\nwant=%s", statusErrSnapshot(got), statusErrSnapshot(legacy))
	}
	if !strings.Contains(got.Error(), "[BODY METADATA v1]") || !strings.Contains(got.Error(), `"sha256":`) {
		t.Fatalf("statusErr missing safe body metadata: %s", got.Error())
	}
}

func statusErrSnapshot(err statusErr) string {
	retryAfter := "<nil>"
	if value := err.RetryAfter(); value != nil {
		retryAfter = value.String()
	}
	return strings.Join([]string{
		"error=" + err.Error(),
		"status=" + http.StatusText(err.StatusCode()),
		"provider_status=" + http.StatusText(err.ProviderStatusCode()),
		"code=" + err.ErrorCode(),
		"retry_after=" + retryAfter,
	}, " ")
}

func equalRetryAfter(left, right *time.Duration) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
