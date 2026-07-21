package handlers

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

func TestReadRequestBodyWithLimitsBoundsIdentityAndRestoresBody(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"test"}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))

	got, err := ReadRequestBodyWithLimits(c, int64(len(payload)), int64(len(payload)))
	if err != nil {
		t.Fatalf("ReadRequestBodyWithLimits() error = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("body = %q, want %q", got, payload)
	}
	restored, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if !bytes.Equal(restored, payload) {
		t.Fatalf("restored body = %q, want %q", restored, payload)
	}
}

func TestReadRequestBodyWithLimitsRejectsChunkedWireOverflowAtLimitPlusOne(t *testing.T) {
	t.Parallel()

	reader := &countingReader{Reader: bytes.NewReader([]byte("12345"))}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Body = io.NopCloser(reader)
	c.Request.ContentLength = -1

	_, err := ReadRequestBodyWithLimits(c, 4, 16)
	var tooLarge *RequestBodyTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Decoded || tooLarge.Limit != 4 {
		t.Fatalf("error = %#v, want wire RequestBodyTooLargeError(limit=4)", err)
	}
	if reader.bytesRead != 5 {
		t.Fatalf("bytes read = %d, want limit+1 (5)", reader.bytesRead)
	}
}

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
	var tooLarge *RequestBodyTooLargeError
	if !errors.As(err, &tooLarge) || !tooLarge.Decoded || tooLarge.Limit != 64 {
		t.Fatalf("error = %#v, want decoded RequestBodyTooLargeError(limit=64)", err)
	}
}

func TestDecodeRequestBodyWithLimitSupportsGzipAndBrotli(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"test-model","input":"hello"}`)
	tests := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "gzip", encoding: "gzip", body: mustEncodeGzip(t, payload)},
		{name: "brotli", encoding: "br", body: mustEncodeBrotli(t, payload)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoded, err := DecodeRequestBodyWithLimit(test.body, test.encoding, int64(len(payload)))
			if err != nil {
				t.Fatalf("DecodeRequestBodyWithLimit() error = %v", err)
			}
			if !bytes.Equal(decoded, payload) {
				t.Fatalf("decoded body = %q, want %q", decoded, payload)
			}
		})
	}
}

func TestWriteRequestBodyErrorUsesStableTooLargeResponse(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	WriteRequestBodyError(c, &RequestBodyTooLargeError{Limit: 64, Decoded: true})

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != "request_too_large" {
		t.Fatalf("error.code = %q, want request_too_large", got)
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

func mustEncodeGzip(t *testing.T, payload []byte) []byte {
	t.Helper()

	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	if _, err := encoder.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return compressed.Bytes()
}

func mustEncodeBrotli(t *testing.T, payload []byte) []byte {
	t.Helper()

	var compressed bytes.Buffer
	encoder := brotli.NewWriter(&compressed)
	if _, err := encoder.Write(payload); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return compressed.Bytes()
}

type countingReader struct {
	io.Reader
	bytesRead int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead += n
	return n, err
}
