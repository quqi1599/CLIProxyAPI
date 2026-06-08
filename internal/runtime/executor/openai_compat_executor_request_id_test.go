package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestOpenAICompatExecutorStreamAddsRequestCorrelationHeaders(t *testing.T) {
	var gotCliproxyID string
	var gotRequestID string
	var gotClientRequestID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCliproxyID = r.Header.Get("X-Cliproxy-Request-Id")
		gotRequestID = r.Header.Get("X-Request-Id")
		gotClientRequestID = r.Header.Get("X-Client-Request-Id")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	ctx := internallogging.WithRequestID(context.Background(), "req-shared-stream-1")
	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	if gotCliproxyID != "req-shared-stream-1" {
		t.Fatalf("X-Cliproxy-Request-Id = %q, want %q", gotCliproxyID, "req-shared-stream-1")
	}
	if gotRequestID != "req-shared-stream-1" {
		t.Fatalf("X-Request-Id = %q, want %q", gotRequestID, "req-shared-stream-1")
	}
	if gotClientRequestID != "req-shared-stream-1" {
		t.Fatalf("X-Client-Request-Id = %q, want %q", gotClientRequestID, "req-shared-stream-1")
	}
}

func TestOpenAICompatExecutorPreservesClientRequestIDHeader(t *testing.T) {
	var gotCliproxyID string
	var gotRequestID string
	var gotClientRequestID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCliproxyID = r.Header.Get("X-Cliproxy-Request-Id")
		gotRequestID = r.Header.Get("X-Request-Id")
		gotClientRequestID = r.Header.Get("X-Client-Request-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	ctx := internallogging.WithRequestID(context.Background(), "req-shared-nonstream-1")
	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Headers: http.Header{
			"X-Request-Id": []string{"client-supplied-req"},
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotCliproxyID != "req-shared-nonstream-1" {
		t.Fatalf("X-Cliproxy-Request-Id = %q, want %q", gotCliproxyID, "req-shared-nonstream-1")
	}
	if gotRequestID != "client-supplied-req" {
		t.Fatalf("X-Request-Id = %q, want %q", gotRequestID, "client-supplied-req")
	}
	if gotClientRequestID != "req-shared-nonstream-1" {
		t.Fatalf("X-Client-Request-Id = %q, want %q", gotClientRequestID, "req-shared-nonstream-1")
	}
}
