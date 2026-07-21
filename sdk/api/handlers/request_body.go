package handlers

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

const (
	maxWireRequestBodyBytes    int64 = 32 << 20 // 32 MiB
	maxDecodedRequestBodyBytes int64 = 32 << 20 // 32 MiB
	minZstdDecoderMemoryBytes  int64 = 1 << 20  // 1 MiB
)

// RequestBodyTooLargeError identifies which request-body budget was exceeded.
type RequestBodyTooLargeError struct {
	Limit   int64
	Decoded bool
}

func (e *RequestBodyTooLargeError) Error() string {
	if e.Decoded {
		return fmt.Sprintf("request body exceeds %d bytes after decompression", e.Limit)
	}
	return fmt.Sprintf("request body exceeds %d bytes on the wire", e.Limit)
}

// IsRequestBodyTooLarge reports whether err is a request body limit failure.
func IsRequestBodyTooLarge(err error) bool {
	var target *RequestBodyTooLargeError
	return errors.As(err, &target)
}

// WriteRequestBodyError writes the stable client response for body read errors.
func WriteRequestBodyError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	detail := ErrorDetail{
		Message: fmt.Sprintf("Invalid request: %v", err),
		Type:    "invalid_request_error",
	}
	if IsRequestBodyTooLarge(err) {
		status = http.StatusRequestEntityTooLarge
		detail.Message = "Request body exceeds the allowed size"
		detail.Code = "request_too_large"
	}
	c.JSON(status, ErrorResponse{Error: detail})
}

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	return ReadRequestBodyWithLimits(c, maxWireRequestBodyBytes, maxDecodedRequestBodyBytes)
}

// ReadRequestBodyWithLimits reads at most maxWireBytes+1 bytes and applies the
// decoded limit after reversing the Content-Encoding chain.
func ReadRequestBodyWithLimits(c *gin.Context, maxWireBytes, maxDecodedBytes int64) ([]byte, error) {
	if c == nil || c.Request == nil {
		return nil, errors.New("request is unavailable")
	}
	if maxWireBytes > 0 && c.Request.ContentLength > maxWireBytes {
		return nil, &RequestBodyTooLargeError{Limit: maxWireBytes}
	}

	raw, err := readRequestBodyWithLimit(c.Request.Body, maxWireBytes, false)
	if err != nil {
		return nil, err
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	encoding := strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return enforceRequestBodyLimit(raw, maxDecodedBytes, true)
	}

	decoded, err := DecodeRequestBodyWithLimit(raw, encoding, maxDecodedBytes)
	if err != nil {
		if json.Valid(raw) {
			return enforceRequestBodyLimit(raw, maxDecodedBytes, true)
		}
		return nil, err
	}
	return decoded, nil
}

// DecodeRequestBody decodes supported Content-Encoding values with a bounded
// decompressed output size.
func DecodeRequestBody(raw []byte, encoding string) ([]byte, error) {
	return DecodeRequestBodyWithLimit(raw, encoding, maxDecodedRequestBodyBytes)
}

// DecodeRequestBodyWithLimit decodes supported Content-Encoding values while
// enforcing a maximum decompressed body size.
func DecodeRequestBodyWithLimit(raw []byte, encoding string, maxDecodedBytes int64) ([]byte, error) {
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBody(body, maxDecodedBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		case "gzip":
			decoded, err := decodeGzipRequestBody(body, maxDecodedBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		case "br":
			decoded, err := readRequestBodyWithLimit(brotli.NewReader(bytes.NewReader(body)), maxDecodedBytes, true)
			if err != nil {
				return nil, fmt.Errorf("failed to decode br request body: %w", err)
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return enforceRequestBodyLimit(body, maxDecodedBytes, true)
}

func decodeZstdRequestBody(raw []byte, maxDecodedBytes int64) ([]byte, error) {
	options := make([]zstd.DOption, 0, 2)
	if maxDecodedBytes > 0 {
		decoderMemoryBytes := max(maxDecodedBytes, minZstdDecoderMemoryBytes)
		options = append(options,
			zstd.WithDecoderMaxMemory(uint64(decoderMemoryBytes)),
			zstd.WithDecoderMaxWindow(uint64(decoderMemoryBytes)),
		)
	}
	decoder, err := zstd.NewReader(bytes.NewReader(raw), options...)
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
			return nil, &RequestBodyTooLargeError{Limit: maxDecodedBytes, Decoded: true}
		}
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err := readRequestBodyWithLimit(decoder, maxDecodedBytes, true)
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
			return nil, &RequestBodyTooLargeError{Limit: maxDecodedBytes, Decoded: true}
		}
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	return decoded, nil
}

func decodeGzipRequestBody(raw []byte, maxDecodedBytes int64) ([]byte, error) {
	decoder, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip request decoder: %w", err)
	}
	defer func() { _ = decoder.Close() }()

	decoded, err := readRequestBodyWithLimit(decoder, maxDecodedBytes, true)
	if err != nil {
		return nil, fmt.Errorf("failed to decode gzip request body: %w", err)
	}
	return decoded, nil
}

func enforceRequestBodyLimit(body []byte, maxBytes int64, decoded bool) ([]byte, error) {
	if maxBytes > 0 && int64(len(body)) > maxBytes {
		return nil, &RequestBodyTooLargeError{Limit: maxBytes, Decoded: decoded}
	}
	return body, nil
}

func readRequestBodyWithLimit(reader io.Reader, maxBytes int64, decoded bool) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	if maxBytes <= 0 {
		return io.ReadAll(reader)
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, &RequestBodyTooLargeError{Limit: maxBytes, Decoded: decoded}
	}
	return body, nil
}
