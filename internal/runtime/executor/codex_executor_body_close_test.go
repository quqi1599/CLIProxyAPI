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
