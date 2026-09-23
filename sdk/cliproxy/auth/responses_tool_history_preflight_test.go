package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestNativeResponsesPreflightUsesScopedTranslatorAndDoesNotMutateInput(t *testing.T) {
	registry := sdktranslator.NewRegistry()
	var calls int
	input := make([]map[string]any, 240)
	for i := range input {
		input[i] = map[string]any{"type": "function_call", "name": "test", "arguments": "{}"}
	}
	translated, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		t.Fatal(err)
	}
	registry.Register(sdktranslator.FormatClaude, sdktranslator.FormatCodex, func(_ string, body []byte, _ bool) []byte {
		calls++
		body[0] = 'x'
		return translated
	}, sdktranslator.ResponseTransform{})
	ctx := sdktranslator.ContextWithRegistry(context.Background(), registry)
	body := []byte(`{"messages":[{"role":"user","content":"source"}]}`)
	metadata := map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex", cliproxyexecutor.MessageCountMetadataKey: 1}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: append([]byte(nil), body...), Metadata: metadata}
	classified, err := classifyNativeResponsesToolHistory(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: body}, opts, false, false)
	if err != nil || calls != 1 || !RequiresNativeResponsesToolHistory(classified.Metadata, body) {
		t.Fatalf("err=%v calls=%d required=%v", err, calls, RequiresNativeResponsesToolHistory(classified.Metadata, body))
	}
	if body[0] != '{' || opts.OriginalRequest[0] != '{' || metadata[nativeResponsesToolHistoryRequiredMetadataKey] != nil {
		t.Fatal("preflight mutated caller body or metadata")
	}
	if _, err := classifyNativeResponsesToolHistory(ctx, []string{"codex"}, cliproxyexecutor.Request{Payload: body}, opts, true, false); err != nil || calls != 1 {
		t.Fatalf("count must not run translation or require capability: err=%v calls=%d", err, calls)
	}
}

func TestNativeResponsesPreflightMissingTranslatorPreservesRegistryFallback(t *testing.T) {
	ctx := sdktranslator.ContextWithRegistry(context.Background(), sdktranslator.NewRegistry())
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}}
	req := cliproxyexecutor.Request{Payload: []byte(`{"messages":[{"role":"user","content":"source"}]}`)}
	_, err := classifyNativeResponsesToolHistory(ctx, []string{"codex"}, req, opts, false, false)
	if err != nil {
		t.Fatalf("preflight added a new global error instead of preserving registry fallback: %v", err)
	}
	if _, err := classifyNativeResponsesToolHistory(ctx, []string{"claude"}, req, opts, false, false); err != nil {
		t.Fatalf("non-Codex route must not require a Codex translator: %v", err)
	}
}

type nativeResponsesPreflightPlugin struct {
	sdktranslator.PluginHooks
	body  []byte
	calls int
}

func (plugin *nativeResponsesPreflightPlugin) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	return body
}

func (plugin *nativeResponsesPreflightPlugin) TranslateRequest(_ context.Context, _, _ sdktranslator.Format, _ string, _ []byte, _ bool) ([]byte, bool) {
	plugin.calls++
	return plugin.body, true
}

func TestNativeResponsesPreflightUsesPluginOnlyTranslator(t *testing.T) {
	registry := sdktranslator.NewRegistry()
	input := make([]map[string]string, 240)
	for i := range input {
		input[i] = map[string]string{"type": "function_call", "name": "test", "arguments": "{}"}
	}
	body, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		t.Fatal(err)
	}
	plugin := &nativeResponsesPreflightPlugin{body: body}
	registry.SetPluginHooks(plugin)
	if registry.HasRequestTransformer(sdktranslator.FormatClaude, sdktranslator.FormatCodex) {
		t.Fatal("fixture must exercise a plugin-only request transform")
	}
	ctx := sdktranslator.ContextWithRegistry(context.Background(), registry)
	for index, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatCodex, ""} {
		opts := cliproxyexecutor.Options{SourceFormat: format, Metadata: map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}}
		req := cliproxyexecutor.Request{Payload: []byte(`{"messages":[{"role":"user","content":"source"}]}`)}
		classified, err := classifyNativeResponsesToolHistory(ctx, []string{"codex"}, req, opts, false, false)
		if err != nil || plugin.calls != index+1 || !RequiresNativeResponsesToolHistory(classified.Metadata, req.Payload) {
			t.Fatalf("plugin-only conversion missed for %s: err=%v calls=%d", format, err, plugin.calls)
		}
	}
}

func TestNativeResponsesPreflightMissingCodexTranslatorKeepsMixedProviderAvailable(t *testing.T) {
	const model = "gpt-5.6-sol"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 5)
	manager.SetTranslatorRegistry(sdktranslator.NewRegistry())
	claude := &authFallbackExecutor{id: "claude"}
	codex := &authFallbackExecutor{id: "codex"}
	manager.RegisterExecutor(claude)
	manager.RegisterExecutor(codex)
	registerGPTChannelFailoverAuths(t, manager, "claude", model, []*Auth{{ID: t.Name(), Provider: "claude"}})
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}}
	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"source"}]}`)}
	_, err := manager.Execute(context.Background(), []string{"codex", "claude"}, req, opts)
	if err != nil || len(claude.ExecuteCalls()) != 1 || len(codex.ExecuteCalls()) != 0 {
		t.Fatalf("Codex preflight suppressed healthy non-Codex candidate: err=%v claude=%v codex=%v", err, claude.ExecuteCalls(), codex.ExecuteCalls())
	}
}

func TestNativeResponsesPreflightDoesNotTrustNegativeHint(t *testing.T) {
	opts := longResponsesToolOptions()
	opts.Metadata[nativeResponsesToolHistoryRequiredMetadataKey] = false
	if !RequiresNativeResponsesToolHistory(opts.Metadata, nil) {
		t.Fatal("negative preflight hint bypassed raw shape guard")
	}
}

func TestNativeResponsesPreflightDoesNotReclassifyAnAlreadyRequiredHistory(t *testing.T) {
	ctx := sdktranslator.ContextWithRegistry(context.Background(), sdktranslator.NewRegistry())
	opts := longResponsesToolOptions()
	opts.SourceFormat = sdktranslator.FormatClaude
	got, err := classifyNativeResponsesToolHistory(ctx, []string{"codex"}, cliproxyexecutor.Request{Payload: []byte(`{"messages":[]}`)}, opts, false, false)
	if err != nil || !RequiresNativeResponsesToolHistory(got.Metadata, nil) {
		t.Fatalf("known long history needs no redundant conversion: %v", err)
	}
}

func TestNativeResponsesPreflightMeasuresTranslatedByteAndToolSurface(t *testing.T) {
	registry := sdktranslator.NewRegistry()
	input := make([]map[string]string, 240)
	for i := range input {
		input[i] = map[string]string{"role": "user", "content": "test"}
	}
	converted, err := json.Marshal(map[string]any{"input": input, "tools": []any{map[string]string{"type": "function", "name": "test"}}, "padding": strings.Repeat("x", 2<<20)})
	if err != nil {
		t.Fatal(err)
	}
	registry.Register(sdktranslator.FormatClaude, sdktranslator.FormatCodex, func(_ string, _ []byte, _ bool) []byte { return converted }, sdktranslator.ResponseTransform{})
	ctx := sdktranslator.ContextWithRegistry(context.Background(), registry)
	// Keep the source below the byte threshold while allowing normal translation
	// overhead. It has one tool; only the translated complex surface crosses it.
	body := []byte(`{"messages":[],"padding":"` + strings.Repeat("x", (2<<20)-100000) + `"}`)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}}
	classified, err := classifyNativeResponsesToolHistory(ctx, []string{"codex"}, cliproxyexecutor.Request{Payload: body}, opts, false, false)
	if err != nil || !RequiresNativeResponsesToolHistory(classified.Metadata, body) {
		t.Fatalf("translated complex byte threshold missed: %v", err)
	}
}

func TestNativeResponsesPreflightRunsOnceAcrossManagerFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			const model = "gpt-5.6-sol"
			registry := sdktranslator.NewRegistry()
			var translations int
			input := make([]map[string]string, 240)
			for i := range input {
				input[i] = map[string]string{"type": "function_call", "name": "test", "arguments": "{}"}
			}
			translated, err := json.Marshal(map[string]any{"input": input})
			if err != nil {
				t.Fatal(err)
			}
			registry.Register(sdktranslator.FormatClaude, sdktranslator.FormatCodex, func(_ string, _ []byte, _ bool) []byte {
				translations++
				return translated
			}, sdktranslator.ResponseTransform{})
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.SetTranslatorRegistry(registry)
			manager.SetRetryConfig(0, 0, 5)
			first, second := t.Name()+"-first", t.Name()+"-second"
			executor := &authFallbackExecutor{id: "codex", executeErrors: map[string]error{first: retryableGPTChannelFailure(429)}, streamFirstErrors: map[string]error{first: retryableGPTChannelFailure(429)}}
			manager.RegisterExecutor(executor)
			registerGPTChannelFailoverAuths(t, manager, "codex", model, []*Auth{
				{ID: first, Provider: "codex", Attributes: map[string]string{"base_url": "https://first.example/v1", "native_responses": "true", "priority": "2"}},
				{ID: second, Provider: "codex", Attributes: map[string]string{"base_url": "https://second.example/v1", "native_responses": "true", "priority": "1"}},
			})
			body := []byte(`{"messages":[{"role":"user","content":"source"}]}`)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body, Metadata: map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}}
			req := cliproxyexecutor.Request{Model: model, Payload: body}
			if stream {
				var result *cliproxyexecutor.StreamResult
				result, err = manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = manager.Execute(context.Background(), []string{"codex"}, req, opts)
			}
			if err != nil || translations != 1 {
				t.Fatalf("err=%v translations=%d, want one request preflight across failover", err, translations)
			}
		})
	}
}
