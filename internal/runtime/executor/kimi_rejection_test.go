package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestKimiForbiddenDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, message, code string
		kind                failurecontract.Kind
		scope               failurecontract.Scope
	}{
		{"service", "Your current subscription does not have access to Kimi Code right now. private-upstream-text", failurecontract.KimiServiceAccessDenied, failurecontract.AuthenticationFailed, failurecontract.ScopeCredential},
		{"model", "Your current subscription does not have access to k3.1-pro. private-upstream-text", failurecontract.KimiModelAccessDenied, failurecontract.ModelUnavailable, failurecontract.ScopeModel},
		{"context", "Your current plan supports only k3.1-pro up to 256K context. private-upstream-text", failurecontract.KimiContextAccessDenied, failurecontract.ContextLengthExceeded, failurecontract.ScopeRequest},
		{"membership", "Unable to verify your membership benefits: private-upstream-text", failurecontract.KimiPermissionUnverified, failurecontract.UpstreamProtocolError, failurecontract.ScopeModel},
		{"opaque", "private-upstream-text", failurecontract.KimiPermissionUnverified, failurecontract.UpstreamProtocolError, failurecontract.ScopeModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, errMarshal := json.Marshal(map[string]any{"error": map[string]string{"type": "permission_denied_error", "message": tc.message}})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			headers := http.Header{"Retry-After": {"75"}}
			for _, err := range []statusErr{
				newUpstreamStatusErr(http.StatusForbidden, headers, "application/json", body, "kimi"),
				newOpenAICompatStatusErr(openAICompatProfile{Kind: "kimi"}, nil, "k3.1-pro", http.StatusForbidden, headers, "application/json", body),
			} {
				f := failurecontract.Classify(err)
				if err.ErrorCode() != tc.code || f.Kind != tc.kind || f.Scope != tc.scope || f.OuterStatus != http.StatusForbidden || f.Retryable {
					t.Fatalf("wrong Kimi failure: %+v", f)
				}
				if f.RetryAfter == nil || *f.RetryAfter != 75*time.Second || strings.Contains(err.Error(), "private-upstream-text") {
					t.Fatalf("unsafe or incomplete Kimi failure: %+v", f)
				}
			}
			other := newUpstreamStatusErr(http.StatusForbidden, headers, "application/json", body, "claude")
			if strings.HasPrefix(other.ErrorCode(), "kimi_") {
				t.Fatalf("changed another provider's failure: %v", other)
			}
		})
	}
}

func TestKimiRejectionOnChatResponsesAndMessages(t *testing.T) {
	for _, wire := range []string{"openai", "claude"} {
		for _, source := range []string{"openai", "claude", "openai-response"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", wire, source, stream), func(t *testing.T) {
					calls := 0
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"Invalid value for 'response_format': private-upstream-text"}}`))}, nil
					}))
					body := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"synthetic"}],"max_tokens":4096}`)
					if source == "openai-response" {
						body = []byte(`{"model":"kimi-k3","input":"synthetic","max_output_tokens":4096}`)
					}
					base := "https://api.kimi.com/coding/v1"
					if wire == "claude" {
						base = "https://api.kimi.com/coding"
					}
					auth := &cliproxyauth.Auth{ID: "fixture", Provider: wire, Attributes: map[string]string{"base_url": base, "api_key": "synthetic", "compat_kind": "kimi"}}
					req := cliproxyexecutor.Request{Model: "k3", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source), OriginalRequest: body, Stream: stream}
					var err error
					if wire == "openai" {
						e := NewOpenAICompatExecutor("kimi-test", &config.Config{})
						if stream {
							_, err = e.ExecuteStream(ctx, auth, req, opts)
						} else {
							_, err = e.Execute(ctx, auth, req, opts)
						}
					} else {
						e := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
						if stream {
							_, err = e.ExecuteStream(ctx, auth, req, opts)
						} else {
							_, err = e.Execute(ctx, auth, req, opts)
						}
					}
					f := failurecontract.Classify(err)
					if calls != 1 || f == nil || f.SemanticCode != "kimi_request_parameter_invalid" || f.ErrorDiagnostic.Param != "response_format" || f.Scope != failurecontract.ScopeRequest || f.Retryable || !strings.Contains(f.PublicMessage, "response_format") || strings.Contains(err.Error(), "private-upstream-text") {
						t.Fatalf("missing or unsafe diagnostic: calls=%d err=%v failure=%+v", calls, err, f)
					}
				})
			}
		}
	}
}

func TestKimiRejectionKeepsFailureAndProviderBoundaries(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"temperature must be 1"}}`)
	for _, status := range []int{401, 403, 429, 500} {
		for _, wire := range []string{"openai", "claude"} {
			err := newUpstreamStatusErr(status, nil, "application/json", body, "kimi")
			if wire == "openai" {
				err = newOpenAICompatStatusErr(openAICompatProfile{Kind: "kimi"}, nil, "k3", status, nil, "application/json", body)
			}
			if strings.HasPrefix(err.ErrorCode(), "kimi_") || err.ProviderStatusCode() != status {
				t.Fatalf("non-validation failure changed: %v", err)
			}
		}
	}
	for _, kind := range []string{"", "deepseek", "minimax"} {
		if err := newOpenAICompatStatusErr(openAICompatProfile{Kind: kind}, nil, "k3", 400, nil, "application/json", body); strings.HasPrefix(err.ErrorCode(), "kimi_") {
			t.Fatalf("other provider changed: %v", err)
		}
	}
	for _, body := range []string{
		`{"error":{"code":"content_policy_violation","message":"temperature must be 1"}}`,
		`{"error":{"type":"invalid_request_error","message":"unknown private-upstream-text"}}`,
	} {
		err := newOpenAICompatStatusErr(openAICompatProfile{Kind: "kimi"}, nil, "k3", 400, nil, "application/json", []byte(body))
		if strings.HasPrefix(err.ErrorCode(), "kimi_") || strings.Contains(err.Error(), "private-upstream-text") {
			t.Fatalf("specific or unknown rejection reclassified: %v", err)
		}
	}
}
