package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestClaudeExecutorCountTokensEmitsSummaryLog(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = append([]byte(nil), body...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"read","input_schema":{"type":"object"}}]}`)
	ctx, releaseReport := retainExecutorTransformReport(logging.WithRequestID(context.Background(), "req-count-claude-1"), len(payload))
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:    "/v1/messages/count_tokens",
			cliproxyexecutor.ClientProfileMetadataKey:  "claude_code",
			cliproxyexecutor.MessageCountMetadataKey:   2,
			cliproxyexecutor.ToolCountMetadataKey:      1,
			cliproxyexecutor.RequestedModelMetadataKey: "claude-3-5-sonnet-20241022[1m]",
		},
	}

	_, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, opts)
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body")
	}
	assertExecutorRequestTransformReport(t, ctx, releaseReport, claudeRequestPlanTransformStage, len(seenBody))

	entry := findCountTokensSummaryEntry(t, hook.AllEntries(), "ClaudeExecutor")
	if got := entry.Data["request_id"]; got != "req-count-claude-1" {
		t.Fatalf("request_id = %#v, want req-count-claude-1", got)
	}
	if got := entry.Data["requested_model"]; got != "claude-3-5-sonnet-20241022[1m]" {
		t.Fatalf("requested_model = %#v", got)
	}
	if got := entry.Data["upstream_model"]; got != "claude-3-5-sonnet-20241022" {
		t.Fatalf("upstream_model = %#v", got)
	}
	if got := entry.Data["input_tokens"]; got != int64(42) {
		t.Fatalf("input_tokens = %#v, want 42", got)
	}
	if got := entry.Data["client_profile"]; got != "claude_code" {
		t.Fatalf("client_profile = %#v", got)
	}
}

func TestOpenAICompatExecutorCountTokensEmitsSummaryLog(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	payload := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}`)
	ctx, releaseReport := retainExecutorTransformReport(logging.WithRequestID(context.Background(), "req-count-openai-1"), len(payload))
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:    "/v1/chat/completions",
			cliproxyexecutor.ClientProfileMetadataKey:  "claude_code",
			cliproxyexecutor.MessageCountMetadataKey:   1,
			cliproxyexecutor.ToolCountMetadataKey:      0,
			cliproxyexecutor.RequestedModelMetadataKey: "gpt-4o-mini[1m]",
		},
	}

	_, err := executor.CountTokens(ctx, nil, cliproxyexecutor.Request{
		Model:   "gpt-4o-mini",
		Payload: payload,
	}, opts)
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}
	assertExecutorRequestTransformReport(t, ctx, releaseReport, openAICompatRequestPlanTransformStage, len(payload))

	entry := findCountTokensSummaryEntry(t, hook.AllEntries(), "OpenAICompatExecutor")
	if got := entry.Data["request_id"]; got != "req-count-openai-1" {
		t.Fatalf("request_id = %#v, want req-count-openai-1", got)
	}
	if got := entry.Data["requested_model"]; got != "gpt-4o-mini[1m]" {
		t.Fatalf("requested_model = %#v", got)
	}
	if got := entry.Data["upstream_model"]; got != "gpt-4o-mini" {
		t.Fatalf("upstream_model = %#v", got)
	}
	if got := entry.Data["client_profile"]; got != "claude_code" {
		t.Fatalf("client_profile = %#v", got)
	}
	if tokens, ok := entry.Data["input_tokens"].(int64); !ok || tokens <= 0 {
		t.Fatalf("input_tokens = %#v, want positive int64", entry.Data["input_tokens"])
	}
}

func TestKimiExecutorCountTokensReportsKimiPlan(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer server.Close()

	executor := NewKimiExecutor(nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	ctx, releaseReport := retainExecutorTransformReport(context.Background(), len(payload))
	_, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{
		Model:   "kimi-k2.5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}
	assertExecutorRequestTransformReport(t, ctx, releaseReport, kimiRequestPlanTransformStage, len(upstreamBody))
}

func TestOpenAICompatCountTokensNeedsThinking(t *testing.T) {
	t.Parallel()

	if openAICompatCountTokensNeedsThinking(
		cliproxyexecutor.Request{Model: "gpt-4o-mini"},
		cliproxyexecutor.Options{},
		[]byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		"gpt-4o-mini",
	) {
		t.Fatal("expected plain payload to skip thinking transforms")
	}

	if !openAICompatCountTokensNeedsThinking(
		cliproxyexecutor.Request{Model: "gpt-4o-mini[1m]"},
		cliproxyexecutor.Options{},
		[]byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		"gpt-4o-mini",
	) {
		t.Fatal("expected suffixed model to require thinking transforms")
	}

	if !openAICompatCountTokensNeedsThinking(
		cliproxyexecutor.Request{Model: "gpt-4o-mini"},
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.ReasoningEffortOriginalMetadataKey: "max",
		}},
		[]byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		"gpt-4o-mini",
	) {
		t.Fatal("expected reasoning metadata to require thinking transforms")
	}
}

func findCountTokensSummaryEntry(t *testing.T, entries []*log.Entry, executorName string) *log.Entry {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry == nil {
			continue
		}
		if entry.Data["event"] == "count_tokens_summary" && entry.Data["executor"] == executorName {
			return entry
		}
	}
	t.Fatalf("count_tokens_summary log entry not found for %s", executorName)
	return nil
}
