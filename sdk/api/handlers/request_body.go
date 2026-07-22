package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const (
	maxWireRequestBodyBytes     int64 = 32 << 20 // 32 MiB
	maxDecodedRequestBodyBytes  int64 = 32 << 20 // 32 MiB
	emergencyRequestBodyBytes   int64 = EmergencyPayloadBodyBytes
	minZstdDecoderMemoryBytes   int64 = 1 << 20 // 1 MiB
	maxRequestEncodingLayers          = 4
	defaultMultipartMemoryBytes int64 = 8 << 20 // 8 MiB
	emergencyMultipartBodyBytes int64 = EmergencyPayloadBodyBytes
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

// NewRequestBodyLimitError returns the canonical typed failure for a request
// body budget violation while preserving the concrete limit as its cause.
func NewRequestBodyLimitError(limit int64, decoded bool) error {
	cause := &RequestBodyTooLargeError{Limit: limit, Decoded: decoded}
	return &failurecontract.Failure{
		Kind:          failurecontract.RequestTooLarge,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusRequestEntityTooLarge,
		ProviderCode:  "request_too_large",
		Cause:         cause,
		PublicMessage: cause.Error(),
	}
}

// WriteRequestBodyError writes the stable client response for body read errors.
func WriteRequestBodyError(c *gin.Context, err error) {
	if IsAdmissionError(err) {
		writeAdmissionHTTPError(c, err)
		return
	}
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

// ParseMultipartFormWithLimits installs a hard wire-size ceiling before the
// standard library starts parsing fields or spilling uploaded files to disk.
// Multipart content encodings are intentionally rejected because decoding a
// streaming multipart body would otherwise bypass this wire budget.
func ParseMultipartFormWithLimits(c *gin.Context, maxWireBytes, maxMemoryBytes, maxFileBytes int64) (*multipart.Form, error) {
	decision := payloadBodyLimitDecisionForContext(nil, payloadBodyKindMultipart, maxWireBytes, maxWireBytes)
	return parseMultipartFormWithDecision(c, decision, maxMemoryBytes, maxFileBytes)
}

// ParseMultipartFormWithPolicy applies an injected public-route policy while
// preserving the provided hard limit when no policy is present.
func ParseMultipartFormWithPolicy(c *gin.Context, fallbackWireBytes, maxMemoryBytes, maxFileBytes int64) (*multipart.Form, error) {
	decision := payloadBodyLimitDecisionForContext(c, payloadBodyKindMultipart, fallbackWireBytes, fallbackWireBytes)
	return parseMultipartFormWithDecision(c, decision, maxMemoryBytes, maxFileBytes)
}

func parseMultipartFormWithDecision(c *gin.Context, decision payloadBodyLimitDecision, maxMemoryBytes, maxFileBytes int64) (form *multipart.Form, err error) {
	if c == nil || c.Request == nil {
		return nil, errors.New("request is unavailable")
	}
	if !identityContentEncoding(c.Request.Header.Get("Content-Encoding")) {
		return nil, errors.New("multipart requests do not support Content-Encoding")
	}
	maxWireBytes := decision.maxWireBytes
	if maxWireBytes <= 0 || maxWireBytes > emergencyMultipartBodyBytes {
		maxWireBytes = emergencyMultipartBodyBytes
	}
	decision.maxWireBytes = maxWireBytes
	decision.maxDecodedBytes = maxWireBytes
	if maxMemoryBytes <= 0 {
		maxMemoryBytes = defaultMultipartMemoryBytes
	}
	if maxMemoryBytes > maxWireBytes {
		maxMemoryBytes = maxWireBytes
	}
	wireBytes := max(c.Request.ContentLength, 0)
	decodedBytes := wireBytes
	policyRejected := false
	defer func() {
		if IsRequestBodyTooLarge(err) && policyRejected {
			wireBytes = max(wireBytes, maxWireBytes+1)
			decodedBytes = max(decodedBytes, wireBytes)
		}
		recordPayloadBodyLimit(c, decision, wireBytes, decodedBytes, policyRejected)
	}()

	if c.Request.ContentLength > maxWireBytes {
		policyRejected = true
		return nil, NewRequestBodyLimitError(maxWireBytes, false)
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWireBytes)
	if err := c.Request.ParseMultipartForm(maxMemoryBytes); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) || errors.Is(err, multipart.ErrMessageTooLarge) {
			policyRejected = true
			return nil, NewRequestBodyLimitError(maxWireBytes, false)
		}
		return nil, err
	}
	form = c.Request.MultipartForm
	vector := complexityVector{}
	if c.Request.ContentLength > 0 {
		vector.WireBytes = c.Request.ContentLength
		vector.DecodedBytes = c.Request.ContentLength
	}
	if form != nil && maxFileBytes > 0 {
		for _, files := range form.File {
			for _, fileHeader := range files {
				if fileHeader != nil && fileHeader.Size > maxFileBytes {
					return nil, fmt.Errorf("upload file %q exceeds %d bytes: %w", fileHeader.Filename, maxFileBytes, NewRequestBodyLimitError(maxFileBytes, false))
				}
			}
		}
	}
	if form != nil {
		var parsedBytes int64
		for key, values := range form.Value {
			vector.ContentPartCount += len(values)
			parsedBytes += int64(len(key))
			for _, value := range values {
				parsedBytes += int64(len(value))
			}
		}
		for key, files := range form.File {
			vector.ContentPartCount += len(files)
			parsedBytes += int64(len(key))
			for _, fileHeader := range files {
				if fileHeader == nil {
					continue
				}
				parsedBytes += fileHeader.Size
				vector.InlineImageBytes += fileHeader.Size
			}
		}
		if vector.DecodedBytes == 0 {
			vector.WireBytes = parsedBytes
			vector.DecodedBytes = parsedBytes
		}
	}
	wireBytes = vector.WireBytes
	decodedBytes = vector.DecodedBytes
	if err := cacheRequestComplexityVector(c, vector, true); err != nil {
		return nil, err
	}
	return form, nil
}

func identityContentEncoding(raw string) bool {
	for _, part := range strings.Split(raw, ",") {
		encoding := strings.TrimSpace(part)
		if encoding != "" && !strings.EqualFold(encoding, "identity") {
			return false
		}
	}
	return true
}

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	decision := payloadBodyLimitDecisionForContext(c, payloadBodyKindJSON, maxWireRequestBodyBytes, maxDecodedRequestBodyBytes)
	return readRequestBodyWithDecision(c, decision)
}

// ReadRequestBodyWithLimits reads at most maxWireBytes+1 bytes and applies the
// decoded limit after reversing the Content-Encoding chain.
func ReadRequestBodyWithLimits(c *gin.Context, maxWireBytes, maxDecodedBytes int64) ([]byte, error) {
	decision := payloadBodyLimitDecisionForContext(nil, payloadBodyKindJSON, maxWireBytes, maxDecodedBytes)
	return readRequestBodyWithDecision(c, decision)
}

// ReadRequestBodyWithPolicy applies an injected public-route policy while
// retaining the caller-provided hard limits when no policy is present.
func ReadRequestBodyWithPolicy(c *gin.Context, fallbackWireBytes, fallbackDecodedBytes int64) ([]byte, error) {
	decision := payloadBodyLimitDecisionForContext(c, payloadBodyKindJSON, fallbackWireBytes, fallbackDecodedBytes)
	return readRequestBodyWithDecision(c, decision)
}

func readRequestBodyWithDecision(c *gin.Context, decision payloadBodyLimitDecision) (body []byte, err error) {
	if c == nil || c.Request == nil {
		return nil, errors.New("request is unavailable")
	}
	maxWireBytes := normalizeRequestBodyLimit(decision.maxWireBytes)
	maxDecodedBytes := normalizeRequestBodyLimit(decision.maxDecodedBytes)
	decision.maxWireBytes = maxWireBytes
	decision.maxDecodedBytes = maxDecodedBytes
	wireBytes := max(c.Request.ContentLength, 0)
	decodedBytes := unknownPayloadBodyBytes
	if identityContentEncoding(c.Request.Header.Get("Content-Encoding")) && c.Request.ContentLength >= 0 {
		decodedBytes = wireBytes
	}
	defer func() {
		if IsRequestBodyTooLarge(err) {
			var target *RequestBodyTooLargeError
			if errors.As(err, &target) && target.Decoded {
				decodedBytes = max(decodedBytes, target.Limit+1)
			} else {
				wireBytes = max(wireBytes, maxWireBytes+1)
				if identityContentEncoding(c.Request.Header.Get("Content-Encoding")) {
					decodedBytes = max(decodedBytes, wireBytes)
				}
			}
		}
		recordPayloadBodyLimit(c, decision, wireBytes, decodedBytes, IsRequestBodyTooLarge(err))
	}()
	if maxWireBytes > 0 && c.Request.ContentLength > maxWireBytes {
		return nil, NewRequestBodyLimitError(maxWireBytes, false)
	}

	raw, err := readRequestBodyWithLimit(c.Request.Body, maxWireBytes, false)
	if err != nil {
		return nil, err
	}
	wireBytes = int64(len(raw))
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	var errLimit error
	encoding := strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		body, errLimit = enforceRequestBodyLimit(raw, maxDecodedBytes, true)
		if errLimit != nil {
			return nil, errLimit
		}
		decodedBytes = int64(len(body))
		if errCache := cacheRequestComplexity(c, body, int64(len(raw))); errCache != nil {
			return nil, errCache
		}
		return body, nil
	}

	decoded, err := DecodeRequestBodyWithLimit(raw, encoding, maxDecodedBytes)
	if err != nil {
		if json.Valid(raw) {
			body, errLimit = enforceRequestBodyLimit(raw, maxDecodedBytes, true)
			if errLimit != nil {
				return nil, errLimit
			}
			decodedBytes = int64(len(body))
			if errCache := cacheRequestComplexity(c, body, int64(len(raw))); errCache != nil {
				return nil, errCache
			}
			return body, nil
		}
		return nil, err
	}
	body = decoded
	decodedBytes = int64(len(body))
	if errCache := cacheRequestComplexity(c, decoded, int64(len(raw))); errCache != nil {
		return nil, errCache
	}
	return body, nil
}

const requestComplexityGinKey = "cliproxy_request_complexity"

type cachedRequestComplexity struct {
	vector complexityVector
	valid  bool
}

func cacheRequestComplexity(c *gin.Context, body []byte, wireBytes int64) error {
	if c == nil {
		return nil
	}
	vector, valid := inspectRequestComplexity(body)
	vector.WireBytes = wireBytes
	return cacheRequestComplexityVector(c, vector, valid)
}

func cacheRequestComplexityVector(c *gin.Context, vector complexityVector, valid bool) error {
	if c == nil {
		return nil
	}
	c.Set(requestComplexityGinKey, cachedRequestComplexity{vector: vector, valid: valid})
	return upgradeIngressAdmission(c, vector)
}

// MarkResponsesChatToolCompatibility selects the tool profile produced by the
// Responses-to-Chat converter without inspecting the transformed body again.
func MarkResponsesChatToolCompatibility(c *gin.Context) {
	markRequestToolCompatibility(c, func(vector *complexityVector) {
		vector.useResponsesChatToolCompatibility()
	})
}

// MarkRequestWithoutToolCompatibility clears tool routing flags after a handler
// rebuilds a request without tool declarations.
func MarkRequestWithoutToolCompatibility(c *gin.Context) {
	markRequestToolCompatibility(c, func(vector *complexityVector) {
		vector.toolCompatibility = toolCompatibility{}
	})
}

// MarkBuiltinImageGenerationToolCompatibility selects the known tool profile
// used by the Images-to-Responses adapter.
func MarkBuiltinImageGenerationToolCompatibility(c *gin.Context) {
	markRequestToolCompatibility(c, func(vector *complexityVector) {
		vector.toolCompatibility = toolCompatibility{hasBuiltinImageGeneration: true}
	})
}

func markRequestToolCompatibility(c *gin.Context, update func(*complexityVector)) {
	if c == nil {
		return
	}
	value, exists := c.Get(requestComplexityGinKey)
	if !exists {
		return
	}
	cached, ok := value.(cachedRequestComplexity)
	if !ok {
		return
	}
	if update != nil {
		update(&cached.vector)
	}
	c.Set(requestComplexityGinKey, cached)
}

func requestComplexityFromContext(ctx context.Context) (complexityVector, bool, bool) {
	if ctx == nil {
		return complexityVector{}, false, false
	}
	c, _ := ctx.Value("gin").(*gin.Context)
	if c == nil {
		return complexityVector{}, false, false
	}
	value, exists := c.Get(requestComplexityGinKey)
	if !exists {
		return complexityVector{}, false, false
	}
	cached, ok := value.(cachedRequestComplexity)
	if !ok {
		return complexityVector{}, false, false
	}
	return cached.vector, cached.valid, true
}

// DecodeRequestBody decodes supported Content-Encoding values with a bounded
// decompressed output size.
func DecodeRequestBody(raw []byte, encoding string) ([]byte, error) {
	return DecodeRequestBodyWithLimit(raw, encoding, maxDecodedRequestBodyBytes)
}

// DecodeRequestBodyWithLimit decodes supported Content-Encoding values while
// enforcing a maximum decompressed body size.
func DecodeRequestBodyWithLimit(raw []byte, encoding string, maxDecodedBytes int64) ([]byte, error) {
	maxDecodedBytes = normalizeRequestBodyLimit(maxDecodedBytes)
	parts := strings.Split(encoding, ",")
	layers := 0
	for _, part := range parts {
		enc := strings.ToLower(strings.TrimSpace(part))
		if enc == "" || enc == "identity" {
			continue
		}
		layers++
		if layers > maxRequestEncodingLayers {
			return nil, fmt.Errorf("request content encoding exceeds %d layers", maxRequestEncodingLayers)
		}
	}
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
			return nil, NewRequestBodyLimitError(maxDecodedBytes, true)
		}
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err := readRequestBodyWithLimit(decoder, maxDecodedBytes, true)
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
			return nil, NewRequestBodyLimitError(maxDecodedBytes, true)
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
		return nil, NewRequestBodyLimitError(maxBytes, decoded)
	}
	return body, nil
}

func readRequestBodyWithLimit(reader io.Reader, maxBytes int64, decoded bool) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	maxBytes = normalizeRequestBodyLimit(maxBytes)
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, NewRequestBodyLimitError(maxBytes, decoded)
	}
	return body, nil
}

func normalizeRequestBodyLimit(limit int64) int64 {
	if limit <= 0 || limit > emergencyRequestBodyBytes {
		return emergencyRequestBodyBytes
	}
	return limit
}
