package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"testing"
)

func TestMiniMaxM3OffSurvivesBothRequestPlans(t *testing.T) {
	for _, source := range []string{"openai", "claude", "openai-response"} {
		payload := []byte(`{"messages":[{"role":"user","content":"OK"}],"max_tokens":4096,"reasoning_effort":"none"}`)
		if source == "claude" {
			payload = []byte(`{"messages":[{"role":"user","content":"OK"}],"max_tokens":4096,"thinking":{"type":"disabled"}}`)
		}
		if source == "openai-response" {
			payload = []byte(`{"input":"OK","max_output_tokens":4096,"reasoning":{"effort":"none"}}`)
		}
		for _, stream := range []bool{false, true} {
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.minimaxi.com/anthropic", "api_key": "fixture", "compat_kind": "minimax"}}
			req := cliproxyexecutor.Request{Model: "MiniMax-M3", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source), OriginalRequest: payload, Stream: stream}
			claude, err := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}).prepareClaudeRequest(context.Background(), auth, req, opts, "MiniMax-M3", stream)
			if err != nil {
				t.Fatal(err)
			}
			openai, err := NewOpenAICompatExecutor("minimax", &config.Config{}).prepareOpenAICompatRequest(context.Background(), auth, req, opts, "https://api.minimaxi.com/v1", "MiniMax-M3", openAICompatProfileForKind("minimax"), stream)
			if err != nil {
				t.Fatal(err)
			}
			for _, body := range [][]byte{claude.bodyForUpstream, openai.body} {
				if gjson.GetBytes(body, "thinking.type").String() != "disabled" || gjson.GetBytes(body, "reasoning_effort").Exists() {
					t.Fatalf("source off lost %s/%v: %s", source, stream, body)
				}
			}
		}
	}
}
