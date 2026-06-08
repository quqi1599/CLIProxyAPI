package auth

import "testing"

func TestStreamSummaryFieldsObservePayloadOpenAIUsageAndFinishReason(t *testing.T) {
	var fields streamSummaryFields
	fields.observePayload([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"completion_tokens\":7,\"total_tokens\":12,\"prompt_tokens\":5}}\n"))

	if fields.outputTokens != 7 {
		t.Fatalf("outputTokens = %d, want 7", fields.outputTokens)
	}
	if !fields.outputTokensObserved {
		t.Fatal("outputTokensObserved = false, want true")
	}
	if fields.finishReason != "tool_calls" {
		t.Fatalf("finishReason = %q, want tool_calls", fields.finishReason)
	}
}

func TestStreamSummaryFieldsObservePayloadClaudeStopReason(t *testing.T) {
	var fields streamSummaryFields
	fields.observePayload([]byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n"))

	if fields.outputTokens != 9 {
		t.Fatalf("outputTokens = %d, want 9", fields.outputTokens)
	}
	if !fields.outputTokensObserved {
		t.Fatal("outputTokensObserved = false, want true")
	}
	if fields.finishReason != "end_turn" {
		t.Fatalf("finishReason = %q, want end_turn", fields.finishReason)
	}
}

func TestStreamSummaryFieldsObservePayloadResponsesIncompleteReason(t *testing.T) {
	var fields streamSummaryFields
	fields.observePayload([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"content_filter\"},\"usage\":{\"output_tokens\":3}}}\n"))

	if fields.outputTokens != 3 {
		t.Fatalf("outputTokens = %d, want 3", fields.outputTokens)
	}
	if !fields.outputTokensObserved {
		t.Fatal("outputTokensObserved = false, want true")
	}
	if fields.finishReason != "content_filter" {
		t.Fatalf("finishReason = %q, want content_filter", fields.finishReason)
	}
}

func TestStreamSummaryFieldsObservePayloadSkipsIrrelevantChunk(t *testing.T) {
	var fields streamSummaryFields
	fields.observePayload([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n"))

	if fields.outputTokens != 0 {
		t.Fatalf("outputTokens = %d, want 0", fields.outputTokens)
	}
	if fields.outputTokensObserved {
		t.Fatal("outputTokensObserved = true, want false")
	}
	if fields.finishReason != "" {
		t.Fatalf("finishReason = %q, want empty", fields.finishReason)
	}
}

func TestStreamSummaryFieldsObservePayloadSkipsNullFinishReasonChunk(t *testing.T) {
	var fields streamSummaryFields
	fields.observePayload([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n"))

	if fields.outputTokens != 0 {
		t.Fatalf("outputTokens = %d, want 0", fields.outputTokens)
	}
	if fields.outputTokensObserved {
		t.Fatal("outputTokensObserved = true, want false")
	}
	if fields.finishReason != "" {
		t.Fatalf("finishReason = %q, want empty", fields.finishReason)
	}
}

func TestStreamSummaryFieldsObservePayloadKeepsUsageWhenFinishReasonIsNull(t *testing.T) {
	var fields streamSummaryFields
	fields.observePayload([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":null}],\"usage\":{\"completion_tokens\":11,\"total_tokens\":19,\"prompt_tokens\":8}}\n"))

	if fields.outputTokens != 11 {
		t.Fatalf("outputTokens = %d, want 11", fields.outputTokens)
	}
	if !fields.outputTokensObserved {
		t.Fatal("outputTokensObserved = false, want true")
	}
	if fields.finishReason != "" {
		t.Fatalf("finishReason = %q, want empty", fields.finishReason)
	}
}

func TestShouldObserveSummaryPayloadSkipsNullFinishReasonOnly(t *testing.T) {
	payload := []byte("{\"choices\":[{\"finish_reason\": null}]}")
	if shouldObserveSummaryPayload(payload) {
		t.Fatal("shouldObserveSummaryPayload() = true, want false for null finish_reason only")
	}
}

func TestShouldObserveSummaryPayloadObservesNonNullFinishReason(t *testing.T) {
	payload := []byte("{\"choices\":[{\"finish_reason\":\"stop\"}]}")
	if !shouldObserveSummaryPayload(payload) {
		t.Fatal("shouldObserveSummaryPayload() = false, want true for non-null finish_reason")
	}
}
