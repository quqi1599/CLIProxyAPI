package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCompactAddsDefaultInstructions(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing instructions",
			payload: `{"model":"gpt-5.4","input":"hello"}`,
		},
		{
			name:    "null instructions",
			payload: `{"model":"gpt-5.4","instructions":null,"input":"hello"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  "test",
			}}

			resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          "responses/compact",
				Stream:       false,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if gotPath != "/responses/compact" {
				t.Fatalf("path = %q, want %q", gotPath, "/responses/compact")
			}
			if !gjson.GetBytes(gotBody, "instructions").Exists() {
				t.Fatalf("expected instructions in compact request body, got %s", string(gotBody))
			}
			if gjson.GetBytes(gotBody, "instructions").Type != gjson.String {
				t.Fatalf("instructions type = %v, want string", gjson.GetBytes(gotBody, "instructions").Type)
			}
			if gjson.GetBytes(gotBody, "instructions").String() != "" {
				t.Fatalf("instructions = %q, want empty string", gjson.GetBytes(gotBody, "instructions").String())
			}
			if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
				t.Fatalf("payload = %s", string(resp.Payload))
			}
		})
	}
}

func TestCodexExecutorRemoteCompactionV2NativeValidatesBeforeStreaming(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	payload := []byte(`{"model":"gpt-5.4","stream":true,"input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: payload,
		Stream:          true,
		Metadata: map[string]any{
			cliproxyexecutor.CompactionIntentMetadataKey:      string(cliproxyexecutor.CompactionIntentV2Trigger),
			cliproxyexecutor.CompactionTriggerModeMetadataKey: cliproxyauth.ResponsesCompactionTriggerNativeStream,
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"type":"compaction"`)) || !bytes.Contains(output.Bytes(), []byte(`"encrypted_content":"opaque"`)) {
		t.Fatalf("stream lost compaction output: %s", output.Bytes())
	}
}

func TestCodexExecutorRemoteCompactionV2RejectsOrdinaryCompletedResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\"}]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	payload := []byte(`{"model":"gpt-5.4","stream":true,"input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload, Stream: true,
		Metadata: map[string]any{cliproxyexecutor.CompactionIntentMetadataKey: string(cliproxyexecutor.CompactionIntentV2Trigger)},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	chunks := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for chunk := range result.Chunks {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || chunks[0].Err == nil || len(chunks[0].Payload) != 0 {
		t.Fatalf("chunks = %+v, want one validation error with no committed payload", chunks)
	}
}

func TestCodexExecutorRemoteCompactionV2BridgeUsesLegacyEndpoint(t *testing.T) {
	t.Parallel()
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","output":[{"type":"compaction","encrypted_content":"opaque"}]}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	payload := []byte(`{"model":"gpt-5.4","stream":true,"previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload, Stream: true,
		Metadata: map[string]any{
			cliproxyexecutor.CompactionIntentMetadataKey:      string(cliproxyexecutor.CompactionIntentV2Trigger),
			cliproxyexecutor.CompactionTriggerModeMetadataKey: cliproxyauth.ResponsesCompactionTriggerBridgeLegacy,
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want /responses/compact", gotPath)
	}
	bridgeIntent, _ := cliproxyexecutor.DetectCompactionIntent(gotBody, "")
	if gjson.GetBytes(gotBody, "previous_response_id").Exists() || gjson.GetBytes(gotBody, "stream").Exists() || bridgeIntent == cliproxyexecutor.CompactionIntentV2Trigger {
		t.Fatalf("unsafe bridge fields reached compact endpoint: %s", gotBody)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if err = helps.ValidateResponsesCompactionStream(output.Bytes()); err != nil {
		t.Fatalf("bridge stream is invalid: %v\n%s", err, output.Bytes())
	}
}

func TestCodexExecutorContextManagementCompactionRestoresTranslatedField(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	payload := []byte(`{"model":"gpt-5.4","stream":true,"context_management":[{"type":"compaction","compact_threshold":1000}],"input":[{"type":"message","role":"user","content":[]}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload, Stream: true,
		Metadata: map[string]any{cliproxyexecutor.CompactionIntentMetadataKey: string(cliproxyexecutor.CompactionIntentContextManagement)},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
	}
	if got := gjson.GetBytes(gotBody, "context_management.0.type").String(); got != "compaction" {
		t.Fatalf("context_management was not restored: %s", gotBody)
	}
}
