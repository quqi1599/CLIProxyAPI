package helps

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestValidateLegacyResponsesCompaction(t *testing.T) {
	t.Parallel()
	valid := []byte(`{"id":"resp_1","output":[{"type":"message","role":"user","content":[]},{"type":"compaction","encrypted_content":"opaque"}]}`)
	if err := ValidateLegacyResponsesCompaction(valid); err != nil {
		t.Fatalf("ValidateLegacyResponsesCompaction(valid) error = %v", err)
	}
	for name, body := range map[string][]byte{
		"missing":    []byte(`{"output":[{"type":"message","role":"user"}]}`),
		"duplicate":  []byte(`{"output":[{"type":"compaction","encrypted_content":"a"},{"type":"compaction","encrypted_content":"b"}]}`),
		"empty":      []byte(`{"output":[{"type":"compaction","encrypted_content":""}]}`),
		"non-string": []byte(`{"output":[{"type":"compaction","encrypted_content":123}]}`),
		"ordering":   []byte(`{"output":[{"type":"compaction","encrypted_content":"a"},{"type":"message","role":"user"}]}`),
	} {
		name := name
		body := body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ValidateLegacyResponsesCompaction(body)
			failure, ok := failurecontract.As(err)
			if !ok || failure.ErrorCode() != "invalid_compaction_response" {
				t.Fatalf("error = %T %v, want invalid_compaction_response", err, err)
			}
		})
	}
}

func TestBuildLegacyResponsesCompactionRequest(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt-5","stream":true,"previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	got, err := BuildLegacyResponsesCompactionRequest(body)
	if err != nil {
		t.Fatalf("BuildLegacyResponsesCompactionRequest() error = %v", err)
	}
	if gjson.GetBytes(got, "stream").Exists() || gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("bridge-only fields survived: %s", got)
	}
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 1 || input[0].Get("type").String() != "message" {
		t.Fatalf("input = %s, want transcript without trigger", gjson.GetBytes(got, "input").Raw)
	}

	_, err = BuildLegacyResponsesCompactionRequest([]byte(`{"previous_response_id":"resp_old","input":[{"type":"compaction_trigger"}]}`))
	failure, ok := failurecontract.As(err)
	if !ok || failure.ErrorCode() != "compaction_context_unavailable" {
		t.Fatalf("trigger-only error = %T %v", err, err)
	}
}

func TestBuildAndValidateResponsesCompactionTriggerStream(t *testing.T) {
	t.Parallel()
	frames, err := BuildResponsesCompactionTriggerStream([]byte(`{"id":"resp_1","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":3}}`), "gpt-5")
	if err != nil {
		t.Fatalf("BuildResponsesCompactionTriggerStream() error = %v", err)
	}
	stream := bytes.Join(frames, nil)
	if err = ValidateResponsesCompactionStream(stream); err != nil {
		t.Fatalf("ValidateResponsesCompactionStream() error = %v\n%s", err, stream)
	}
	if !bytes.Contains(stream, []byte(`"encrypted_content":"opaque"`)) || !bytes.Contains(stream, []byte("event: response.completed")) {
		t.Fatalf("generated stream lost compaction semantics: %s", stream)
	}
}

func TestWrapResponsesCompactionStreamWithholdsInvalidOutput(t *testing.T) {
	t.Parallel()
	upstream := make(chan cliproxyexecutor.StreamChunk, 1)
	upstream <- cliproxyexecutor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n\n")}
	close(upstream)
	wrapped := WrapResponsesCompactionStream(context.Background(), &cliproxyexecutor.StreamResult{Chunks: upstream})
	chunks := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for chunk := range wrapped.Chunks {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || chunks[0].Err == nil || len(chunks[0].Payload) != 0 {
		t.Fatalf("chunks = %+v, want one validation error and no payload", chunks)
	}
	failure, ok := failurecontract.As(chunks[0].Err)
	if !ok || failure.ErrorCode() != "invalid_compaction_response" {
		t.Fatalf("error = %T %v", chunks[0].Err, chunks[0].Err)
	}
}

func TestWrapResponsesCompactionStreamNormalizesBufferedErrorBeforeOutput(t *testing.T) {
	t.Parallel()
	upstream := make(chan cliproxyexecutor.StreamChunk, 2)
	upstream <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_item.added"}`)}
	upstream <- cliproxyexecutor.StreamChunk{Err: &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: 502, StreamPhase: failurecontract.StreamPhaseAfterOutput, OutputCommitted: true,
		ProviderCode: "compaction_upstream_error", SemanticCode: "compaction_upstream_error",
	}}
	close(upstream)

	wrapped := WrapResponsesCompactionStream(context.Background(), &cliproxyexecutor.StreamResult{Chunks: upstream})
	chunk := <-wrapped.Chunks
	if len(chunk.Payload) != 0 || chunk.Err == nil {
		t.Fatalf("chunk = %+v, want error without payload", chunk)
	}
	failure, ok := failurecontract.As(chunk.Err)
	if !ok || failure.StreamPhase != failurecontract.StreamPhaseBeforeOutput || failure.OutputCommitted {
		t.Fatalf("failure = %#v, want before-output error", failure)
	}
}

func TestValidateResponsesCompactionStreamRejectsTerminalSequenceViolations(t *testing.T) {
	t.Parallel()
	completed := `{"type":"response.completed","sequence_number":2,"response":{"status":"completed","output":[{"type":"compaction","encrypted_content":"opaque"}]}}`
	for name, stream := range map[string]string{
		"duplicate completed":   "data: " + completed + "\n\ndata: " + completed + "\n\n",
		"event after completed": "data: " + completed + "\n\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3}\n\n",
		"failed":                "data: {\"type\":\"response.failed\",\"sequence_number\":1}\n\n",
		"malformed data":        "data: not-json\n\ndata: " + completed + "\n\n",
		"event type mismatch":   "event: response.failed\ndata: " + completed + "\n\n",
	} {
		name := name
		stream := stream
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ValidateResponsesCompactionStream([]byte(stream))
			failure, ok := failurecontract.As(err)
			if !ok || failure.ErrorCode() != "invalid_compaction_stream" {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

func TestValidateResponsesCompactionStreamAcceptsRawWebsocketEvents(t *testing.T) {
	t.Parallel()
	stream := []byte("{\"type\":\"response.created\",\"sequence_number\":0}\n" +
		"{\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}}\n" +
		"{\"type\":\"response.done\",\"sequence_number\":2,\"response\":{\"status\":\"completed\",\"output\":[]}}")
	if err := ValidateResponsesCompactionStream(stream); err != nil {
		t.Fatalf("ValidateResponsesCompactionStream(raw websocket) error = %v", err)
	}
}

func TestResponsesCompactionValidationFieldsDoNotExposeEncryptedContent(t *testing.T) {
	t.Parallel()
	const secret = "opaque-secret-that-must-not-be-logged"
	data := []byte(`{"output":[{"type":"compaction","encrypted_content":"` + secret + `"}]}`)
	fields := responsesCompactionValidationFields("legacy_json", data, nil)
	if got := fields["compaction_item_count"]; got != 1 {
		t.Fatalf("compaction_item_count = %v, want 1", got)
	}
	if got := fields["encrypted_content_bytes"]; got != len(secret) {
		t.Fatalf("encrypted_content_bytes = %v, want %d", got, len(secret))
	}
	if strings.Contains(fmt.Sprint(fields), secret) {
		t.Fatalf("validation fields leaked encrypted content: %v", fields)
	}
}
