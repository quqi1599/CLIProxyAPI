package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorCompactFallsBackToChatCompletionsForProfile(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"kimi-k2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("newapi-provider", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "newapi-provider",
			Kind: "newapi",
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "newapi-provider",
		"compat_kind": "newapi",
	}}
	payload := []byte(`{"model":"kimi-k2","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "kimi-k2",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	if !gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("expected chat completions payload, got %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("unexpected responses input payload, got %s", string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "response" {
		t.Fatalf("response object = %q, want %q; payload=%s", got, "response", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorQwen38ConvertsDisabledThinkingIntent(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-qwen","object":"chat.completion","created":1,"model":"qwen3.8-max-preview","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("qwen-provider", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "qwen-provider",
			Kind: "qwen",
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/compatible-mode/v1",
		"api_key":     "test",
		"compat_name": "qwen-provider",
		"compat_kind": "qwen",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model: "qwen3.8-max",
		Payload: []byte(`{
			"model":"qwen3.8-max",
			"messages":[{"role":"user","content":"Reply exactly OK"}],
			"reasoning_effort":"none",
			"stream":false
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !gjson.GetBytes(gotBody, "enable_thinking").Bool() {
		t.Fatalf("enable_thinking should be forced true: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "low" {
		t.Fatalf("reasoning_effort = %q, want low: %s", got, string(gotBody))
	}
}

func TestOpenAICompatExecutorParsesRetryAfterHints(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		body       string
		want       time.Duration
		wantStatus int
	}{
		{
			name:       "header",
			header:     "7",
			body:       `{"error":{"message":"rate limit exceeded"}}`,
			want:       7 * time.Second,
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "body",
			body:       `{"error":{"message":"quota exhausted","retry_after":9}}`,
			want:       9 * time.Second,
			wantStatus: http.StatusTooManyRequests,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL + "/v1",
				"api_key":  "test",
			}}
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5",
				Payload: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai"),
			})
			if err == nil {
				t.Fatal("expected error")
			}
			status, ok := err.(statusErr)
			if !ok {
				t.Fatalf("error type = %T, want statusErr", err)
			}
			if status.StatusCode() != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status.StatusCode(), tt.wantStatus)
			}
			retryAfter := status.RetryAfter()
			if retryAfter == nil {
				t.Fatal("expected retry-after hint")
			}
			if *retryAfter != tt.want {
				t.Fatalf("retry-after = %v, want %v", *retryAfter, tt.want)
			}
		})
	}
}

func TestOpenAICompatExecutorStreamScrubsUnsupportedFieldsForProfile(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("newapi-provider", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "newapi-provider",
			Kind: "newapi",
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "newapi-provider",
		"compat_kind": "newapi",
	}}

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model: "kimi-k2",
		Payload: []byte(`{
			"model":"kimi-k2",
			"messages":[{"role":"assistant","content":"thinking","reasoning_content":"hidden"}],
			"stream":true,
			"parallel_tool_calls":true,
			"reasoning":{"effort":"high"},
			"metadata":{"tenant":"demo"},
			"store":true
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
	}

	for _, path := range []string{
		"stream_options",
		"parallel_tool_calls",
		"reasoning",
		"metadata",
		"store",
		"messages.0.reasoning_content",
	} {
		if gjson.GetBytes(gotBody, path).Exists() {
			t.Fatalf("unexpected field %s in payload: %s", path, string(gotBody))
		}
	}
}

func TestSanitizeOpenAICompatHTTPRequestBodyRejectsLargeToolHistory(t *testing.T) {
	body := buildOpenAICompatToolHistoryBody(125, strings.Repeat("x", 32*1024))
	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/chat/completions", strings.NewReader(body))

	err := sanitizeOpenAICompatHTTPRequestBody(req, openAICompatProfileForKind("newapi"), "https://example.test/v1")
	if err == nil {
		t.Fatal("expected large OpenAI-compatible tool history rejection")
	}
	status, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status.StatusCode(), http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "large_openai_tool_history") {
		t.Fatalf("error = %q, want large_openai_tool_history marker", err.Error())
	}
}

func TestSanitizeOpenAICompatHTTPRequestBodyAllowsPreviouslyGuardedToolHistory(t *testing.T) {
	body := buildOpenAICompatToolHistoryBody(45, strings.Repeat("x", 24*1024))
	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/chat/completions", strings.NewReader(body))

	if err := sanitizeOpenAICompatHTTPRequestBody(req, openAICompatProfileForKind("newapi"), "https://example.test/v1"); err != nil {
		t.Fatalf("unexpected rejection for previously guarded OpenAI-compatible tool history: %v", err)
	}
}

func TestOpenAICompatExecutorRejectsLargeToolHistoryBeforeUpstream(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("newapi-provider", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "newapi-provider",
			Kind: "newapi",
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "newapi-provider",
		"compat_kind": "newapi",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(buildOpenAICompatToolHistoryBody(125, strings.Repeat("x", 32*1024))),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}

	_, err := executor.Execute(context.Background(), auth, req, opts)
	assertLargeOpenAICompatToolHistoryError(t, err)
	_, err = executor.ExecuteStream(context.Background(), auth, req, opts)
	assertLargeOpenAICompatToolHistoryError(t, err)
	if called {
		t.Fatal("upstream should not be called for large tool history")
	}
}

func assertLargeOpenAICompatToolHistoryError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected large OpenAI-compatible tool history rejection")
	}
	status, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status.StatusCode(), http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "large_openai_tool_history") {
		t.Fatalf("error = %q, want large_openai_tool_history marker", err.Error())
	}
}

func buildOpenAICompatToolHistoryBody(count int, content string) string {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.5","messages":[{"role":"assistant","content":"read files","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`)
	for i := 0; i < count; i++ {
		b.WriteString(`,{"role":"tool","tool_call_id":"call_1","content":"`)
		b.WriteString(content)
		b.WriteString(`"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestOpenAICompatExecutorClaudeSourceNormalizesKimiToolReferences(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"kimi-k2.6","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("kimi-provider", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "kimi-provider",
			Kind: "kimi",
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "kimi-provider",
		"compat_kind": "kimi",
	}}

	payload := []byte(`{
		"model":"kimi-k2.6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":[{"type":"text","text":"read it"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read:file","input":{"path":"/tmp/a.txt"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":"ok"},
				{"type":"text","text":"continue"}
			]}
		],
		"tools":[{
			"name":"read:file",
			"description":"Read a file",
			"input_schema":{
				"type":"object",
				"properties":{"path":{"type":"string"}},
				"required":["path"]
			}
		}],
		"tool_choice":{"type":"tool","name":"read:file"}
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "kimi-k2.6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := gjson.GetBytes(gotBody, "tools.0.type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function: %s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "tools.0.function.name").String(); got != "read_file" {
		t.Fatalf("tool name = %q, want read_file: %s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto for kimi thinking mode: %s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.1.tool_calls.0.function.name").String(); got != "read_file" {
		t.Fatalf("tool_call name = %q, want read_file: %s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "tools.0.input_schema").Exists() {
		t.Fatalf("input_schema should be converted away: %s", string(gotBody))
	}
}

func TestOpenAICompatExecutorClaudeSourceDowngradesToolSearch(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"glm-4.6","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("zhipu-provider", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "zhipu-provider",
			Kind: "zhipu",
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "zhipu-provider",
		"compat_kind": "zhipu",
	}}

	payload := []byte(`{
		"model":"glm-4.6",
		"max_tokens":1024,
		"messages":[{"role":"user","content":[{"type":"text","text":"read it"}]}],
		"tools":[
			{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
			{"name":"mcp__files__read","description":"Read files","defer_loading":true,"input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-4.6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := len(gjson.GetBytes(gotBody, "tools").Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1: %s", got, string(gotBody))
	}
	if strings.HasPrefix(gjson.GetBytes(gotBody, "tools.0.function.name").String(), "tool_search_tool_") {
		t.Fatalf("tool search tool should not reach upstream: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "tools.0.function.name").String(); got != "mcp__files__read" {
		t.Fatalf("tool name = %q, want mcp__files__read: %s", got, string(gotBody))
	}
}

func TestOpenAICompatExecutorMiniMaxClaudeSourceRestoresSystemRole(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"MiniMax-M2.7-highspeed","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("minimax-provider", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "minimax-provider",
			Kind: "minimax",
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "minimax-provider",
		"compat_kind": "minimax",
	}}

	payload := []byte(`{
		"model":"MiniMax-M2.7-highspeed",
		"max_tokens":8,
		"messages":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":"Reply with OK only."}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "MiniMax-M2.7-highspeed",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := gjson.GetBytes(gotBody, "messages.0.role").String(); got != "system" {
		t.Fatalf("messages.0.role = %q, want system: %s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content").String(); got != "You are concise." {
		t.Fatalf("messages.0.content = %q, want restored system instructions: %s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.1.role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user: %s", got, string(gotBody))
	}
}

func TestOpenAICompatPayloadRepairsInvalidStringEscapesForMiniMax(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M2.7-highspeed",
		"messages":[{"role":"user","content":"- **归档**：\archive/20260516 and *破甲**\ufeff\v**"}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("minimax"), "MiniMax-M2.7-highspeed", "https://api.minimaxi.com/v1")

	if !gjson.ValidBytes(out) {
		t.Fatalf("repaired MiniMax payload should be valid JSON: %s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); !strings.Contains(got, `\archive/20260516`) || !strings.Contains(got, `\v`) {
		t.Fatalf("literal backslash text not preserved, got %q payload=%s", got, string(out))
	}
}

func TestOpenAICompatPayloadPreservesMiniMaxSystemRoleAndRemovesUnsupportedPenalties(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M3",
		"messages":[
			{"role":"system","content":"You are a helpful assistant."},
			{"role":"user","content":"hi"}
		],
		"frequency_penalty":1,
		"presence_penalty":1,
		"reasoning_effort":"xhigh",
		"thinking":{"type":"enabled"},
		"top_p":0.95
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("minimax"), "MiniMax-M3", "https://api.minimaxi.com/v1")

	for _, path := range []string{"frequency_penalty", "presence_penalty"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be omitted for MiniMax: %s", path, string(out))
		}
	}
	if !gjson.GetBytes(out, "thinking").Exists() || !gjson.GetBytes(out, "top_p").Exists() {
		t.Fatalf("supported MiniMax fields should be preserved: %s", string(out))
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "system" {
		t.Fatalf("messages.0.role = %q, want system: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadNormalizesMiniMaxToolCallArguments(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M3",
		"messages":[{
			"role":"assistant",
			"tool_calls":[
				{"id":"call_empty","type":"function","function":{"name":"empty","arguments":""}},
				{"id":"call_text","type":"function","function":{"name":"text","arguments":"not-json"}},
				{"id":"call_object","type":"function","function":{"name":"object","arguments":{"path":"README.md"}}}
			]
		},
		{"role":"tool","tool_call_id":"call_empty","content":"ok"},
		{"role":"tool","tool_call_id":"call_text","content":"ok"},
		{"role":"tool","tool_call_id":"call_object","content":"ok"}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("minimax"), "MiniMax-M3", "https://api.minimaxi.com/v1")

	for _, path := range []string{
		`messages.0.tool_calls.0.function.arguments`,
		`messages.0.tool_calls.1.function.arguments`,
		`messages.0.tool_calls.2.function.arguments`,
	} {
		if got := gjson.GetBytes(out, path).String(); !gjson.Valid(got) {
			t.Fatalf("%s = %q, want valid JSON string; payload=%s", path, got, string(out))
		}
	}
	if got := gjson.GetBytes(out, `messages.0.tool_calls.2.function.arguments`).String(); got != `{"path":"README.md"}` {
		t.Fatalf("object arguments = %q, want serialized object; payload=%s", got, string(out))
	}
}

func TestOpenAICompatPayloadGenericKeepsSystemRole(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.5",
		"messages":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":"hi"}
		]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "gpt-5.5", "https://api.openai.com/v1")

	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "system" {
		t.Fatalf("messages.0.role = %q, want system: %s", got, string(out))
	}
}

func TestInferOpenAICompatKindFromBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "kimi moonshot", baseURL: "https://api.moonshot.ai/v1", want: "kimi"},
		{name: "kimi moonshot cn", baseURL: "https://api.moonshot.cn/v1", want: "kimi"},
		{name: "kimi coding", baseURL: "https://api.kimi.com/coding/v1", want: "kimi"},
		{name: "minimax openai", baseURL: "https://api.minimax.io/v1", want: "minimax"},
		{name: "zhipu coding", baseURL: "https://open.bigmodel.cn/api/coding/paas/v4", want: "zhipu"},
		{name: "zai", baseURL: "https://api.z.ai/api/paas/v4", want: "zhipu"},
		{name: "deepseek", baseURL: "https://api.deepseek.com/v1", want: "deepseek"},
		{name: "xiaomi openai", baseURL: "https://api.xiaomimimo.com/v1", want: "xiaomi"},
		{name: "xiaomi token plan", baseURL: "https://token-plan-cn.xiaomimimo.com/v1", want: "xiaomi"},
		{name: "xiaomi token plan singapore", baseURL: "https://token-plan-sgp.xiaomimimo.com/v1", want: "xiaomi"},
		{name: "xiaomi token plan europe anthropic", baseURL: "https://token-plan-ams.xiaomimimo.com/anthropic", want: "xiaomi"},
		{name: "doubao ark openai", baseURL: "https://ark.cn-beijing.volces.com/api/v3", want: "doubao"},
		{name: "qwen token plan openai", baseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", want: "qwen"},
		{name: "qwen workspace openai", baseURL: "https://workspace-id.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions", want: "qwen"},
		{name: "unknown", baseURL: "https://example.com/v1", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inferOpenAICompatKindFromBaseURL(tt.baseURL); got != tt.want {
				t.Fatalf("inferOpenAICompatKindFromBaseURL(%q) = %q, want %q", tt.baseURL, got, tt.want)
			}
		})
	}
}

func TestOpenAICompatPayloadQwen38ForcesThinkingOnlyMode(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"qwen3.8-max-preview",
		"messages":[{"role":"user","content":"hi"}],
		"enable_thinking":false,
		"reasoning_effort":"none",
		"reasoning":{"effort":"none"},
		"thinking":{"type":"disabled","budget_tokens":0},
		"thinking_budget":0,
		"temperature":0.2
	}`)

	out := scrubOpenAICompatPayloadForModel(
		payload,
		openAICompatProfileForKind("qwen"),
		"qwen3.8-max",
		"https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
	)

	if !gjson.GetBytes(out, "enable_thinking").Bool() {
		t.Fatalf("enable_thinking should be forced true: %s", string(out))
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "low" {
		t.Fatalf("reasoning_effort = %q, want low: %s", got, string(out))
	}
	for _, path := range []string{"reasoning", "thinking", "thinking_budget"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed after normalizing disabled thinking: %s", path, string(out))
		}
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want unchanged 0.2: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadQwen38KeepsSingleThinkingControl(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		payload    string
		wantEffort string
		wantBudget int64
	}{
		{
			name:       "valid effort wins over budget",
			payload:    `{"model":"qwen3.8-max-preview","reasoning_effort":"xhigh","thinking_budget":4096}`,
			wantEffort: "xhigh",
		},
		{
			name:       "nested budget becomes top level",
			payload:    `{"model":"qwen3.8-max-preview","thinking":{"type":"enabled","budget_tokens":4096}}`,
			wantBudget: 4096,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := scrubOpenAICompatPayloadForModel(
				[]byte(tt.payload),
				openAICompatProfileForKind("qwen"),
				"qwen3.8-max",
				"https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
			)
			if !gjson.GetBytes(out, "enable_thinking").Bool() {
				t.Fatalf("enable_thinking should be true: %s", string(out))
			}
			if got := gjson.GetBytes(out, "reasoning_effort").String(); got != tt.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q: %s", got, tt.wantEffort, string(out))
			}
			if got := gjson.GetBytes(out, "thinking_budget").Int(); got != tt.wantBudget {
				t.Fatalf("thinking_budget = %d, want %d: %s", got, tt.wantBudget, string(out))
			}
			if tt.wantEffort != "" && gjson.GetBytes(out, "thinking_budget").Exists() {
				t.Fatalf("thinking_budget should be removed when reasoning_effort is present: %s", string(out))
			}
			if gjson.GetBytes(out, "thinking").Exists() {
				t.Fatalf("nested thinking should be removed: %s", string(out))
			}
		})
	}
}

func TestOpenAICompatPayloadQwen38DoesNotAffectEarlierModels(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"qwen3.7-max","enable_thinking":false,"reasoning_effort":"none"}`)
	out := scrubOpenAICompatPayloadForModel(
		payload,
		openAICompatProfileForKind("qwen"),
		"qwen3.7-max",
		"https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
	)

	if gjson.GetBytes(out, "enable_thinking").Bool() {
		t.Fatalf("qwen3.7 enable_thinking should remain false: %s", string(out))
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "none" {
		t.Fatalf("qwen3.7 reasoning_effort = %q, want unchanged none: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadRepairsUnansweredToolCalls(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":"will call tools","tool_calls":[
				{"id":"call_01","type":"function","function":{"name":"read_file","arguments":"{}"}},
				{"id":"call_02","type":"function","function":{"name":"glob","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_01","content":"ok"},
			{"role":"user","content":"continue"}
		]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.moonshot.ai/v1")

	if got := len(gjson.GetBytes(out, "messages.1.tool_calls").Array()); got != 1 {
		t.Fatalf("tool_calls length = %d, want 1: %s", got, string(out))
	}
	if gjson.GetBytes(out, `messages.1.tool_calls.#(id=="call_02")`).Exists() {
		t.Fatalf("unanswered call_02 should be removed: %s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != "call_01" {
		t.Fatalf("kept tool result id = %q, want call_01: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadDropsToolOnlyAssistantMessage(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_01","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"user","content":"continue"}
		]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.moonshot.ai/v1")

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %s", len(messages), string(out))
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("remaining role = %q, want user: %s", got, string(out))
	}
}

func TestOpenAICompatHTTPRequestBodyRepairsKimiOrphanReplyToolCall(t *testing.T) {
	payload := `{
		"model":"kimi-k2.5",
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":"reply pending","tool_calls":[{"id":"reply:0","type":"function","function":{"name":"reply","arguments":"{}"}}]},
			{"role":"user","content":"continue"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/chat/completions", strings.NewReader(payload))

	if err := sanitizeOpenAICompatHTTPRequestBody(req, openAICompatProfileForKind("kimi"), "https://api.kimi.com/coding/v1"); err != nil {
		t.Fatalf("sanitizeOpenAICompatHTTPRequestBody() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if gjson.GetBytes(body, "messages.1.tool_calls").Exists() {
		t.Fatalf("orphan reply tool_call should be removed: %s", string(body))
	}
	if got := gjson.GetBytes(body, "messages.1.content").String(); got != "reply pending" {
		t.Fatalf("assistant content = %q, want preserved text: %s", got, string(body))
	}
}

func TestOpenAICompatPayloadDropsEmptyMessages(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.5",
		"messages":[
			{"role":"user","content":[]},
			{"role":"assistant","content":[{"type":"text","text":""}]},
			{"role":"user","content":"continue"}
		]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "gpt-5.5", "https://api.openai.com/v1")

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1: %s", len(messages), string(out))
	}
	if got := messages[0].Get("content").String(); got != "continue" {
		t.Fatalf("remaining content = %q, want continue: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadDropsMalformedToolCallsAndResults(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.5",
		"messages":[
			{"role":"assistant","content":"checking","tool_calls":[
				{"id":"call_ok","type":"function","function":{"name":"read:file","arguments":{"path":"README.md"}}},
				{"id":"call_bad","type":"function","function":{"arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_ok","content":"ok"},
			{"role":"tool","tool_call_id":"call_bad","content":"bad"},
			{"role":"user","content":"next"}
		]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "gpt-5.5", "https://api.openai.com/v1")

	if got := len(gjson.GetBytes(out, "messages.0.tool_calls").Array()); got != 1 {
		t.Fatalf("tool_calls length = %d, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.function.name").String(); got != "read_file" {
		t.Fatalf("normalized function.name = %q, want read_file: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.function.arguments").String(); got != `{"path":"README.md"}` {
		t.Fatalf("normalized function.arguments = %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_ok" {
		t.Fatalf("kept tool result id = %q, want call_ok: %s", got, string(out))
	}
	if gjson.GetBytes(out, `messages.#(tool_call_id=="call_bad")`).Exists() {
		t.Fatalf("malformed tool result should be removed: %s", string(out))
	}
}

func TestOpenAICompatPayloadDropsDuplicateToolCalls(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.5",
		"messages":[
			{"role":"assistant","content":"checking","tool_calls":[
				{"id":"call_dup","type":"function","function":{"name":"read_file","arguments":"{}"}},
				{"id":"call_dup","type":"function","function":{"name":"read_file","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_dup","content":"ok"},
			{"role":"user","content":"next"}
		]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "gpt-5.5", "https://api.openai.com/v1")

	if got := len(gjson.GetBytes(out, "messages.0.tool_calls").Array()); got != 1 {
		t.Fatalf("tool_calls length = %d, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_dup" {
		t.Fatalf("kept tool result id = %q, want call_dup: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiNormalizesToolsAndDisablesStrict(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"read:file",
			"description":"Read a file",
			"input_schema":{
				"type":"object",
				"properties":{"path":{"type":"string"}},
				"required":["path"]
			},
			"strict":true
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.moonshot.ai/v1")

	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "read_file" {
		t.Fatalf("tool name = %q, want read_file: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.strict").Bool(); got {
		t.Fatalf("kimi strict should be disabled for schema compatibility: %s", string(out))
	}
	if gjson.GetBytes(out, "tools.0.input_schema").Exists() {
		t.Fatalf("input_schema should be converted away: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.path.type").String(); got != "string" {
		t.Fatalf("converted parameters missing path type, got %q: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiSanitizesMoonshotSchemaFlavor(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"inspect",
			"description":"Inspect values",
			"input_schema":{
				"type":"object",
				"properties":{
					"type":"object",
					"additionalProperties":"object"
				},
				"additionalProperties":"object",
				"required":null
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.moonshot.ai/v1")

	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.type.type").String(); got != "object" {
		t.Fatalf("properties.type should be an object schema, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.additionalProperties.type").String(); got != "object" {
		t.Fatalf("properties.additionalProperties should be an object schema, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.additionalProperties.type").String(); got != "object" {
		t.Fatalf("root additionalProperties should be an object schema, got %q: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.function.parameters.required").Exists() {
		t.Fatalf("required=null should be removed: %s", string(out))
	}
}

func TestOpenAICompatPayloadKimiRemovesParentTypeFromAnyOfSchema(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"inspect",
			"description":"Inspect values",
			"input_schema":{
				"type":"object",
				"properties":{
					"modules":{
						"type":"array",
						"anyOf":[
							{"type":"array","items":{"type":"string"}},
							{"type":"null"}
						]
					}
				}
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.moonshot.ai/v1")

	if gjson.GetBytes(out, "tools.0.function.parameters.properties.modules.type").Exists() {
		t.Fatalf("anyOf parent type should be removed for moonshot schema flavor: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.modules.anyOf.0.type").String(); got != "array" {
		t.Fatalf("anyOf branch type should be preserved, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.modules.anyOf.0.items.type").String(); got != "string" {
		t.Fatalf("anyOf branch items should be preserved, got %q: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiRemovesParentPropertiesFromAnyOfSchema(t *testing.T) {
	payload := []byte(`{
		"model":"k2.6-code-preview",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"run_tool",
				"parameters":{
					"type":"object",
					"properties":{
						"arguments":{
							"type":"object",
							"properties":{"path":{"type":"string"}},
							"required":["path"],
							"additionalProperties":false,
							"anyOf":[
								{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]},
								{"type":"string"}
							]
						}
					}
				}
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "k2.6-code-preview", "https://api.moonshot.ai/v1")
	argumentsPath := "tools.0.function.parameters.properties.arguments"

	if !gjson.GetBytes(out, argumentsPath+".anyOf").Exists() {
		t.Fatalf("arguments anyOf should be preserved: %s", string(out))
	}
	for _, path := range []string{"type", "properties", "required", "additionalProperties"} {
		if gjson.GetBytes(out, argumentsPath+"."+path).Exists() {
			t.Fatalf("arguments parent %s should be removed for moonshot schema flavor: %s", path, string(out))
		}
	}
	if got := gjson.GetBytes(out, argumentsPath+".anyOf.0.properties.path.type").String(); got != "string" {
		t.Fatalf("anyOf object branch should preserve path schema, got %q: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadZhipuForcesAutoToolChoice(t *testing.T) {
	payload := []byte(`{
		"model":"glm-4.6",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":"required"
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("zhipu"), "glm-4.6", "https://open.bigmodel.cn/api/paas/v4")

	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadZhipuExpandsFunctionDeclarations(t *testing.T) {
	payload := []byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"functionDeclarations":[{
				"name":"read:file",
				"description":"read a file",
				"parametersJsonSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
			}]
		}],
		"tool_choice":{"type":"function","function":{"name":"read:file"}}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("zhipu"), "glm-5.2", "https://open.bigmodel.cn/api/paas/v4")

	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tools.0.type = %q, want function: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "read_file" {
		t.Fatalf("function name = %q, want read_file: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.path.type").String(); got != "string" {
		t.Fatalf("path type = %q, want string: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadZhipuConvertsDataURLImagesToRawBase64(t *testing.T) {
	payload := []byte(`{
		"model":"glm-4.5v",
		"messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"high"}},
			{"type":"text","text":"describe"}
		]}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("zhipu"), "glm-4.5v", "https://open.bigmodel.cn/api/paas/v4")

	if got := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String(); got != "AAAA" {
		t.Fatalf("image_url.url = %q, want raw base64: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.image_url.detail").String(); got != "high" {
		t.Fatalf("image detail = %q, want high: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiPreservesDataURLImages(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-latest",
		"messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		]}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-latest", "https://api.moonshot.ai/v1")

	if got := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String(); got != "data:image/png;base64,AAAA" {
		t.Fatalf("image_url.url = %q, want data URL preserved: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiPreservesAssistantReasoningContent(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[
			{"role":"assistant","content":"planning","reasoning_content":"actual reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"reasoning_effort":"high"
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.kimi.com/coding/v1")

	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "actual reasoning" {
		t.Fatalf("messages.0.reasoning_content = %q, want actual reasoning: %s", got, string(out))
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be removed for kimi compat payload: %s", string(out))
	}
	if gjson.GetBytes(out, "thinking.keep").Exists() {
		t.Fatalf("thinking.keep should be removed for kimi k2.5/k2.6 compat payload: %s", string(out))
	}
}

func TestOpenAICompatPayloadKimiRepairsMissingAssistantToolCallReasoning(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[
			{"role":"assistant","content":"I will read it","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.kimi.com/coding/v1")

	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "I will read it" {
		t.Fatalf("messages.0.reasoning_content = %q, want content fallback: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiK26NormalizesThinkingToolChoiceAndSampling(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[
			{"role":"assistant","content":"planning","reasoning_content":"actual reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"read_file"}},
		"thinking":{"type":"high"},
		"temperature":0.2,
		"top_p":0.4,
		"n":2,
		"presence_penalty":1.0,
		"frequency_penalty":1.0
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.kimi.com/coding/v1")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled: %s", got, string(out))
	}
	if gjson.GetBytes(out, "thinking.keep").Exists() {
		t.Fatalf("thinking.keep should be removed for kimi k2.5/k2.6 compat payload: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != kimiThinkingTemperature {
		t.Fatalf("temperature = %v, want %v: %s", got, kimiThinkingTemperature, string(out))
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != kimiTopP {
		t.Fatalf("top_p = %v, want %v: %s", got, kimiTopP, string(out))
	}
	if got := gjson.GetBytes(out, "n").Int(); got != 1 {
		t.Fatalf("n = %v, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "presence_penalty").Float(); got != 0 {
		t.Fatalf("presence_penalty = %v, want 0: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "frequency_penalty").Float(); got != 0 {
		t.Fatalf("frequency_penalty = %v, want 0: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiK27NormalizesThinkingToolChoiceAndSampling(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"read_file"}},
		"thinking":{"type":"high","keep":"all"},
		"temperature":0.2,
		"top_p":0.4,
		"n":2,
		"presence_penalty":1.0,
		"frequency_penalty":1.0
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.7", "https://api.kimi.com/coding/v1")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled: %s", got, string(out))
	}
	if gjson.GetBytes(out, "thinking.keep").Exists() {
		t.Fatalf("thinking.keep should be removed for kimi k2.7 compat payload: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != kimiThinkingTemperature {
		t.Fatalf("temperature = %v, want %v: %s", got, kimiThinkingTemperature, string(out))
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != kimiTopP {
		t.Fatalf("top_p = %v, want %v: %s", got, kimiTopP, string(out))
	}
	if got := gjson.GetBytes(out, "n").Int(); got != 1 {
		t.Fatalf("n = %v, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "presence_penalty").Float(); got != 0 {
		t.Fatalf("presence_penalty = %v, want 0: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "frequency_penalty").Float(); got != 0 {
		t.Fatalf("frequency_penalty = %v, want 0: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiK3UsesMaxReasoningAndRequiredToolChoice(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k3",
		"messages":[
			{"role":"assistant","content":"planning","reasoning_content":"actual reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"read_file"}},
		"thinking":{"type":"disabled","budget_tokens":0},
		"reasoning_effort":"low",
		"temperature":0.2,
		"top_p":0.4,
		"n":2,
		"presence_penalty":1.0,
		"frequency_penalty":1.0
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k3", "https://api.moonshot.ai/v1")

	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "max" {
		t.Fatalf("reasoning_effort = %q, want max: %s", got, string(out))
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("K2 thinking object should be removed for kimi-k3: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "required" {
		t.Fatalf("tool_choice = %q, want required: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "actual reasoning" {
		t.Fatalf("messages.0.reasoning_content = %q, want actual reasoning: %s", got, string(out))
	}
	for _, path := range []string{"temperature", "top_p", "n", "presence_penalty", "frequency_penalty"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be omitted for kimi-k3: %s", path, string(out))
		}
	}
}

func TestOpenAICompatPayloadKimiK3PreservesDynamicToolMessage(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k3",
		"messages":[
			{"role":"system","tools":[{"type":"function","function":{"name":"calculate","description":"Calculate an expression","parameters":{"type":"object","properties":{"expression":{"type":"string"}},"required":["expression"]}}}]},
			{"role":"user","content":"Calculate 23 * 47"}
		],
		"tool_choice":"required"
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k3", "https://api.moonshot.ai/v1")

	if got := gjson.GetBytes(out, "messages.0.tools.0.function.name").String(); got != "calculate" {
		t.Fatalf("dynamic tool name = %q, want calculate: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "required" {
		t.Fatalf("tool_choice = %q, want required: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadKimiK25WebSearchDisablesThinking(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.5",
		"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"builtin_function","function":{"name":"$web_search"}}],
		"tool_choice":"required",
		"thinking":{"type":"enabled","keep":"all"},
		"temperature":1.0
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.5", "https://api.moonshot.ai/v1")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, string(out))
	}
	if gjson.GetBytes(out, "thinking.keep").Exists() {
		t.Fatalf("thinking.keep should be removed for web_search non-thinking mode: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != kimiInstantTemperature {
		t.Fatalf("temperature = %v, want %v: %s", got, kimiInstantTemperature, string(out))
	}
}

func TestOpenAICompatPayloadKimiForCodingForcesTemperatureOnly(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-for-coding",
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.6,
		"top_p":0.4,
		"n":2
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-for-coding", "https://api.kimi.com/coding/v1")

	if got := gjson.GetBytes(out, "temperature").Float(); got != kimiThinkingTemperature {
		t.Fatalf("temperature = %v, want %v: %s", got, kimiThinkingTemperature, string(out))
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.4 {
		t.Fatalf("top_p = %v, want 0.4: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "n").Int(); got != 2 {
		t.Fatalf("n = %v, want 2: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadXiaomiScrubsUnsupportedOpenAIExtras(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5",
		"messages":[{"role":"assistant","content":"thinking","reasoning_content":"hidden"}],
		"stream_options":{"include_usage":true},
		"parallel_tool_calls":true,
		"reasoning_effort":"high",
		"metadata":{"tenant":"demo"},
		"store":true,
		"thinking":{"type":"enabled"}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5", "https://api.xiaomimimo.com/v1")

	for _, path := range []string{
		"stream_options",
		"parallel_tool_calls",
		"reasoning_effort",
		"metadata",
		"store",
	} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("unexpected field %s in payload: %s", path, string(out))
		}
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "hidden" {
		t.Fatalf("messages.0.reasoning_content = %q, want hidden: %s", got, string(out))
	}
	if !gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("native thinking field should be preserved: %s", string(out))
	}
}

func TestOpenAICompatPayloadXiaomiRepairsAssistantToolCallReasoning(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5-pro",
		"messages":[
			{"role":"assistant","content":"first plan","reasoning_content":"r1"},
			{"role":"assistant","content":"read file","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"thinking":{"type":"enabled"}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5-pro", "https://token-plan-cn.xiaomimimo.com/v1")

	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != "r1" {
		t.Fatalf("messages.1.reasoning_content = %q, want inherited r1: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadXiaomiMapsReasoningEffortToNativeThinking(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5-pro",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high",
		"temperature":0.2,
		"top_p":0.4
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5-pro", "https://api.xiaomimimo.com/v1")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled: %s", got, string(out))
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be removed after Xiaomi native thinking mapping: %s", string(out))
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 1.0 {
		t.Fatalf("temperature = %v, want 1.0 for Xiaomi thinking mode: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.95 {
		t.Fatalf("top_p = %v, want 0.95 for Xiaomi thinking mode: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadXiaomiClampsMimo25ProTokenFields(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5-pro",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":384000,
		"max_completion_tokens":"200000",
		"max_output_tokens":131073
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5-pro", "https://api.xiaomimimo.com/v1")

	for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if got := gjson.GetBytes(out, path).Int(); got != 131072 {
			t.Fatalf("%s = %d, want 131072: %s", path, got, string(out))
		}
	}
}

func TestOpenAICompatPayloadXiaomiMimo25PreservesImageURL(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		]}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5", "https://token-plan-cn.xiaomimimo.com/v1")

	if got := gjson.GetBytes(out, "messages.0.content.1.type").String(); got != "image_url" {
		t.Fatalf("image part type = %q, want image_url: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.image_url.url").String(); got != "data:image/png;base64,AAAA" {
		t.Fatalf("image URL = %q, want original data URL: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadXiaomiMimo25ProTokenClampLowerBoundAndInvalid(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5-pro",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":0,
		"max_completion_tokens":"nope"
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5-pro", "https://api.xiaomimimo.com/v1")

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 1 {
		t.Fatalf("max_tokens = %d, want 1: %s", got, string(out))
	}
	if gjson.GetBytes(out, "max_completion_tokens").Exists() {
		t.Fatalf("invalid max_completion_tokens should be removed: %s", string(out))
	}
}

func TestOpenAICompatPayloadXiaomiNormalizesClaudeStyleTools(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"inspect",
			"description":"Inspect values",
			"input_schema":{
				"type":"object",
				"properties":{"type":"object"},
				"required":null
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5", "https://api.xiaomimimo.com/v1")

	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.input_schema").Exists() {
		t.Fatalf("input_schema should be converted away: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.type.type").String(); got != "object" {
		t.Fatalf("properties.type should be an object schema, got %q: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadXiaomiSanitizesToolSchemaAndArguments(t *testing.T) {
	payload := []byte(`{
		"model":"mimo-v2.5-pro",
		"messages":[
			{"role":"assistant","content":"call","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"unterminated"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{
			"type":"function",
			"function":{
				"name":"lookup",
				"strict":true,
				"parameters":{
					"type":"object",
					"title":"LookupArgs",
					"additionalProperties":false,
					"properties":{
						"query":{"anyOf":[{"type":"string"},{"type":"null"}],"default":""},
						"tags":{"type":"array","items":{"type":"string","minLength":1}}
					},
					"required":["query"]
				}
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("xiaomi"), "mimo-v2.5-pro", "https://token-plan-cn.xiaomimimo.com/v1")

	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.function.arguments").String(); got != "{}" {
		t.Fatalf("tool call arguments = %q, want repaired empty object: %s", got, string(out))
	}
	for _, path := range []string{
		"tools.0.function.strict",
		"tools.0.function.parameters.title",
		"tools.0.function.parameters.additionalProperties",
		"tools.0.function.parameters.properties.query.anyOf",
		"tools.0.function.parameters.properties.query.default",
		"tools.0.function.parameters.properties.tags.items.minLength",
	} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("unexpected Xiaomi-incompatible schema field %s: %s", path, string(out))
		}
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.query.type").String(); got != "string" {
		t.Fatalf("query.type = %q, want string: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.tags.items.type").String(); got != "string" {
		t.Fatalf("tags.items.type = %q, want string: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadNormalizesFunctionNameReferences(t *testing.T) {
	payload := []byte(`{
		"model":"kimi-k2.6",
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read:file","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"read:file","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":{"type":"function","function":{"name":"read:file"}},
		"thinking":{"type":"disabled"}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "kimi-k2.6", "https://api.moonshot.ai/v1")

	for _, path := range []string{
		"tools.0.function.name",
		"tool_choice.function.name",
		"messages.0.tool_calls.0.function.name",
	} {
		if got := gjson.GetBytes(out, path).String(); got != "read_file" {
			t.Fatalf("%s = %q, want read_file: %s", path, got, string(out))
		}
	}
}

func TestOpenAICompatPayloadDeepSeekStripsStrictOutsideBeta(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"lookup",
				"description":"Lookup records",
				"strict":true,
				"parameters":{
					"type":"object",
					"properties":{"query":{"type":"string"}},
					"additionalProperties":false
				}
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if gjson.GetBytes(out, "tools.0.function.strict").Exists() {
		t.Fatalf("strict should be removed outside beta endpoint: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.query.type").String(); got != "string" {
		t.Fatalf("parameters were not preserved, got query.type=%q payload=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function; payload=%s", got, string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekConvertsInputSchemaTools(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"read_file",
			"description":"Read a file",
			"input_schema":{
				"type":"object",
				"properties":{"path":{"type":"string"}},
				"required":["path"]
			},
			"strict":true
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function; payload=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "read_file" {
		t.Fatalf("tool name = %q, want read_file; payload=%s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.input_schema").Exists() {
		t.Fatalf("input_schema should be converted away: %s", string(out))
	}
	if gjson.GetBytes(out, "tools.0.function.strict").Exists() {
		t.Fatalf("strict should be removed outside beta endpoint: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.path.type").String(); got != "string" {
		t.Fatalf("converted parameters missing path type, got %q payload=%s", got, string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekSanitizesLooseSchemaValues(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"inspect",
				"parameters":{
					"type":"object",
					"properties":{
						"type":"object",
						"required":"array"
					},
					"additionalProperties":"object",
					"required":null
				}
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.type.type").String(); got != "object" {
		t.Fatalf("properties.type should be an object schema, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.required.type").String(); got != "array" {
		t.Fatalf("properties.required should be an array schema, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.additionalProperties.type").String(); got != "object" {
		t.Fatalf("additionalProperties should be an object schema, got %q: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.function.parameters.required").Exists() {
		t.Fatalf("required=null should be removed: %s", string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekNormalizesThinkingBudget(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"thinking_budget":50,
		"thinking":{"type":"enabled","budget_tokens":99999},
		"reasoning_effort":"xhigh"
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if got := gjson.GetBytes(out, "thinking_budget").Int(); got != 100 {
		t.Fatalf("thinking_budget = %d, want 100: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "thinking.budget_tokens").Int(); got != 32768 {
		t.Fatalf("thinking.budget_tokens = %d, want 32768: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "max" {
		t.Fatalf("reasoning_effort = %q, want max: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekThinkingModeStripsToolChoice(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"thinking":{"type":"enabled"},
		"thinking_budget":1024
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice should be removed for DeepSeek thinking mode: %s", string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekKeepsToolChoiceWhenThinkingDisabled(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"thinking":{"type":"disabled"}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if got := gjson.GetBytes(out, "tool_choice.function.name").String(); got != "lookup" {
		t.Fatalf("tool_choice.function.name = %q, want lookup: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadDoubaoDeepSeekMapsReasoningEffort(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"xhigh",
		"thinking":{"type":"enabled","budget_tokens":99999},
		"tools":[{
			"name":"read:file",
			"description":"Read file",
			"input_schema":{"type":"object","properties":{"path":{"type":"string"}}}
		}],
		"tool_choice":{"type":"function","function":{"name":"read:file"}}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("doubao"), "deepseek-v4-pro", "https://ark.cn-beijing.volces.com/api/v3")

	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high: %s", got, string(out))
	}
	for _, path := range []string{
		"reasoning_effort",
		"thinking",
		"thinking_budget",
		"output_config.effort",
	} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed for doubao DeepSeek compat: %s", path, string(out))
		}
	}
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Fatalf("tool type = %q, want function: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "read_file" {
		t.Fatalf("tool function name = %q, want read_file: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice.function.name").String(); got != "read_file" {
		t.Fatalf("tool_choice function name = %q, want read_file: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekRemovesBudgetWhenThinkingDisabled(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"thinking_budget":50,
		"thinking":{"type":"disabled","budget_tokens":50}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	for _, path := range []string{"thinking_budget", "thinking.budget_tokens"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed when thinking is disabled: %s", path, string(out))
		}
	}
}

func TestOpenAICompatPayloadDeepSeekReasoningNoneDisablesThinking(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"thinking_budget":50,
		"reasoning_effort":"none"
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, string(out))
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be removed when disabling DeepSeek thinking: %s", string(out))
	}
	if gjson.GetBytes(out, "thinking_budget").Exists() {
		t.Fatalf("thinking_budget should be removed when disabling DeepSeek thinking: %s", string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekResponsesReasoningNoneDisablesThinking(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"input":"hi",
		"reasoning":{"effort":"none"}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, string(out))
	}
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed when disabling DeepSeek thinking: %s", path, string(out))
		}
	}
}

func TestOpenAICompatPayloadDeepSeekReasoningAutoUsesProviderDefault(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"auto"
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/v1")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled: %s", got, string(out))
	}
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed when using DeepSeek provider default effort: %s", path, string(out))
		}
	}
}

func TestOpenAICompatPayloadDeepSeekBudgetScrubSkipsOtherCompatProfiles(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"thinking_budget":50
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("kimi"), "deepseek-v4-pro", "https://api.kimi.com/coding/v1")

	if got := gjson.GetBytes(out, "thinking_budget").Int(); got != 50 {
		t.Fatalf("thinking_budget = %d, want unchanged 50 for kimi compat: %s", got, string(out))
	}
}

func TestOpenAICompatPayloadGenericSanitizesFunctionSchemaWithoutDroppingStrict(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"inspect",
				"strict":true,
				"parameters":{
					"type":"object",
					"properties":{"type":"object"},
					"additionalProperties":"object",
					"required":null
				}
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "gpt-5", "https://api.openai.com/v1")

	if got := gjson.GetBytes(out, "tools.0.function.strict").Bool(); !got {
		t.Fatalf("generic strict flag should be preserved: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.type.type").String(); got != "object" {
		t.Fatalf("properties.type should be an object schema, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.additionalProperties.type").String(); got != "object" {
		t.Fatalf("additionalProperties should be an object schema, got %q: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.function.parameters.required").Exists() {
		t.Fatalf("required=null should be removed: %s", string(out))
	}
}

func TestOpenAICompatPayloadDeepSeekKeepsStrictOnBetaAndNormalizesSchema(t *testing.T) {
	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"lookup",
				"description":"Lookup records",
				"strict":true,
				"parameters":{
					"type":"object",
					"properties":{
						"query":{"type":"string"},
						"limit":{"type":"integer"}
					},
					"required":["query"]
				}
			}
		}]
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, genericOpenAICompatProfile(), "deepseek-v4-pro", "https://api.deepseek.com/beta")

	if got := gjson.GetBytes(out, "tools.0.function.strict").Bool(); !got {
		t.Fatalf("strict should be kept on beta endpoint: %s", string(out))
	}
	additionalProperties := gjson.GetBytes(out, "tools.0.function.parameters.additionalProperties")
	if !additionalProperties.Exists() {
		t.Fatalf("additionalProperties=false should be present in strict schema: %s", string(out))
	}
	if additionalProperties.Raw != "false" {
		t.Fatalf("additionalProperties should be false, got %s: %s", additionalProperties.Raw, string(out))
	}
	if !requiredContains(out, "tools.0.function.parameters.required", "query") ||
		!requiredContains(out, "tools.0.function.parameters.required", "limit") {
		t.Fatalf("strict schema should require all object properties: %s", string(out))
	}
}

func requiredContains(payload []byte, path string, want string) bool {
	values := gjson.GetBytes(payload, path)
	if !values.IsArray() {
		return false
	}
	for _, value := range values.Array() {
		if value.String() == want {
			return true
		}
	}
	return false
}
