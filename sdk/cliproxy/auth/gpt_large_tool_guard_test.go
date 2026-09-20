package auth

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestGPTLargeToolHistoryRecognizesWorkBuddyGPT56Shape(t *testing.T) {
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:          "/v1/responses",
			cliproxyexecutor.ClientProfileMetadataKey:        "workbuddy",
			cliproxyexecutor.MessageCountMetadataKey:         272,
			cliproxyexecutor.ToolCountMetadataKey:            12,
			cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
		},
	}
	if !isGPTLargeToolHistoryResponsesRequest([]string{"codex"}, "gpt-5.6-sol", opts) {
		t.Fatal("expected WorkBuddy GPT-5.6 tool history to be classified as large")
	}
}

func TestPreferGPTNativeResponsesAuths(t *testing.T) {
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:          "/v1/responses",
			cliproxyexecutor.ClientProfileMetadataKey:        "workbuddy",
			cliproxyexecutor.MessageCountMetadataKey:         272,
			cliproxyexecutor.ToolCountMetadataKey:            12,
			cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
		},
	}
	custom := &Auth{ID: "custom", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "key", "base_url": "https://compat.example.com/v1"}}
	native := &Auth{ID: "native", Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
	got, excluded := preferGPTNativeResponsesAuths([]*Auth{custom, native}, []string{"codex"}, "gpt-5.6-sol", opts)
	if excluded != 1 || len(got) != 1 || got[0].ID != native.ID {
		t.Fatalf("got auths=%v excluded=%d, want only native auth", got, excluded)
	}
}

func TestPreferGPTNativeResponsesAuthsKeepsFallbackWhenNoNativeRoute(t *testing.T) {
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:          "/v1/responses",
			cliproxyexecutor.ClientProfileMetadataKey:        "workbuddy",
			cliproxyexecutor.MessageCountMetadataKey:         272,
			cliproxyexecutor.ToolCountMetadataKey:            12,
			cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
		},
	}
	a := &Auth{ID: "a", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "key", "base_url": "https://a.example.com/v1"}}
	b := &Auth{ID: "b", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "key", "base_url": "https://b.example.com/v1"}}
	got, excluded := preferGPTNativeResponsesAuths([]*Auth{a, b}, []string{"codex"}, "gpt-5.6-sol", opts)
	if excluded != 0 || len(got) != 2 {
		t.Fatalf("got auths=%v excluded=%d, want all configured fallbacks", got, excluded)
	}
}

func TestNativeGPTResponsesAuthRejectsCustomOAuthBaseURL(t *testing.T) {
	auth := &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth", "base_url": "https://compat.example.com/v1"}}
	if isNativeGPTResponsesAuth(auth) {
		t.Fatal("custom OAuth base URL must not be treated as a native Responses route")
	}
}
