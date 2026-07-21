package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type codexCountingBody struct {
	io.Reader
	closes int
}

type codexRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f codexRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (b *codexCountingBody) Close() error {
	b.closes++
	return nil
}

func TestCodexExecutorNonStreamClosesResponseBodyOnce(t *testing.T) {
	tests := []struct {
		name        string
		alt         string
		path        string
		contentType string
		response    string
	}{
		{
			name:        "responses",
			path:        "/backend-api/codex/responses",
			contentType: "text/event-stream",
			response:    "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"output\":[]}}\n\n",
		},
		{
			name:        "compact",
			alt:         "responses/compact",
			path:        "/backend-api/codex/responses/compact",
			contentType: "application/json",
			response:    `{"id":"resp_1","object":"response.compaction"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &codexCountingBody{Reader: strings.NewReader(tt.response)}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", codexRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != tt.path {
					t.Fatalf("request path = %q, want %q", req.URL.Path, tt.path)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{tt.contentType}},
					Body:       body,
					Request:    req,
				}, nil
			}))

			executor := NewCodexExecutor(&config.Config{})
			_, err := executor.Execute(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test"}}, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          tt.alt,
			})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if body.closes != 1 {
				t.Fatalf("response body close count = %d, want 1", body.closes)
			}
		})
	}
}
