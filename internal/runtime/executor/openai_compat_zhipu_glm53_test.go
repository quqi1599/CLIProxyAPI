package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorZhipuGLM53NormalizesForcedThinking(t *testing.T) {
	registerThinkingModelForProvider(t, "zhipu-glm53-forced-thinking", "zhipu", "glm-5.3", []string{"low", "high", "max"})

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-glm53","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	exec := newZhipuGLM53TestExecutor()
	auth := zhipuGLM53TestAuth(server.URL + "/api/coding/paas/v4")
	payload := []byte(`{
		"model":"glm-5.3",
		"messages":[{"role":"user","content":"hi"}],
		"enable_thinking":false,
		"reasoning_effort":"none",
		"thinking":{"type":"disabled","clear_thinking":true}
	}`)
	ctx := internalpayload.WithTransformReport(context.Background(), int64(len(payload)))
	_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "glm-5.3",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled: %s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "low" {
		t.Fatalf("reasoning_effort = %q, want low: %s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "enable_thinking").Exists() {
		t.Fatalf("enable_thinking alias reached upstream: %s", gotBody)
	}
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("transform report missing")
	}
	foundDowngrade := false
	for _, stage := range report.Stages {
		if stage.Stage != openAICompatProviderResolveTransformStage {
			continue
		}
		for _, downgrade := range stage.Downgrades {
			if downgrade == openAICompatZhipuGLM53ThinkingDowngrade {
				foundDowngrade = true
			}
		}
	}
	if !foundDowngrade {
		t.Fatalf("transform report did not record GLM-5.3 thinking downgrade: %+v", report)
	}
}

func TestOpenAICompatExecutorZhipuGLM53MapsCompatibilityEfforts(t *testing.T) {
	registerThinkingModelForProvider(t, "zhipu-glm53-efforts", "zhipu", "glm-5.3", []string{"low", "high", "max"})

	for _, test := range []struct {
		name       string
		effort     string
		wantEffort string
	}{
		{name: "minimal", effort: "minimal", wantEffort: "low"},
		{name: "medium", effort: "medium", wantEffort: "high"},
		{name: "xhigh", effort: "xhigh", wantEffort: "max"},
		{name: "max", effort: "max", wantEffort: "max"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-glm53","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()

			payload := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + test.effort + `"}`)
			_, err := newZhipuGLM53TestExecutor().Execute(context.Background(), zhipuGLM53TestAuth(server.URL+"/api/paas/v4"), cliproxyexecutor.Request{
				Model:   "glm-5.3",
				Payload: payload,
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != test.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q: %s", got, test.wantEffort, gotBody)
			}
			if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "enabled" {
				t.Fatalf("thinking.type = %q, want enabled: %s", got, gotBody)
			}
		})
	}
}

func TestOpenAICompatExecutorZhipuGLM53NormalizesTranslatedProtocols(t *testing.T) {
	registerThinkingModelForProvider(t, "zhipu-glm53-translated-protocols", "zhipu", "glm-5.3", []string{"low", "high", "max"})

	for _, test := range []struct {
		name         string
		sourceFormat string
		payload      []byte
		wantEffort   string
	}{
		{
			name:         "Claude disabled thinking",
			sourceFormat: "claude",
			payload:      []byte(`{"model":"glm-5.3","max_tokens":1024,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`),
			wantEffort:   "low",
		},
		{
			name:         "Responses medium effort",
			sourceFormat: "openai-response",
			payload:      []byte(`{"model":"glm-5.3","reasoning":{"effort":"medium"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
			wantEffort:   "high",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-glm53","object":"chat.completion","created":1,"model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			_, err := newZhipuGLM53TestExecutor().Execute(context.Background(), zhipuGLM53TestAuth(server.URL+"/api/paas/v4"), cliproxyexecutor.Request{
				Model:   "glm-5.3",
				Payload: test.payload,
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(test.sourceFormat)})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != test.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q: %s", got, test.wantEffort, gotBody)
			}
			if got := gjson.GetBytes(gotBody, "thinking.type").String(); got != "enabled" {
				t.Fatalf("thinking.type = %q, want enabled: %s", got, gotBody)
			}
		})
	}
}

func TestOpenAICompatExecutorZhipuGLM53RejectsIncompletePreservedHistory(t *testing.T) {
	registerThinkingModelForProvider(t, "zhipu-glm53-history", "zhipu", "glm-5.3", []string{"low", "high", "max"})

	upstreamCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	_, err := newZhipuGLM53TestExecutor().Execute(context.Background(), zhipuGLM53TestAuth(server.URL+"/api/coding/paas/v4"), cliproxyexecutor.Request{
		Model: "glm-5.3",
		Payload: []byte(`{
			"model":"glm-5.3",
			"messages":[
				{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}
			]
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err == nil {
		t.Fatal("Execute() should reject incomplete preserved thinking history")
	}
	if upstreamCalled {
		t.Fatal("upstream should not be called for incomplete preserved thinking history")
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InvalidThinkingHistory || typed.Scope != failurecontract.ScopeRequest || typed.ProviderCode != zhipuGLM53InvalidThinkingHistoryCode || typed.Retryable {
		t.Fatalf("failure = %+v, want non-retryable request-scoped GLM-5.3 history error", typed)
	}
}

func TestOpenAICompatExecutorZhipuGLM53PreservesRealReasoningHistory(t *testing.T) {
	registerThinkingModelForProvider(t, "zhipu-glm53-real-history", "zhipu", "glm-5.3", []string{"low", "high", "max"})

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-glm53","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	_, err := newZhipuGLM53TestExecutor().Execute(context.Background(), zhipuGLM53TestAuth(server.URL+"/api/coding/paas/v4"), cliproxyexecutor.Request{
		Model: "glm-5.3",
		Payload: []byte(`{
			"model":"glm-5.3",
			"thinking":{"type":"enabled","clear_thinking":false},
			"reasoning_effort":"max",
			"messages":[
				{"role":"assistant","reasoning_content":"real reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}
			]
		}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.reasoning_content").String(); got != "real reasoning" {
		t.Fatalf("reasoning_content = %q, want preserved: %s", got, gotBody)
	}
	if gjson.GetBytes(gotBody, "messages.0.reasoning_content").String() == "[reasoning unavailable]" {
		t.Fatalf("reasoning history was synthesized: %s", gotBody)
	}
}

func newZhipuGLM53TestExecutor() *OpenAICompatExecutor {
	return NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "zhipu-test",
			Kind: "zhipu",
		}},
	})
}

func zhipuGLM53TestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     baseURL,
			"api_key":      "test",
			"compat_name":  "zhipu-test",
			"compat_kind":  "zhipu",
			"provider_key": "zhipu",
		},
	}
}
