package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestRejectLargeCodexToolHistoryBeforeUpstream(t *testing.T) {
	metadata := map[string]any{
		cliproxyexecutor.ClientProfileMetadataKey:        "workbuddy",
		cliproxyexecutor.MessageCountMetadataKey:         272,
		cliproxyexecutor.ToolCountMetadataKey:            12,
		cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey: "test-key",
			"base_url":                   "https://compat.example.com/v1",
		},
	}
	err := rejectLargeCodexToolHistory(context.Background(), []byte(`{"input":[]}`), metadata, auth)
	if err == nil {
		t.Fatal("expected non-native long tool history to be rejected")
	}
	if !strings.Contains(err.Error(), "request_feature_unsupported: codex_tool_history_too_large") {
		t.Fatalf("error = %q, want actionable request_feature_unsupported marker", err)
	}
}

func TestRejectLargeCodexToolHistoryAllowsNativeAuth(t *testing.T) {
	metadata := map[string]any{
		cliproxyexecutor.ClientProfileMetadataKey:        "workbuddy",
		cliproxyexecutor.MessageCountMetadataKey:         272,
		cliproxyexecutor.ToolCountMetadataKey:            12,
		cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey: "test-key",
			"base_url":                   "https://api.openai.com/v1",
		},
	}
	if err := rejectLargeCodexToolHistory(context.Background(), nil, metadata, auth); err != nil {
		t.Fatalf("native Responses auth should be allowed, got %v", err)
	}
}

func TestLargeCodexToolHistoryRequiresToolHistory(t *testing.T) {
	metadata := map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex", cliproxyexecutor.MessageCountMetadataKey: 240}
	if cliproxyauth.RequiresNativeResponsesToolHistory(metadata, nil) {
		t.Fatal("plain long conversation without tool history should not trigger the tool-history guard")
	}
}

func TestCodexToolHistoryGuardUsesDeclaredCapability(t *testing.T) {
	for _, test := range []struct {
		name       string
		baseURL    string
		capability string
		wantReject bool
	}{
		{name: "declared custom Responses", baseURL: "https://compat.example.com/v1", capability: "true"},
		{name: "explicitly disabled official", baseURL: "https://api.openai.com/v1", capability: "false", wantReject: true},
		{name: "unknown custom", baseURL: "https://compat.example.com/v1", wantReject: true},
		{name: "native default", baseURL: "https://api.openai.com/v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{
				"base_url": test.baseURL, "native_responses": test.capability,
			}}
			err := rejectLargeCodexToolHistory(context.Background(), nil, longCodexToolHistoryMetadata(), auth)
			if (err != nil) != test.wantReject {
				t.Fatalf("reject = %v, want %v", err, test.wantReject)
			}
			if test.wantReject {
				assertCodexToolHistoryRejection(t, err)
			}
		})
	}
}

func TestCodexToolHistoryGuardPreservesShortAndPlainConversations(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": "https://compat.example.com/v1"}}
	for _, test := range []struct {
		name         string
		profile      string
		messages     int
		interactions int
	}{
		{name: "short tool history", profile: "codex", messages: 239, interactions: 20},
		{name: "long plain conversation", profile: "workbuddy", messages: 350},
		{name: "unrecognized client unchanged", profile: "other", messages: 350, interactions: 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := map[string]any{
				cliproxyexecutor.ClientProfileMetadataKey:        test.profile,
				cliproxyexecutor.MessageCountMetadataKey:         test.messages,
				cliproxyexecutor.ToolInteractionCountMetadataKey: test.interactions,
			}
			if err := rejectLargeCodexToolHistory(context.Background(), nil, metadata, auth); err != nil {
				t.Fatalf("unaffected request rejected: %v", err)
			}
		})
	}
}

func TestCodexToolHistoryGuardAcrossHTTPAndWebsocketTransports(t *testing.T) {
	for _, transport := range []string{"http", "http-stream", "websocket", "websocket-stream"} {
		for _, capability := range []string{"", "false", "true"} {
			t.Run(transport+"/native="+capability, func(t *testing.T) {
				var attempts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp-guard-test","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`)
					if strings.HasPrefix(transport, "websocket") {
						upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
						conn, err := upgrader.Upgrade(w, r, nil)
						if err != nil {
							t.Errorf("upgrade websocket: %v", err)
							return
						}
						defer func() { _ = conn.Close() }()
						if _, _, err := conn.ReadMessage(); err != nil {
							t.Errorf("read request: %v", err)
							return
						}
						if err := conn.WriteMessage(websocket.TextMessage, completed); err != nil {
							t.Errorf("write response: %v", err)
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(append(append([]byte("data: "), completed...), []byte("\n\n")...))
				}))
				defer server.Close()

				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
				auth := &cliproxyauth.Auth{ID: "guard-transport-test", Provider: "codex", Attributes: map[string]string{
					cliproxyauth.AttributeAPIKey: "test-key", "base_url": server.URL, "native_responses": capability,
				}}
				req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"test"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: longCodexToolHistoryMetadata()}
				var err error
				var stream *cliproxyexecutor.StreamResult
				switch transport {
				case "http":
					_, err = NewCodexExecutor(cfg).Execute(context.Background(), auth, req, opts)
				case "http-stream":
					stream, err = NewCodexExecutor(cfg).ExecuteStream(context.Background(), auth, req, opts)
				case "websocket":
					_, err = NewCodexWebsocketsExecutor(cfg).Execute(context.Background(), auth, req, opts)
				case "websocket-stream":
					stream, err = NewCodexWebsocketsExecutor(cfg).ExecuteStream(context.Background(), auth, req, opts)
				}
				if capability != "true" {
					assertCodexToolHistoryRejection(t, err)
					if got := attempts.Load(); got != 0 {
						t.Fatalf("unverified route received %d upstream requests, want zero", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("declared Responses route rejected: %v", err)
				}
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Fatalf("stream failed: %v", chunk.Err)
						}
					}
				}
				if got := attempts.Load(); got != 1 {
					t.Fatalf("declared route received %d requests, want exactly one", got)
				}
			})
		}
	}
}

func TestCodexToolHistoryGuardRequestPlanModes(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": "https://compat.example.com/v1"}}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: []byte(`{"model":"gpt-5.6-sol","input":[]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: longCodexToolHistoryMetadata()}
	for _, test := range []struct {
		name string
		mode codexRequestPlanMode
	}{
		{name: "execute", mode: codexRequestPlanExecute},
		{name: "stream", mode: codexRequestPlanStream},
		{name: "compact", mode: codexRequestPlanCompact},
		{name: "local count", mode: codexRequestPlanCount},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := executor.prepareCodexRequestPlan(context.Background(), auth, req, opts, req.Model, test.mode)
			if test.mode == codexRequestPlanCount {
				if err != nil {
					t.Fatalf("local token counting rejected: %v", err)
				}
				return
			}
			assertCodexToolHistoryRejection(t, err)
		})
	}
}

func longCodexToolHistoryMetadata() map[string]any {
	return map[string]any{
		cliproxyexecutor.ClientProfileMetadataKey:        "codex",
		cliproxyexecutor.MessageCountMetadataKey:         272,
		cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
	}
}

func assertCodexToolHistoryRejection(t *testing.T, err error) {
	t.Helper()
	var rejected statusErr
	if !errors.As(err, &rejected) || rejected.StatusCode() != http.StatusBadRequest || rejected.ErrorCode() != "request_feature_unsupported" {
		t.Fatalf("error = %v, want deterministic 400/request_feature_unsupported", err)
	}
	for _, expected := range []string{"codex_tool_history_too_large", "未确认或未声明", "新建会话", "显式声明"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("error %q missing actionable marker %q", err, expected)
		}
	}
}
