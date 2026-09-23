package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

func TestRequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey(t *testing.T) {
	ctx := WithExecutionSessionID(context.Background(), "session-1")

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("ExecutionSessionMetadataKey = %v, want %q", got, "session-1")
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
}

func TestRequestExecutionMetadataCarriesPinnedAuthFallbackOnlyWhenEnabled(t *testing.T) {
	hardPinned := WithPinnedAuthID(context.Background(), "auth-1")
	hardMetadata := requestExecutionMetadata(hardPinned)
	if got := hardMetadata[coreexecutor.PinnedAuthMetadataKey]; got != "auth-1" {
		t.Fatalf("PinnedAuthMetadataKey = %v, want auth-1", got)
	}
	if _, ok := hardMetadata[coreexecutor.PinnedAuthFallbackMetadataKey]; ok {
		t.Fatal("hard pin unexpectedly enabled fallback")
	}

	fallbackPinned := WithPinnedAuthFallback(hardPinned)
	fallbackMetadata := requestExecutionMetadata(fallbackPinned)
	if got := fallbackMetadata[coreexecutor.PinnedAuthFallbackMetadataKey]; got != true {
		t.Fatalf("PinnedAuthFallbackMetadataKey = %v, want true", got)
	}
}

func TestInferClientProfileFromHeadersDetectsWorkBuddy(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "WorkBuddy/5.1")

	if got := inferClientProfileFromHeaders(headers); got != "workbuddy" {
		t.Fatalf("profile = %q, want workbuddy", got)
	}
}

func TestInferClientProfileFromHeadersDetectsCodeBuddy(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Client-Name", "CodeBuddy")

	if got := inferClientProfileFromHeaders(headers); got != "workbuddy" {
		t.Fatalf("profile = %q, want workbuddy", got)
	}
}

func TestInferClientProfileFromHeadersDetectsClaudeCode(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
	}{
		{name: "official cli user agent", header: "User-Agent", value: "claude-cli/2.1.153 (external, cli)"},
		{name: "client name", header: "X-Client-Name", value: "Claude Code"},
		{name: "hyphenated app name", header: "X-App-Name", value: "claude-code"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set(test.header, test.value)

			if got := inferClientProfileFromHeaders(headers); got != "claude_code" {
				t.Fatalf("profile = %q, want claude_code", got)
			}
		})
	}
}

func TestInferClientProfileFromHeadersPrefersWorkBuddyWrapper(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.153 (external, cli)")
	headers.Set("X-App-Name", "WorkBuddy")

	if got := inferClientProfileFromHeaders(headers); got != "workbuddy" {
		t.Fatalf("profile = %q, want workbuddy", got)
	}
}

func TestHTTPCodexMetadataReachesToolHistoryGuard(t *testing.T) {
	input := make([]map[string]string, 240)
	for index := range input {
		input[index] = map[string]string{"role": "user", "content": "synthetic history"}
	}
	tools := make([]map[string]any, 8)
	for index := range tools {
		tools[index] = map[string]any{"type": "function", "name": "synthetic_tool", "parameters": map[string]any{"type": "object"}}
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": input, "tools": tools})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		headers map[string]string
		profile string
		guard   bool
	}{
		{name: "official cli", headers: map[string]string{"User-Agent": "codex_cli_rs/0.133.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9"}, profile: "codex_cli", guard: true},
		{name: "tui", headers: map[string]string{"User-Agent": "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.135.0)"}, profile: "codex_tui", guard: true},
		{name: "client name", headers: map[string]string{"X-Client-Name": "Codex"}, profile: "codex", guard: true},
		{name: "app name", headers: map[string]string{"X-App-Name": " codex "}, profile: "codex", guard: true},
		{name: "embedded product token", headers: map[string]string{"User-Agent": "HTTPClient/1.0 codex_cli_rs/0.133.0"}, profile: "codex_cli", guard: true},
		{name: "case insensitive", headers: map[string]string{"User-Agent": "CODEX_CLI_RS/0.133.0"}, profile: "codex_cli", guard: true},
		{name: "workbuddy wrapper wins", headers: map[string]string{"User-Agent": "codex_cli_rs/0.133.0", "X-App-Name": "WorkBuddy"}, profile: "workbuddy", guard: true},
		{name: "ordinary client", headers: map[string]string{"User-Agent": "curl/8.1.0"}},
		{name: "model name is not client", headers: map[string]string{"X-Title": "gpt-5.3-codex"}},
		{name: "arbitrary codex text", headers: map[string]string{"X-Title": "My Codex Project"}},
		{name: "lookalike product", headers: map[string]string{"User-Agent": "notcodex_cli_rs/1.0"}},
		{name: "unrecognized wrapper", headers: map[string]string{"User-Agent": "my-codex-client/1.0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			for key, value := range test.headers {
				ginCtx.Request.Header.Set(key, value)
			}
			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			meta := requestExecutionMetadata(ctx)
			setRequestShapeAndToolMetadata(meta, body)
			gotProfile, _ := meta[coreexecutor.ClientProfileMetadataKey].(string)
			if gotProfile != test.profile {
				t.Fatalf("HTTP metadata profile = %q, want %q", gotProfile, test.profile)
			}
			if got := coreauth.RequiresNativeResponsesToolHistory(meta, body); got != test.guard {
				t.Fatalf("HTTP metadata guard = %t, want %t", got, test.guard)
			}
		})
	}
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
	if got := meta[coreexecutor.ReasoningEffortOriginalMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortOriginalMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
	if got := meta[coreexecutor.ReasoningEffortOriginalMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortOriginalMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetServiceTierMetadataExtractsValue(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"priority"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "priority" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "priority")
	}
}

func TestSetServiceTierMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "default" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "default")
	}
}
