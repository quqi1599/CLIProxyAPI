package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	claudecodex "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/claude"
	chatcodex "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/openai/chat-completions"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCapabilityTranslatedHistoryBoundary(t *testing.T) {
	const model = "gpt-5.6-sol"
	messages := make([]map[string]any, 230)
	for i := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages[i] = map[string]any{"role": role, "content": "synthetic history"}
	}
	uses := []any{map[string]any{"type": "text", "text": "run tools"}}
	results := []any{map[string]any{"type": "text", "text": "tool results"}}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("call_%d", i)
		uses = append(uses, map[string]any{"type": "tool_use", "id": id, "name": "audit_tool", "input": map[string]any{}})
		results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"})
	}
	messages[228] = map[string]any{"role": "assistant", "content": uses}
	messages[229] = map[string]any{"role": "user", "content": results}
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 10, "messages": messages, "tools": []any{map[string]any{"name": "audit_tool", "input_schema": map[string]any{"type": "object"}}}})
	metadata := map[string]any{coreexecutor.ClientProfileMetadataKey: "workbuddy", coreexecutor.MessageCountMetadataKey: 230, coreexecutor.ToolInteractionCountMetadataKey: 20, coreexecutor.RequestPathMetadataKey: "/v1/messages"}
	converted := claudecodex.ConvertClaudeRequestToCodex(model, body, false)
	t.Logf("original messages=%d translated items=%d source-only guard=%v converted-body guard=%v; manager must preflight the converted shape", len(messages), gjson.GetBytes(converted, "input.#").Int(), coreauth.RequiresNativeResponsesToolHistory(metadata, body), coreauth.RequiresNativeResponsesToolHistory(metadata, converted))
	var verifiedHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifiedHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-audit\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()
	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	exec := NewCodexExecutor(cfg)
	verified := &coreauth.Auth{ID: t.Name() + "-verified", Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "synthetic-key", "native_responses": "true", "priority": "1"}}
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body, Metadata: metadata}
	req := coreexecutor.Request{Model: model, Payload: body}
	if _, err := exec.Execute(context.Background(), verified, req, opts); err != nil {
		t.Fatalf("verified route control failed: %v", err)
	}
	verifiedHits.Store(0)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			verifiedHits.Store(0)
			m := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			m.SetRetryConfig(0, 0, 5)
			m.RegisterExecutor(exec)
			unknown := &coreauth.Auth{ID: t.Name() + "-unknown", Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "synthetic-other-key", "priority": "100"}}
			ready := verified.Clone()
			ready.ID = t.Name() + "-verified"
			for _, a := range []*coreauth.Auth{unknown, ready} {
				registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
				defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
				if _, err := m.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if stream {
				var result *coreexecutor.StreamResult
				result, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = m.Execute(context.Background(), []string{"codex"}, req, opts)
			}
			if err != nil || verifiedHits.Load() != 1 {
				t.Fatalf("compatible available route never used: error=%v hits=%d", err, verifiedHits.Load())
			}
		})
	}
}

func capabilityBoundaryPayload(t *testing.T, format sdktranslator.Format) []byte {
	t.Helper()
	const model = "gpt-5.6-sol"
	messages := make([]map[string]any, 228)
	for i := range messages {
		messages[i] = map[string]any{"role": "user", "content": "synthetic history"}
	}
	body := map[string]any{"model": model, "max_tokens": 10}
	if format == sdktranslator.FormatOpenAI {
		calls := make([]any, 10)
		for i := range calls {
			calls[i] = map[string]any{"id": fmt.Sprintf("call_%d", i), "type": "function", "function": map[string]any{"name": "boundary_tool", "arguments": "{}"}}
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": "run tools", "tool_calls": calls})
		for i := range calls {
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "content": "ok"})
		}
		body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "boundary_tool", "parameters": map[string]any{"type": "object"}}}}
	} else {
		uses := []any{map[string]any{"type": "text", "text": "run tools"}}
		results := []any{map[string]any{"type": "text", "text": "tool results"}}
		for i := 0; i < 10; i++ {
			uses = append(uses, map[string]any{"type": "tool_use", "id": fmt.Sprintf("call_%d", i), "name": "boundary_tool", "input": map[string]any{}})
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("call_%d", i), "content": "ok"})
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": uses}, map[string]any{"role": "user", "content": results})
		body["tools"] = []any{map[string]any{"name": "boundary_tool", "input_schema": map[string]any{"type": "object"}}}
	}
	body["messages"] = messages
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if format == sdktranslator.FormatOpenAIResponse {
		return claudecodex.ConvertClaudeRequestToCodex(model, payload, false)
	}
	return payload
}

func TestCapabilityTranslatedHistoryManagerMatrix(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		for _, stream := range []bool{false, true} {
			for _, original := range []string{"same", "sdk_missing", "sdk_understated"} {
				t.Run(fmt.Sprintf("%s/stream=%v/original=%s", format, stream, original), func(t *testing.T) {
					const model = "gpt-5.6-sol"
					body := capabilityBoundaryPayload(t, format)
					var knownHits, unknownHits atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("Authorization") != "Bearer verified-synthetic" {
							unknownHits.Add(1)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						knownHits.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-boundary\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
					}))
					defer server.Close()
					manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
					manager.SetRetryConfig(0, 0, 5)
					manager.RegisterExecutor(NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}))
					auths := []*coreauth.Auth{
						{ID: t.Name() + "-bad-native", Provider: "codex", Attributes: map[string]string{"priority": "200"}, ModelStates: map[string]*coreauth.ModelState{model: {
							Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour), LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized},
							Health: coreauth.HealthState{Observed: true, LastStatusCode: http.StatusUnauthorized, BreakerState: coreauth.HealthBreakerOpen, OpenUntil: time.Now().Add(time.Hour)},
						}}},
						{ID: t.Name() + "-unknown", Provider: "codex", Attributes: map[string]string{"priority": "100", "base_url": server.URL, "api_key": "unknown-synthetic"}},
						{ID: t.Name() + "-verified", Provider: "codex", Attributes: map[string]string{"priority": "1", "base_url": server.URL, "api_key": "verified-synthetic", "native_responses": "true"}},
					}
					for _, auth := range auths {
						registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
						defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
						if _, err := manager.Register(context.Background(), auth); err != nil {
							t.Fatal(err)
						}
					}
					opts := coreexecutor.Options{SourceFormat: format, OriginalRequest: body, TokenCount: true, Metadata: map[string]any{
						coreexecutor.ClientProfileMetadataKey: "codex", coreexecutor.MessageCountMetadataKey: 1, coreexecutor.ToolInteractionCountMetadataKey: 1,
					}}
					if original == "sdk_missing" {
						opts.OriginalRequest = nil
					} else if original == "sdk_understated" {
						opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"short"}]}`)
					}
					req := coreexecutor.Request{Model: model, Payload: body}
					var err error
					if stream {
						var result *coreexecutor.StreamResult
						result, err = manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = manager.Execute(context.Background(), []string{"codex"}, req, opts)
					}
					if err != nil || knownHits.Load() != 1 || unknownHits.Load() != 0 {
						t.Fatalf("err=%v verified=%d unknown=%d, want verified once only", err, knownHits.Load(), unknownHits.Load())
					}
				})
			}
		}
	}
}

func TestCapabilityTranslatedHistoryRealConverterControl(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI} {
		body := capabilityBoundaryPayload(t, format)
		translated := claudecodex.ConvertClaudeRequestToCodex("gpt-5.6-sol", body, false)
		if format == sdktranslator.FormatOpenAI {
			translated = chatcodex.ConvertOpenAIRequestToCodex("gpt-5.6-sol", body, false)
		}
		if sourceCount, translatedCount := gjson.GetBytes(body, "messages.#").Int(), gjson.GetBytes(translated, "input.#").Int(); sourceCount >= 240 || translatedCount < 240 {
			t.Fatalf("invalid boundary fixture %s: source=%d translated=%d", format, sourceCount, translatedCount)
		}
	}
}
