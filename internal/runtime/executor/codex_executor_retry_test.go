package executor

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/tidwall/gjson"
)

func TestParseCodexRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("resets_in_seconds", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":123}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 123*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 123*time.Second)
		}
	})

	t.Run("prefers resets_at", func(t *testing.T) {
		resetAt := now.Add(5 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":1}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 5*time.Minute {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 5*time.Minute)
		}
	})

	t.Run("fallback when resets_at is past", func(t *testing.T) {
		resetAt := now.Add(-1 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":77}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 77*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 77*time.Second)
		}
	})

	t.Run("non-429 status code", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusBadRequest, body, now); got != nil {
			t.Fatalf("expected nil for non-429, got %v", *got)
		}
	})

	t.Run("non usage_limit_reached error type", func(t *testing.T) {
		body := []byte(`{"error":{"type":"server_error","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusTooManyRequests, body, now); got != nil {
			t.Fatalf("expected nil for non-usage_limit_reached, got %v", *got)
		}
	})
}

func TestNewCodexStatusErrTreatsCapacityAsRetryableRateLimit(t *testing.T) {
	body := []byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`)

	err := newCodexStatusErr(http.StatusBadRequest, body)

	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if err.RetryAfter() != nil {
		t.Fatalf("expected nil explicit retryAfter for capacity fallback, got %v", *err.RetryAfter())
	}
}

func TestNewCodexStatusErrTreatsUsageLimitAsRetryableRateLimit(t *testing.T) {
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_in_seconds":120}}`)

	err := newCodexStatusErr(http.StatusBadRequest, body)

	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	retryAfter := err.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected retryAfter from usage_limit_reached, got nil")
	}
	if *retryAfter != 120*time.Second {
		t.Fatalf("retryAfter = %v, want %v", *retryAfter, 120*time.Second)
	}
}

func TestNewCodexStatusErrTreatsTransientRateLimitAsRetryableRateLimit(t *testing.T) {
	body := []byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`)

	err := newCodexStatusErr(http.StatusBadRequest, body)

	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if got := err.ErrorCode(); got != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want rate_limit_exceeded", got)
	}
	if err.RetryAfter() != nil {
		t.Fatalf("expected nil explicit retryAfter for transient rate limit, got %v", *err.RetryAfter())
	}
}

func TestNewCodexStatusErrAssignsFailureScope(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		wantKind   failurecontract.Kind
		wantScope  failurecontract.Scope
		retryable  bool
	}{
		{
			name:       "model capacity",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`),
			wantKind:   failurecontract.RateLimited,
			wantScope:  failurecontract.ScopeModel,
			retryable:  true,
		},
		{
			name:       "credential usage limit",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"usage_limit_reached","message":"usage limit","resets_in_seconds":60}}`),
			wantKind:   failurecontract.QuotaExceeded,
			wantScope:  failurecontract.ScopeCredential,
			retryable:  true,
		},
		{
			name:       "credential rate limit",
			statusCode: http.StatusTooManyRequests,
			body:       []byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"rate limit reached"}}`),
			wantKind:   failurecontract.RateLimited,
			wantScope:  failurecontract.ScopeCredential,
			retryable:  true,
		},
		{
			name:       "provider unavailable",
			statusCode: http.StatusServiceUnavailable,
			body:       []byte(`{"error":{"type":"server_error","code":"upstream_unavailable","message":"temporarily unavailable"}}`),
			wantKind:   failurecontract.ProviderUnavailable,
			wantScope:  failurecontract.ScopeProvider,
			retryable:  true,
		},
		{
			name:       "provider server error reported as 400",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"server_error","message":"unexpected eof"}}`),
			wantKind:   failurecontract.ProviderUnavailable,
			wantScope:  failurecontract.ScopeProvider,
			retryable:  true,
		},
		{
			name:       "provider server error code reported as 400",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"server_error","code":"server_error","message":"internal error"}}`),
			wantKind:   failurecontract.ProviderUnavailable,
			wantScope:  failurecontract.ScopeProvider,
			retryable:  true,
		},
		{
			name:       "provider server error code overrides generic request type",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"invalid_request_error","code":"server_error","message":"internal error"}}`),
			wantKind:   failurecontract.ProviderUnavailable,
			wantScope:  failurecontract.ScopeProvider,
			retryable:  true,
		},
		{
			name:       "provider server error nested in body",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"body":{"error":{"type":"invalid_request_error","code":"server_error","message":"internal error"}}}`),
			wantKind:   failurecontract.ProviderUnavailable,
			wantScope:  failurecontract.ScopeProvider,
			retryable:  true,
		},
		{
			name:       "provider server error in top level error code",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"invalid_request_error","message":"internal error"},"error_code":"server_error"}`),
			wantKind:   failurecontract.ProviderUnavailable,
			wantScope:  failurecontract.ScopeProvider,
			retryable:  true,
		},
		{
			name:       "provider server error in alternate nested code",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"invalid_request_error","err_code":"server_error","message":"internal error"}}`),
			wantKind:   failurecontract.ProviderUnavailable,
			wantScope:  failurecontract.ScopeProvider,
			retryable:  true,
		},
		{
			name:       "invalid request",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"invalid_request_error","code":"invalid_request_error","message":"invalid payload"}}`),
			wantKind:   failurecontract.InvalidRequest,
			wantScope:  failurecontract.ScopeRequest,
		},
		{
			name:       "specific server error code remains request scoped",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"type":"server_error","code":"billing_config_error","message":"billing configuration failure"}}`),
			wantKind:   failurecontract.InvalidRequest,
			wantScope:  failurecontract.ScopeRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := newCodexStatusErr(tc.statusCode, tc.body)
			typed, ok := failurecontract.As(err)
			if !ok {
				t.Fatalf("newCodexStatusErr returned no typed failure: %v", err)
			}
			if typed.Kind != tc.wantKind || typed.Scope != tc.wantScope || typed.Retryable != tc.retryable {
				t.Fatalf("typed failure = kind:%q scope:%q retryable:%v, want kind:%q scope:%q retryable:%v", typed.Kind, typed.Scope, typed.Retryable, tc.wantKind, tc.wantScope, tc.retryable)
			}
		})
	}
}

func TestIsCodexUsageLimitError(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{
			name: "nested usage_limit_reached",
			body: []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`),
			want: true,
		},
		{
			name: "top-level usage_limit_reached",
			body: []byte(`{"type":"usage_limit_reached"}`),
			want: true,
		},
		{
			name: "transient rate limit is excluded",
			body: []byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`),
			want: false,
		},
		{
			name: "empty body",
			body: nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCodexUsageLimitError(tc.body); got != tc.want {
				t.Fatalf("isCodexUsageLimitError = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewCodexStatusErrClassifiesKnownCodexFailures(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		wantStatus int
		wantType   string
		wantCode   string
	}{
		{
			name:       "context length status",
			statusCode: http.StatusRequestEntityTooLarge,
			body:       []byte(`{"error":{"message":"context length exceeded","type":"invalid_request_error","code":"context_length_exceeded"}}`),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantType:   "invalid_request_error",
			wantCode:   "context_too_large",
		},
		{
			name:       "thinking signature",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"message":"Invalid signature in thinking block","type":"invalid_request_error","code":"invalid_request_error"}}`),
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantCode:   "thinking_signature_invalid",
		},
		{
			name:       "previous response missing",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"message":"No response found for previous_response_id resp_123","type":"invalid_request_error","code":"previous_response_not_found"}}`),
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantCode:   "previous_response_not_found",
		},
		{
			name:       "stateless persisted item missing",
			statusCode: http.StatusNotFound,
			body:       []byte(`{"error":{"message":"Item with id 'rs_123' not found. Items are not persisted when store is set to false.","type":"invalid_request_error","code":null}}`),
			wantStatus: http.StatusNotFound,
			wantType:   "invalid_request_error",
			wantCode:   "previous_response_not_found",
		},
		{
			name:       "auth unavailable",
			statusCode: http.StatusUnauthorized,
			body:       []byte(`{"error":{"message":"invalid or expired token","type":"authentication_error","code":"invalid_api_key"}}`),
			wantStatus: http.StatusUnauthorized,
			wantType:   "authentication_error",
			wantCode:   "auth_unavailable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := newCodexStatusErr(tc.statusCode, tc.body)

			if got := err.StatusCode(); got != tc.wantStatus {
				t.Fatalf("status code = %d, want %d", got, tc.wantStatus)
			}
			assertCodexErrorCode(t, err.Error(), tc.wantType, tc.wantCode)
		})
	}
}

func TestNormalizeCodexStatelessPayloadDropsPersistedItemIDs(t *testing.T) {
	body := []byte(`{
		"store": false,
		"previous_response_id": "resp_old",
		"input": [
			{"type":"reasoning","id":"rs_123","encrypted_content":"enc_123","summary":[]},
			{"type":"message","id":"msg_123","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call_output","id":"out_123","call_id":"call_123","output":"ok"}
		]
	}`)

	got := normalizeCodexStatelessPayload(body)

	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id should be removed: %s", string(got))
	}
	for idx := range gjson.GetBytes(got, "input").Array() {
		if gjson.GetBytes(got, "input."+strconv.Itoa(idx)+".id").Exists() {
			t.Fatalf("input item id at index %d should be removed: %s", idx, string(got))
		}
	}
	if gotSig := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotSig != "enc_123" {
		t.Fatalf("encrypted_content = %q, want enc_123", gotSig)
	}
	if gotCallID := gjson.GetBytes(got, "input.2.call_id").String(); gotCallID != "call_123" {
		t.Fatalf("call_id = %q, want call_123", gotCallID)
	}
}

func TestNewCodexStatusErrSummarizesUnclassifiedErrors(t *testing.T) {
	const secret = "codex-unclassified-error-sentinel"
	body := []byte(`{"error":{"message":"documentation mentions too many tokens, but this is a billing configuration failure ` + secret + `","type":"server_error","code":"billing_config_error"}}`)

	err := newCodexStatusErr(http.StatusBadGateway, body)

	if got := err.StatusCode(); got != http.StatusBadGateway {
		t.Fatalf("status code = %d, want %d", got, http.StatusBadGateway)
	}
	got := err.Error()
	if strings.Contains(got, secret) || strings.Contains(got, "billing_config_error") || !strings.Contains(got, "[BODY METADATA v1]") || !strings.Contains(got, "sha256") {
		t.Fatalf("error body = %s, want metadata without upstream body", got)
	}
}

func assertCodexErrorCode(t *testing.T, raw string, wantType string, wantCode string) {
	t.Helper()

	var payload struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("error body is not valid JSON: %v; body=%s", err, raw)
	}
	if payload.Error.Type != wantType {
		t.Fatalf("error.type = %q, want %q; body=%s", payload.Error.Type, wantType, raw)
	}
	if payload.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q; body=%s", payload.Error.Code, wantCode, raw)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
