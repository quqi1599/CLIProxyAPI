package executor

import (
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestNormalizeOpenAICompatStatus_PaymentLikeMessage(t *testing.T) {
	t.Parallel()

	tests := []string{
		"insufficient balance",
		"账户余额不足",
		"余额不足，请充值后重试",
	}

	for _, message := range tests {
		if got := normalizeOpenAICompatStatus(http.StatusBadRequest, message); got != http.StatusPaymentRequired {
			t.Fatalf("normalizeOpenAICompatStatus(%q) = %d, want %d", message, got, http.StatusPaymentRequired)
		}
	}
}

func TestNormalizeOpenAICompatStatus_QuotaLikeMessage(t *testing.T) {
	t.Parallel()

	if got := normalizeOpenAICompatStatus(http.StatusBadRequest, "insufficient_quota"); got != http.StatusTooManyRequests {
		t.Fatalf("normalizeOpenAICompatStatus(quota) = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestNormalizeOpenAICompatStatus_KimiBillingCycleUsageLimit(t *testing.T) {
	t.Parallel()

	message := "You've reached your usage limit for this billing cycle. Your quota will be refreshed in the next cycle. Upgrade to get more: https://www.kimi.com/code/console?from=quota-upgrade"
	if got := normalizeOpenAICompatStatus(http.StatusForbidden, message); got != http.StatusTooManyRequests {
		t.Fatalf("normalizeOpenAICompatStatus(kimi billing cycle quota) = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestNewOpenAICompatStatusErr_ParsesRetryAfter(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Retry-After": {"12"}}
	err := newOpenAICompatStatusErr(openAICompatProfileForKind("kimi"), nil, "kimi-k2.5", http.StatusTooManyRequests, headers, "application/json", []byte(`{"error":{"message":"rate limit"}}`))

	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusTooManyRequests)
	}
	retryAfter := err.RetryAfter()
	if retryAfter == nil {
		t.Fatal("RetryAfter() = nil, want non-nil")
	}
	if *retryAfter != 12*time.Second {
		t.Fatalf("RetryAfter() = %v, want %v", *retryAfter, 12*time.Second)
	}
}

func TestNewOpenAICompatStatusErr_EmptyBodyHasErrorCode(t *testing.T) {
	t.Parallel()

	err := newOpenAICompatStatusErr(openAICompatProfileForKind("codex"), nil, "gpt-5.5", http.StatusInternalServerError, nil, "application/json", nil)

	if err.StatusCode() != http.StatusInternalServerError {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusInternalServerError)
	}
	if err.ErrorCode() != openAICompatEmptyUpstreamResponseCode {
		t.Fatalf("ErrorCode() = %q, want %q", err.ErrorCode(), openAICompatEmptyUpstreamResponseCode)
	}
	if !strings.Contains(err.Error(), "empty upstream response") || !strings.Contains(err.Error(), `"bytes":0`) || !strings.Contains(err.Error(), `"sha256":`) {
		t.Fatalf("Error() = %q, want safe empty-body metadata", err.Error())
	}
}

func TestNewOpenAICompatStatusErr_ParsesSSEDataErrorBody(t *testing.T) {
	t.Parallel()

	const secret = "openai-compat-sse-error-sentinel"
	body := []byte("event: error\ndata: {\"error\":{\"message\":\"invalid function arguments json string " + secret + "\",\"type\":\"invalid_request_error\",\"code\":\"invalid_function_arguments\"}}\n\n")
	err := newOpenAICompatStatusErr(openAICompatProfileForKind("mimo"), nil, "mimo-v2.5-pro", http.StatusBadRequest, nil, "text/event-stream", body)

	if err.StatusCode() != http.StatusBadRequest {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusBadRequest)
	}
	if err.ErrorCode() != "invalid_function_arguments" {
		t.Fatalf("ErrorCode() = %q, want invalid_function_arguments", err.ErrorCode())
	}
	if got := err.Error(); strings.Contains(got, secret) || !strings.Contains(got, "error_code=invalid_function_arguments") || !strings.Contains(got, `"sha256":`) || !strings.Contains(got, `"content_type":"text/event-stream"`) {
		t.Fatalf("Error() = %q, want safe parsed classification and metadata", got)
	}
}

func TestNewOpenAICompatStatusErr_KimiBillingCycleUsageLimitHasRetryAfter(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"message":"You've reached your usage limit for this billing cycle. Your quota will be refreshed in the next cycle. Upgrade to get more: https://www.kimi.com/code/console?from=quota-upgrade"}}`)
	err := newOpenAICompatStatusErr(openAICompatProfileForKind("kimi"), nil, "kimi-k2.6", http.StatusForbidden, nil, "application/json", body)

	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusTooManyRequests)
	}
	retryAfter := err.RetryAfter()
	if retryAfter == nil {
		t.Fatal("RetryAfter() = nil, want non-nil")
	}
	if *retryAfter != openAICompatAccountQuotaRetryWait {
		t.Fatalf("RetryAfter() = %v, want %v", *retryAfter, openAICompatAccountQuotaRetryWait)
	}
}

func TestNewOpenAICompatPayloadDiagnostic_CollectsDeepSeekFailureHints(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[
					{"type":"input_text","text":"hi"},
					{"type":"input_image","image_url":"https://example.com/a.png"}
				]
			}
		],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"temperature":0.6,
		"top_p":0.95,
		"max_tokens":8192,
		"max_completion_tokens":4096,
		"max_output_tokens":2048,
		"thinking":{"type":"enabled","budget_tokens":512},
		"stop":["DONE","END"]
	}`)

	profile := openAICompatProfile{Kind: "deepseek"}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    "https://api.deepseek.com/v1",
		"compat_name": "deepseek-official",
	}}

	diag := newOpenAICompatPayloadDiagnostic(nil, payload, profile, auth, "deepseek-v4-pro", "/v1/chat/completions", "/v1/chat/completions", nil, nil)

	if diag.CompatKindSource != "base_url_inference" {
		t.Fatalf("CompatKindSource = %q, want base_url_inference", diag.CompatKindSource)
	}
	if diag.PayloadSize != len(payload) {
		t.Fatalf("PayloadSize = %d, want %d", diag.PayloadSize, len(payload))
	}
	if diag.ToolDefinitionCount != 1 {
		t.Fatalf("ToolDefinitionCount = %d, want 1", diag.ToolDefinitionCount)
	}
	if got := diag.ContentPartTypes; len(got) != 2 || got[0] != "input_image:1" || got[1] != "input_text:1" {
		t.Fatalf("ContentPartTypes = %#v, want input_image:1,input_text:1", got)
	}
	if got := diag.InputItemTypes; len(got) != 1 || got[0] != "message:1" {
		t.Fatalf("InputItemTypes = %#v, want message:1", got)
	}
	if diag.Temperature != "0.6" {
		t.Fatalf("Temperature = %q, want 0.6", diag.Temperature)
	}
	if diag.TopP != "0.95" {
		t.Fatalf("TopP = %q, want 0.95", diag.TopP)
	}
	if diag.MaxTokens != 8192 {
		t.Fatalf("MaxTokens = %d, want 8192", diag.MaxTokens)
	}
	if diag.MaxCompletionTokens != 4096 {
		t.Fatalf("MaxCompletionTokens = %d, want 4096", diag.MaxCompletionTokens)
	}
	if diag.MaxOutputTokens != 2048 {
		t.Fatalf("MaxOutputTokens = %d, want 2048", diag.MaxOutputTokens)
	}
	if diag.ThinkingBudget != 512 {
		t.Fatalf("ThinkingBudget = %d, want 512", diag.ThinkingBudget)
	}
	if diag.StopCount != 2 {
		t.Fatalf("StopCount = %d, want 2", diag.StopCount)
	}
}
