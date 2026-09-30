package auth

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestMiniMaxM31RemainsEligibleForMultimodalAndLongContext(t *testing.T) {
	for _, model := range []string{"MiniMax-M3.1", "MiniMax-M3.1-flash", "MiniMax-M3.1-Flash-Preview"} {
		for _, payload := range []string{
			`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}]}`,
			`{"messages":[{"role":"user","content":"long context"}],"max_tokens":200000}`,
		} {
			got := filterMiniMaxM3RequiredExecutionModels("claude-sonnet-4-6", cliproxyexecutor.Request{Payload: []byte(payload)}, cliproxyexecutor.Options{}, []string{"MiniMax-M2.7", model})
			if len(got) != 1 || got[0] != model {
				t.Fatalf("eligible models = %v, want %s", got, model)
			}
		}
	}
}
