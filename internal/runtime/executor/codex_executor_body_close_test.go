package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
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

type codexBlockingBody struct {
	closed chan struct{}
	once   sync.Once
	closes atomic.Int32
	prefix []byte
}

func (b *codexBlockingBody) Read(buffer []byte) (int, error) {
	if len(b.prefix) > 0 {
		read := copy(buffer, b.prefix)
		b.prefix = b.prefix[read:]
		return read, nil
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *codexBlockingBody) Close() error {
	b.closes.Add(1)
	b.once.Do(func() { close(b.closed) })
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
			response:    `{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}]}`,
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

func TestCodexExecutorRejectsOversizedUpstreamErrorBody(t *testing.T) {
	body := &codexCountingBody{Reader: strings.NewReader(strings.Repeat("x", int(helps.DefaultUpstreamErrorBodyBytes+1)))}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", codexRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
			Request:    req,
		}, nil
	}))

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test"}}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err == nil {
		t.Fatal("Execute() error = nil, want bounded upstream failure")
	}
	failure, ok := failurecontract.As(err)
	if !ok || failure.ProviderCode != "upstream_error_body_too_large" {
		t.Fatalf("Execute() error = %#v, want upstream_error_body_too_large", err)
	}
	if body.closes != 1 {
		t.Fatalf("response body close count = %d, want 1", body.closes)
	}
}

func TestCodexExecutorStreamBoundsSSEEventAndClosesOnce(t *testing.T) {
	body := &codexCountingBody{Reader: io.MultiReader(
		strings.NewReader("data: "),
		strings.NewReader(strings.Repeat("x", int(helps.DefaultUpstreamSSEEventBytes))),
	)}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", codexRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    req,
		}, nil
	}))

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test"}}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	failure, ok := failurecontract.As(streamErr)
	if !ok || failure.ProviderCode != "upstream_sse_event_too_large" {
		t.Fatalf("stream error = %#v, want upstream_sse_event_too_large", streamErr)
	}
	if body.closes != 1 {
		t.Fatalf("response body close count = %d, want 1", body.closes)
	}
}

func TestCodexExecutorStreamCancelClosesResponseOnce(t *testing.T) {
	body := &codexBlockingBody{closed: make(chan struct{}), prefix: []byte("data: ")}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", codexRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    req,
		}, nil
	}))

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test"}}, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	result.Close()
	result.Close()
	done := make(chan struct{})
	go func() {
		for range result.Chunks {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
	if got := body.closes.Load(); got != 1 {
		t.Fatalf("response body close count = %d, want 1", got)
	}
}

func TestCodexExecutorCompletedStreamClosesWithoutWaitingForConnection(t *testing.T) {
	for _, format := range []string{"openai-response", "openai", "claude"} {
		t.Run(format, func(t *testing.T) {
			const completed = `data: {"type":"response.completed","response":{"id":"resp_terminal_fixture","object":"response","status":"completed","model":"gpt-6.1-sol","output":[{"id":"msg_terminal_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}],"usage":{"input_tokens":17,"output_tokens":3,"total_tokens":20}}}` + "\n\n"
			body := &codexBlockingBody{closed: make(chan struct{}), prefix: []byte(completed)}
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", codexRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
			}))
			result, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture"}}, cliproxyexecutor.Request{
				Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"OK","stream":true}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), ResponseFormat: sdktranslator.FromString(format), Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			defer result.Close()
			done := make(chan error, 1)
			go func() {
				var terminalErr error
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						terminalErr = chunk.Err
					}
				}
				done <- terminalErr
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("completed stream became a failure: %v", err)
				}
			case <-time.After(500 * time.Millisecond):
				result.Close()
				<-done
				t.Fatal("completed response still waits for transport EOF or caller cancellation")
			}
			if got := body.closes.Load(); got != 1 {
				t.Fatalf("body closed %d times, want 1", got)
			}
		})
	}
}

type codexTrailingReadError struct{}

func (codexTrailingReadError) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCodexExecutorOnlyIgnoresReadFailureAfterConfirmedCompletion(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_terminal", true: "after_terminal"}[completed], func(t *testing.T) {
			prefix := `data: {"type":"response.output_text.delta","item_id":"msg_fixture","output_index":0,"content_index":0,"delta":"OK"}` + "\n\n"
			if completed {
				prefix += `data: {"type":"response.completed","response":{"id":"resp_fixture","object":"response","status":"completed","output":[{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":17,"output_tokens":3,"total_tokens":20}}}` + "\n\n"
			}
			body := &codexCountingBody{Reader: io.MultiReader(strings.NewReader(prefix), codexTrailingReadError{})}
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", codexRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
			}))
			result, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture"}}, cliproxyexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"OK"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			var streamErr error
			var output strings.Builder
			for chunk := range result.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
			}
			if completed {
				if streamErr != nil || !strings.Contains(output.String(), `"type":"response.completed"`) {
					t.Fatalf("completion lost or became a failure: %v", streamErr)
				}
			} else if streamErr == nil {
				t.Fatal("pre-terminal read failure was swallowed")
			}
			if body.closes != 1 {
				t.Fatalf("body closed %d times, want 1", body.closes)
			}
		})
	}
}
