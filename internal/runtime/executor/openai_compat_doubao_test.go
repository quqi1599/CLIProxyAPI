package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatPayloadDoubaoSeed20NormalizesArkChatPayload(t *testing.T) {
	payload := []byte(`{
		"model":"doubao-seed-2.0-pro",
		"messages":[
			{"role":"user","content":[
				{"type":"input_text","text":"inspect"},
				{"type":"input_image","image_url":"https://cdn.example.com/a.png"},
				{"type":"image_url","image_url":"https://cdn.example.com/b.png"},
				{"type":"input_video","video_url":{"url":"https://cdn.example.com/a.mp4"}}
			]},
			{"role":"assistant","content":"calling","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{bad json"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"temperature":1.8,
		"max_tokens":100,
		"max_completion_tokens":100000,
		"max_output_tokens":20,
		"user":"customer-1",
		"store":true,
		"metadata":{"tenant":"demo"},
		"parallel_tool_calls":true,
		"response_format":{"type":"json_object"}
	}`)

	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("doubao"), "doubao-seed-2.0-pro", "https://ark.cn-beijing.volces.com/api/v3")

	if !gjson.ValidBytes(out) {
		t.Fatalf("payload should remain valid JSON: %s", string(out))
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != doubaoSeed20MaxTemperature {
		t.Fatalf("temperature = %v, want %v: %s", got, doubaoSeed20MaxTemperature, string(out))
	}
	if got := gjson.GetBytes(out, "max_completion_tokens").Int(); got != doubaoSeed20MaxCompletionTokens {
		t.Fatalf("max_completion_tokens = %d, want %d: %s", got, doubaoSeed20MaxCompletionTokens, string(out))
	}
	for _, path := range []string{"max_tokens", "max_output_tokens", "user", "store", "metadata", "parallel_tool_calls", "response_format"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed: %s", path, string(out))
		}
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "text" {
		t.Fatalf("input_text type = %q, want text: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.type").String(); got != "image_url" {
		t.Fatalf("input_image type = %q, want image_url: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.image_url.url").String(); got != "https://cdn.example.com/a.png" {
		t.Fatalf("input_image url = %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.image_url.url").String(); got != "https://cdn.example.com/b.png" {
		t.Fatalf("image_url string was not wrapped, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.3.type").String(); got != "video_url" {
		t.Fatalf("input_video type = %q, want video_url: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.tool_calls.0.function.arguments").String(); got != "{}" {
		t.Fatalf("tool arguments = %q, want empty JSON object: %s", got, string(out))
	}
}

func TestOpenAICompatExecutorDoubaoResponsesPassthroughPreservesMCPTool(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","output":[]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	upstreamAuth := &auth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    server.URL + "/api/v3",
			"api_key":     "test",
			"compat_kind": "doubao",
		},
	}

	_, err := executor.Execute(context.Background(), upstreamAuth, cliproxyexecutor.Request{
		Model: "doubao-seed-1.6",
		Payload: []byte(`{
			"model":"doubao-seed-1.6",
			"input":[{"role":"user","content":"search docs"}],
			"tools":[{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.test"}],
			"stream":false
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/responses",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/api/v3/responses" {
		t.Fatalf("path = %q, want /api/v3/responses", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "tools.0.type").String(); got != "mcp" {
		t.Fatalf("tools.0.type = %q, want mcp: %s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("responses payload should not be translated to chat messages: %s", string(gotBody))
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("responses input should be preserved: %s", string(gotBody))
	}
}

func TestOpenAICompatExecutorDoubaoResponsesStreamPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	upstreamAuth := &auth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    server.URL + "/api/v3",
			"api_key":     "test",
			"compat_kind": "doubao",
		},
	}

	result, err := executor.ExecuteStream(context.Background(), upstreamAuth, cliproxyexecutor.Request{
		Model: "doubao-seed-1.6",
		Payload: []byte(`{
			"model":"doubao-seed-1.6",
			"input":[{"role":"user","content":"search docs"}],
			"tools":[{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.test"}],
			"stream":true
		}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/responses",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var gotStream strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		gotStream.Write(chunk.Payload)
	}
	if gotPath != "/api/v3/responses" {
		t.Fatalf("path = %q, want /api/v3/responses", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "tools.0.type").String(); got != "mcp" {
		t.Fatalf("tools.0.type = %q, want mcp: %s", got, string(gotBody))
	}
	if !strings.Contains(gotStream.String(), `"response.output_text.delta"`) {
		t.Fatalf("stream payload did not preserve responses event: %q", gotStream.String())
	}
}

func TestOpenAICompatExecutorDoubaoLogsCompatibilityDiagnosticOn400(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Tt-Logid", "volc-log-123")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"BadRequest","message":"we could not parse the JSON body"}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	upstreamAuth := &auth.Auth{
		ID:       "auth-doubao-1",
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    server.URL + "/api/v3",
			"api_key":     "test",
			"compat_kind": "doubao",
			"compat_name": "ark-channel",
		},
	}
	ctx := logging.WithRequestID(context.Background(), "req-doubao-1")

	_, err := executor.Execute(ctx, upstreamAuth, cliproxyexecutor.Request{
		Model: "doubao-seed-2.0-pro",
		Payload: []byte(`{
			"model":"doubao-seed-2.0-pro",
			"messages":[{"role":"user","content":"hi"}],
			"temperature":1.8,
			"user":"customer-1"
		}`),
	}, cliproxyexecutor.Options{
		Headers: http.Header{
			"X-Newapi-Channel-Id": []string{"3"},
		},
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions",
		},
	})
	if err == nil {
		t.Fatal("expected upstream 400 error")
	}
	if got := gjson.GetBytes(gotBody, "temperature").Float(); got != doubaoSeed20MaxTemperature {
		t.Fatalf("upstream temperature = %v, want %v: %s", got, doubaoSeed20MaxTemperature, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "user").Exists() {
		t.Fatalf("user field should not reach upstream: %s", string(gotBody))
	}

	entry := findCompatibilityDiagnosticEntry(t, hook.AllEntries())
	if got := entry.Data["request_id"]; got != "req-doubao-1" {
		t.Fatalf("request_id = %#v, want req-doubao-1", got)
	}
	if got := entry.Data["compat_kind"]; got != "doubao" {
		t.Fatalf("compat_kind = %#v, want doubao", got)
	}
	if got := entry.Data["compat_kind_source"]; got != "auth_attribute:compat_kind" {
		t.Fatalf("compat_kind_source = %#v, want auth_attribute:compat_kind", got)
	}
	if got := entry.Data["channel"]; got != "3" {
		t.Fatalf("channel = %#v, want 3", got)
	}
	if got := entry.Data["endpoint"]; got != "/chat/completions" {
		t.Fatalf("endpoint = %#v, want /chat/completions", got)
	}
	if got := entry.Data["request_path"]; got != "/v1/chat/completions" {
		t.Fatalf("request_path = %#v, want /v1/chat/completions", got)
	}
	if got := entry.Data["upstream_request_id"]; got != "volc-log-123" {
		t.Fatalf("upstream_request_id = %#v, want volc-log-123", got)
	}
	if !logFieldContains(entry.Data["removed_fields"], "user") {
		t.Fatalf("removed_fields should contain user, got %#v", entry.Data["removed_fields"])
	}
	if !logFieldContains(entry.Data["modified_fields"], "temperature") {
		t.Fatalf("modified_fields should contain temperature, got %#v", entry.Data["modified_fields"])
	}
	if _, exists := entry.Data["payload"]; exists {
		t.Fatal("diagnostic log should not include raw payload")
	}
}

func TestOpenAICompatExecutorDeepSeekLogsCompatibilityShapeOn400(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "deepseek-log-1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"shape mismatch"}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "deepseek-official",
			Kind: "deepseek",
		}},
	})
	upstreamAuth := &auth.Auth{
		ID:       "auth-deepseek-1",
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_kind":  "deepseek",
			"compat_name":  "deepseek-official",
			"provider_key": "deepseek",
		},
	}
	ctx := logging.WithRequestID(context.Background(), "req-deepseek-1")

	_, err := executor.Execute(ctx, upstreamAuth, cliproxyexecutor.Request{
		Model: "deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-v4-pro",
			"messages":[
				{"role":"system","content":[{"type":"text","text":"system"}]},
				{"role":"assistant","reasoning_content":"plan","content":[{"type":"text","text":"calling"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"path\":\"README.md\"}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"},
				{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://cdn.example.com/a.png"}}]}
			],
			"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
			"tool_choice":{"type":"auto"},
			"parallel_tool_calls":false,
			"response_format":{"type":"json_schema"},
			"thinking":{"type":"enabled"},
			"reasoning_effort":"max"
		}`),
	}, cliproxyexecutor.Options{
		Headers: http.Header{
			"X-Newapi-Channel-Id": []string{"8"},
		},
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions",
		},
	})
	if err == nil {
		t.Fatal("expected upstream 400 error")
	}

	entry := findCompatibilityDiagnosticEntry(t, hook.AllEntries())
	if got := entry.Data["request_id"]; got != "req-deepseek-1" {
		t.Fatalf("request_id = %#v, want req-deepseek-1", got)
	}
	if got := entry.Data["compat_kind"]; got != "deepseek" {
		t.Fatalf("compat_kind = %#v, want deepseek", got)
	}
	if got := entry.Data["channel"]; got != "8" {
		t.Fatalf("channel = %#v, want 8", got)
	}
	if got := entry.Data["message_count"]; got != 4 {
		t.Fatalf("message_count = %#v, want 4", got)
	}
	if !logFieldContains(entry.Data["message_roles"], "assistant:1") {
		t.Fatalf("message_roles should contain assistant:1, got %#v", entry.Data["message_roles"])
	}
	if !logFieldContains(entry.Data["message_roles"], "tool:1") {
		t.Fatalf("message_roles should contain tool:1, got %#v", entry.Data["message_roles"])
	}
	if got := entry.Data["message_role_sequence"]; got != "system>assistant>tool>user" {
		t.Fatalf("message_role_sequence = %#v, want system>assistant>tool>user", got)
	}
	if !logFieldContains(entry.Data["message_content_kinds"], "array:3") {
		t.Fatalf("message_content_kinds should contain array:3, got %#v", entry.Data["message_content_kinds"])
	}
	if !logFieldContains(entry.Data["message_content_kinds"], "string:1") {
		t.Fatalf("message_content_kinds should contain string:1, got %#v", entry.Data["message_content_kinds"])
	}
	if !logFieldContains(entry.Data["content_part_types"], "text:3") {
		t.Fatalf("content_part_types should contain text:3, got %#v", entry.Data["content_part_types"])
	}
	if !logFieldContains(entry.Data["content_part_types"], "image_url:1") {
		t.Fatalf("content_part_types should contain image_url:1, got %#v", entry.Data["content_part_types"])
	}
	if got := entry.Data["tool_definition_count"]; got != 1 {
		t.Fatalf("tool_definition_count = %#v, want 1", got)
	}
	if !logFieldContains(entry.Data["tool_types"], "function:1") {
		t.Fatalf("tool_types should contain function:1, got %#v", entry.Data["tool_types"])
	}
	if got := entry.Data["tool_call_count"]; got != 1 {
		t.Fatalf("tool_call_count = %#v, want 1", got)
	}
	if got := entry.Data["assistant_tool_call_messages"]; got != 1 {
		t.Fatalf("assistant_tool_call_messages = %#v, want 1", got)
	}
	if got := entry.Data["tool_result_messages"]; got != 1 {
		t.Fatalf("tool_result_messages = %#v, want 1", got)
	}
	if got := entry.Data["reasoning_messages"]; got != 1 {
		t.Fatalf("reasoning_messages = %#v, want 1", got)
	}
	if got := entry.Data["max_content_parts"]; got != 2 {
		t.Fatalf("max_content_parts = %#v, want 2", got)
	}
	if got := entry.Data["tool_choice_type"]; got != "auto" {
		t.Fatalf("tool_choice_type = %#v, want auto", got)
	}
	if got := entry.Data["thinking_type"]; got != "enabled" {
		t.Fatalf("thinking_type = %#v, want enabled", got)
	}
	if got := entry.Data["reasoning_effort"]; got != "max" {
		t.Fatalf("reasoning_effort = %#v, want max", got)
	}
	if got := entry.Data["response_format_type"]; got != "json_schema" {
		t.Fatalf("response_format_type = %#v, want json_schema", got)
	}
	if got := entry.Data["parallel_tool_calls"]; got != "false" {
		t.Fatalf("parallel_tool_calls = %#v, want false", got)
	}
	if got := entry.Data["upstream_request_id"]; got != "deepseek-log-1" {
		t.Fatalf("upstream_request_id = %#v, want deepseek-log-1", got)
	}
	if got := entry.Data["upstream_error_code"]; got != "invalid_request_error" {
		t.Fatalf("upstream_error_code = %#v, want invalid_request_error", got)
	}
	if _, exists := entry.Data["payload"]; exists {
		t.Fatal("diagnostic log should not include raw payload")
	}
}

func findCompatibilityDiagnosticEntry(t *testing.T, entries []*log.Entry) *log.Entry {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry == nil {
			continue
		}
		if entry.Data["event"] == "compatibility_diagnostic" {
			return entry
		}
	}
	t.Fatal("compatibility_diagnostic log entry not found")
	return nil
}

func logFieldContains(value any, want string) bool {
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			if item == want {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if str, ok := item.(string); ok && str == want {
				return true
			}
		}
	}
	return false
}
