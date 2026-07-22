package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type antigravityBoundsRoundTripperFunc func(*http.Request) (*http.Response, error)

func (fn antigravityBoundsRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type antigravityBoundsBody struct {
	io.Reader
	closes int
}

func (body *antigravityBoundsBody) Close() error {
	body.closes++
	return nil
}

func antigravityBoundsAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         "antigravity-bounds",
		Attributes: map[string]string{"base_url": "https://antigravity.test"},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

func antigravityBoundsRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "gemini-2.5-flash",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
	}
}

func antigravityBrotliBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := brotli.NewWriter(&encoded)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return encoded.Bytes()
}

func TestAntigravityExecutorBoundsAndDecodesNonStreamResponse(t *testing.T) {
	payload := []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}}`)
	body := &antigravityBoundsBody{Reader: bytes.NewReader(antigravityBrotliBytes(t, payload))}
	var upstreamRequestBytes int64
	ctx := internalpayload.WithTransformReport(context.Background(), int64(len(antigravityBoundsRequest().Payload)))
	releaseReport := internalpayload.RetainTransformReport(ctx)
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", antigravityBoundsRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requestBody, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("read upstream request: %v", errRead)
		}
		upstreamRequestBytes = int64(len(requestBody))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":     []string{"application/json"},
				"Content-Encoding": []string{"br"},
				"Content-Length":   []string{"123"},
				"ETag":             []string{`"stale"`},
			},
			Body:    body,
			Request: req,
		}, nil
	}))

	response, err := NewAntigravityExecutor(&config.Config{}).Execute(
		ctx,
		antigravityBoundsAuth(),
		antigravityBoundsRequest(),
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !bytes.Equal(response.Payload, payload) {
		t.Fatalf("Execute() payload = %s, want %s", response.Payload, payload)
	}
	if response.Headers.Get("Content-Encoding") != "" || response.Headers.Get("Content-Length") != "" || response.Headers.Get("ETag") != "" {
		t.Fatalf("stale response headers = %#v", response.Headers)
	}
	if body.closes != 1 {
		t.Fatalf("response body close count = %d, want 1", body.closes)
	}
	assertTransformStageContract(t, ctx, releaseReport, "request_plan.antigravity", upstreamRequestBytes)
}

func TestAntigravityExecutorRejectsOversizedErrorBody(t *testing.T) {
	body := &antigravityBoundsBody{Reader: io.LimitReader(antigravityXReader{}, helps.DefaultUpstreamErrorBodyBytes+1)}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", antigravityBoundsRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: body, Request: req}, nil
	}))

	_, err := NewAntigravityExecutor(&config.Config{}).Execute(
		ctx,
		antigravityBoundsAuth(),
		antigravityBoundsRequest(),
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider || typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != "upstream_error_body_too_large" {
		t.Fatalf("failure = %#v", typed)
	}
	if body.closes != 1 {
		t.Fatalf("response body close count = %d, want 1", body.closes)
	}
}

func TestAntigravityExecutorBoundsAndDecodesSSE(t *testing.T) {
	eventPayload := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}}`
	body := &antigravityBoundsBody{Reader: bytes.NewReader(antigravityBrotliBytes(t, []byte("data: "+eventPayload+"\r\r")))}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", antigravityBoundsRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":     []string{"text/event-stream"},
				"Content-Encoding": []string{"br"},
				"Content-Length":   []string{"123"},
			},
			Body:    body,
			Request: req,
		}, nil
	}))

	result, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(
		ctx,
		antigravityBoundsAuth(),
		antigravityBoundsRequest(),
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	result.Cancel()
	if !strings.Contains(output.String(), `"text":"ok"`) {
		t.Fatalf("stream output = %s", output.String())
	}
	if result.Headers.Get("Content-Encoding") != "" || result.Headers.Get("Content-Length") != "" {
		t.Fatalf("stale response headers = %#v", result.Headers)
	}
	if body.closes != 1 {
		t.Fatalf("response body close count = %d, want 1", body.closes)
	}
}

func TestAntigravityExecutorRejectsOversizedSSEEvent(t *testing.T) {
	body := &antigravityBoundsBody{Reader: io.MultiReader(
		strings.NewReader("data: "),
		io.LimitReader(antigravityXReader{}, helps.DefaultUpstreamSSEEventBytes+1),
		strings.NewReader("\n\n"),
	)}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", antigravityBoundsRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
	}))

	result, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(
		ctx,
		antigravityBoundsAuth(),
		antigravityBoundsRequest(),
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	typed, ok := failurecontract.As(streamErr)
	if !ok || typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider || typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != "upstream_sse_event_too_large" {
		t.Fatalf("failure = %#v", typed)
	}
	if body.closes != 1 {
		t.Fatalf("response body close count = %d, want 1", body.closes)
	}
}

func TestAppendAntigravityBufferedPayloadEnforcesAggregateLimit(t *testing.T) {
	var buffer bytes.Buffer
	if err := appendAntigravityBufferedPayload(&buffer, []byte("abc"), 4); err != nil {
		t.Fatalf("exact-limit append error = %v", err)
	}
	if got := buffer.String(); got != "abc\n" {
		t.Fatalf("exact-limit buffer = %q, want %q", got, "abc\\n")
	}

	err := appendAntigravityBufferedPayload(&buffer, []byte("x"), 4)
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider || typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != "upstream_success_body_too_large" {
		t.Fatalf("failure = %#v", typed)
	}
	if got := buffer.String(); got != "abc\n" {
		t.Fatalf("overflow mutated buffer = %q", got)
	}
}

func TestAntigravityExecutorStreamCancelInterruptsGroundingHEAD(t *testing.T) {
	const (
		model       = "gemini-3.1-flash-lite-cancel-test"
		redirectURL = "https://vertexaisearch.cloud.google.com/grounding-api-redirect/cancel-test"
	)
	registry.GetGlobalRegistry().RegisterClient("test-antigravity-stream-cancel", "antigravity", []*registry.ModelInfo{{
		ID:                model,
		SupportsWebSearch: true,
	}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("test-antigravity-stream-cancel") })

	event := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"groundingMetadata":{"groundingChunks":[{"web":{"uri":"` + redirectURL + `"}}]}}]}}`
	body := &antigravityBoundsBody{Reader: strings.NewReader("data: " + event + "\n\n")}
	headStarted := make(chan struct{})
	headCanceled := make(chan struct{})
	parentCtx, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent)
	ctx := context.WithValue(parentCtx, "cliproxy.roundtripper", antigravityBoundsRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodPost:
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
				Request:    req,
			}, nil
		case http.MethodHead:
			close(headStarted)
			<-req.Context().Done()
			close(headCanceled)
			return nil, req.Context().Err()
		default:
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
	}))

	result, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(
		ctx,
		antigravityBoundsAuth(),
		cliproxyexecutor.Request{
			Model: model,
			Payload: []byte(`{
				"model":"` + model + `",
				"messages":[{"role":"user","content":"search"}],
				"tools":[{"type":"web_search_20250305","name":"web_search"}]
			}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case <-headStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("grounding HEAD did not start")
	}
	result.Cancel()
	select {
	case <-headCanceled:
	case <-time.After(2 * time.Second):
		cancelParent()
		t.Fatal("StreamResult.Cancel did not cancel grounding HEAD")
	}
	streamClosed := make(chan struct{})
	go func() {
		for range result.Chunks {
		}
		close(streamClosed)
	}()
	select {
	case <-streamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream channel did not close after cancellation")
	}
	if body.closes != 1 {
		t.Fatalf("response body close count = %d, want 1", body.closes)
	}
}

type antigravityXReader struct{}

func (antigravityXReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = 'x'
	}
	return len(buffer), nil
}
