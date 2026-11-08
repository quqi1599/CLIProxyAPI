package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestKimiCodePreservesClientIdentityAcrossProtocols(t *testing.T) {
	for _, wire := range []string{"openai", "claude"} {
		for _, source := range []string{"openai", "claude", "openai-response"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", wire, source, stream), func(t *testing.T) {
					calls := 0
					ctx := contextWithGinHeaders(map[string]string{"User-Agent": "real-coding-client/1.2.3"})
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						if r.Header.Get("User-Agent") != "real-coding-client/1.2.3" {
							t.Errorf("client identity replaced: %q", r.Header.Get("User-Agent"))
						}
						for _, h := range []string{"X-Msh-Device-Id", "X-Stainless-Runtime", "X-App", "Anthropic-Beta"} {
							if r.Header.Get(h) != "" {
								t.Errorf("synthetic header %s reached Kimi", h)
							}
						}
						return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"synthetic rejection"}}`))}, nil
					}))
					body := []byte(`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"synthetic"}],"max_tokens":4096}`)
					if source == "openai-response" {
						body = []byte(`{"model":"kimi-k3.1-pro","input":"synthetic","max_output_tokens":4096}`)
					}
					base := "https://api.kimi.com/coding/v1"
					if wire == "claude" {
						base = "https://api.kimi.ai/coding"
					}
					auth := &cliproxyauth.Auth{ID: "identity", Provider: wire, Attributes: map[string]string{"base_url": base, "api_key": "synthetic", "compat_kind": "kimi"}}
					req := cliproxyexecutor.Request{Model: "k3.1-pro", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source), OriginalRequest: body, Stream: stream}
					if wire == "openai" {
						e := NewOpenAICompatExecutor("kimi-test", &config.Config{})
						if stream {
							_, _ = e.ExecuteStream(ctx, auth, req, opts)
						} else {
							_, _ = e.Execute(ctx, auth, req, opts)
						}
					} else {
						e := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
						if stream {
							_, _ = e.ExecuteStream(ctx, auth, req, opts)
						} else {
							_, _ = e.Execute(ctx, auth, req, opts)
						}
					}
					if calls != 1 {
						t.Fatalf("upstream calls=%d", calls)
					}
				})
			}
		}
	}
}
