package auth

import (
	"encoding/json"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSupportsNativeResponsesDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, declaration string
		want                        bool
	}{
		{"default", "", "", true},
		{"official", "https://api.openai.com/v1", "", true},
		{"unknown", "https://custom.example/v1", "", false},
		{"verified_custom", "https://custom.example/v1", "true", true},
		{"official_optout", "https://api.openai.com/v1", "false", false},
		{"malformed_declaration", "https://api.openai.com/v1", "tru", false},
		{"lookalike", "https://api.openai.com.example/v1", "", false},
		{"userinfo", "https://user@api.openai.com/v1", "", false},
		{"not_https", "http://api.openai.com/v1", "", false},
		{"invalid_url", "://invalid", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{Provider: "codex", Attributes: map[string]string{"base_url": tc.endpoint, "native_responses": tc.declaration}}
			if got := SupportsNativeResponses(a); got != tc.want {
				t.Fatalf("capability=%t, want %t", got, tc.want)
			}
		})
	}
	if SupportsNativeResponses(nil) || SupportsNativeResponses(&Auth{Provider: "claude"}) {
		t.Fatal("non-Codex auth must not gain Codex capability")
	}
}

func TestRequiresNativeResponsesToolHistoryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, profile                        string
		messages, tools, interactions, bytes int
		toolTypes                            string
		want                                 bool
	}{
		{"short", "codex", 239, 12, 12, 0, "", false},
		{"boundary", "codex", 240, 0, 8, 0, "", true},
		{"plain_long", "codex", 320, 0, 0, 0, "", false},
		{"small_tools", "workbuddy", 272, 7, 7, 0, "", false},
		{"large_complex", "codex_tui", 240, 1, 1, 2 << 20, "namespace", true},
		{"small_complex", "codex_cli", 240, 1, 1, (2 << 20) - 1, "namespace", false},
		{"unknown_client", "other", 320, 130, 30, 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := map[string]any{
				cliproxyexecutor.ClientProfileMetadataKey:        tc.profile,
				cliproxyexecutor.MessageCountMetadataKey:         tc.messages,
				cliproxyexecutor.ToolCountMetadataKey:            tc.tools,
				cliproxyexecutor.ToolInteractionCountMetadataKey: tc.interactions,
				cliproxyexecutor.RequestBodyBytesMetadataKey:     tc.bytes,
				cliproxyexecutor.ToolShapeTypesMetadataKey:       tc.toolTypes,
			}
			if got := RequiresNativeResponsesToolHistory(meta, nil); got != tc.want {
				t.Fatalf("requires=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestRequiresNativeResponsesToolHistoryBodyAndMetadataAgree(t *testing.T) {
	input := make([]map[string]string, 240)
	for i := range input {
		input[i] = map[string]string{"type": "message", "role": "user", "content": "test"}
	}
	for i := 0; i < 8; i++ {
		input[i] = map[string]string{"type": "function_call_output", "call_id": "test", "output": "ok"}
	}
	body, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}
	if !RequiresNativeResponsesToolHistory(meta, body) {
		t.Fatal("body-only history must be recognized")
	}
	meta[cliproxyexecutor.MessageCountMetadataKey] = 1
	meta[cliproxyexecutor.ToolInteractionCountMetadataKey] = 1
	if !RequiresNativeResponsesToolHistory(meta, body) {
		t.Fatal("understated metadata must not bypass body evidence")
	}
	meta = map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex", cliproxyexecutor.MessageCountMetadataKey: 240}
	body = []byte(`{"tools":[{"type":"namespace"}],"padding":"` + strings.Repeat("x", 2<<20) + `"}`)
	if !RequiresNativeResponsesToolHistory(meta, body) {
		t.Fatal("large complex body must use same routing and executor rule")
	}
}

func TestRequiresNativeResponsesToolHistoryTranslatedInputShapes(t *testing.T) {
	for _, shape := range []string{"chat_calls", "chat_results", "legacy_function", "anthropic_tools"} {
		t.Run(shape, func(t *testing.T) {
			messages := make([]map[string]any, 272)
			for i := range messages {
				messages[i] = map[string]any{"role": "user", "content": "test"}
			}
			for i := 0; i < 8; i++ {
				switch shape {
				case "chat_calls":
					messages[i] = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"type": "function", "function": map[string]any{"name": "test", "arguments": "{}"}}}}
				case "chat_results":
					messages[i] = map[string]any{"role": "tool", "tool_call_id": "test", "content": "ok"}
				case "legacy_function":
					messages[i] = map[string]any{"role": "assistant", "function_call": map[string]any{"name": "test", "arguments": "{}"}}
				case "anthropic_tools":
					messages[i] = map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "name": "test", "input": map[string]any{}}}}
				}
			}
			body, err := json.Marshal(map[string]any{"messages": messages})
			if err != nil {
				t.Fatal(err)
			}
			if !RequiresNativeResponsesToolHistory(map[string]any{cliproxyexecutor.ClientProfileMetadataKey: "codex"}, body) {
				t.Fatal("pretranslation tool history was missed")
			}
		})
	}
}
