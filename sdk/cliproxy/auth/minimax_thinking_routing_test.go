package auth

import (
	"reflect"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestSonnetFiltersMandatoryMiniMaxThinkingBeforeExecution(t *testing.T) {
	for _, tc := range []struct{ format, body string }{
		{"openai", `{"reasoning_effort":"none"}`},
		{"claude", `{"thinking":{"type":"disabled"},"output_config":{"effort":"low"}}`},
		{"openai-response", `{"reasoning":{"effort":"none"}}`},
	} {
		candidates := []string{"MiniMax-M3.1", "MiniMax-M3.1-flash", "MiniMax-M3.1-Flash-Preview", "MiniMax-M2.7", "MiniMax-M3", "other-model"}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.format), OriginalRequest: []byte(tc.body)}
		req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"reasoning_effort":"low"}`)}
		got := filterMiniMaxM3RequiredExecutionModels(req.Model, req, opts, candidates)
		if !reflect.DeepEqual(got, []string{"MiniMax-M3", "other-model"}) || len(candidates) != 6 {
			t.Fatalf("unsupported thinking candidate remained: %v", got)
		}
		if got := filterClaudeSonnetMiniMaxThinking("MiniMax-M3.1", req, opts, candidates); !reflect.DeepEqual(got, candidates) {
			t.Fatal("direct model validation changed")
		}
		if got := filterClaudeSonnetMiniMaxThinking(req.Model, req, opts, []string{"MiniMax-M3.1(max)"}); len(got) != 1 {
			t.Fatal("explicit candidate suffix precedence changed")
		}
		if got := filterClaudeSonnetMiniMaxThinking(req.Model, req, opts, []string{"MiniMax-M3.1"}); len(got) != 0 {
			t.Fatal("incompatible single candidate was retained")
		}
	}
	for _, body := range []string{`{}`, `{"reasoning_effort":"low"}`, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`} {
		models := []string{"MiniMax-M3.1", "MiniMax-M2.7", "MiniMax-M3"}
		got := filterClaudeSonnetMiniMaxThinking("claude-sonnet-4-6", cliproxyexecutor.Request{Payload: []byte(body)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}, models)
		if !reflect.DeepEqual(got, models) {
			t.Fatal("enabled/default thinking candidate excluded", got)
		}
	}
}
