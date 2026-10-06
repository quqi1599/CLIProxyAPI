package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type miniMaxReceiptRoundTripper func(*http.Request) (*http.Response, error)

func (f miniMaxReceiptRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMiniMaxCanonicalModelAndSafeRejectionOnBothExecutors(t *testing.T) {
	for _, wire := range []string{"openai", "claude"} {
		for _, source := range []string{"openai", "claude", "openai-response"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", wire, source, stream), func(t *testing.T) {
					calls := 0
					base, effortPath := "https://api.minimaxi.com/v1", "reasoning_effort"
					if wire == "claude" {
						base, effortPath = "https://api.minimaxi.com/anthropic", "output_config.effort"
					}
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						body, _ := io.ReadAll(r.Body)
						if gjson.GetBytes(body, "model").String() != "MiniMax-M3.1-Flash-Preview" || gjson.GetBytes(body, effortPath).String() != "low" || gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
							t.Errorf("invalid model/effort: %s", body)
						}
						return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"bad_request_error","message":"invalid params, unknown model 'private-model' (2013)"}}`))}, nil
					}))
					body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"OK"}],"max_tokens":4096,"reasoning_effort":"low"}`)
					if source == "claude" {
						body = []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"OK"}],"max_tokens":4096,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`)
					}
					if source == "openai-response" {
						body = []byte(`{"model":"claude-sonnet-4-6","input":"OK","max_output_tokens":4096,"reasoning":{"effort":"low"}}`)
					}
					auth := &cliproxyauth.Auth{ID: "fixture", Provider: wire, Attributes: map[string]string{"base_url": base, "api_key": "fixture", "compat_kind": "minimax"}}
					req := cliproxyexecutor.Request{Model: "MiniMax-M3.1-flash", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source), OriginalRequest: body, Stream: stream}
					var err error
					if wire == "openai" {
						e := NewOpenAICompatExecutor("minimax-test", &config.Config{})
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
					if calls != 1 || f == nil || f.SemanticCode != "minimax_model_not_supported" || f.Scope != failurecontract.ScopeRequest || f.Retryable || strings.Contains(err.Error(), "private-model") {
						t.Fatalf("unsafe or missing diagnostic: calls=%d err=%v failure=%+v", calls, err, f)
					}
				})
			}
		}
	}
}

func TestMiniMaxDiagnosticBoundaries(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{"error":{"message":"requires adaptive thinking private-value"}}`, "minimax_adaptive_thinking_required"},
		{`{"error":{"message":"invalid output_config.effort private-value"}}`, "minimax_effort_invalid"},
		{`{"error":{"message":"reasoning_split=false is not supported private-value"}}`, "minimax_reasoning_split_unsupported"},
		{`{"base_resp":{"status_code":2013,"status_msg":"unknown model private-value"}}`, "minimax_model_not_supported"},
	} {
		err := newUpstreamStatusErr(400, nil, "application/json", []byte(tc.body), "minimax")
		if err.ErrorCode() != tc.code || strings.Contains(err.Error(), "private-value") {
			t.Fatal(err)
		}
	}
	for _, status := range []int{400, 401, 402, 403, 429, 500} {
		body := []byte(`{"error":{"code":"content_policy_violation","message":"unknown model private-value"}}`)
		before := newUpstreamStatusErr(status, nil, "application/json", body)
		after := newUpstreamStatusErr(status, nil, "application/json", body, "minimax")
		if before.ErrorCode() != after.ErrorCode() || before.StatusCode() != after.StatusCode() {
			t.Fatal("specific failure changed", status)
		}
	}
	if err := newUpstreamStatusErr(400, nil, "application/json", []byte(`{"error":{"message":"private-value (2013)"}}`), "minimax"); strings.HasPrefix(err.ErrorCode(), "minimax_") {
		t.Fatal("numeric code was guessed")
	}
}
