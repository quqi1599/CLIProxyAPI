package handlers

import (
	"bytes"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeRequestBodyWithLimitDecodesZstd(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"test-model","input":"hello"}`)
	compressed := mustEncodeZstd(t, payload)

	decoded, err := DecodeRequestBodyWithLimit(compressed, "zstd", int64(len(payload)))
	if err != nil {
		t.Fatalf("DecodeRequestBodyWithLimit() error = %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded body = %q, want %q", string(decoded), string(payload))
	}
}

func TestDecodeRequestBodyWithLimitRejectsOversizedZstd(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("a"), 65)
	compressed := mustEncodeZstd(t, payload)

	_, err := DecodeRequestBodyWithLimit(compressed, "zstd", 64)
	if err == nil {
		t.Fatal("DecodeRequestBodyWithLimit() error = nil, want size limit error")
	}
	if !strings.Contains(err.Error(), "request body exceeds 64 bytes after decompression") {
		t.Fatalf("DecodeRequestBodyWithLimit() error = %q, want size limit detail", err.Error())
	}
}

func mustEncodeZstd(t *testing.T, payload []byte) []byte {
	t.Helper()

	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	return compressed.Bytes()
}
