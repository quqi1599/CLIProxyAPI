package executor

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestMiniMaxM31RequestPlansPreserveEffort(t *testing.T) {
	for _, model := range []string{"MiniMax-M3.1", "MiniMax-M3.1-flash", "MiniMax-M3.1-Flash-Preview"} {
		for _, format := range []string{"openai", "claude", "openai-response"} {
			for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
				t.Run(model+"/"+format+"/"+effort, func(t *testing.T) {
					payload := []byte(`{"model":"client-alias","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`)
					field := "reasoning_effort"
					if format == "claude" {
						field = "output_config.effort"
					}
					if format == "openai-response" {
						payload = []byte(`{"model":"client-alias","input":"hello","max_output_tokens":1024}`)
						field = "reasoning.effort"
					}
					if effort != "" {
						payload, _ = sjson.SetBytes(payload, field, effort)
					}
					auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.minimax.cn/anthropic", "api_key": "test", "compat_kind": "minimax"}}
					req := cliproxyexecutor.Request{Model: model, Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format), OriginalRequest: payload, Stream: true}
					claudePlan, err := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}).prepareClaudeRequest(context.Background(), auth, req, opts, model, true)
					if err != nil {
						t.Fatalf("claude plan: %v", err)
					}
					assertMiniMaxM31Plan(t, claudePlan.bodyForUpstream, "output_config.effort", "MiniMax-M3.1-Flash-Preview", effort)
					openaiPlan, err := NewOpenAICompatExecutor("minimax-test", &config.Config{}).prepareOpenAICompatRequest(context.Background(), auth, req, opts, "https://api.minimax.cn/v1", model, openAICompatProfileForKind("minimax"), true)
					if err != nil {
						t.Fatalf("openai plan: %v", err)
					}
					assertMiniMaxM31Plan(t, openaiPlan.body, "reasoning_effort", "MiniMax-M3.1-Flash-Preview", effort)
					if !gjson.GetBytes(openaiPlan.body, "stream_options.include_usage").Bool() {
						t.Fatalf("stream usage missing: %s", openaiPlan.body)
					}
				})
			}
		}
	}
}

func assertMiniMaxM31Plan(t *testing.T, body []byte, field, model, effort string) {
	t.Helper()
	if got := gjson.GetBytes(body, field).String(); got != effort {
		t.Fatalf("%s = %q, want %q: %s", field, got, effort, body)
	}
	if got := gjson.GetBytes(body, "model").String(); got != model {
		t.Fatalf("model changed: %q", got)
	}
	if gjson.GetBytes(body, "thinking.type").String() == "disabled" || gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
		t.Fatalf("invalid thinking control: %s", body)
	}
}

func TestMiniMaxM31HistoryAndMultimodal(t *testing.T) {
	payload := []byte(`{"model":"MiniMax-M3.1-Flash-Preview","reasoning_effort":"low","messages":[{"role":"assistant","reasoning_content":"returned reasoning","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}]}`)
	out := scrubOpenAICompatPayloadForModel(payload, openAICompatProfileForKind("minimax"), "MiniMax-M3.1-Flash-Preview", "https://api.minimax.cn/v1")
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "returned reasoning" {
		t.Fatalf("lost reasoning history: %s", out)
	}
	for _, model := range []string{"MiniMax-M3.1", "MiniMax-M3.1-flash", "MiniMax-M3.1-Flash-Preview"} {
		body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"video","source":{"type":"url","url":"https://example.com/video.mp4"}}]}]}`, model))
		out := downgradeClaudeToolSearchForCompat("https://api.minimax.cn/anthropic", body)
		if gjson.GetBytes(out, "messages.0.content.0.type").String() != "image" || gjson.GetBytes(out, "messages.0.content.1.type").String() != "video" {
			t.Fatalf("multimodal content lost: %s", out)
		}
	}
}

func TestMiniMaxM31RawHTTPThinking(t *testing.T) {
	for _, format := range []string{"openai", "claude"} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disabled=%t", format, disabled), func(t *testing.T) {
				body := `{"model":"MiniMax-M3.1-Flash-Preview","stream":true,"messages":[{"role":"user","content":"hi"}]}`
				if disabled {
					body = strings.TrimSuffix(body, "}") + `,"thinking":{"type":"disabled"}}`
				}
				req := httptest.NewRequest("POST", "https://api.minimax.cn/anthropic/v1/messages", strings.NewReader(body))
				var err error
				if format == "claude" {
					_, err = sanitizeClaudeHTTPRequestToolNames(req)
				} else {
					err = sanitizeOpenAICompatHTTPRequestBody(req, openAICompatProfileForKind("minimax"), "https://api.minimax.cn/v1")
				}
				if disabled {
					status, ok := err.(interface{ StatusCode() int })
					if !ok || status.StatusCode() != 400 {
						t.Fatalf("expected local 400, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				out, _ := io.ReadAll(req.Body)
				if gjson.GetBytes(out, "thinking.type").String() == "disabled" {
					t.Fatalf("default thinking disabled: %s", out)
				}
			})
		}
	}
}

func TestMiniMaxM31ClaudeSamplingAndExplicitDisable(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			body := []byte(`{"model":"MiniMax-M3.1-Flash-Preview","max_tokens":1024,"temperature":0.3,"top_p":0.8,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`)
			if disabled {
				body, _ = sjson.SetBytes(body, "thinking.type", "disabled")
			}
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.minimax.cn/anthropic", "api_key": "test"}}
			plan, err := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}).prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{Model: "MiniMax-M3.1-Flash-Preview", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: body, Stream: stream}, "MiniMax-M3.1-Flash-Preview", stream)
			if disabled {
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 {
					t.Fatalf("expected local 400, got %v", err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(plan.bodyForUpstream, "temperature").Float() != 0.3 || gjson.GetBytes(plan.bodyForUpstream, "top_p").Float() != 0.8 {
				t.Fatalf("sampling controls overwritten: %s", plan.bodyForUpstream)
			}
		}
	}
}
