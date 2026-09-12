package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestDeepSeekV41NativeResponsesPreservesImagesToolsAndThinkingOff(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		t.Run(model, func(t *testing.T) {
			payload := []byte(`{"model":"` + model + `","input":[{"role":"user","content":[{"type":"input_text","text":"Inspect this image"},{"type":"input_image","image_url":"https://example.com/image.png"}]}],"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}},{"type":"custom","name":"apply_patch"}],"reasoning":{"effort":"none"},"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}}}`)
			exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			plan, err := exec.prepareOpenAICompatRequest(context.Background(), nil, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, "https://api.deepseek.com", model, openAICompatProfileForKind("deepseek"), false)
			if err != nil {
				t.Fatal(err)
			}
			if plan.endpoint != "/responses" {
				t.Fatalf("endpoint = %s", plan.endpoint)
			}
			body := plan.body
			for path, want := range map[string]string{
				"model":                       helps.OfficialDeepSeekModel(model, "https://api.deepseek.com"),
				"input.0.content.1.image_url": "https://example.com/image.png",
				"tools.0.name":                "web_search", "tools.1.name": "apply_patch",
				"reasoning.effort": "none", "text.format.type": "json_schema",
			} {
				if got := gjson.GetBytes(body, path).String(); got != want {
					t.Fatalf("%s = %q, want %q; body=%s", path, got, want, body)
				}
			}
			if gjson.GetBytes(body, "messages").Exists() {
				t.Fatal("native Responses was converted into Chat")
			}
		})
	}
}

func TestDeepSeekV41ClaudeImageSurvivesCapabilityScrub(t *testing.T) {
	for _, stream := range []bool{false, true} {
		model := "deepseek-v4.1-flash"
		payload := []byte(`{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"messages":[{"role":"user","content":[{"type":"text","text":"Describe"},{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}}]}]}`)
		exec := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
		auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.deepseek.com/anthropic", "api_key": "test", "compat_kind": "deepseek"}}
		plan, err := exec.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, model, stream)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(plan.bodyForUpstream, "messages.0.content.1.source.url").String(); got != "https://example.com/image.png" {
			t.Fatalf("image lost during prepare: %s", plan.bodyForUpstream)
		}
		if got := gjson.GetBytes(plan.bodyForUpstream, "model").String(); got != "deepseek-flash" {
			t.Fatalf("upstream model = %q", got)
		}
	}
}

func TestDeepSeekV41CrossProtocolImagesSurvive(t *testing.T) {
	const imageData = "iVBORw0KGgo="
	model := "deepseek-v4.1-flash"
	chatPayload := []byte(`{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"messages":[{"role":"user","content":[{"type":"text","text":"Describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + imageData + `"}}]}]}`)
	claudePayload := []byte(`{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"messages":[{"role":"user","content":[{"type":"text","text":"Describe"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + imageData + `"}}]}]}`)
	t.Run("chat to claude", func(t *testing.T) {
		exec := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
		auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.deepseek.com/anthropic", "api_key": "test", "compat_kind": "deepseek"}}
		plan, err := exec.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: chatPayload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}, model, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(plan.bodyForUpstream, "messages.0.content.1.source.data").String(); got != imageData {
			t.Fatalf("image lost during Chat to Claude conversion: %s", plan.bodyForUpstream)
		}
	})
	t.Run("claude to chat", func(t *testing.T) {
		exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
		plan, err := exec.prepareOpenAICompatRequest(context.Background(), nil, cliproxyexecutor.Request{Model: model, Payload: claudePayload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "https://api.deepseek.com", model, openAICompatProfileForKind("deepseek"), false)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(plan.body, "messages.0.content.1.image_url.url").String(); got != "data:image/png;base64,"+imageData {
			t.Fatalf("image lost during Claude to Chat conversion: %s", plan.body)
		}
	})
}

func TestDeepSeekV41ThinkingHistoryChecksNonToolAssistantTurns(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-flash"} {
		body := []byte(`{"thinking":{"type":"enabled"},"tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"continue"}]}`)
		for _, client := range []string{"workbuddy", "claude_code"} {
			out, _, downgraded, report, err := normalizeThinkingHistoryForModelWithReportForClient(body, "openai", model, client)
			if err != nil || !downgraded || report.DowngradeReason != thinkingHistoryClientDowngradeReason {
				t.Fatalf("model=%s client=%s downgraded=%v report=%+v err=%v", model, client, downgraded, report, err)
			}
			if gjson.GetBytes(out, "messages").Raw != gjson.GetBytes(body, "messages").Raw {
				t.Fatalf("history was synthesized or changed: %s", out)
			}
			if gjson.GetBytes(out, "thinking.type").String() != "disabled" {
				t.Fatalf("thinking still enabled: %s", out)
			}
		}
		out, _, downgraded, report, err := normalizeThinkingHistoryForModelWithReportForClient(body, "openai", model, "")
		if err != nil || !downgraded || report.DowngradeReason != thinkingHistoryFlashDowngradeReason {
			t.Fatalf("flash explicit thinking should downgrade safely: downgraded=%v report=%+v err=%v", downgraded, report, err)
		}
		if gjson.GetBytes(out, "thinking.type").String() != "disabled" {
			t.Fatalf("flash thinking was not disabled: %s", out)
		}
	}
	proBody := []byte(`{"thinking":{"type":"enabled"},"reasoning_effort":"high","tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`)
	if _, _, _, _, err := normalizeThinkingHistoryForModelWithReportForClient(proBody, "openai", "deepseek-v4-pro", ""); err == nil {
		t.Fatal("explicit thinking for DeepSeek Pro should remain rejected")
	}
	noTools := []byte(`{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":"hello"},{"role":"user","content":"continue"}]}`)
	out, changed, downgraded, _, err := normalizeThinkingHistoryForModelWithReportForClient(noTools, "openai", "deepseek-flash", "workbuddy")
	if err != nil || changed || downgraded || !bytes.Equal(out, noTools) {
		t.Fatalf("ordinary conversation changed: %s, err=%v", out, err)
	}
}

func TestDeepSeekV41ToolChoicePreservesExecutionIntent(t *testing.T) {
	for _, choice := range []string{`"none"`, `"auto"`, `"required"`, `{"type":"function","function":{"name":"lookup"}}`} {
		body := []byte(`{"model":"deepseek-flash","thinking":{"type":"enabled"},"reasoning_effort":"high","tool_choice":` + choice + `}`)
		out := scrubDeepSeekThinkingToolChoice(body, "deepseek-flash", "https://api.deepseek.com", "deepseek")
		if gjson.GetBytes(out, "tool_choice").Raw != choice {
			t.Fatalf("tool choice changed: %s", out)
		}
		forced := strings.Contains(choice, "required") || strings.Contains(choice, "function")
		if disabled := gjson.GetBytes(out, "thinking.type").String() == "disabled"; disabled != forced {
			t.Fatalf("forced=%v output=%s", forced, out)
		}
	}
	defaultOn := []byte(`{"model":"deepseek-flash","tool_choice":"required"}`)
	if out := scrubDeepSeekThinkingToolChoice(defaultOn, "deepseek-flash", "https://api.deepseek.com", "deepseek"); gjson.GetBytes(out, "thinking.type").String() != "disabled" {
		t.Fatalf("forced tools did not disable default-on thinking: %s", out)
	}
}

func TestDeepSeekV41RejectsIgnoredBuiltinSearchButKeepsLocalSearch(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-flash"} {
		body := []byte(`{"tools":[{"type":"web_search"}]}`)
		err := rejectDeepSeekUnsupportedResponsesTools(context.Background(), body, openAICompatProfileForKind("deepseek"), model, "/v1/responses", "/responses", "openai-response")
		if err == nil {
			t.Fatalf("%s silently ignored built-in search", model)
		}
		if !deepSeekResponsesToolSupported("function", "web_search", "/responses", model) || !deepSeekResponsesToolSupported("custom", "apply_patch", "/responses", model) {
			t.Fatal("client-executed tools were rejected")
		}
		if !thinking.IsDeepSeekV4Model(model) {
			t.Fatal("model lost DeepSeek protections")
		}
	}
}
