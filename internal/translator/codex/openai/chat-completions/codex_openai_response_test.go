package chat_completions

import (
	"context"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAI_StreamIncludesCachedTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	created := []byte(`data: {"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.2-codex"}}`)
	if out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.2-codex", nil, nil, created, &param); len(out) != 0 {
		t.Fatalf("response.created should not emit chunks, got %d", len(out))
	}

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.2-codex","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":64},"output_tokens_details":{"reasoning_tokens":7}}}}`)
	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.2-codex", nil, nil, completed, &param)
	if len(out) != 1 {
		t.Fatalf("response.completed should emit one chunk, got %d", len(out))
	}

	chunk := gjson.ParseBytes(out[0])
	if got := chunk.Get("usage.prompt_tokens_details.cached_tokens").Int(); got != 64 {
		t.Fatalf("cached_tokens mismatch: got %d, want %d", got, 64)
	}
	if got := chunk.Get("usage.completion_tokens_details.reasoning_tokens").Int(); got != 7 {
		t.Fatalf("reasoning_tokens mismatch: got %d, want %d", got, 7)
	}
}

func TestConvertCodexResponseToOpenAINonStreamIncludesCachedTokens(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_2","created_at":1700000001,"model":"gpt-5.2-codex","status":"completed","usage":{"input_tokens":88,"output_tokens":12,"total_tokens":100,"input_tokens_details":{"cached_tokens":33}},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)

	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.2-codex", nil, nil, raw, nil)
	if len(out) == 0 {
		t.Fatalf("expected non-empty response")
	}

	resp := gjson.ParseBytes(out)
	if got := resp.Get("usage.prompt_tokens_details.cached_tokens").Int(); got != 33 {
		t.Fatalf("cached_tokens mismatch: got %d, want %d", got, 33)
	}
}

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.GetBytes(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallChunkOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls").Exists() {
		t.Fatalf("expected tool_calls to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_ToolCallArgumentsDeltaOmitsNullContentFields(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"websearch"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected tool call announcement chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"query\":\"OpenAI\"}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	if gjson.GetBytes(out[0], "choices.0.delta.content").Exists() {
		t.Fatalf("expected content to be omitted, got %s", string(out[0]))
	}
	if gjson.GetBytes(out[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("expected reasoning_content to be omitted, got %s", string(out[0]))
	}
	if !gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.arguments").Exists() {
		t.Fatalf("expected tool call arguments delta to exist, got %s", string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_DropsArgumentsDeltaWithoutToolAnnouncement(t *testing.T) {
	ctx := internallogging.WithToolStreamRepairTracking(context.Background())
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"cmd\":\"pwd\"}"}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected orphan arguments delta to be dropped, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"cmd\":\"pwd\"}"}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected orphan arguments done to be dropped, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_123","name":"Bash","arguments":"{\"cmd\":\"pwd\"}"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected fallback tool call chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("tool call index = %d, want 0; chunk=%s", got, string(out[0]))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "Bash" {
		t.Fatalf("tool call name = %q, want Bash; chunk=%s", got, string(out[0]))
	}
	stats := internallogging.GetToolStreamRepairStats(ctx)
	if stats.OrphanToolDeltaDroppedCount != 1 || stats.ToolDoneFallbackEmittedCount != 1 {
		t.Fatalf("repair stats = %+v, want orphan=1 fallback=1", stats)
	}
}

func TestConvertCodexResponseToOpenAI_InvalidToolAnnouncementFallsBackToDone(t *testing.T) {
	ctx := internallogging.WithToolStreamRepairTracking(context.Background())
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_123","name":""}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected invalid tool announcement to be dropped, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"query\":\"OpenAI\"}"}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected arguments delta for invalid announcement to be dropped, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_123","name":"websearch","arguments":"{\"query\":\"OpenAI\"}"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected fallback tool call chunk, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "websearch" {
		t.Fatalf("tool call name = %q, want websearch; chunk=%s", got, string(out[0]))
	}
	stats := internallogging.GetToolStreamRepairStats(ctx)
	if stats.InvalidToolAnnouncementDroppedCount != 1 || stats.ToolDoneFallbackEmittedCount != 1 {
		t.Fatalf("repair stats = %+v, want invalid=1 fallback=1", stats)
	}
}

func TestConvertCodexResponseToOpenAI_ToolArgumentDeltasUseOutputIndex(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":3,"item":{"type":"function_call","call_id":"call_a","name":"first"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected first tool call announcement, got %d", len(out))
	}
	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.added","output_index":5,"item":{"type":"function_call","call_id":"call_b","name":"second"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected second tool call announcement, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","output_index":3,"delta":"{\"a\":1}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected first tool call arguments delta, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("tool call index = %d, want 0; chunk=%s", got, string(out[0]))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.function_call_arguments.delta","output_index":5,"delta":"{\"b\":2}"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected second tool call arguments delta, got %d", len(out))
	}
	if got := gjson.GetBytes(out[0], "choices.0.delta.tool_calls.0.index").Int(); got != 1 {
		t.Fatalf("tool call index = %d, want 1; chunk=%s", got, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_StreamPartialImageEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out[0]))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate image chunk to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToOpenAI_StreamImageGenerationCallDoneEmitsDeltaImages(t *testing.T) {
	ctx := context.Background()
	var param any

	out := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected output_item.done to be suppressed when identical to last partial image, got %d", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", nil, nil, []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotURL := gjson.GetBytes(out[0], "choices.0.delta.images.0.image_url.url").String()
	if gotURL != "data:image/jpeg;base64,Ymll" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/jpeg;base64,Ymll", gotURL, string(out[0]))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamImageGenerationCallAddsMessageImages(t *testing.T) {
	ctx := context.Background()

	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.4","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.4", nil, nil, raw, nil)

	gotURL := gjson.GetBytes(out, "choices.0.message.images.0.image_url.url").String()
	if gotURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("expected image url %q, got %q; chunk=%s", "data:image/png;base64,aGVsbG8=", gotURL, string(out))
	}
}

func TestConvertCodexResponseToOpenAI_NonStreamMultiMessageEmptyTrailingKeepsContent(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":"the real answer"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking again"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":""}]}` +
		`]}}`)
	out := ConvertCodexResponseToOpenAINonStream(ctx, "gpt-5.5", nil, nil, raw, nil)

	got := gjson.GetBytes(out, "choices.0.message.content")
	if !got.Exists() || got.Type == gjson.Null {
		t.Fatalf("content was dropped to null by trailing empty message; resp=%s", string(out))
	}
	if got.String() != "the real answer" {
		t.Fatalf("expected content %q, got %q; resp=%s", "the real answer", got.String(), string(out))
	}
}
