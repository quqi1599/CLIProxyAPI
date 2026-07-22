package claude

import (
	"bytes"
	"compress/gzip"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestDecodeClaudeCodeResponseBoundsUnexpectedGzip(t *testing.T) {
	payload := []byte(`{"type":"message","content":"ok"}`)
	compressed := gzipClaudeCodeResponse(t, payload)
	decoded, err := decodeClaudeCodeResponse(compressed, int64(len(payload)))
	if err != nil {
		t.Fatalf("decodeClaudeCodeResponse() error = %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded = %q, want %q", decoded, payload)
	}

	oversized := gzipClaudeCodeResponse(t, bytes.Repeat([]byte("x"), 65))
	if _, err = decodeClaudeCodeResponse(oversized, 64); err == nil {
		t.Fatal("oversized error = nil, want decoded size failure")
	}
	if typed, ok := failurecontract.As(err); !ok || typed.Kind != failurecontract.UpstreamProtocolError || typed.ProviderCode != "upstream_success_body_too_large" {
		t.Fatalf("oversized error = %#v, want upstream_success_body_too_large", typed)
	}
}

func TestDecodeClaudeCodeResponseRejectsConcatenatedGzip(t *testing.T) {
	first := gzipClaudeCodeResponse(t, []byte(`{"first":true}`))
	second := gzipClaudeCodeResponse(t, []byte(`{"second":true}`))
	concatenated := append(append([]byte(nil), first...), second...)
	_, err := decodeClaudeCodeResponse(concatenated, 1024)
	if typed, ok := failurecontract.As(err); !ok || typed.Kind != failurecontract.UpstreamProtocolError || typed.ProviderCode != "upstream_response_decode_failed" {
		t.Fatalf("concatenated error = %#v, want upstream_response_decode_failed", typed)
	}
}

func TestDecodeClaudeCodeResponseReusesIdentityBytes(t *testing.T) {
	payload := []byte(`{"type":"message"}`)
	decoded, err := decodeClaudeCodeResponse(payload, 1)
	if err != nil {
		t.Fatalf("decodeClaudeCodeResponse() error = %v", err)
	}
	if len(decoded) == 0 || &decoded[0] != &payload[0] {
		t.Fatal("identity response was unnecessarily copied")
	}
}

func gzipClaudeCodeResponse(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("write gzip payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buffer.Bytes()
}
