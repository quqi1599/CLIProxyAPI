package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

const maxDecodedRequestBodyBytes int64 = 32 << 20 // 32 MiB

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}

	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	decoded, err := DecodeRequestBody(raw, encoding)
	if err != nil {
		if json.Valid(raw) {
			return raw, nil
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
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte, maxDecodedBytes int64) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err := readDecodedRequestBodyWithLimit(decoder, maxDecodedBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	return decoded, nil
}

func readDecodedRequestBodyWithLimit(reader io.Reader, maxDecodedBytes int64) ([]byte, error) {
	if maxDecodedBytes <= 0 {
		return io.ReadAll(reader)
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, maxDecodedBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(decoded)) > maxDecodedBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes after decompression", maxDecodedBytes)
	}
	return decoded, nil
}
