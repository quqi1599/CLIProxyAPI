package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
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

func TestNormalizeRequestBodyLimitAlwaysKeepsAHardCeiling(t *testing.T) {
	t.Parallel()

	for _, limit := range []int64{0, -1, emergencyRequestBodyBytes + 1} {
		if got := normalizeRequestBodyLimit(limit); got != emergencyRequestBodyBytes {
			t.Fatalf("normalizeRequestBodyLimit(%d) = %d, want %d", limit, got, emergencyRequestBodyBytes)
		}
	}
	if got := normalizeRequestBodyLimit(1024); got != 1024 {
		t.Fatalf("normalizeRequestBodyLimit(1024) = %d, want 1024", got)
	}
}

func TestReadRequestBodyCachesWireAndDecodedBytes(t *testing.T) {
	payload := []byte(`{"input":"` + strings.Repeat("payload", 32) + `"}`)
	compressed := mustEncodeGzip(t, payload)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed))
	c.Request.Header.Set("Content-Encoding", "gzip")

	if _, err := ReadRequestBody(c); err != nil {
		t.Fatalf("ReadRequestBody() error = %v", err)
	}
	vector, valid, cached := requestComplexityFromContext(context.WithValue(context.Background(), "gin", c))
	if !cached || !valid {
		t.Fatalf("complexity cached=%t valid=%t", cached, valid)
	}
	if vector.WireBytes != int64(len(compressed)) || vector.DecodedBytes != int64(len(payload)) {
		t.Fatalf("body sizes = wire:%d decoded:%d", vector.WireBytes, vector.DecodedBytes)
	}
	var handler *BaseAPIHandler
	reportCtx, release, err := handler.inspectAndAcquireAdmission(context.WithValue(context.Background(), "gin", c), payload, &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("inspectAndAcquireAdmission() error = %v", err)
	}
	report, ok := internalpayload.TransformReportFromContext(reportCtx)
	if !ok || report.WireInputBytes != int64(len(compressed)) || report.InputBytes != int64(len(payload)) {
		t.Fatalf("transform report sizes = wire:%d decoded:%d exists:%t", report.WireInputBytes, report.InputBytes, ok)
	}
	release()
}

func TestReadRequestBodyCachesOriginalComplexityForExecution(t *testing.T) {
	payload := []byte(`{"model":"test","input":[{"type":"function_call_output","output":"original-output"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))

	got, err := ReadRequestBody(c)
	if err != nil {
		t.Fatalf("ReadRequestBody() error = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("body = %q, want original payload", got)
	}

	ctx := context.WithValue(context.Background(), "gin", c)
	options := modelExecutionOptions{
		complexityDimensions: executionComplexityDimensions("openai", "", false, false, false),
	}
	var handler *BaseAPIHandler
	_, release, err := handler.inspectAndAcquireAdmission(ctx, []byte("transformed-body-is-not-rescanned"), &options)
	if err != nil {
		t.Fatalf("inspectAndAcquireAdmission() error = %v", err)
	}
	release()

	if options.complexity == nil || !options.complexityValid {
		t.Fatalf("cached complexity = %#v valid=%t", options.complexity, options.complexityValid)
	}
	vector := *options.complexity
	if vector.WireBytes != int64(len(payload)) || vector.DecodedBytes != int64(len(payload)) || vector.MessageCount != 1 || vector.ToolOutputBytes != int64(len("original-output")) {
		t.Fatalf("cached original vector = %+v", vector)
	}
	if vector.SourceFormat != "openai" || vector.Endpoint != "chat" || vector.Stream {
		t.Fatalf("cached dimensions = %q/%q/%t", vector.SourceFormat, vector.Endpoint, vector.Stream)
	}

	nestedBody := []byte(`{"messages":[{"role":"user","content":"nested"}]}`)
	nestedOptions := modelExecutionOptions{
		InternalSource:       true,
		complexityDimensions: executionComplexityDimensions("openai", "", false, false, false),
	}
	_, releaseNested, errNested := handler.inspectAndAcquireAdmission(ctx, nestedBody, &nestedOptions)
	if errNested != nil {
		t.Fatalf("nested inspectAndAcquireAdmission() error = %v", errNested)
	}
	releaseNested()
	if nestedOptions.complexity == nil || nestedOptions.complexity.DecodedBytes != int64(len(nestedBody)) || nestedOptions.complexity.MessageCount != 1 {
		t.Fatalf("nested complexity reused outer cache: %+v", nestedOptions.complexity)
	}
}

func TestMarkResponsesChatToolCompatibilityUsesPrecomputedProfile(t *testing.T) {
	body := []byte(`{"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec"}]}],"tools":[{"type":"image_generation"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	if _, err := ReadRequestBody(c); err != nil {
		t.Fatalf("ReadRequestBody() error = %v", err)
	}
	MarkResponsesChatToolCompatibility(c)

	ctx := context.WithValue(context.Background(), "gin", c)
	vector, valid, cached := requestComplexityFromContext(ctx)
	if !cached || !valid {
		t.Fatalf("cached complexity valid=%t cached=%t", valid, cached)
	}
	if vector.hasBuiltinImageGeneration || vector.hasSearchTool || !vector.hasNonSearchTool {
		t.Fatalf("responses-to-chat compatibility = %+v", vector.toolCompatibility)
	}
	if vector.InteractionCount != 0 || vector.DeclaredToolCount != 2 {
		t.Fatalf("tool structure = interactions %d, declarations %d", vector.InteractionCount, vector.DeclaredToolCount)
	}
}

func TestToolCompatibilityMarkersOverrideOnlyRoutingProfile(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))

	if _, err := ReadRequestBody(c); err != nil {
		t.Fatalf("ReadRequestBody() error = %v", err)
	}
	MarkRequestWithoutToolCompatibility(c)
	ctx := context.WithValue(context.Background(), "gin", c)
	cleared, valid, cached := requestComplexityFromContext(ctx)
	if !cached || !valid || cleared.hasBuiltinImageGeneration || cleared.hasSearchTool || cleared.hasNonSearchTool {
		t.Fatalf("cleared compatibility = %+v valid=%t cached=%t", cleared.toolCompatibility, valid, cached)
	}
	if cleared.DeclaredToolCount != 1 || cleared.MessageCount != 1 {
		t.Fatalf("markers changed structural metrics: %+v", cleared)
	}

	MarkBuiltinImageGenerationToolCompatibility(c)
	image, valid, cached := requestComplexityFromContext(ctx)
	if !cached || !valid || !image.hasBuiltinImageGeneration || image.hasSearchTool || image.hasNonSearchTool {
		t.Fatalf("image compatibility = %+v valid=%t cached=%t", image.toolCompatibility, valid, cached)
	}
	if image.DeclaredToolCount != 1 || image.MessageCount != 1 {
		t.Fatalf("marker changed structural metrics: %+v", image)
	}
}

func TestBuiltinImageGenerationCompatibilityMarkerFiltersJSONAdapters(t *testing.T) {
	providers := []string{"openai-compatibility", "codex", "xai"}
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "generation", path: "/v1/images/generations", body: `{"model":"gpt-image-2","prompt":"draw"}`},
		{name: "edit", path: "/v1/images/edits", body: `{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,AA=="}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			if _, err := ReadRequestBody(c); err != nil {
				t.Fatalf("ReadRequestBody() error = %v", err)
			}

			ctx := context.WithValue(context.Background(), "gin", c)
			before, valid, cached := requestComplexityFromContext(ctx)
			if !cached || !valid {
				t.Fatalf("cached complexity valid=%t cached=%t", valid, cached)
			}
			if got := filterProvidersByToolCompatibility(providers, &before); !reflect.DeepEqual(got, providers) {
				t.Fatalf("providers before adapter marker = %v, want %v", got, providers)
			}

			MarkBuiltinImageGenerationToolCompatibility(c)
			after, valid, cached := requestComplexityFromContext(ctx)
			if !cached || !valid {
				t.Fatalf("marked complexity valid=%t cached=%t", valid, cached)
			}
			if got, want := filterProvidersByToolCompatibility(providers, &after), []string{"codex"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("providers after adapter marker = %v, want %v", got, want)
			}
		})
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	MarkBuiltinImageGenerationToolCompatibility(c)
	ctx := context.WithValue(context.Background(), "gin", c)
	if _, _, cached := requestComplexityFromContext(ctx); cached {
		t.Fatal("marker created a cache entry without an inspected request body")
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

func TestDecodeRequestBodyWithLimitRejectsExcessiveEncodingLayers(t *testing.T) {
	t.Parallel()

	_, err := DecodeRequestBodyWithLimit([]byte("body"), "gzip, gzip, gzip, gzip, gzip", 64)
	if err == nil || !strings.Contains(err.Error(), "exceeds 4 layers") {
		t.Fatalf("DecodeRequestBodyWithLimit() error = %v, want encoding-layer limit", err)
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

func TestRequestBodyLimitErrorUsesCanonicalFailureContract(t *testing.T) {
	t.Parallel()

	err := NewRequestBodyLimitError(64, true)
	typed, ok := failurecontract.As(err)
	if !ok {
		t.Fatalf("error type = %T, want typed failure", err)
	}
	if typed.Kind != failurecontract.RequestTooLarge || typed.Scope != failurecontract.ScopeRequest || typed.HTTPStatus != http.StatusRequestEntityTooLarge || typed.ProviderCode != "request_too_large" {
		t.Fatalf("failure = %+v", typed)
	}
	var cause *RequestBodyTooLargeError
	if !errors.As(err, &cause) || cause.Limit != 64 || !cause.Decoded {
		t.Fatalf("cause = %#v", cause)
	}
}

func TestParseMultipartFormWithLimits(t *testing.T) {
	t.Parallel()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "hello"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err = part.Write([]byte("image-data")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	t.Run("success", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())

		form, errParse := ParseMultipartFormWithLimits(c, int64(body.Len()), 8, 16)
		if errParse != nil {
			t.Fatalf("ParseMultipartFormWithLimits() error = %v", errParse)
		}
		if got := form.Value["prompt"]; len(got) != 1 || got[0] != "hello" {
			t.Fatalf("prompt = %v", got)
		}
		if got := form.File["image"]; len(got) != 1 || got[0].Filename != "image.png" {
			t.Fatalf("image files = %#v", got)
		}
	})

	t.Run("chunked overflow", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
		c.Request.ContentLength = -1
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())

		_, errParse := ParseMultipartFormWithLimits(c, int64(body.Len()-1), 8, 16)
		typed, ok := failurecontract.As(errParse)
		if !ok || typed.Kind != failurecontract.RequestTooLarge || typed.Scope != failurecontract.ScopeRequest || typed.HTTPStatus != http.StatusRequestEntityTooLarge {
			t.Fatalf("failure = %#v", typed)
		}
	})

	t.Run("single file overflow", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())

		_, errParse := ParseMultipartFormWithLimits(c, int64(body.Len()), 8, 4)
		typed, ok := failurecontract.As(errParse)
		if !ok || typed.Kind != failurecontract.RequestTooLarge || typed.HTTPStatus != http.StatusRequestEntityTooLarge {
			t.Fatalf("failure = %#v", typed)
		}
		if !strings.Contains(errParse.Error(), `upload file "image.png" exceeds 4 bytes`) {
			t.Fatalf("error = %q", errParse)
		}
	})

	t.Run("encoded multipart rejected", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())
		c.Request.Header.Set("Content-Encoding", "gzip")

		_, errParse := ParseMultipartFormWithLimits(c, int64(body.Len()), 8, 16)
		if errParse == nil || !strings.Contains(errParse.Error(), "do not support Content-Encoding") {
			t.Fatalf("error = %v", errParse)
		}
	})
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
