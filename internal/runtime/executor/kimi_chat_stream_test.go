package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestKimiChatStreamCompletion(t *testing.T) {
	const content = `data: {"id":"test","choices":[{"index":0,"delta":{"content":"partial answer"},"finish_reason":null}]}` + "\n\n"
	const finish = `data: {"id":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	const usage = `data: {"choices":[],"usage":{"prompt_tokens":17,"completion_tokens":3,"total_tokens":20}}` + "\n\n"
	const done = "data: [DONE]\n\n"
	const incomplete = "upstream_stream_incomplete"
	const streamError = "upstream_stream_error"
	body := []byte(`{"model":"k3.1-pro","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	for _, route := range []string{"compat", "claude"} {
		for _, tc := range []struct {
			name, stream, wantCode string
			interrupted            bool
		}{
			{name: "complete", stream: content + finish + usage + done},
			{name: "finish without done", stream: content + finish + usage},
			{name: "upstream done", stream: content + done},
			{name: "length", stream: content + strings.Replace(finish, `"stop"`, `"length"`, 1) + done},
			{name: "tools", stream: content + strings.Replace(finish, `"stop"`, `"tool_calls"`, 1) + done},
			{name: "EOF after partial output", stream: content, wantCode: incomplete},
			{name: "usage does not finish the stream", stream: content + usage, wantCode: incomplete},
			{name: "socket closes mid-response", stream: content, interrupted: true, wantCode: incomplete},
			{name: "EOF before output", stream: ": heartbeat\n\n", wantCode: incomplete},
			{name: "broken data", stream: content + "data: {\"choices\":[", wantCode: incomplete},
			{name: "error before output", stream: `data: {"error":{"message":"private-upstream-content"}}` + "\n\n", wantCode: streamError},
			{name: "error after content and usage", stream: content + usage + `data: {"error":{"message":"private-upstream-content"}}` + "\n\n" + done, wantCode: streamError},
			{name: "named error", stream: content + "event: error\r\ndata: private-upstream-content\r\n\r\n", wantCode: streamError},
			{name: "named error without data", stream: content + "event: error\n\n", wantCode: streamError},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				e, auth := kimiKVVExecutor(route, &config.Config{})
				calls := 0
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.String() != "https://api.kimi.com/coding/v1/chat/completions" {
						t.Errorf("unexpected route: %s", r.URL)
					}
					var stream io.Reader = strings.NewReader(tc.stream)
					if tc.interrupted {
						stream = io.MultiReader(stream, iotest.ErrReader(io.ErrUnexpectedEOF))
					}
					return &http.Response{
						StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body: io.NopCloser(stream),
					}, nil
				}))
				output, err := executeKimiKVV(ctx, e, auth, body, sdktranslator.FormatOpenAI, true)
				if calls != 1 {
					t.Fatalf("upstream calls = %d, want 1", calls)
				}
				if strings.Contains(tc.stream, "partial answer") && !strings.Contains(output, "partial answer") {
					t.Fatal("partial content was lost")
				}
				if tc.wantCode == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				failure, ok := failurecontract.As(err)
				if !ok || failure.ErrorCode() != tc.wantCode || failure.HTTPStatus != http.StatusBadGateway ||
					failure.OuterStatus != http.StatusOK || failure.Scope != failurecontract.ScopeRequest || failure.Retryable {
					t.Fatalf("error = %v, want a non-replayable %s after HTTP 200", err, tc.wantCode)
				}
				if strings.Contains(output+err.Error(), "private-upstream-content") || strings.Contains(output, "[DONE]") {
					t.Fatal("failed stream emitted an upstream error payload or success marker")
				}
			})
		}
	}
}
