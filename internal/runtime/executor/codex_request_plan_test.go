package executor

import (
	"bytes"
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexResponsesRequestPlanStreamParity(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	payload := []byte(`{"model":"gpt-5","instructions":null,"previous_response_id":"resp-old","prompt_cache_retention":"24h","safety_identifier":"safe","stream_options":{"include_usage":true},"input":[{"id":"msg-1","type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	original := bytes.Clone(payload)
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		ResponseFormat:  sdktranslator.FromString("openai-response"),
		OriginalRequest: original,
	}

	nonStream, err := executor.prepareCodexResponsesRequestPlan(context.Background(), nil, req, opts, "gpt-5", false)
	if err != nil {
		t.Fatalf("prepare non-stream plan: %v", err)
	}
	stream, err := executor.prepareCodexResponsesRequestPlan(context.Background(), nil, req, opts, "gpt-5", true)
	if err != nil {
		t.Fatalf("prepare stream plan: %v", err)
	}

	if !bytes.Equal(nonStream.body, stream.body) {
		t.Fatalf("stream/non-stream request plan drift:\nnon-stream=%s\nstream=%s", nonStream.body, stream.body)
	}
	if nonStream.from != opts.SourceFormat || nonStream.to != sdktranslator.FormatCodex || nonStream.responseFormat != opts.ResponseFormat {
		t.Fatalf("unexpected plan formats: from=%q to=%q response=%q", nonStream.from, nonStream.to, nonStream.responseFormat)
	}
	if !bytes.Equal(nonStream.originalPayloadSource, original) {
		t.Fatalf("original payload source changed: got %s want %s", nonStream.originalPayloadSource, original)
	}
	if !gjson.GetBytes(nonStream.body, "stream").Bool() || gjson.GetBytes(nonStream.body, "model").String() != "gpt-5" {
		t.Fatalf("model or stream normalization missing: %s", nonStream.body)
	}
	for _, path := range []string{"previous_response_id", "prompt_cache_retention", "safety_identifier", "stream_options", "input.0.id"} {
		if gjson.GetBytes(nonStream.body, path).Exists() {
			t.Fatalf("stateless request still contains %s: %s", path, nonStream.body)
		}
	}
	if instructions := gjson.GetBytes(nonStream.body, "instructions"); instructions.Type != gjson.String || instructions.String() != "" {
		t.Fatalf("instructions not normalized to empty string: %s", nonStream.body)
	}
}
