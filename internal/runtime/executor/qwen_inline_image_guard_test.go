package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestQwenPreparePathsRejectLargeInlineBase64ImageRequest(t *testing.T) {
	base64Image := strings.Repeat("A", helps.QwenInlineBase64ImagePayloadLimitBytes)
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "openai compatible",
			run: func() error {
				body := []byte(`{"model":"qwen3.7-plus","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + base64Image + `"}}]}]}`)
				executor := NewOpenAICompatExecutor("qwen-provider", &config.Config{})
				_, err := executor.prepareOpenAICompatRequest(
					context.Background(),
					nil,
					cliproxyexecutor.Request{Model: "qwen3.7-plus", Payload: body},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI},
					"https://example.com/compatible-mode/v1",
					"qwen3.7-plus",
					openAICompatProfileForKind("qwen"),
					false,
				)
				return err
			},
		},
		{
			name: "claude compatible",
			run: func() error {
				body := []byte(`{"model":"qwen3.7-plus","max_tokens":128,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64Image + `"}}]}]}`)
				executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
				auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
					"api_key":     "test-key",
					"base_url":    "https://example.com/apps/anthropic",
					"compat_kind": "qwen",
				}}
				_, err := executor.prepareClaudeRequest(
					context.Background(),
					auth,
					cliproxyexecutor.Request{Model: "qwen3.7-plus", Payload: body},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
					"qwen3.7-plus",
					false,
				)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil {
				t.Fatal("expected large inline image request to be rejected")
			}
			typed, ok := failurecontract.As(err)
			if !ok || typed.Kind != failurecontract.RequestTooLarge || typed.Scope != failurecontract.ScopeRequest || typed.Retryable {
				t.Fatalf("error = %#v, want non-retryable request-too-large", err)
			}
			if typed.HTTPStatus != http.StatusRequestEntityTooLarge || typed.ProviderCode != "request_too_large" {
				t.Fatalf("failure status/code = %d/%q", typed.HTTPStatus, typed.ProviderCode)
			}
			for _, want := range []string{"压缩图片", "HTTPS URL", "单纯放大 CPA 上限"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want %q", err.Error(), want)
				}
			}
		})
	}
}

func TestRejectQwenLargeInlineBase64ImageRequestAllowsImageURL(t *testing.T) {
	padding := strings.Repeat("A", helps.QwenInlineBase64ImagePayloadLimitBytes)
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}},{"type":"text","text":"` + padding + `"}]}]}`)
	if err := rejectQwenLargeInlineBase64ImageRequest(context.Background(), body, "qwen", "qwen3.7-plus", "/chat/completions", "OpenAICompatExecutor"); err != nil {
		t.Fatalf("remote image URL request was rejected: %v", err)
	}
}
