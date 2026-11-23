package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGLM54ChatRequest(t *testing.T) {
	for _, tc := range []struct {
		name, format, payload, model, effort string
	}{
		{"explicit effort wins", "openai", `{"messages":[],"thinking":{"type":"disabled"},"reasoning_effort":"medium"}`, "glm-5.4", "high"},
		{"default", "openai", `{"messages":[]}`, "glm-5.4", "max"},
		{"disable alone", "claude", `{"messages":[],"thinking":{"type":"disabled"}}`, "glm-5.4", "low"},
		{"claude controls", "claude", `{"messages":[],"thinking":{"type":"disabled"},"output_config":{"effort":"ultra"}}`, "glm-5.4", "max"},
		{"responses light", "openai-response", `{"input":"hello","reasoning":{"effort":"light"}}`, "glm-5.4", "low"},
		{"unknown", "openai", `{"messages":[],"reasoning_effort":"future-level"}`, "glm-5.4", "max"},
		{"CPA suffix", "openai", `{"messages":[],"reasoning_effort":"high"}`, "glm-5.4(low)", "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseURL := "https://api.z.ai/api/coding/paas/v4"
			exec := newZhipuGLM53TestExecutor()
			req := cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.payload)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.format)}
			plan, err := exec.prepareOpenAICompatRequest(context.Background(), zhipuGLM53TestAuth(baseURL), req, opts, baseURL, "glm-5.4", openAICompatProfileForKind("zhipu"), false)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(plan.body, "reasoning_effort").String() != tc.effort || gjson.GetBytes(plan.body, "thinking.type").String() != "enabled" {
				t.Fatalf("unexpected controls: %s", plan.body)
			}
		})
	}
}

func TestGLM54StreamVisionToolHistory(t *testing.T) {
	payload := []byte(`{"model":"glm-5.4","stream":true,"tool_stream":true,"messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","reasoning_content":" prior\nthought  ","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"s\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"checking\"}}]}\n\ndata: {\"id\":\"s\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	res, err := newZhipuGLM53TestExecutor().ExecuteStream(context.Background(), zhipuGLM53TestAuth(server.URL+"/api/coding/paas/v4"), cliproxyexecutor.Request{Model: "glm-5.4", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	body := <-requestBody
	if gjson.GetBytes(body, "messages.0.content.0.image_url.url").String() != "data:image/png;base64,AAAA" || gjson.GetBytes(body, "messages.1.reasoning_content").String() != " prior\nthought  " || !gjson.GetBytes(body, "tool_stream").Bool() {
		t.Fatalf("unexpected wire request: %s", body)
	}
	if !strings.Contains(output.String(), "checking") || !strings.Contains(output.String(), "lookup") {
		t.Fatalf("incomplete stream: %s", output.String())
	}
}

func TestGLM54ClaudeNative(t *testing.T) {
	body := []byte(`{"model":"glm-5.4","max_tokens":200,"thinking":{"type":"disabled","clear_thinking":true},"output_config":{"effort":"medium"},"temperature":0.6,"tool_choice":{"type":"any"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`)
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://open.bigmodel.cn/api/anthropic", "api_key": "test"}}
	plan, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{Model: "glm-5.4", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, "glm-5.4", false)
	if err != nil {
		t.Fatal(err)
	}
	out := plan.bodyForUpstream
	for path, want := range map[string]string{"output_config.effort": "high", "thinking.type": "adaptive", "thinking.clear_thinking": "true", "temperature": "0.6", "tool_choice.type": "any", "messages.0.content.0.source.data": "AAAA"} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q: %s", path, got, want, out)
		}
	}
}
