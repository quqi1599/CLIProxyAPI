package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
