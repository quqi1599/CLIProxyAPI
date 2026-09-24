package auth

import (
	"bytes"
	"context"
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const nativeResponsesToolHistoryRequiredMetadataKey = "__cliproxy_native_responses_tool_history_required"

const codexToolHistoryPreflightFailureMetadataKey = "__cliproxy_codex_tool_history_preflight_failure"

// Keep a rejected speculative transform local to Codex candidates. A different
// provider may execute the source protocol without this conversion at all.
// The private wrapper cannot be supplied through client JSON metadata.
type codexToolHistoryPreflightFailure struct {
	err error
}

func codexToolHistoryPreflightError(opts cliproxyexecutor.Options) error {
	if opts.TokenCount {
		return nil
	}
	if failure, ok := opts.Metadata[codexToolHistoryPreflightFailureMetadataKey].(*codexToolHistoryPreflightFailure); ok && failure != nil {
		return failure.err
	}
	return nil
}

func codexToolHistorySelectionError(opts cliproxyexecutor.Options) error {
	if err := codexToolHistoryPreflightError(opts); err != nil {
		return err
	}
	return nativeResponsesToolHistorySelectionError()
}

func withCodexToolHistoryMetadata(opts cliproxyexecutor.Options, key string, value any) cliproxyexecutor.Options {
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	metadata[key] = value
	opts.Metadata = metadata
	return opts
}

// classifyNativeResponsesToolHistory runs once before the manager's retry loop.
// Source messages are not Responses items: one Anthropic/Chat message can expand
// to multiple messages, calls and results. Use the request-scoped translator,
// rather than a second approximation of its conversion rules, before selection.
// Only a restrictive result is propagated; false/untrusted metadata can never
// override the actual source or executor body evidence.
func classifyNativeResponsesToolHistory(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, tokenCount, stream bool) (cliproxyexecutor.Options, error) {
	if tokenCount || !isProtectedResponsesToolHistoryClient(opts.Metadata) {
		return opts, nil
	}
	hasCodex := false
	for _, provider := range providers {
		hasCodex = hasCodex || isCodexProviderName(provider)
	}
	if !hasCodex {
		return opts, nil
	}
	bodies := [][]byte{req.Payload}
	if len(opts.OriginalRequest) > 0 && !bytes.Equal(req.Payload, opts.OriginalRequest) {
		bodies = append(bodies, opts.OriginalRequest)
	}
	required := false
	for _, body := range bodies {
		if RequiresNativeResponsesToolHistory(opts.Metadata, body) {
			required = true
			break
		}
	}
	if !required {
		registry := sdktranslator.RegistryFromContext(ctx)
		for _, body := range bodies {
			if len(body) == 0 {
				continue
			}
			// Preserve registry semantics, including plugin-only transforms and
			// passthrough for an unregistered format. Missing conversion is not
			// a new global request error that may suppress non-Codex candidates.
			translated := registry.TranslateRequest(opts.SourceFormat, sdktranslator.FormatCodex,
				thinking.ParseSuffix(req.Model).ModelName, internalpayload.CloneBytes(body), stream)
			if err := internalpayload.EnforceRequestTransform(ctx, "routing.codex.tool_history", int64(len(body)), int64(len(translated)), internalpayload.AmplificationOverride{}); err != nil {
				return withCodexToolHistoryMetadata(opts, codexToolHistoryPreflightFailureMetadataKey, &codexToolHistoryPreflightFailure{err: err}), nil
			}
			if RequiresNativeResponsesToolHistory(opts.Metadata, translated) {
				required = true
				break
			}
		}
	}
	if required {
		opts = withCodexToolHistoryMetadata(opts, nativeResponsesToolHistoryRequiredMetadataKey, true)
	}
	return opts, nil
}

func isProtectedResponsesToolHistoryClient(metadata map[string]any) bool {
	switch strings.ToLower(metadataString(metadata, cliproxyexecutor.ClientProfileMetadataKey)) {
	case "workbuddy", "codex", "codex_cli", "codex_tui":
		return true
	default:
		return false
	}
}

// RequiresNativeResponsesToolHistory describes the request shape protected by
// both routing and execution. Retry-budget thresholds are intentionally separate:
// a long plain-text conversation alone is not a tool compatibility failure.
func RequiresNativeResponsesToolHistory(metadata map[string]any, body []byte) bool {
	if !isProtectedResponsesToolHistoryClient(metadata) {
		return false
	}
	if required, _ := metadata[nativeResponsesToolHistoryRequiredMetadataKey].(bool); required {
		return true
	}
	messages := intMetadataValue(metadata[cliproxyexecutor.MessageCountMetadataKey])
	tools := intMetadataValue(metadata[cliproxyexecutor.ToolCountMetadataKey])
	declared := intMetadataValue(metadata[cliproxyexecutor.DeclaredToolCountMetadataKey])
	interactions := intMetadataValue(metadata[cliproxyexecutor.ToolInteractionCountMetadataKey])
	complex := complexResponsesToolSurface(metadataString(metadata, cliproxyexecutor.ToolShapeTypesMetadataKey))
	// HTTP handlers already provide measured shape counters. Avoid repeatedly
	// scanning a multi-megabyte history when those counters establish the guard.
	if messages >= gptWorkBuddyToolHistoryMessages &&
		(tools >= gptWorkBuddyToolHistoryInteractions || declared >= gptWorkBuddyToolHistoryDeclaredTools || interactions >= gptWorkBuddyToolHistoryInteractions) {
		return true
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		input = gjson.GetBytes(body, "messages")
	}
	if input.IsArray() {
		items := input.Array()
		messages = max(messages, len(items))
		bodyInteractions := 0
		for _, item := range items {
			kind := strings.ToLower(item.Get("type").String())
			if strings.Contains(kind, "tool") || strings.Contains(kind, "function_call") {
				bodyInteractions++
			}
			// SDK callers can submit Chat/Anthropic input before Codex translation.
			// Count structured interactions here as well as Responses input items.
			role := strings.ToLower(item.Get("role").String())
			if role == "tool" || role == "function" {
				bodyInteractions++
			}
			if calls := item.Get("tool_calls"); calls.IsArray() {
				bodyInteractions += len(calls.Array())
			}
			if item.Get("function_call").IsObject() {
				bodyInteractions++
			}
			if content := item.Get("content"); content.IsArray() {
				for _, part := range content.Array() {
					if strings.Contains(strings.ToLower(part.Get("type").String()), "tool") {
						bodyInteractions++
					}
				}
			}
		}
		interactions = max(interactions, bodyInteractions)
	}
	if entries := gjson.GetBytes(body, "tools"); entries.IsArray() {
		items := entries.Array()
		declared = max(declared, len(items))
		tools = max(tools, len(items))
		for _, item := range items {
			complex = complex || complexResponsesToolSurface(item.Get("type").String())
		}
	}
	if messages < gptWorkBuddyToolHistoryMessages {
		return false
	}
	if tools >= gptWorkBuddyToolHistoryInteractions || declared >= gptWorkBuddyToolHistoryDeclaredTools || interactions >= gptWorkBuddyToolHistoryInteractions {
		return true
	}
	payloadBytes := max(len(body), intMetadataValue(metadata[cliproxyexecutor.RequestBodyBytesMetadataKey]))
	return complex && payloadBytes >= 2<<20
}

func complexResponsesToolSurface(toolTypes string) bool {
	toolTypes = strings.ToLower(toolTypes)
	return strings.Contains(toolTypes, "namespace") || strings.Contains(toolTypes, "custom") ||
		strings.Contains(toolTypes, "mcp") || strings.Contains(toolTypes, "function")
}
