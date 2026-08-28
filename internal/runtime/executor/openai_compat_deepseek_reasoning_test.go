package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func registerThinkingModelForProvider(t *testing.T, clientID, provider, modelID string, levels []string) {
	t.Helper()

	var support *registry.ThinkingSupport
	if levels != nil {
		support = &registry.ThinkingSupport{Levels: append([]string(nil), levels...)}
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(clientID)
	reg.RegisterClient(clientID, provider, []*registry.ModelInfo{{
		ID:       modelID,
		Thinking: support,
	}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})
}

func TestOpenAICompatExecutorDeepSeekOfficialAllowsMaxReasoningEffort(t *testing.T) {
	registerThinkingModelForProvider(t, "deepseek-official-openai", "deepseek", "deepseek-v4-pro", []string{"low", "medium", "high", "xhigh", "max"})

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "deepseek-official",
			Kind: "deepseek",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "deepseek-official",
			"compat_kind":  "deepseek",
			"provider_key": "deepseek",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"enabled"},
			"reasoning_effort":"max"
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "max" {
		t.Fatalf("reasoning_effort = %q, want max; body=%s", got, string(gotBody))
	}
}

func TestOpenAICompatExecutorDeepSeekDowngradesIncompleteDefaultHistoryBeforeUpstream(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-history","object":"chat.completion","model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "deepseek",
	}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"messages":[
				{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"},
				{"role":"user","content":"continue"}
			]
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "messages.0.reasoning_content").Exists() {
		t.Fatalf("missing reasoning content was synthesized: %s", gotBody)
	}
}

func TestOpenAICompatExecutorDeepSeekRejectsIncompleteExplicitHistoryBeforeUpstream(t *testing.T) {
	upstreamCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "deepseek",
	}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"reasoning_effort":"high",
			"messages":[
				{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}
			]
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err == nil {
		t.Fatal("Execute() should reject explicit thinking with incomplete history")
	}
	if upstreamCalled {
		t.Fatal("upstream should not be called for deterministic incomplete history")
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InvalidThinkingHistory || typed.Scope != failurecontract.ScopeRequest || typed.ProviderCode != deepSeekInvalidThinkingHistoryCode || typed.Retryable {
		t.Fatalf("failure = %+v, want non-retryable request-scoped DeepSeek history error", typed)
	}
	if err.Error() != deepSeekInvalidThinkingHistoryMessage {
		t.Fatalf("error = %q, want customer guidance", err.Error())
	}
}

func TestOpenAICompatExecutorDeepSeekWorkBuddyDowngradesIncompleteExplicitHistory(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-workbuddy","object":"chat.completion","model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "deepseek",
	}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"reasoning_effort":"high",
			"messages":[
				{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.ClientProfileMetadataKey:           "workbuddy",
			cliproxyexecutor.ReasoningEffortOriginalMetadataKey: "high",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be removed after downgrade: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "messages.0.reasoning_content").Exists() {
		t.Fatalf("missing reasoning history was synthesized: %s", gotBody)
	}
}

func TestOpenAICompatExecutorDeepSeekClaudeCodeDowngradesIncompleteExplicitHistory(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-claude-code","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "deepseek",
	}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-flash",
		Payload: []byte(`{
			"model":"deepseek-v4-flash",
			"reasoning_effort":"high",
			"messages":[
				{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}
			]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.ClientProfileMetadataKey:           "claude_code",
			cliproxyexecutor.ReasoningEffortOriginalMetadataKey: "high",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be removed after downgrade: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "messages.0.reasoning_content").Exists() {
		t.Fatalf("missing reasoning history was synthesized: %s", gotBody)
	}
}

func TestOpenAICompatExecutorDeepSeekRechecksIncompleteHistoryAfterPayloadConfig(t *testing.T) {
	upstreamCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "deepseek-v4-pro", Protocol: "openai"}},
			Params: map[string]any{"thinking.type": "enabled"},
		}}},
	})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "deepseek",
	}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"messages":[
				{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}
			]
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err == nil {
		t.Fatal("Execute() should reject payload config that re-enables thinking with incomplete history")
	}
	if upstreamCalled {
		t.Fatal("upstream should not be called after payload config re-enables unsafe thinking")
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InvalidThinkingHistory || typed.Scope != failurecontract.ScopeRequest || typed.Retryable {
		t.Fatalf("failure = %+v, want non-retryable request-scoped DeepSeek history error", typed)
	}
}

func TestOpenAICompatExecutorDeepSeekChatNormalizesOpenAIAliases(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    server.URL + "/v1",
			"api_key":     "test",
			"compat_kind": "deepseek",
		},
	}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"messages":[{"role":"user","content":"return json"}],
			"enable_thinking":false,
			"max_completion_tokens":4096,
			"response_format":{"type":"json_object"}
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096; body=%s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "enable_thinking").Exists() || gjson.GetBytes(gotBody, "max_completion_tokens").Exists() {
		t.Fatalf("unsupported Chat aliases reached upstream: %s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "response_format.type").String(); got != "json_object" {
		t.Fatalf("response_format.type = %q, want json_object; body=%s", got, gotBody)
	}
}

func TestOpenAICompatExecutorDeepSeekChatRejectsJSONSchemaBeforeUpstream(t *testing.T) {
	tests := []struct {
		name         string
		sourceFormat string
		payload      []byte
	}{
		{
			name:         "chat response format",
			sourceFormat: "openai",
			payload:      []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}}}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstreamCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "openai-compatibility",
				Attributes: map[string]string{
					"base_url":    server.URL + "/v1",
					"api_key":     "test",
					"compat_kind": "deepseek",
				},
			}
			_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "deepseek-v4-pro",
				Payload: test.payload,
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(test.sourceFormat)})
			if err == nil {
				t.Fatal("expected request_feature_unsupported error")
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstreamCalls = %d, want 0", upstreamCalls)
			}
			status, ok := err.(interface {
				StatusCode() int
				ErrorCode() string
			})
			if !ok {
				t.Fatalf("error type %T does not expose status/error code", err)
			}
			if status.StatusCode() != http.StatusBadRequest || status.ErrorCode() != "request_feature_unsupported" {
				t.Fatalf("status/code = %d/%q, want 400/request_feature_unsupported", status.StatusCode(), status.ErrorCode())
			}
			if !strings.Contains(err.Error(), "deepseek_chat_json_schema") || !strings.Contains(err.Error(), "CPA 不会静默降级 json_schema") {
				t.Fatalf("error = %q, want stable marker and explicit no-downgrade guidance", err.Error())
			}
		})
	}
}

func TestOpenAICompatExecutorDeepSeekFlashUsesNativeResponses(t *testing.T) {
	registerThinkingModelForProvider(t, "deepseek-flash-responses", "deepseek", "deepseek-v4-flash", []string{"low", "high", "max"})

	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","model":"deepseek-v4-flash","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    server.URL + "/v1",
			"api_key":     "test",
			"compat_kind": "deepseek",
		},
	}
	payload := []byte(`{
		"model":"deepseek-v4-flash",
		"input":[{"role":"developer","content":"Be concise."},{"role":"user","content":"Inspect the repository."}],
		"tools":[
			{"type":"function","name":"lookup","description":"Look up data","parameters":{"type":"object","properties":{"q":{"type":"string"}}},"strict":true},
			{"type":"custom","name":"apply_patch","description":"Apply a patch"},
			{"type":"web_search"}
		],
		"tool_choice":"auto",
		"reasoning":{"effort":"low"},
		"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}},
		"stream":false
	}`)

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("native Responses request was translated to chat: %s", string(gotBody))
	}
	for path, want := range map[string]string{
		"input.0.role":     "developer",
		"tools.0.type":     "function",
		"tools.0.name":     "lookup",
		"tools.1.type":     "custom",
		"tools.1.name":     "apply_patch",
		"tools.2.type":     "web_search",
		"tool_choice":      "auto",
		"reasoning.effort": "low",
		"text.format.type": "json_schema",
	} {
		if got := gjson.GetBytes(gotBody, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, string(gotBody))
		}
	}
	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "response" {
		t.Fatalf("response object = %q, want response; payload=%s", got, string(resp.Payload))
	}
}

func TestOpenAICompatExecutorDeepSeekResponsesRejectsUnsupportedToolsBeforeUpstream(t *testing.T) {
	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		t.Run(model, func(t *testing.T) {
			upstreamCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "openai-compatibility",
				Attributes: map[string]string{
					"base_url":    server.URL + "/v1",
					"api_key":     "test",
					"compat_kind": "deepseek",
				},
			}
			payload := []byte(fmt.Sprintf(`{
				"model":%q,
				"input":[{"role":"user","content":"Inspect the repository."}],
				"tools":[
					{"type":"function","name":"lookup","parameters":{"type":"object"}},
					{"type":"namespace","name":"workspace"},
					{"type":"web_search"},
					{"type":"custom","name":"shell"}
				]
			}`, model))

			_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   model,
				Payload: payload,
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
			})
			if err == nil {
				t.Fatal("expected request_feature_unsupported error")
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstreamCalls = %d, want 0", upstreamCalls)
			}
			status, ok := err.(interface {
				StatusCode() int
				ErrorCode() string
			})
			if !ok {
				t.Fatalf("error type %T does not expose status/error code", err)
			}
			if status.StatusCode() != http.StatusBadRequest || status.ErrorCode() != "request_feature_unsupported" {
				t.Fatalf("status/code = %d/%q, want 400/request_feature_unsupported", status.StatusCode(), status.ErrorCode())
			}
			for _, marker := range []string{"deepseek_responses_unsupported_tools", "工具命名空间(namespace)", "自定义工具(custom:shell)", "网页搜索(web_search)", "CPA 不会静默删除"} {
				if !strings.Contains(err.Error(), marker) {
					t.Fatalf("error = %q, want marker %q", err.Error(), marker)
				}
			}
		})
	}
}

func TestOpenAICompatExecutorHTTPDeepSeekResponsesAllowsWebSearch(t *testing.T) {
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[]}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"api_key":     "test",
			"compat_kind": "deepseek",
		},
	}
	req := httptest.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{
		"model":"deepseek-v4-flash",
		"input":"Search the web.",
		"tools":[{"type":"web_search"}]
	}`))
	req.RequestURI = ""

	resp, err := exec.HttpRequest(context.Background(), auth, req)
	if err != nil {
		t.Fatalf("HttpRequest error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}
}

func TestOpenAICompatExecutorDeepSeekProUsesNativeResponses(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","model":"deepseek-v4-pro","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    server.URL + "/v1",
			"api_key":     "test",
			"compat_kind": "deepseek",
		},
	}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","input":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"},{"type":"custom","name":"apply_patch"}],"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gjson.GetBytes(gotBody, "messages").Exists() || !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("Pro Responses request should remain native: %s", string(gotBody))
	}
	for path, want := range map[string]string{
		"tools.0.type":     "web_search",
		"tools.1.type":     "custom",
		"tools.1.name":     "apply_patch",
		"text.format.type": "json_schema",
	} {
		if got := gjson.GetBytes(gotBody, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, gotBody)
		}
	}
	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "response" {
		t.Fatalf("response object = %q, want response; payload=%s", got, string(resp.Payload))
	}
}

func TestOpenAICompatExecutorDeepSeekResponsesRejectsUnsupportedStateAndAttachments(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		marker string
	}{
		{name: "previous response state", body: `{"model":"deepseek-v4-pro","input":"hi","previous_response_id":"resp_1"}`, marker: "deepseek_responses_state"},
		{name: "stored response state", body: `{"model":"deepseek-v4-pro","input":"hi","store":true}`, marker: "deepseek_responses_state"},
		{name: "image input", body: `{"model":"deepseek-v4-pro","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`, marker: "deepseek_official_image_input"},
		{name: "file input", body: `{"model":"deepseek-v4-pro","input":[{"role":"user","content":[{"type":"input_file","file_data":"data:text/plain;base64,SGk="}]}]}`, marker: "deepseek_official_file_input"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstreamCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "deepseek",
			}}
			_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model: "deepseek-v4-pro", Payload: []byte(test.body),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
			if err == nil || !strings.Contains(err.Error(), test.marker) {
				t.Fatalf("error = %v, want marker %q", err, test.marker)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstreamCalls = %d, want 0", upstreamCalls)
			}
		})
	}
}

func TestOpenAICompatExecutorDeepSeekFIMUsesBetaCompletions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"text_completion","model":"deepseek-v4-pro","choices":[{"index":0,"text":" middle ","finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/beta", "api_key": "test", "compat_kind": "deepseek",
	}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","prompt":"left","suffix":"right","max_tokens":128,"thinking":{"type":"disabled"}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/completions",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/beta/completions" {
		t.Fatalf("path = %q, want /beta/completions", gotPath)
	}
	if gjson.GetBytes(gotBody, "messages").Exists() || gjson.GetBytes(gotBody, "thinking").Exists() {
		t.Fatalf("FIM payload was converted to chat or kept thinking controls: %s", gotBody)
	}
	if got := gjson.GetBytes(gotBody, "suffix").String(); got != "right" {
		t.Fatalf("suffix = %q, want right; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "text_completion" {
		t.Fatalf("object = %q, want text_completion; payload=%s", got, resp.Payload)
	}
}

func TestOpenAICompatExecutorDeepSeekFIMRejectsThinking(t *testing.T) {
	err := validateDeepSeekFIMRequest([]byte(`{"model":"deepseek-v4-pro","prompt":"left","suffix":"right","reasoning_effort":"high"}`))
	if err == nil || !strings.Contains(err.Error(), "deepseek_fim_non_thinking_only") {
		t.Fatalf("error = %v, want non-thinking FIM marker", err)
	}
}

func TestOpenAICompatRequestURLRoutesOfficialDeepSeekBetaFeatures(t *testing.T) {
	profile := openAICompatProfileForKind("deepseek")
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		body     string
		want     string
	}{
		{name: "fim", baseURL: "https://api.deepseek.com/v1", endpoint: "/completions", body: `{"prompt":"left"}`, want: "https://api.deepseek.com/beta/completions"},
		{name: "chat prefix", baseURL: "https://api.deepseek.com/v1", endpoint: "/chat/completions", body: `{"messages":[{"role":"assistant","content":"prefix","prefix":true}]}`, want: "https://api.deepseek.com/beta/chat/completions"},
		{name: "ordinary chat", baseURL: "https://api.deepseek.com/v1", endpoint: "/chat/completions", body: `{"messages":[{"role":"user","content":"hi"}]}`, want: "https://api.deepseek.com/v1/chat/completions"},
		{name: "third party unchanged", baseURL: "https://deepseek.example.com/v1", endpoint: "/completions", body: `{"prompt":"left"}`, want: "https://deepseek.example.com/v1/completions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := openAICompatRequestURL(test.baseURL, profile, test.endpoint, []byte(test.body)); got != test.want {
				t.Fatalf("URL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOpenAICompatExecutorDeepSeekFlashResponsesStreamHasNoDoneMarker(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    server.URL + "/v1",
			"api_key":     "test",
			"compat_kind": "deepseek",
		},
	}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "DeepSeek-算力/deepseek-v4-flash(high)",
		Payload: []byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true,"reasoning":{"effort":"high"}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var output strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		output.Write(chunk.Payload)
		output.WriteByte('\n')
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("native Responses request must not contain chat stream_options: %s", string(gotBody))
	}
	if !strings.Contains(output.String(), `"response.completed"`) {
		t.Fatalf("missing response.completed event: %q", output.String())
	}
	if strings.Contains(output.String(), "[DONE]") {
		t.Fatalf("native Responses stream must not append [DONE]: %q", output.String())
	}
}

func TestOpenAICompatExecutorDeepSeekOfficialClaudeSourceAllowsMaxEffort(t *testing.T) {
	registerThinkingModelForProvider(t, "deepseek-official-claude", "deepseek", "deepseek-v4-pro", []string{"low", "medium", "high", "xhigh", "max"})

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "deepseek-official",
			Kind: "deepseek",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/anthropic",
			"api_key":      "test",
			"compat_name":  "deepseek-official",
			"compat_kind":  "deepseek",
			"provider_key": "deepseek",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"adaptive"},
			"output_config":{"effort":"max"}
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "max" {
		t.Fatalf("reasoning_effort = %q, want max; body=%s", got, string(gotBody))
	}
}

func TestOpenAICompatExecutorDeepSeekIntentRemapClampsMaxToFinalSupport(t *testing.T) {
	registerThinkingModelForProvider(t, "generic-openai-compat-remap", "openai-compatibility", "generic-openai-model", []string{"low", "medium", "high"})

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"generic-openai-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "generic-openai-model",
		Payload: []byte(`{
			"model":"generic-openai-model",
			"messages":[{"role":"user","content":"hi"}],
			"reasoning_effort":"max"
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey:          "deepseek-v4-pro[1m]",
			cliproxyexecutor.ReasoningEffortOriginalMetadataKey: "max",
			cliproxyexecutor.ClientProfileMetadataKey:           "claude_code",
		},
	})
	if err == nil {
		if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "high" {
			t.Fatalf("reasoning_effort = %q, want high; body=%s", got, string(gotBody))
		}
		return
	}
	t.Fatalf("Execute error: %v", err)
}

func TestOpenAICompatExecutorDeepSeekIntentStripsReasoningWhenFinalModelHasNoThinkingSupport(t *testing.T) {
	registerThinkingModelForProvider(t, "generic-openai-compat-no-thinking", "openai-compatibility", "plain-openai-model", nil)

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"plain-openai-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "test",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "plain-openai-model",
		Payload: []byte(`{
			"model":"plain-openai-model",
			"messages":[{"role":"user","content":"hi"}],
			"reasoning_effort":"max"
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey:          "deepseek-v4-pro",
			cliproxyexecutor.ReasoningEffortOriginalMetadataKey: "max",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gjson.GetBytes(gotBody, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be stripped; body=%s", string(gotBody))
	}
}

func TestOpenAICompatExecutorDeepSeekOfficialReasoningNoneDisablesThinking(t *testing.T) {
	registerThinkingModelForProvider(t, "deepseek-official-openai-none", "deepseek", "deepseek-v4-pro", []string{"low", "medium", "high", "xhigh", "max"})

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "deepseek-official",
			Kind: "deepseek",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "deepseek-official",
			"compat_kind":  "deepseek",
			"provider_key": "deepseek",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"messages":[{"role":"user","content":"hi"}]
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey:          "deepseek-v4-pro",
			cliproxyexecutor.ReasoningEffortOriginalMetadataKey: "none",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be stripped for DeepSeek official none: %s", string(gotBody))
	}
}

func TestOpenAICompatExecutorGenericProviderDoesNotGloballyAllowMax(t *testing.T) {
	registerThinkingModelForProvider(t, "generic-openai-compat", "openai-compatibility", "generic-openai-model", []string{"low", "medium", "high"})

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": serverURLForUnsupportedDeepSeekTest(),
			"api_key":  "test",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "generic-openai-model",
		Payload: []byte(`{
			"model":"generic-openai-model",
			"messages":[{"role":"user","content":"hi"}],
			"reasoning_effort":"max"
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "generic-openai-model",
		},
	})
	if err == nil {
		t.Fatal("expected validation error for unrelated generic openai-compat max effort")
	}
	if !strings.Contains(err.Error(), `level "max" not supported`) {
		t.Fatalf("error = %v, want level not supported", err)
	}
}

func serverURLForUnsupportedDeepSeekTest() string {
	return "https://example.com/v1"
}
