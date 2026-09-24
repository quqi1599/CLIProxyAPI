package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	claudecodex "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/claude"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func codexAmplificationSource(t *testing.T) []byte {
	t.Helper()
	values := make([]string, 150000)
	for i := range values {
		values[i] = "x"
	}
	body, err := json.Marshal(map[string]any{
		"model": "gpt-5.6-sol",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "call_1", "name": "audit", "input": map[string]any{"values": values},
			}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "ok"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	translated := claudecodex.ConvertClaudeRequestToCodex("gpt-5.6-sol", body, false)
	if !internalpayload.ObserveAmplification(int64(len(body)), int64(len(translated)), internalpayload.AmplificationOverride{}).Exceeded {
		t.Fatal("fixture must exceed the actual Codex conversion allowance")
	}
	return body
}

func TestCodexToolHistoryAmplificationIsCandidateScoped(t *testing.T) {
	body := codexAmplificationSource(t)
	for _, mode := range []internalpayload.AmplificationMode{"", internalpayload.AmplificationModeEnforce, internalpayload.AmplificationModeObserve} {
		for _, operation := range []string{"execute", "stream", "count"} {
			for _, mixed := range []bool{false, true} {
				t.Run(fmt.Sprintf("mode=%s/%s/mixed=%t", mode, operation, mixed), func(t *testing.T) {
					const model = "gpt-5.6-sol"
					ctx := internalpayload.WithAmplificationMode(context.Background(), mode)
					manager := NewManager(nil, &FillFirstSelector{}, nil)
					manager.SetRetryConfig(0, 0, 5)
					translators := sdktranslator.NewRegistry()
					translators.Register(sdktranslator.FormatClaude, sdktranslator.FormatCodex, claudecodex.ConvertClaudeRequestToCodex, sdktranslator.ResponseTransform{})
					manager.SetTranslatorRegistry(translators)
					codex := &authFallbackExecutor{id: "codex"}
					claude := &authFallbackExecutor{id: "claude"}
					manager.RegisterExecutor(codex)
					manager.RegisterExecutor(claude)
					registerGPTChannelFailoverAuths(t, manager, "codex", model, []*Auth{{ID: t.Name() + "-codex", Provider: "codex", Attributes: map[string]string{"priority": "100", "native_responses": "true"}}})
					providers := []string{"codex"}
					if mixed {
						providers = append(providers, "claude")
						registerGPTChannelFailoverAuths(t, manager, "claude", model, []*Auth{{ID: t.Name() + "-claude", Provider: "claude", Attributes: map[string]string{"priority": "1"}}})
					}
					metadata := map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "workbuddy"}
					req := cliproxyexecutor.Request{Model: model, Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body, Metadata: metadata}
					var err error
					var codexCalls, claudeCalls []string
					switch operation {
					case "execute":
						_, err = manager.Execute(ctx, providers, req, opts)
						codexCalls, claudeCalls = codex.ExecuteCalls(), claude.ExecuteCalls()
					case "count":
						_, err = manager.ExecuteCount(ctx, providers, req, opts)
						codexCalls, claudeCalls = codex.CountCalls(), claude.CountCalls()
					default:
						var stream *cliproxyexecutor.StreamResult
						stream, err = manager.ExecuteStream(ctx, providers, req, opts)
						if err == nil {
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
						codexCalls, claudeCalls = codex.StreamCalls(), claude.StreamCalls()
					}
					blocked := mode != internalpayload.AmplificationModeObserve && operation != "count"
					if blocked && !mixed {
						failure, ok := failurecontract.As(err)
						if !ok || failure.HTTPStatus != 400 || failure.ErrorCode() != "request_transform_expansion_exceeded" || len(codexCalls) != 0 {
							t.Fatalf("Codex-only unsafe transform escaped: error=%v calls=%v", err, codexCalls)
						}
					} else if blocked {
						if err != nil || len(codexCalls) != 0 || len(claudeCalls) != 1 {
							t.Fatalf("healthy native provider suppressed: error=%v codex=%v claude=%v", err, codexCalls, claudeCalls)
						}
					} else if err != nil || len(codexCalls) != 1 || len(claudeCalls) != 0 {
						t.Fatalf("observe/count routing changed: error=%v codex=%v claude=%v", err, codexCalls, claudeCalls)
					}
					if _, mutated := metadata[codexToolHistoryPreflightFailureMetadataKey]; mutated {
						t.Fatal("preflight mutated caller-owned metadata")
					}
				})
			}
		}
	}
}

func TestCodexToolHistoryPreflightRejectionCannotBeOverriddenByNativeCapability(t *testing.T) {
	opts := cliproxyexecutor.Options{Metadata: map[string]any{codexToolHistoryPreflightFailureMetadataKey: &codexToolHistoryPreflightFailure{err: fmt.Errorf("rejected speculative transform")}}}
	auths := []*Auth{{ID: "native-codex", Provider: "codex", Attributes: map[string]string{"native_responses": "true"}}, {ID: "native-claude", Provider: "claude"}}
	filtered, excluded := preferGPTNativeResponsesAuths(auths, []string{"codex", "claude"}, "gpt-5.6-sol", opts)
	if excluded != 1 || len(filtered) != 1 || filtered[0].ID != "native-claude" {
		t.Fatalf("native-responses declaration bypassed rejection: filtered=%v excluded=%d", filtered, excluded)
	}
	opts.TokenCount = true
	if filtered, excluded = preferGPTNativeResponsesAuths(auths, []string{"codex", "claude"}, "gpt-5.6-sol", opts); excluded != 0 || len(filtered) != 2 {
		t.Fatal("generation preflight rejected token counting")
	}
	opts.TokenCount = false
	opts.Metadata[codexToolHistoryPreflightFailureMetadataKey] = map[string]any{"err": "untrusted JSON"}
	if codexToolHistoryPreflightError(opts) != nil {
		t.Fatal("client JSON forged an internal preflight failure")
	}
}
