package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCapGeminiMaxOutputTokensUsesOutputTokenLimit(t *testing.T) {
	body := []byte(`{"generationConfig":{"maxOutputTokens":500000,"temperature":0.2},"contents":[]}`)

	out := capGeminiMaxOutputTokens(body, "gemini-3.1-pro-preview")

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 65536 {
		t.Fatalf("maxOutputTokens = %d, want 65536", got)
	}
	if got := gjson.GetBytes(out, "generationConfig.temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", got)
	}
}

func TestCapGeminiMaxOutputTokensLeavesAllowedOrUnknown(t *testing.T) {
	tests := []struct {
		name  string
		model string
		body  []byte
		want  int64
	}{
		{
			name:  "allowed value",
			model: "gemini-3.1-pro-preview",
			body:  []byte(`{"generationConfig":{"maxOutputTokens":64000}}`),
			want:  64000,
		},
		{
			name:  "unknown model",
			model: "custom-gemini-model",
			body:  []byte(`{"generationConfig":{"maxOutputTokens":500000}}`),
			want:  500000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := capGeminiMaxOutputTokens(tt.body, tt.model)
			if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != tt.want {
				t.Fatalf("maxOutputTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGeminiExecutorExecuteCapsMaxOutputTokensBeforeUpstream(t *testing.T) {
	var upstreamMaxOutputTokens int64
	var upstreamRequestBytes int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		upstreamRequestBytes = int64(len(body))
		upstreamMaxOutputTokens = gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "test-key",
		"base_url": server.URL,
	}}
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-preview",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":500000}}`),
	}

	ctx := internalpayload.WithTransformReport(context.Background(), int64(len(req.Payload)))
	releaseReport := internalpayload.RetainTransformReport(ctx)
	if _, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if upstreamMaxOutputTokens != 65536 {
		t.Fatalf("upstream maxOutputTokens = %d, want 65536", upstreamMaxOutputTokens)
	}
	assertTransformStageContract(t, ctx, releaseReport, "request_plan.gemini", upstreamRequestBytes)
}

func TestGeminiExecutorDecodesBrotliResponseAndStripsFramingHeaders(t *testing.T) {
	want := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	compressed := brotliPayload(t, want)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	resp, err := exec.Execute(context.Background(), auth, geminiTestRequest(), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !bytes.Equal(resp.Payload, want) {
		t.Fatalf("payload = %q, want %q", resp.Payload, want)
	}
	if got := resp.Headers.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := resp.Headers.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty", got)
	}
}

func TestGeminiExecutorRejectsOversizedErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(helps.DefaultUpstreamErrorBodyBytes+1)))
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	_, err := exec.Execute(context.Background(), auth, geminiTestRequest(), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	failure, ok := failurecontract.As(err)
	if !ok || failure.ProviderCode != "upstream_error_body_too_large" {
		t.Fatalf("error = %#v, want upstream_error_body_too_large", err)
	}
}

func TestGeminiExecutorStreamDecodesBrotliSSEByEvent(t *testing.T) {
	jsonChunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	compressed := brotliPayload(t, append(append([]byte("event: message\r\ndata: "), jsonChunk...), []byte("\r\n\r\n")...))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	result, err := exec.ExecuteStream(context.Background(), auth, geminiTestRequest(), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	defer result.Close()
	if result.Cancel == nil {
		t.Fatal("Cancel is nil")
	}
	if got := result.Headers.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	var chunks [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		chunks = append(chunks, chunk.Payload)
	}
	if len(chunks) != 1 || !bytes.Equal(chunks[0], jsonChunk) {
		t.Fatalf("chunks = %q, want one JSON chunk", chunks)
	}
	result.Close()
}

func TestGeminiExecutorStreamCancelDoesNotEmitReadError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	result, err := exec.ExecuteStream(context.Background(), auth, geminiTestRequest(), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	first := <-result.Chunks
	if first.Err != nil || len(first.Payload) == 0 {
		t.Fatalf("first chunk = %#v", first)
	}
	result.Close()
	result.Close()
	select {
	case chunk, ok := <-result.Chunks:
		if ok && chunk.Err != nil {
			t.Fatalf("cancel emitted stream error = %v", chunk.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after cancel")
	}
}

func TestGeminiExecutorStreamRejectsEventOverDefaultLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(append(bytes.Repeat([]byte("x"), int(helps.DefaultUpstreamSSEEventBytes)), '\n', '\n'))
	}))
	defer server.Close()

	exec := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	result, err := exec.ExecuteStream(context.Background(), auth, geminiTestRequest(), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	defer result.Close()
	for chunk := range result.Chunks {
		if chunk.Err == nil {
			continue
		}
		failure, ok := failurecontract.As(chunk.Err)
		if !ok || failure.Kind != failurecontract.UpstreamProtocolError || failure.Scope != failurecontract.ScopeProvider || failure.ProviderCode != "upstream_sse_event_too_large" {
			t.Fatalf("stream error = %#v, want typed 16 MiB SSE limit failure", chunk.Err)
		}
		return
	}
	t.Fatal("stream completed without a 16 MiB SSE limit failure")
}

func TestGeminiVertexExecutorDecodesBrotliResponse(t *testing.T) {
	want := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	compressed := brotliPayload(t, want)
	var upstreamRequestBytes int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read upstream request: %v", errRead)
		}
		upstreamRequestBytes = int64(len(requestBody))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write(compressed)
	}))
	defer server.Close()

	exec := NewGeminiVertexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	req := geminiTestRequest()
	ctx := internalpayload.WithTransformReport(context.Background(), int64(len(req.Payload)))
	releaseReport := internalpayload.RetainTransformReport(ctx)
	resp, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !bytes.Equal(resp.Payload, want) {
		t.Fatalf("payload = %q, want %q", resp.Payload, want)
	}
	if got := resp.Headers.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	assertTransformStageContract(t, ctx, releaseReport, "request_plan.vertex", upstreamRequestBytes)
}

func geminiTestRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-preview",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}
}

func brotliPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := brotli.NewWriter(&out)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("write brotli payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close brotli writer: %v", err)
	}
	return out.Bytes()
}
