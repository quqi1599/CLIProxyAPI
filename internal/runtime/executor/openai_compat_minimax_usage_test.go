package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatMiniMaxM3StreamUsage(t *testing.T) {
	const usageJSON = `{"prompt_tokens":1980,"completion_tokens":32,"total_tokens":2012,"prompt_tokens_details":{"cached_tokens":1979}}`
	for _, tt := range []struct {
		name         string
		format       string
		payload      string
		finishUsage  bool
		omitDone     bool
		wantInput    int64
		inputPath    string
		cachePath    string
		outputPath   string
		terminalType string
	}{
		{name: "claude trailing usage", format: "claude", payload: `{"model":"claude-sonnet-4-6","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"OK"}]}`, wantInput: 1, inputPath: "usage.input_tokens", cachePath: "usage.cache_read_input_tokens", outputPath: "usage.output_tokens", terminalType: "message_stop"},
		{name: "claude finish usage", format: "claude", payload: `{"model":"claude-sonnet-4-6","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"OK"}]}`, finishUsage: true, wantInput: 1, inputPath: "usage.input_tokens", cachePath: "usage.cache_read_input_tokens", outputPath: "usage.output_tokens", terminalType: "message_stop"},
		{name: "claude clean EOF", format: "claude", payload: `{"model":"claude-sonnet-4-6","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"OK"}]}`, omitDone: true, wantInput: 1, inputPath: "usage.input_tokens", cachePath: "usage.cache_read_input_tokens", outputPath: "usage.output_tokens", terminalType: "message_stop"},
		{name: "chat missing option", format: "openai", payload: `{"model":"MiniMax-M3","stream":true,"messages":[{"role":"user","content":"OK"}]}`, wantInput: 1980, inputPath: "usage.prompt_tokens", cachePath: "usage.prompt_tokens_details.cached_tokens", outputPath: "usage.completion_tokens"},
		{name: "chat false option", format: "openai", payload: `{"model":"MiniMax-M3","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"OK"}]}`, wantInput: 1980, inputPath: "usage.prompt_tokens", cachePath: "usage.prompt_tokens_details.cached_tokens", outputPath: "usage.completion_tokens"},
		{name: "responses via chat", format: "openai-response", payload: `{"model":"claude-sonnet-4-6","stream":true,"input":"OK"}`, wantInput: 1980, inputPath: "response.usage.input_tokens", cachePath: "response.usage.input_tokens_details.cached_tokens", outputPath: "response.usage.output_tokens", terminalType: "response.completed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requestBodies := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Errorf("read request: %v", errRead)
				}
				requestBodies <- body
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("upstream path = %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"id\":\"cache-probe\",\"model\":\"MiniMax-M3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}],\"usage\":null}\n\n")
				finish := `{"id":"cache-probe","model":"MiniMax-M3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`
				// Reproduce the provider contract: usage is absent unless requested.
				includeUsage := gjson.GetBytes(body, "stream_options.include_usage").Bool()
				if includeUsage && tt.finishUsage {
					finish += `,"usage":` + usageJSON
				}
				_, _ = fmt.Fprintf(w, "data: %s}\n\n", finish)
				if includeUsage && !tt.finishUsage {
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"cache-probe\",\"model\":\"MiniMax-M3\",\"choices\":[],\"usage\":%s}\n\n", usageJSON)
				}
				if !tt.omitDone {
					_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor("minimax-test", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "minimax",
			}}
			payload := []byte(tt.payload)
			stream, err := executor.ExecuteStream(context.Background(), auth,
				cliproxyexecutor.Request{Model: "MiniMax-M3", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tt.format), OriginalRequest: payload, Stream: true})
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			defer stream.Cancel()
			var output bytes.Buffer
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream error: %v", chunk.Err)
				}
				output.Write(chunk.Payload)
				output.WriteByte('\n')
			}
			body := <-requestBodies
			if !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
				t.Fatalf("MiniMax-M3 stream must request usage: %s", body)
			}
			if got := gjson.GetBytes(body, "model").String(); got != "MiniMax-M3" {
				t.Fatalf("upstream model = %q", got)
			}
			usageEvents, terminalEvents := 0, 0
			for _, line := range strings.Split(output.String(), "\n") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				// Chat passthrough yields JSON chunks; translated formats yield SSE.
				if !gjson.Valid(data) {
					continue
				}
				if tt.terminalType != "" && gjson.Get(data, "type").String() == tt.terminalType {
					terminalEvents++
				}
				if gjson.Get(data, tt.cachePath).Int() != 1979 {
					continue
				}
				usageEvents++
				if got := gjson.Get(data, tt.inputPath).Int(); got != tt.wantInput {
					t.Errorf("input tokens = %d, want %d: %s", got, tt.wantInput, data)
				}
				if got := gjson.Get(data, tt.outputPath).Int(); got != 32 {
					t.Errorf("output tokens = %d, want 32", got)
				}
			}
			if usageEvents != 1 {
				t.Errorf("cache usage events = %d, want 1: %s", usageEvents, output.String())
			}
			if tt.terminalType != "" && terminalEvents != 1 {
				t.Errorf("terminal events = %d, want 1: %s", terminalEvents, output.String())
			}
		})
	}
}

func TestOpenAICompatMiniMaxM3NonStreamUsage(t *testing.T) {
	requestBodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request: %v", errRead)
		}
		requestBodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"cache-probe","model":"MiniMax-M3","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1980,"completion_tokens":32,"total_tokens":2012,"prompt_tokens_details":{"cached_tokens":1979}}}`)
	}))
	defer server.Close()
	executor := NewOpenAICompatExecutor("minimax-test", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "minimax",
	}}
	payload := []byte(`{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"OK"}]}`)
	response, err := executor.Execute(context.Background(), auth,
		cliproxyexecutor.Request{Model: "MiniMax-M3", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if body := <-requestBodies; gjson.GetBytes(body, "stream_options").Exists() {
		t.Fatalf("nonstream request must not inject stream_options: %s", body)
	}
	for path, want := range map[string]int64{"usage.input_tokens": 1, "usage.output_tokens": 32, "usage.cache_read_input_tokens": 1979} {
		if got := gjson.GetBytes(response.Payload, path).Int(); got != want {
			t.Errorf("%s = %d, want %d: %s", path, got, want, response.Payload)
		}
	}
}

func TestOpenAICompatMiniMaxM3UsageCapabilityScope(t *testing.T) {
	for _, tt := range []struct {
		kind, model string
		wantUsage   bool
	}{
		{kind: "minimax", model: "MiniMax-M3", wantUsage: true},
		{kind: "minimax", model: "minimax-m3", wantUsage: true},
		{kind: "minimax", model: "MiniMax-M2.7"},
		{kind: "minimax", model: "MiniMax-M2.7-highspeed"},
		{kind: "minimax", model: "image-01"},
		{kind: "xiaomi", model: "MiniMax-M3"},
	} {
		t.Run(tt.kind+"/"+tt.model, func(t *testing.T) {
			profile := openAICompatCapabilityProfileForModel(openAICompatProfileForKind(tt.kind), tt.model)
			if profile.SupportsStreamUsage != tt.wantUsage {
				t.Fatalf("SupportsStreamUsage = %v, want %v", profile.SupportsStreamUsage, tt.wantUsage)
			}
			out := scrubOpenAICompatPayloadForModel([]byte(`{"stream":true,"stream_options":{"include_usage":true}}`), openAICompatProfileForKind(tt.kind), tt.model, "https://api.minimaxi.com/v1")
			if got := gjson.GetBytes(out, "stream_options.include_usage").Bool(); got != tt.wantUsage {
				t.Fatalf("scrubbed include_usage = %v, want %v", got, tt.wantUsage)
			}
		})
	}
}
