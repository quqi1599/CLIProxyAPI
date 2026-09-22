package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatAliyunDeepSeekStreamIntegrity(t *testing.T) {
	const content = `data: {"id":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"OK"},"finish_reason":null}],"usage":null}` + "\n\n"
	const finish = `data: {"id":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	const usage = `data: {"id":"probe","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":8,"total_tokens":108,"prompt_tokens_details":{"cached_tokens":64}}}`
	const done = "data: [DONE]\n\n"
	for _, format := range []string{"openai", "claude", "openai-response"} {
		for _, tt := range []struct {
			name, tail, wantCode string
		}{
			{"complete", finish + usage + "\n\n" + done, ""},
			{"usage at EOF without newline", finish + usage, ""},
			{"zero output is valid", finish + strings.ReplaceAll(usage, `"completion_tokens":8`, `"completion_tokens":0`) + "\n\n" + done, ""},
			{"usage in finish event", strings.ReplaceAll(usage, `"choices":[]`, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`) + "\n\n" + done, ""},
			{"tool finish", strings.ReplaceAll(finish, `"stop"`, `"tool_calls"`) + usage + "\n\n" + done, ""},
			{"missing tail EOF", "", "upstream_stream_incomplete"},
			{"missing tail DONE", done, "upstream_stream_incomplete"},
			{"finish but no usage EOF", finish, "upstream_stream_usage_missing"},
			{"finish but no usage DONE", finish + done, "upstream_stream_usage_missing"},
			{"usage but no finish", usage + "\n\n" + done, "upstream_stream_incomplete"},
			{"empty usage is not usage", finish + "data: {\"choices\":[],\"usage\":{}}\n\n" + done, "upstream_stream_usage_missing"},
			{"partial usage is not complete", finish + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100}}\n\n" + done, "upstream_stream_usage_missing"},
			{"negative usage", finish + strings.ReplaceAll(usage, `"completion_tokens":8`, `"completion_tokens":-1`) + "\n\n" + done, "upstream_stream_usage_missing"},
			{"string usage", finish + strings.ReplaceAll(usage, `"completion_tokens":8`, `"completion_tokens":"8"`) + "\n\n" + done, "upstream_stream_usage_missing"},
			{"HTTP 200 stream error", "data: {\"error\":{\"type\":\"server_error\",\"message\":\"private-upstream-content\"}}\n\n" + done, "upstream_stream_error"},
			{"HTTP 200 stream error then EOF", "data: {\"error\":{\"message\":\"private-upstream-content\"}}", "upstream_stream_error"},
		} {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				var attempts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					body, _ := io.ReadAll(r.Body)
					if !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
						t.Error("include_usage was not sent")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, content+tt.tail)
				}))
				defer server.Close()
				executor := NewOpenAICompatExecutor("aliyun-test", &config.Config{})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "qwen"}}
				payload := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"OK"}]}`)
				if format == "openai-response" {
					payload = []byte(`{"model":"deepseek-v4.1-flash","stream":true,"input":"OK"}`)
				}
				stream, err := executor.ExecuteStream(context.Background(), auth,
					cliproxyexecutor.Request{Model: "deepseek-v4.1-flash", Payload: payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format), OriginalRequest: payload, Stream: true})
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Cancel()
				var output bytes.Buffer
				var streamErr error
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						if streamErr != nil {
							t.Fatal("multiple terminal errors")
						}
						streamErr = chunk.Err
					}
					output.Write(chunk.Payload)
				}
				if tt.wantCode == "" {
					if streamErr != nil {
						t.Fatalf("complete stream failed: %v", streamErr)
					}
					if !strings.Contains(output.String(), "cached_tokens") && !strings.Contains(output.String(), "cache_read_input_tokens") {
						t.Fatal("valid usage did not reach the client")
					}
				} else {
					failure, ok := failurecontract.As(streamErr)
					if !ok || failure.ErrorCode() != tt.wantCode || failure.HTTPStatus != http.StatusBadGateway || failure.Retryable {
						t.Fatalf("error = %v, want non-retryable %s", streamErr, tt.wantCode)
					}
					if strings.Contains(output.String(), `"type":"response.completed"`) || strings.Contains(output.String(), `"type":"message_stop"`) {
						t.Fatal("incomplete stream emitted successful completion")
					}
					if strings.Contains(output.String(), "private-upstream-content") || strings.Contains(streamErr.Error(), "private-upstream-content") {
						t.Fatal("raw upstream error leaked")
					}
				}
				if attempts.Load() != 1 {
					t.Fatalf("request was replayed: %d", attempts.Load())
				}
			})
		}
	}
}
