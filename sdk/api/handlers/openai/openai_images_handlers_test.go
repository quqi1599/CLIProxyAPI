package openai

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func performImagesEndpointRequest(t *testing.T, endpointPath string, contentType string, body io.Reader, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(endpointPath, handler)

	req := httptest.NewRequest(http.MethodPost, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func assertUnsupportedImagesModelResponse(t *testing.T, resp *httptest.ResponseRecorder, model string) {
	t.Helper()

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}

	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model " + model + " is not supported on " + imagesGenerationsPath + " or " + imagesEditsPath + ". Use " + gptImage15Model + ", " + defaultImagesToolModel + ", " + defaultXAIImagesModel + ", " + xaiImagesQualityModel + ", or a configured openai-compatibility image model."
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
	if errorType := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); errorType != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", errorType)
	}
}

func assertImagesPromptRequiredResponse(t *testing.T, resp *httptest.ResponseRecorder, path string) {
	t.Helper()

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	if want := imagesPromptRequiredMessage(defaultImagesToolModel, path); message != want {
		t.Fatalf("error message = %q, want %q", message, want)
	}
	if errorType := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); errorType != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", errorType)
	}
}

func TestImagesModelValidationAllowsGPTImageAndXAIModels(t *testing.T) {
	for _, model := range []string{"gpt-image-1.5", "codex/gpt-image-1.5", "gpt-image-2", "codex/gpt-image-2", "grok-imagine-image", "xai/grok-imagine-image", "grok-imagine-image-quality", "xai/grok-imagine-image-quality"} {
		if !isSupportedImagesModel(model) {
			t.Fatalf("expected %s to be supported", model)
		}
	}
	if isSupportedImagesModel("gpt-5.4-mini") {
		t.Fatal("expected gpt-5.4-mini to be rejected")
	}
	if isSupportedImagesModel("codex/grok-imagine-image") {
		t.Fatal("expected codex/grok-imagine-image to be rejected")
	}
}

func TestImagesModelValidationAllowsOpenAICompatImageModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-openai-compat-image-model-validation"
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{
		{ID: "compat-image-model", Object: "model", OwnedBy: "compat", Type: registry.OpenAIImageModelType},
		{ID: "compat-chat-model", Object: "model", OwnedBy: "compat", Type: "openai-compatibility"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	if !isSupportedImagesModel("compat-image-model") {
		t.Fatal("expected configured openai-compatibility image model to be supported")
	}
	if isSupportedImagesModel("compat-chat-model") {
		t.Fatal("expected non-image openai-compatibility model to be rejected")
	}
}

func TestBuildXAIImagesGenerationsRequest(t *testing.T) {
	rawJSON := []byte(`{"model":"xai/grok-imagine-image-quality","prompt":"abstract art","aspect_ratio":"landscape","resolution":"2k","n":2,"response_format":"url"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "xai/grok-imagine-image-quality", "url")

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image-quality" {
		t.Fatalf("model = %q, want grok-imagine-image-quality", got)
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "abstract art" {
		t.Fatalf("prompt = %q, want abstract art", got)
	}
	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
	if got := gjson.GetBytes(req, "response_format").String(); got != "url" {
		t.Fatalf("response_format = %q, want url", got)
	}
	if got := gjson.GetBytes(req, "n").Int(); got != 2 {
		t.Fatalf("n = %d, want 2", got)
	}
}

func TestBuildXAIImagesEditRequest(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"data:image/png;base64,AA==", "https://example.com/image.png"}, "b64_json", "3:2", "1k", 0)

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image" {
		t.Fatalf("model = %q, want grok-imagine-image", got)
	}
	if got := gjson.GetBytes(req, "images.0.type").String(); got != "image_url" {
		t.Fatalf("images.0.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "images.0.url").String(); got != "data:image/png;base64,AA==" {
		t.Fatalf("images.0.url = %q", got)
	}
	if got := gjson.GetBytes(req, "images.1.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("images.1.url = %q", got)
	}
	if gjson.GetBytes(req, "image").Exists() {
		t.Fatalf("multiple image edits must use images array: %s", string(req))
	}
}

func TestBuildXAIImagesEditRequestSingleImage(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"https://example.com/image.png"}, "url", "", "", 0)

	if got := gjson.GetBytes(req, "image.type").String(); got != "image_url" {
		t.Fatalf("image.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "image.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("image.url = %q", got)
	}
	if gjson.GetBytes(req, "images").Exists() {
		t.Fatalf("single image edit must use image object: %s", string(req))
	}
}

func TestCollectXAIImagesFromJSONNormalizesLooseImageReferences(t *testing.T) {
	raw := []byte(`{
		"image":{"image_url":{"url":"data:image;base64,iVBORw0KGgo="}},
		"images":[
			"iVBORw0KGgo=",
			{"b64_json":"iVBORw0KGgo="},
			{"url":"https://example.com/image.png"}
		]
	}`)

	images, err := collectXAIImagesFromJSON(raw)
	if err != nil {
		t.Fatalf("collectXAIImagesFromJSON() error = %v", err)
	}

	want := []string{
		"data:image/png;base64,iVBORw0KGgo=",
		"data:image/png;base64,iVBORw0KGgo=",
		"data:image/png;base64,iVBORw0KGgo=",
		"https://example.com/image.png",
	}
	if len(images) != len(want) {
		t.Fatalf("images len = %d, want %d: %#v", len(images), len(want), images)
	}
	for i := range want {
		if images[i] != want[i] {
			t.Fatalf("images[%d] = %q, want %q; all=%#v", i, images[i], want[i], images)
		}
	}
}

func TestCollectXAIImagesFromJSONRejectsTooManyReferences(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"images":[`)
	for idx := 0; idx < maxImagesEditReferences+1; idx++ {
		if idx > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`"https://example.com/image.png"`)
	}
	body.WriteString(`]}`)

	images, err := collectXAIImagesFromJSON([]byte(body.String()))
	if err == nil {
		t.Fatalf("images = %d, want reference-limit error", len(images))
	}
	if !strings.Contains(err.Error(), "at most 16") {
		t.Fatalf("error = %q, want stable reference limit", err)
	}
}

func TestMultipartFileToDataURLRepairsGenericImageContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
	header.Set("Content-Type", "image")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove form files: %v", errRemove)
		}
	}()

	dataURL, err := multipartFileToDataURL(form.File["image"][0])
	if err != nil {
		t.Fatalf("multipartFileToDataURL() error = %v", err)
	}
	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("data URL = %q, want image/png data URL", dataURL)
	}
}

func TestMultipartFileToDataURLRejectsOversizedUpload(t *testing.T) {
	previousLimit := openAICompatImagesMaxUploadFileBytes
	openAICompatImagesMaxUploadFileBytes = 4
	defer func() {
		openAICompatImagesMaxUploadFileBytes = previousLimit
	}()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "large.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove form files: %v", errRemove)
		}
	}()

	_, err := multipartFileToDataURL(form.File["image"][0])
	if err == nil {
		t.Fatal("multipartFileToDataURL() error = nil, want upload size error")
	}
	if !strings.Contains(err.Error(), `upload file "large.png" exceeds 4 bytes`) {
		t.Fatalf("multipartFileToDataURL() error = %q, want size detail", err.Error())
	}
}

func TestBuildOpenAICompatImagesJSONRequestPreservesStreamForStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":false}`), "upstream-image", true)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if !gjson.GetBytes(req, "stream").Bool() {
		t.Fatalf("stream flag missing: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestDropsStreamForNonStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":true}`), "upstream-image", false)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if gjson.GetBytes(req, "stream").Exists() {
		t.Fatalf("stream flag should be removed from non-streaming request: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestDropsToolControlFields(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","tool_choice":{"type":"image_generation"},"tools":[{"type":"image_generation"}],"parallel_tool_calls":true}`), "upstream-image", false)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "draw" {
		t.Fatalf("prompt = %q, want draw; body=%s", got, string(req))
	}
	for _, field := range openAICompatImagesToolControlFields {
		if gjson.GetBytes(req, field).Exists() {
			t.Fatalf("%s should be removed from images request: %s", field, string(req))
		}
	}
}

func TestBuildOpenAICompatImagesJSONRequestPreservesUnknownFieldOrder(t *testing.T) {
	raw := []byte(`{"before":{"nested":[1,2]},"model":"old","prompt":"draw","tools":[{"type":"image_generation"}],"middle":7,"stream":false,"after":"keep"}`)
	req := buildOpenAICompatImagesJSONRequest(raw, "upstream-image", true)

	if got := gjson.GetBytes(req, "before.nested.1").Int(); got != 2 {
		t.Fatalf("unknown nested field lost: %s", req)
	}
	if got := gjson.GetBytes(req, "after").String(); got != "keep" {
		t.Fatalf("unknown trailing field lost: %s", req)
	}
	if gjson.GetBytes(req, "tools").Exists() {
		t.Fatalf("tools field was not removed: %s", req)
	}
	ordered := []string{`"before"`, `"model"`, `"prompt"`, `"middle"`, `"stream"`, `"after"`}
	previous := -1
	for _, field := range ordered {
		index := strings.Index(string(req), field)
		if index <= previous {
			t.Fatalf("field order changed at %s: %s", field, req)
		}
		previous = index
	}
}

func BenchmarkPayloadGrowthOpenAICompatImagesRequest(b *testing.B) {
	raw := []byte(`{"before":{"nested":"` + strings.Repeat("x", 1<<20) + `"},"model":"old","prompt":"draw","tools":[],"parallel_tool_calls":true,"stream":false,"after":"keep"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for idx := 0; idx < b.N; idx++ {
		if out := buildOpenAICompatImagesJSONRequest(raw, "upstream-image", true); len(out) == 0 {
			b.Fatal("empty output")
		}
	}
}

func TestBuildOpenAICompatImagesEditJSONRequestFromMultipartDropsToolControlFields(t *testing.T) {
	form := &multipart.Form{
		Value: map[string][]string{
			"size":                {"1024x1024"},
			"quality":             {"high"},
			"response_format":     {"url"},
			"output_compression":  {"80"},
			"tool_choice":         {`{"type":"image_generation"}`},
			"tools[0][type]":      {"image_generation"},
			"parallel_tool_calls": {"true"},
		},
	}

	req := buildOpenAICompatImagesEditJSONRequestFromMultipart(form, "gpt-image-2", "edit it", []string{"data:image/png;base64,AA=="}, "data:image/png;base64,BB==", false)

	if got := gjson.GetBytes(req, "model").String(); got != "gpt-image-2" {
		t.Fatalf("model = %q, want gpt-image-2; body=%s", got, string(req))
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "edit it" {
		t.Fatalf("prompt = %q, want edit it; body=%s", got, string(req))
	}
	if got := gjson.GetBytes(req, "images.0.image_url").String(); got != "data:image/png;base64,AA==" {
		t.Fatalf("image url = %q; body=%s", got, string(req))
	}
	if got := gjson.GetBytes(req, "mask.image_url").String(); got != "data:image/png;base64,BB==" {
		t.Fatalf("mask url = %q; body=%s", got, string(req))
	}
	if got := gjson.GetBytes(req, "output_compression").Int(); got != 80 {
		t.Fatalf("output_compression = %d, want 80; body=%s", got, string(req))
	}
	for _, field := range []string{"tool_choice", "tools", "parallel_tool_calls", "tools[0][type]"} {
		if gjson.GetBytes(req, field).Exists() {
			t.Fatalf("%s should be removed from images edit request: %s", field, string(req))
		}
	}
}

func TestBuildOpenAICompatImagesMultipartRequestPreservesStreamAndFileContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
		t.Fatalf("write prompt field: %v", errWrite)
	}
	if errWrite := writer.WriteField("tool_choice", `{"type":"image_generation"}`); errWrite != nil {
		t.Fatalf("write tool_choice field: %v", errWrite)
	}
	if errWrite := writer.WriteField("tools[0][type]", "image_generation"); errWrite != nil {
		t.Fatalf("write tools field: %v", errWrite)
	}
	if errWrite := writer.WriteField("parallel_tool_calls", "true"); errWrite != nil {
		t.Fatalf("write parallel_tool_calls field: %v", errWrite)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read source form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove source form files: %v", errRemove)
		}
	}()

	out, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, "upstream-image", true)
	if errBuild != nil {
		t.Fatalf("buildOpenAICompatImagesMultipartRequest error: %v", errBuild)
	}
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		t.Fatalf("parse content type: %v", errParse)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	rewrittenReader := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	rewrittenForm, errRead := rewrittenReader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read rewritten form: %v", errRead)
	}
	defer func() {
		if errRemove := rewrittenForm.RemoveAll(); errRemove != nil {
			t.Fatalf("remove rewritten form files: %v", errRemove)
		}
	}()
	if got := rewrittenForm.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model values = %#v, want upstream-image", got)
	}
	if got := rewrittenForm.Value["stream"]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("stream values = %#v, want true", got)
	}
	if got := rewrittenForm.Value["prompt"]; len(got) != 1 || got[0] != "edit" {
		t.Fatalf("prompt values = %#v, want edit", got)
	}
	for _, field := range []string{"tool_choice", "tools[0][type]", "parallel_tool_calls"} {
		if got := rewrittenForm.Value[field]; len(got) != 0 {
			t.Fatalf("%s values = %#v, want removed", field, got)
		}
	}
	if got := rewrittenForm.File["image"]; len(got) != 1 || got[0].Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image headers = %#v, want image/png", got)
	}
}

func TestBuildOpenAICompatImagesMultipartRequestRejectsOversizedBody(t *testing.T) {
	previousLimit := openAICompatImagesMaxMultipartBodyBytes
	openAICompatImagesMaxMultipartBodyBytes = 4
	defer func() {
		openAICompatImagesMaxMultipartBodyBytes = previousLimit
	}()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "large.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read source form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove source form files: %v", errRemove)
		}
	}()

	_, _, errBuild := buildOpenAICompatImagesMultipartRequest(form, "upstream-image", false)
	if errBuild == nil {
		t.Fatal("buildOpenAICompatImagesMultipartRequest() error = nil, want upload size error")
	}
	if !strings.Contains(errBuild.Error(), "multipart upload exceeds 4 bytes") {
		t.Fatalf("buildOpenAICompatImagesMultipartRequest() error = %q, want size detail", errBuild.Error())
	}
}

func TestShouldStreamImagesRequestRequiresSSEAccept(t *testing.T) {
	tests := []struct {
		name      string
		requested bool
		accept    string
		want      bool
	}{
		{name: "not requested", requested: false, accept: "text/event-stream", want: false},
		{name: "missing accept", requested: true, accept: "", want: false},
		{name: "json accept", requested: true, accept: "application/json", want: false},
		{name: "wildcard accept", requested: true, accept: "*/*", want: false},
		{name: "sse accept", requested: true, accept: "text/event-stream", want: true},
		{name: "sse accept list", requested: true, accept: "application/json, text/event-stream; charset=utf-8", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			req := httptest.NewRequest(http.MethodPost, imagesEditsPath, nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			c.Request = req

			if got := shouldStreamImagesRequest(c, tt.requested); got != tt.want {
				t.Fatalf("shouldStreamImagesRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestImageEmptyOutputMessageSummarizesUpstreamReason(t *testing.T) {
	const secret = "image-upstream-error-sentinel"
	payload := []byte("{\"type\":\"response.completed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"" + secret + "\"},\"output\":[]}}")

	got := imageEmptyOutputMessage(payload)

	if strings.Contains(got, secret) || !strings.Contains(got, "upstream image generation failed: status=failed") || !strings.Contains(got, "sha256") {
		t.Fatalf("message = %q, want safe failure classification and metadata", got)
	}
}

func TestImageEmptyOutputMessageSummarizesToolCallStatus(t *testing.T) {
	const secret = "image-tool-error-sentinel"
	payload := []byte("{\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"image_generation_call\",\"status\":\"failed\",\"error\":{\"message\":\"" + secret + "\"}}]}}")

	got := imageEmptyOutputMessage(payload)

	if strings.Contains(got, secret) || !strings.Contains(got, "upstream image generation call failed: status=failed") || !strings.Contains(got, "sha256") {
		t.Fatalf("message = %q, want safe tool failure classification and metadata", got)
	}
}

func TestBuildImagesAPIResponseFromXAI(t *testing.T) {
	payload := []byte(`{"created":123,"data":[{"b64_json":"AA==","revised_prompt":"refined","mime_type":"image/png"}],"usage":{"total_tokens":0}}`)

	out, err := buildImagesAPIResponseFromXAI(payload, "b64_json")
	if err != nil {
		t.Fatalf("buildImagesAPIResponseFromXAI() error = %v", err)
	}

	if got := gjson.GetBytes(out, "created").Int(); got != 123 {
		t.Fatalf("created = %d, want 123", got)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("data.0.b64_json = %q, want AA==", got)
	}
	if got := gjson.GetBytes(out, "data.0.revised_prompt").String(); got != "refined" {
		t.Fatalf("data.0.revised_prompt = %q, want refined", got)
	}
	if !gjson.GetBytes(out, "usage").Exists() {
		t.Fatalf("usage missing: %s", string(out))
	}
}

func TestImagesGenerationsRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsJSONRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsMultipartRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("prompt", "edit this"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsMultipartRejectsTooManyFilesBeforeReadingThem(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", defaultXAIImagesModel); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if err := writer.WriteField("prompt", "edit"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	for idx := 0; idx < maxImagesEditReferences+1; idx++ {
		part, errCreate := writer.CreateFormFile("image[]", fmt.Sprintf("image-%d.png", idx))
		if errCreate != nil {
			t.Fatalf("create image %d: %v", idx, errCreate)
		}
		if _, errWrite := part.Write([]byte("x")); errWrite != nil {
			t.Fatalf("write image %d: %v", idx, errWrite)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String(); !strings.Contains(message, "at most 16 images") {
		t.Fatalf("message = %q, want image-count limit", message)
	}
}

func TestImagesEditsMultipartUsesStableTooLargeResponse(t *testing.T) {
	handler := &OpenAIAPIHandler{}

	t.Run("total body", func(t *testing.T) {
		previousLimit := openAICompatImagesMaxMultipartBodyBytes
		openAICompatImagesMaxMultipartBodyBytes = 4
		defer func() { openAICompatImagesMaxMultipartBodyBytes = previousLimit }()

		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("model", "gpt-image-2"); err != nil {
			t.Fatalf("write model: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}

		resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)
		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusRequestEntityTooLarge, resp.Body.String())
		}
		if got := gjson.GetBytes(resp.Body.Bytes(), "error.code").String(); got != "request_too_large" {
			t.Fatalf("error.code = %q", got)
		}
	})

	t.Run("unused file", func(t *testing.T) {
		previousLimit := openAICompatImagesMaxUploadFileBytes
		openAICompatImagesMaxUploadFileBytes = 4
		defer func() { openAICompatImagesMaxUploadFileBytes = previousLimit }()

		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("model", "gpt-image-2"); err != nil {
			t.Fatalf("write model: %v", err)
		}
		if err := writer.WriteField("prompt", "edit this"); err != nil {
			t.Fatalf("write prompt: %v", err)
		}
		part, err := writer.CreateFormFile("unused", "unused.bin")
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		if _, err = part.Write([]byte("12345")); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if err = writer.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}

		resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)
		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusRequestEntityTooLarge, resp.Body.String())
		}
		if got := gjson.GetBytes(resp.Body.Bytes(), "error.code").String(); got != "request_too_large" {
			t.Fatalf("error.code = %q", got)
		}
	})
}

func TestImagesGenerationsMissingPromptReturnsActionableMessage(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-image-2"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	assertImagesPromptRequiredResponse(t, resp, imagesGenerationsPath)
}

func TestImagesEditsJSONMissingPromptReturnsActionableMessage(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-image-2","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	assertImagesPromptRequiredResponse(t, resp, imagesEditsPath)
}

func TestImagesEditsMultipartMissingPromptReturnsActionableMessage(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)

	assertImagesPromptRequiredResponse(t, resp, imagesEditsPath)
}

func TestImagesGenerationsRejectsUnsafePromptBeforeUpstream(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-image-2","prompt":"三格教室故事板，一个学生对未成年人施暴，展示流血伤口"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if got := resp.Header().Get("X-CPA-Local-Guard"); got != "image_prompt" {
		t.Fatalf("X-CPA-Local-Guard = %q, want image_prompt", got)
	}
	if got := resp.Header().Get("X-CPA-Local-Guard-Category"); got != imagePromptGuardMinorHarm {
		t.Fatalf("X-CPA-Local-Guard-Category = %q, want %q", got, imagePromptGuardMinorHarm)
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); got != localImagePromptGuardErrorType {
		t.Fatalf("error.type = %q, want %q", got, localImagePromptGuardErrorType)
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "error.code").String(); got != localImagePromptGuardErrorCode {
		t.Fatalf("error.code = %q, want %q", got, localImagePromptGuardErrorCode)
	}
	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	for _, want := range []string{
		"CPA本地拦截",
		"禁止生成此类图片",
		"中华人民共和国网络安全法",
		"重复提交可能导致账号限制",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error.message %q should contain %q", message, want)
		}
	}
}

func TestImagesEditsJSONRejectsUnsafePromptBeforeUpstream(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-image-2","prompt":"a student assaulting a child with blood","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); got != localImagePromptGuardErrorType {
		t.Fatalf("error.type = %q, want %q", got, localImagePromptGuardErrorType)
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "error.code").String(); got != localImagePromptGuardErrorCode {
		t.Fatalf("error.code = %q, want %q", got, localImagePromptGuardErrorCode)
	}
	if got := resp.Header().Get("X-CPA-Local-Guard-Category"); got != imagePromptGuardMinorHarm {
		t.Fatalf("X-CPA-Local-Guard-Category = %q, want %q", got, imagePromptGuardMinorHarm)
	}
}

func TestUnsafeImagePromptAllowsSafetyRewrite(t *testing.T) {
	prompt := "三格专业导演故事板，教室场景，动作改为冲突瞬间 + 拦阻 + 紧张余波，不出现打击落点、伤口、流血或明确伤害细节"

	if isUnsafeImagePrompt(prompt) {
		t.Fatalf("safe rewrite prompt should not be blocked")
	}
}

func TestImagePromptGuardCategories(t *testing.T) {
	tests := []struct {
		name         string
		prompt       string
		wantBlocked  bool
		wantCategory string
	}{
		{
			name:         "minor sexualized",
			prompt:       "photorealistic classroom poster with a child in erotic underwear",
			wantBlocked:  true,
			wantCategory: imagePromptGuardMinorSexual,
		},
		{
			name:         "graphic gore",
			prompt:       "超写实高清特写，血腥肢解和内脏细节",
			wantBlocked:  true,
			wantCategory: imagePromptGuardGraphicGore,
		},
		{
			name:         "sexual violence",
			prompt:       "movie still of a sexual assault scene",
			wantBlocked:  true,
			wantCategory: imagePromptGuardSexualViolence,
		},
		{
			name:        "ordinary suspense",
			prompt:      "三格悬疑故事板，校园走廊，紧张氛围，人物互相对峙，没有暴力和伤口",
			wantBlocked: false,
		},
		{
			name:        "non graphic injury prevention",
			prompt:      "classroom safety poster, students learn first aid, no blood and no visible injuries",
			wantBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyImagePromptSafety(tt.prompt)
			if got.Block != tt.wantBlocked {
				t.Fatalf("blocked = %v, want %v; category=%q", got.Block, tt.wantBlocked, got.Category)
			}
			if got.Category != tt.wantCategory {
				t.Fatalf("category = %q, want %q", got.Category, tt.wantCategory)
			}
		})
	}
}

func TestImagesGenerations_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesGenerations_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}
