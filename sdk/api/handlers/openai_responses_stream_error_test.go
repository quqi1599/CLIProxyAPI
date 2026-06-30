package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestBuildOpenAIResponsesStreamErrorChunk(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusInternalServerError, "unexpected EOF", 0)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	if payload["code"] != "upstream_error" {
		t.Fatalf("code = %v, want %q", payload["code"], "upstream_error")
	}
	if payload["message"] != "上游模型通道临时异常或超时。系统已尝试可用通道后仍失败，请稍后重试或切换模型；这通常不是提示词格式问题。" {
		t.Fatalf("message = %v", payload["message"])
	}
	if payload["sequence_number"] != float64(0) {
		t.Fatalf("sequence_number = %v, want %v", payload["sequence_number"], 0)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkExtractsHTTPErrorBody(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(
		http.StatusInternalServerError,
		`{"error":{"message":"oops","type":"server_error","code":"internal_server_error"}}`,
		0,
	)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	if payload["code"] != "upstream_error" {
		t.Fatalf("code = %v, want %q", payload["code"], "upstream_error")
	}
	if payload["message"] != "上游模型通道临时异常或超时。系统已尝试可用通道后仍失败，请稍后重试或切换模型；这通常不是提示词格式问题。" {
		t.Fatalf("message = %v", payload["message"])
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkNormalizesContextWindowJSON(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(
		http.StatusBadRequest,
		`{"error":{"message":"invalid params, context window exceeds limit (2013)","type":"bad_request_error"},"sequence_number":7}`,
		0,
	)

	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	if payload["code"] != contextWindowExceededErrorCode {
		t.Fatalf("code = %v, want %q", payload["code"], contextWindowExceededErrorCode)
	}
	if payload["message"] != UserFacingContextWindowMessage() {
		t.Fatalf("message = %v, want %q", payload["message"], UserFacingContextWindowMessage())
	}
	if payload["sequence_number"] != float64(7) {
		t.Fatalf("sequence_number = %v, want %v", payload["sequence_number"], 7)
	}
}
