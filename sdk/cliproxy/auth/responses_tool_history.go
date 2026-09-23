package auth

import (
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// RequiresNativeResponsesToolHistory describes the request shape protected by
// both routing and execution. Retry-budget thresholds are intentionally separate:
// a long plain-text conversation alone is not a tool compatibility failure.
func RequiresNativeResponsesToolHistory(metadata map[string]any, body []byte) bool {
	switch strings.ToLower(metadataString(metadata, cliproxyexecutor.ClientProfileMetadataKey)) {
	case "workbuddy", "codex", "codex_cli", "codex_tui":
	default:
		return false
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
