package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAggregateSonnetToGPTThinkingUsesSourceIntent(t *testing.T) {
	for _, tc := range []struct{ name, controls, suffix, want string }{
		{name: "upstream default"},
		{name: "adaptive default", controls: `,"thinking":{"type":"adaptive"}`},
		{name: "enabled default", controls: `,"thinking":{"type":"enabled"}`},
		{name: "standalone low", controls: `,"output_config":{"effort":"low"}`, want: "low"},
		{name: "standalone max", controls: `,"output_config":{"effort":"max"}`, want: "max"},
		{name: "explicit xhigh", controls: `,"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}`, want: "xhigh"},
		{name: "manual budget", controls: `,"thinking":{"type":"enabled","budget_tokens":8192}`, want: "medium"},
		{name: "explicit disabled", controls: `,"thinking":{"type":"disabled"},"output_config":{"effort":"max"}`, want: "none"},
		{name: "suffix wins", controls: `,"output_config":{"effort":"low"}`, suffix: "(high)", want: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]` + tc.controls + `}`)
			req := cliproxyexecutor.Request{Model: "gpt-6-luna" + tc.suffix, Payload: body}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body, Metadata: map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "claude-sonnet-4-6" + tc.suffix}}
			for _, mode := range []codexRequestPlanMode{codexRequestPlanExecute, codexRequestPlanStream, codexRequestPlanCount} {
				plan, err := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}).prepareCodexRequestPlan(context.Background(), nil, req, opts, "gpt-6-luna", mode)
				if err != nil {
					t.Fatalf("codex mode %d: %v", mode, err)
				}
				if got := gjson.GetBytes(plan.body, "reasoning.effort").String(); got != tc.want {
					t.Fatalf("codex mode %d effort = %q, want %q", mode, got, tc.want)
				}
				if got := gjson.GetBytes(plan.body, "model").String(); got != "gpt-6-luna" {
					t.Fatalf("upstream model = %q", got)
				}
			}
			plan, err := NewOpenAICompatExecutor("aggregate-test", &config.Config{}).prepareOpenAICompatRequest(context.Background(), nil, req, opts, "https://example.com/v1", "gpt-6-luna", genericOpenAICompatProfile(), true)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(plan.body, "reasoning_effort").String(); got != tc.want {
				t.Fatalf("chat effort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAggregateSonnetToGPTUsesFinalRegisteredCapabilities(t *testing.T) {
	const upstream = "gpt-6-luna"
	reg := registry.GetGlobalRegistry()
	// Synthetic capability metadata tests route semantics, not official model specs.
	reg.RegisterClient(t.Name(), "codex", []*registry.ModelInfo{{ID: upstream, UserDefined: true, Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}}})
	t.Cleanup(func() { reg.UnregisterClient(t.Name()) })
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hello"}]}`)
	plan, err := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}).prepareCodexRequestPlan(context.Background(), nil, cliproxyexecutor.Request{Model: upstream, Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body}, upstream, codexRequestPlanStream)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(plan.body, "reasoning.effort").String(); got != "high" {
		t.Fatalf("final-model effort = %q, want high", got)
	}
}

func TestAggregateSonnetToGPTPreservesToolHistory(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior provider reasoning","signature":"claude-signature"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"city":"Shanghai"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"},{"type":"text","text":"continue"}]}]}`)
	plan, err := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}).prepareCodexRequestPlan(context.Background(), nil, cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body}, "gpt-6-luna", codexRequestPlanStream)
	if err != nil {
		t.Fatal(err)
	}
	calls, outputs := 0, 0
	for _, item := range gjson.GetBytes(plan.body, "input").Array() {
		switch item.Get("type").String() {
		case "reasoning":
			t.Fatalf("foreign reasoning replayed: %s", item.Raw)
		case "function_call":
			calls++
			if item.Get("call_id").String() != "call_1" || item.Get("name").String() != "lookup" || gjson.Get(item.Get("arguments").String(), "city").String() != "Shanghai" {
				t.Fatalf("tool call corrupted: %s", item.Raw)
			}
		case "function_call_output":
			outputs++
			if item.Get("call_id").String() != "call_1" || item.Get("output").String() != "sunny" {
				t.Fatalf("tool result corrupted: %s", item.Raw)
			}
		}
	}
	if calls != 1 || outputs != 1 {
		t.Fatalf("tool history calls=%d results=%d", calls, outputs)
	}
}
