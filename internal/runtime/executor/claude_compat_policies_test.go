package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeCompatPolicyFixturesMatchLegacyBehavior(t *testing.T) {
	fixturePaths := []string{
		"testdata/compat/claude_deepseek_capabilities.json",
		"testdata/compat/claude_doubao_capabilities.json",
		"testdata/compat/claude_xiaomi_capabilities.json",
	}
	for _, fixturePath := range fixturePaths {
		fixture := readOpenAICompatPolicyFixture(t, fixturePath)
		t.Run(fixture.CompatKind, func(t *testing.T) {
			for _, fixtureCase := range fixture.Cases {
				t.Run(fixtureCase.Name, func(t *testing.T) {
					baseURL := fixture.BaseURL
					if fixtureCase.BaseURL != "" {
						baseURL = fixtureCase.BaseURL
					}
					legacy := downgradeClaudeToolSearchForCompatKind(fixture.CompatKind, baseURL, fixtureCase.Input)
					if !bytes.Equal(legacy, fixtureCase.Input) {
						var err error
						legacy, _, err = normalizeClaudeEmptyToolResults(legacy)
						if err != nil {
							t.Fatalf("normalizeClaudeEmptyToolResults() error = %v", err)
						}
					}
					assertOpenAICompatJSONEqual(t, legacy, fixtureCase.Expected)

					ctx := internalpayload.WithAmplificationMode(
						internalpayload.WithTransformReport(context.Background(), int64(len(fixtureCase.Input))),
						internalpayload.AmplificationModeObserve,
					)
					actual, managed, err := applyClaudeCompatProviderCapabilities(ctx, fixtureCase.Input, fixture.CompatKind, baseURL, compat.MatchContext{
						Model:        fixtureCase.Model,
						Endpoint:     "messages",
						Mode:         "non-stream",
						SourceFormat: "claude",
						TargetFormat: "claude",
					})
					if err != nil {
						t.Fatalf("applyClaudeCompatProviderCapabilities() error = %v", err)
					}
					if !managed {
						t.Fatal("registered Claude capability policy was not selected")
					}
					assertOpenAICompatJSONEqual(t, actual, fixtureCase.Expected)
					assertOpenAICompatJSONEqual(t, actual, legacy)

					report, ok := internalpayload.TransformReportFromContext(ctx)
					if !ok || len(report.Stages) != 1 {
						t.Fatalf("transform report = %+v, ok=%v", report, ok)
					}
					stage := report.Stages[0]
					if stage.Stage != claudeProviderCapabilityTransformStage || !slices.Equal(stage.AppliedPolicies, []string{fixture.PolicyID}) {
						t.Fatalf("transform stage = %+v", stage)
					}
					if !slices.Equal(stage.Downgrades, fixtureCase.Downgrades) {
						t.Fatalf("downgrades = %v, want %v", stage.Downgrades, fixtureCase.Downgrades)
					}
					if bytes.Contains(fixtureCase.Expected, []byte(claudeEmptyToolResultText)) {
						if stage.PatchedCount != 1 || stage.SyntheticBytes != int64(len(claudeEmptyToolResultText)) {
							t.Fatalf("empty tool result accounting = %+v", stage)
						}
					}

					second, _, err := applyClaudeCompatProviderCapabilities(context.Background(), actual, fixture.CompatKind, baseURL, compat.MatchContext{Model: fixtureCase.Model})
					if err != nil {
						t.Fatalf("second applyClaudeCompatProviderCapabilities() error = %v", err)
					}
					assertOpenAICompatJSONEqual(t, second, actual)
				})
			}
		})
	}
}

func TestClaudeCompatPolicyInventory(t *testing.T) {
	report, err := claudeCompatPolicyInventory()
	if err != nil {
		t.Fatalf("claudeCompatPolicyInventory() error = %v", err)
	}
	if len(report.Policies) != 3 {
		t.Fatalf("policy count = %d, want 3", len(report.Policies))
	}
	wantIDs := []string{
		claudeCompatDeepSeekCapabilityPolicyID,
		claudeCompatDoubaoCapabilityPolicyID,
		claudeCompatXiaomiCapabilityPolicyID,
	}
	for index, policy := range report.Policies {
		if policy.ID != wantIDs[index] {
			t.Fatalf("policy %d ID = %q, want %q", index, policy.ID, wantIDs[index])
		}
		if policy.Phase != compat.ProviderCapabilityScrub || policy.Match.ProviderFamily != "claude" {
			t.Fatalf("policy %q metadata = %+v", policy.ID, policy)
		}
		if !slices.Equal(policy.MutatedFields, []string{"body.messages", "body.tool_choice", "body.tools"}) {
			t.Fatalf("policy %q mutated fields = %v", policy.ID, policy.MutatedFields)
		}
		fixturePath := filepath.Join("..", "..", "..", filepath.Clean(policy.Lifecycle.Fixture))
		if _, err = os.Stat(fixturePath); err != nil {
			t.Fatalf("policy fixture %q: %v", policy.Lifecycle.Fixture, err)
		}
	}
}

func TestPrepareClaudeRequestAppliesImageCapabilityPolicyWithoutTools(t *testing.T) {
	tests := []struct {
		name         string
		compatKind   string
		baseURL      string
		model        string
		policyID     string
		wantImage    bool
		wantImageURL bool
	}{
		{name: "deepseek", compatKind: "deepseek", baseURL: "https://api.deepseek.com/anthropic", model: "deepseek-v4-pro", policyID: claudeCompatDeepSeekCapabilityPolicyID},
		{name: "doubao", compatKind: "doubao", baseURL: "https://ark.cn-beijing.volces.com/api/coding", model: "deepseek-v3.2", policyID: claudeCompatDoubaoCapabilityPolicyID, wantImage: true},
		{name: "xiaomi mimo v2.5", compatKind: "xiaomi", baseURL: "https://token-plan-cn.xiaomimimo.com/anthropic", model: "mimo-v2.5", policyID: claudeCompatXiaomiCapabilityPolicyID, wantImage: true},
		{name: "xiaomi other model", compatKind: "xiaomi", baseURL: "https://token-plan-cn.xiaomimimo.com/anthropic", model: "mimo-v2.5-pro", policyID: claudeCompatXiaomiCapabilityPolicyID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
				"api_key":     "test-key",
				"base_url":    test.baseURL,
				"compat_kind": test.compatKind,
			}}
			payload := []byte(`{"model":"` + test.model + `","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}]}]}`)
			var first []byte
			for _, stream := range []bool{false, true} {
				ctx, releaseReport := retainExecutorTransformReport(context.Background(), len(payload))
				plan, err := executor.prepareClaudeRequest(ctx, auth, cliproxyexecutor.Request{Model: test.model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, test.model, stream)
				if err != nil {
					t.Fatalf("prepareClaudeRequest(stream=%v) error = %v", stream, err)
				}
				content := gjson.GetBytes(plan.bodyForUpstream, "messages.0.content").Array()
				if got := hasClaudePartType(content, "image"); got != test.wantImage {
					t.Fatalf("stream=%v image=%v, want %v: %s", stream, got, test.wantImage, plan.bodyForUpstream)
				}
				if got := hasClaudePartType(content, "image_url"); got != test.wantImageURL {
					t.Fatalf("stream=%v image_url=%v, want %v: %s", stream, got, test.wantImageURL, plan.bodyForUpstream)
				}
				report, ok := internalpayload.TransformReportFromContext(ctx)
				if !ok {
					t.Fatal("transform report missing")
				}
				matched := 0
				for _, stage := range report.Stages {
					if stage.Stage == claudeProviderCapabilityTransformStage && slices.Equal(stage.AppliedPolicies, []string{test.policyID}) {
						matched++
					}
				}
				if matched != 1 {
					t.Fatalf("provider capability stage count = %d, want 1; report=%+v", matched, report)
				}
				releaseReport()
				if first == nil {
					first = plan.bodyForUpstream
				} else {
					assertOpenAICompatJSONEqual(t, plan.bodyForUpstream, first)
				}
			}
		})
	}
}

func TestClaudeCompatPolicyPreservesUnsupportedImageOnlyUserMessage(t *testing.T) {
	tests := []struct {
		name       string
		compatKind string
		baseURL    string
		model      string
		part       string
	}{
		{
			name:       "deepseek native image",
			compatKind: "deepseek",
			baseURL:    "https://api.deepseek.com/anthropic",
			model:      "deepseek-v4-pro",
			part:       `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`,
		},
		{
			name:       "doubao image url",
			compatKind: "doubao",
			baseURL:    "https://ark.cn-beijing.volces.com/api/coding",
			model:      "deepseek-v3.2",
			part:       `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`,
		},
		{
			name:       "xiaomi unsupported model image",
			compatKind: "xiaomi",
			baseURL:    "https://token-plan-cn.xiaomimimo.com/anthropic",
			model:      "mimo-v2.5-pro",
			part:       `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"model":"` + test.model + `","messages":[{"role":"user","content":[` + test.part + `]}]}`)
			ctx := internalpayload.WithAmplificationMode(
				internalpayload.WithTransformReport(context.Background(), int64(len(payload))),
				internalpayload.AmplificationModeObserve,
			)
			actual, managed, err := applyClaudeCompatProviderCapabilities(ctx, payload, test.compatKind, test.baseURL, compat.MatchContext{Model: test.model})
			if err != nil {
				t.Fatalf("applyClaudeCompatProviderCapabilities() error = %v", err)
			}
			if !managed {
				t.Fatal("registered Claude capability policy was not selected")
			}
			messages := gjson.GetBytes(actual, "messages").Array()
			if len(messages) != 1 {
				t.Fatalf("messages = %s, want one preserved user message", gjson.GetBytes(actual, "messages").Raw)
			}
			content := messages[0].Get("content").Array()
			if len(content) != 1 || content[0].Get("type").String() != "text" || content[0].Get("text").String() != claudeUnsupportedContentPlaceholderText {
				t.Fatalf("preserved content = %s", messages[0].Get("content").Raw)
			}
			report, ok := internalpayload.TransformReportFromContext(ctx)
			if !ok || len(report.Stages) != 1 {
				t.Fatalf("transform report = %+v, ok=%v", report, ok)
			}
			stage := report.Stages[0]
			if stage.PatchedCount != 1 || stage.SyntheticBytes != int64(len(claudeUnsupportedContentPlaceholderText)) {
				t.Fatalf("placeholder accounting = %+v", stage)
			}
			second, _, err := applyClaudeCompatProviderCapabilities(context.Background(), actual, test.compatKind, test.baseURL, compat.MatchContext{Model: test.model})
			if err != nil {
				t.Fatalf("second applyClaudeCompatProviderCapabilities() error = %v", err)
			}
			assertOpenAICompatJSONEqual(t, second, actual)
		})
	}
}

func TestPrepareClaudeRequestPreservesUnsupportedImageOnlyUserMessage(t *testing.T) {
	tests := []struct {
		name       string
		compatKind string
		baseURL    string
		model      string
		part       string
	}{
		{
			name:       "deepseek",
			compatKind: "deepseek",
			baseURL:    "https://api.deepseek.com/anthropic",
			model:      "deepseek-v4-pro",
			part:       `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`,
		},
		{
			name:       "doubao",
			compatKind: "doubao",
			baseURL:    "https://ark.cn-beijing.volces.com/api/coding",
			model:      "deepseek-v3.2",
			part:       `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`,
		},
		{
			name:       "xiaomi",
			compatKind: "xiaomi",
			baseURL:    "https://token-plan-cn.xiaomimimo.com/anthropic",
			model:      "mimo-v2.5-pro",
			part:       `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
				"api_key":     "test-key",
				"base_url":    test.baseURL,
				"compat_kind": test.compatKind,
			}}
			payload := []byte(`{"model":"` + test.model + `","max_tokens":128,"messages":[{"role":"user","content":[` + test.part + `]}]}`)
			for _, stream := range []bool{false, true} {
				plan, err := executor.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{
					Model:   test.model,
					Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, test.model, stream)
				if err != nil {
					t.Fatalf("prepareClaudeRequest(stream=%v) error = %v", stream, err)
				}
				messages := gjson.GetBytes(plan.bodyForUpstream, "messages").Array()
				if len(messages) != 1 || messages[0].Get("role").String() != "user" {
					t.Fatalf("stream=%v messages=%s", stream, gjson.GetBytes(plan.bodyForUpstream, "messages").Raw)
				}
				content := messages[0].Get("content").Array()
				if len(content) != 1 || content[0].Get("text").String() != claudeUnsupportedContentPlaceholderText {
					t.Fatalf("stream=%v content=%s", stream, messages[0].Get("content").Raw)
				}
			}
		})
	}
}

func TestSanitizeClaudeHTTPRequestToolNamesUsesExplicitCompatKind(t *testing.T) {
	payload := `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	requestURLs := []string{
		"https://custom-relay.example/v1/messages",
		"https://api.anthropic.com/v1/messages",
	}
	for _, requestURL := range requestURLs {
		t.Run(requestURL, func(t *testing.T) {
			ctx := internalpayload.WithAmplificationMode(
				internalpayload.WithTransformReport(context.Background(), int64(len(payload))),
				internalpayload.AmplificationModeObserve,
			)
			req := httptest.NewRequest(http.MethodPost, requestURL, strings.NewReader(payload)).WithContext(ctx)
			if _, err := sanitizeClaudeHTTPRequestToolNamesForCompatKind(req, "deepseek"); err != nil {
				t.Fatalf("sanitizeClaudeHTTPRequestToolNamesForCompatKind() error = %v", err)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if hasClaudePartType(gjson.GetBytes(body, "messages.0.content").Array(), "image") {
				t.Fatalf("explicit DeepSeek capability did not remove image: %s", body)
			}
			if got := gjson.GetBytes(body, "messages.0.content.0.text").String(); got != claudeUnsupportedContentPlaceholderText {
				t.Fatalf("explicit DeepSeek capability dropped image-only message, got placeholder %q: %s", got, body)
			}
			report, ok := internalpayload.TransformReportFromContext(ctx)
			if !ok || len(report.Stages) != 1 || !slices.Equal(report.Stages[0].AppliedPolicies, []string{claudeCompatDeepSeekCapabilityPolicyID}) {
				t.Fatalf("transform report = %+v, ok=%v", report, ok)
			}
		})
	}
}

func BenchmarkClaudeCompatImageCapabilityScrub(b *testing.B) {
	for _, partCount := range []int{0, 16, 64, 256, 1024} {
		b.Run("parts_"+strconv.Itoa(partCount), func(b *testing.B) {
			payload := buildClaudeCompatImageBenchmarkPayload(partCount, 4*1024)
			match := compat.MatchContext{Model: "deepseek-v4-pro", Endpoint: "messages", Mode: "non-stream", SourceFormat: "claude", TargetFormat: "claude"}
			out, managed, err := applyClaudeCompatProviderCapabilities(context.Background(), payload, "deepseek", "https://api.deepseek.com/anthropic", match)
			if err != nil || !managed || len(out) == 0 {
				b.Fatalf("benchmark setup failed: managed=%v output=%d error=%v", managed, len(out), err)
			}
			if partCount > 0 && (gjson.GetBytes(out, "messages.#").Int() != 1 || gjson.GetBytes(out, "messages.0.content.0.text").String() != claudeUnsupportedContentPlaceholderText) {
				b.Fatalf("benchmark setup dropped image-only message: %s", out)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for range b.N {
				if _, _, err := applyClaudeCompatProviderCapabilities(context.Background(), payload, "deepseek", "https://api.deepseek.com/anthropic", match); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func buildClaudeCompatImageBenchmarkPayload(partCount, imageBytes int) []byte {
	part := `{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", imageBytes) + `"}}`
	parts := make([]string, partCount)
	for index := range parts {
		parts[index] = part
	}
	content := internalpayload.BuildRaw(parts)
	payload := make([]byte, 0, len(content)+96)
	payload = append(payload, `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":`...)
	payload = append(payload, content...)
	return append(payload, "}]}"...)
}
