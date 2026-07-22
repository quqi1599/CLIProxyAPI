package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultImagesMainModel      = "gpt-5.4-mini"
	gptImage15Model             = "gpt-image-1.5"
	defaultImagesToolModel      = "gpt-image-2"
	defaultXAIImagesModel       = "grok-imagine-image"
	xaiImagesQualityModel       = "grok-imagine-image-quality"
	xaiImagesHandlerType        = "openai-image"
	xaiImagesDefaultAspectRatio = "1:1"
	xaiImagesDefaultResolution  = "1k"
	imagesGenerationsPath       = "/v1/images/generations"
	imagesEditsPath             = "/v1/images/edits"
	maxImagesEditReferences     = 16
)

var openAICompatImagesMaxUploadFileBytes int64 = 20 << 20
var openAICompatImagesMaxMultipartBodyBytes int64 = 128 << 20

type imageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

type imageURLPayload struct {
	ImageURL string `json:"image_url"`
}

type imageGenerationToolPayload struct {
	Type              string           `json:"type"`
	Action            string           `json:"action"`
	Model             string           `json:"model"`
	Size              string           `json:"size,omitempty"`
	Quality           string           `json:"quality,omitempty"`
	Background        string           `json:"background,omitempty"`
	OutputFormat      string           `json:"output_format,omitempty"`
	InputFidelity     string           `json:"input_fidelity,omitempty"`
	Moderation        string           `json:"moderation,omitempty"`
	OutputCompression *int64           `json:"output_compression,omitempty"`
	PartialImages     *int64           `json:"partial_images,omitempty"`
	InputImageMask    *imageURLPayload `json:"input_image_mask,omitempty"`
}

type sseFrameAccumulator struct {
	pending []byte
}

type xaiImageResult struct {
	B64JSON       string
	URL           string
	RevisedPrompt string
	MimeType      string
}

type imagesStreamExecutionResult struct {
	Data            <-chan []byte
	UpstreamHeaders http.Header
	Errs            <-chan *interfaces.ErrorMessage
}

func setImagesSSEHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
}

func (h *OpenAIAPIHandler) newImagesStreamKeepAliveTicker() (*time.Ticker, <-chan time.Time) {
	if h == nil || h.BaseAPIHandler == nil {
		return nil, nil
	}
	interval := handlers.StreamingKeepAliveInterval(h.Cfg)
	if interval <= 0 {
		return nil, nil
	}
	ticker := time.NewTicker(interval)
	return ticker, ticker.C
}

func writeImagesStreamKeepAlive(c *gin.Context, flusher http.Flusher) {
	_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
	flusher.Flush()
}

func writeImagesStreamErrorEvent(c *gin.Context, errMsg *interfaces.ErrorMessage) {
	if errMsg == nil {
		return
	}
	status := http.StatusInternalServerError
	if errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
		errText = errMsg.Error.Error()
	}
	body := handlers.BuildErrorResponseBody(status, errText)
	_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", string(body))
}

func (h *OpenAIAPIHandler) waitImagesStreamExecution(c *gin.Context, flusher http.Flusher, execute func() imagesStreamExecutionResult) (imagesStreamExecutionResult, bool, bool) {
	resultChan := make(chan imagesStreamExecutionResult, 1)
	go func() {
		resultChan <- execute()
	}()

	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	defer func() {
		if keepAlive != nil {
			keepAlive.Stop()
		}
	}()

	streamStarted := false
	for {
		select {
		case <-c.Request.Context().Done():
			return imagesStreamExecutionResult{}, streamStarted, true
		case result := <-resultChan:
			return result, streamStarted, false
		case <-keepAliveC:
			setImagesSSEHeaders(c)
			writeImagesStreamKeepAlive(c, flusher)
			streamStarted = true
		}
	}
}

func (a *sseFrameAccumulator) AddChunk(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}

	if responsesSSENeedsLineBreak(a.pending, chunk) {
		a.pending = append(a.pending, '\n')
	}
	a.pending = append(a.pending, chunk...)

	var frames [][]byte
	for {
		frameLen := responsesSSEFrameLen(a.pending)
		if frameLen == 0 {
			break
		}
		frames = append(frames, a.pending[:frameLen])
		copy(a.pending, a.pending[frameLen:])
		a.pending = a.pending[:len(a.pending)-frameLen]
	}

	if len(bytes.TrimSpace(a.pending)) == 0 {
		a.pending = a.pending[:0]
		return frames
	}
	if len(a.pending) == 0 || !responsesSSECanEmitWithoutDelimiter(a.pending) {
		return frames
	}
	frames = append(frames, a.pending)
	a.pending = a.pending[:0]
	return frames
}

func (a *sseFrameAccumulator) Flush() [][]byte {
	if len(a.pending) == 0 {
		return nil
	}

	var frames [][]byte
	for {
		frameLen := responsesSSEFrameLen(a.pending)
		if frameLen == 0 {
			break
		}
		frames = append(frames, a.pending[:frameLen])
		copy(a.pending, a.pending[frameLen:])
		a.pending = a.pending[:len(a.pending)-frameLen]
	}

	if len(bytes.TrimSpace(a.pending)) == 0 {
		a.pending = nil
		return frames
	}
	if responsesSSECanEmitWithoutDelimiter(a.pending) {
		frames = append(frames, a.pending)
	}
	a.pending = nil
	return frames
}

func imagesModelParts(model string) (prefix string, baseModel string) {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[:idx]), strings.TrimSpace(model[idx+1:])
	}
	return "", model
}

func imagesModelBase(model string) string {
	_, baseModel := imagesModelParts(model)
	return strings.ToLower(strings.TrimSpace(baseModel))
}

func isXAIImagesModel(model string) bool {
	prefix, baseModel := imagesModelParts(model)
	baseModel = strings.ToLower(strings.TrimSpace(baseModel))
	if baseModel != defaultXAIImagesModel && baseModel != xaiImagesQualityModel {
		return false
	}

	prefix = strings.ToLower(strings.TrimSpace(prefix))
	return prefix == "" || prefix == "xai" || prefix == "x-ai" || prefix == "grok"
}

func isSupportedImagesModel(model string) bool {
	if isCodexImagesToolModel(model) {
		return true
	}
	return isXAIImagesModel(model) || isOpenAICompatImagesModel(model)
}

func isCodexImagesToolModel(model string) bool {
	baseModel := imagesModelBase(model)
	return baseModel == gptImage15Model || baseModel == defaultImagesToolModel
}

func isOpenAICompatImagesModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	info := registry.LookupModelInfo(model)
	return info != nil && info.Type == registry.OpenAIImageModelType
}

func rejectUnsupportedImagesModel(c *gin.Context, model string) bool {
	if isSupportedImagesModel(model) {
		return false
	}

	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Model %s is not supported on %s or %s. Use %s, %s, %s, %s, or a configured openai-compatibility image model.", model, imagesGenerationsPath, imagesEditsPath, gptImage15Model, defaultImagesToolModel, defaultXAIImagesModel, xaiImagesQualityModel),
			Type:    "invalid_request_error",
		},
	})
	return true
}

func imagesPromptRequiredMessage(model string, endpointPath string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		model = defaultImagesToolModel
	}
	endpointPath = strings.TrimSpace(endpointPath)
	if endpointPath == "" {
		endpointPath = imagesGenerationsPath
	}

	return fmt.Sprintf("Invalid request: prompt is required on %s. %s needs a non-empty prompt that describes what image to create or how to edit the input image. For image generation, send JSON to POST %s with fields model=%s and prompt. For image edit/reference-image tasks, send multipart/form-data to POST %s with fields model=%s, prompt, and image file(s). This is a request-parameter issue, not a channel or model outage.", endpointPath, model, imagesGenerationsPath, model, imagesEditsPath, model)
}

func normalizeImagesResponseFormat(responseFormat string) string {
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		return "url"
	}
	return "b64_json"
}

func shouldStreamImagesRequest(c *gin.Context, requested bool) bool {
	if !requested || c == nil {
		return false
	}
	for _, part := range strings.Split(c.GetHeader("Accept"), ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/event-stream") {
			return true
		}
	}
	return false
}

func canonicalXAIImagesModel(model string) string {
	baseModel := imagesModelBase(model)
	if baseModel == xaiImagesQualityModel {
		return xaiImagesQualityModel
	}
	return defaultXAIImagesModel
}

func xaiImagesAspectRatio(raw string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1:1", "square":
		return "1:1"
	case "16:9", "landscape":
		return "16:9"
	case "9:16", "portrait":
		return "9:16"
	case "4:3":
		return "4:3"
	case "3:4":
		return "3:4"
	case "3:2":
		return "3:2"
	case "2:3":
		return "2:3"
	default:
		return fallback
	}
}

func xaiImagesAspectRatioFromSize(size string, fallback string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	switch size {
	case "1024x1024", "2048x2048", "1:1":
		return "1:1"
	case "1792x1024", "16:9":
		return "16:9"
	case "1024x1792", "9:16":
		return "9:16"
	case "1536x1024", "3:2":
		return "3:2"
	case "1024x1536", "2:3":
		return "2:3"
	default:
		return fallback
	}
}

func xaiImagesResolution(raw string, size string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1k", "2k":
		return strings.ToLower(strings.TrimSpace(raw))
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(size)), "2048") {
		return "2k"
	}
	return fallback
}

func xaiImagesRef(imageURL string) []byte {
	ref := []byte(`{"type":"image_url","url":""}`)
	ref, _ = sjson.SetBytes(ref, "url", strings.TrimSpace(imageURL))
	return ref
}

func buildXAIImagesBaseRequest(model string, prompt string, responseFormat string, aspectRatio string, resolution string, n int64) []byte {
	req := []byte(`{}`)
	req, _ = sjson.SetBytes(req, "model", canonicalXAIImagesModel(model))
	req, _ = sjson.SetBytes(req, "prompt", strings.TrimSpace(prompt))
	req, _ = sjson.SetBytes(req, "response_format", normalizeImagesResponseFormat(responseFormat))
	if aspectRatio != "" {
		req, _ = sjson.SetBytes(req, "aspect_ratio", aspectRatio)
	}
	if resolution != "" {
		req, _ = sjson.SetBytes(req, "resolution", resolution)
	}
	if n > 0 {
		req, _ = sjson.SetBytes(req, "n", n)
	}
	return req
}

func buildXAIImagesGenerationsRequest(rawJSON []byte, model string, responseFormat string) []byte {
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	size := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String())
	aspectRatio := xaiImagesAspectRatio(gjson.GetBytes(rawJSON, "aspect_ratio").String(), "")
	aspectRatio = xaiImagesAspectRatioFromSize(size, aspectRatio)
	if aspectRatio == "" {
		aspectRatio = xaiImagesDefaultAspectRatio
	}
	resolution := xaiImagesResolution(gjson.GetBytes(rawJSON, "resolution").String(), size, xaiImagesDefaultResolution)
	n := int64(0)
	if v := gjson.GetBytes(rawJSON, "n"); v.Exists() && v.Type == gjson.Number {
		n = v.Int()
	}
	return buildXAIImagesBaseRequest(model, prompt, responseFormat, aspectRatio, resolution, n)
}

func buildXAIImagesEditRequest(model string, prompt string, images []string, responseFormat string, aspectRatio string, resolution string, n int64) []byte {
	req := buildXAIImagesBaseRequest(model, prompt, responseFormat, aspectRatio, resolution, n)
	imageRefs := make([][]byte, 0, min(len(images), maxImagesEditReferences))
	for _, img := range images {
		if strings.TrimSpace(img) != "" {
			imageRefs = append(imageRefs, xaiImagesRef(strings.TrimSpace(img)))
		}
	}
	if len(imageRefs) == 1 {
		req, _ = sjson.SetRawBytes(req, "image", imageRefs[0])
		return req
	}
	if len(imageRefs) > 0 {
		req, _ = sjson.SetRawBytes(req, "images", internalpayload.BuildRaw(imageRefs))
	}
	return req
}

func collectXAIImagesFromJSON(rawJSON []byte) ([]string, error) {
	images := make([]string, 0, maxImagesEditReferences)
	appendImage := func(url string) bool {
		url = normalizeImageReference(url)
		if url == "" {
			return true
		}
		if len(images) == maxImagesEditReferences {
			return false
		}
		images = append(images, url)
		return true
	}
	appendObject := func(image gjson.Result) bool {
		if image.Type == gjson.String {
			return appendImage(image.String())
		}
		if image.Type != gjson.JSON {
			return true
		}
		for _, url := range []string{
			image.Get("image_url.url").String(),
			func() string {
				if imageURL := image.Get("image_url"); imageURL.Type == gjson.String {
					return imageURL.String()
				}
				return ""
			}(),
			image.Get("url").String(),
			image.Get("data_url").String(),
			image.Get("b64_json").String(),
			image.Get("base64").String(),
		} {
			if !appendImage(url) {
				return false
			}
		}
		return true
	}

	if image := gjson.GetBytes(rawJSON, "image"); image.Exists() {
		if !appendObject(image) {
			return nil, fmt.Errorf("image edits support at most %d image references", maxImagesEditReferences)
		}
	}
	if imagesResult := gjson.GetBytes(rawJSON, "images"); imagesResult.IsArray() {
		overflow := false
		imagesResult.ForEach(func(_, img gjson.Result) bool {
			if !appendObject(img) {
				overflow = true
				return false
			}
			return true
		})
		if overflow {
			return nil, fmt.Errorf("image edits support at most %d image references", maxImagesEditReferences)
		}
	}
	return images, nil
}

func xaiImagesEditOptionsFromJSON(rawJSON []byte) (aspectRatio string, resolution string, n int64) {
	size := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String())
	aspectRatio = xaiImagesAspectRatio(gjson.GetBytes(rawJSON, "aspect_ratio").String(), "")
	aspectRatio = xaiImagesAspectRatioFromSize(size, aspectRatio)
	resolution = xaiImagesResolution(gjson.GetBytes(rawJSON, "resolution").String(), size, "")
	if v := gjson.GetBytes(rawJSON, "n"); v.Exists() && v.Type == gjson.Number {
		n = v.Int()
	}
	return aspectRatio, resolution, n
}

func mimeTypeFromOutputFormat(outputFormat string) string {
	if outputFormat == "" {
		return "image/png"
	}
	if strings.Contains(outputFormat, "/") {
		return outputFormat
	}
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func normalizeImageReference(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if normalized, ok := normalizeImageDataURL(raw); ok {
		return normalized
	}
	if mediaType, ok := detectBase64ImageMediaType(raw); ok {
		return "data:" + mediaType + ";base64," + compactBase64(raw)
	}
	return raw
}

func normalizeImageDataURL(raw string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "data:") {
		return "", false
	}
	comma := strings.Index(raw, ",")
	if comma < 0 {
		return raw, true
	}
	meta := strings.TrimSpace(raw[len("data:"):comma])
	data := compactBase64(raw[comma+1:])
	if data == "" {
		return raw, true
	}
	parts := strings.Split(meta, ";")
	mediaType := strings.ToLower(strings.TrimSpace(parts[0]))
	isBase64 := false
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			isBase64 = true
			break
		}
	}
	if !isBase64 {
		return raw, true
	}
	if strings.HasPrefix(mediaType, "image/") {
		return "data:" + mediaType + ";base64," + data, true
	}
	if detected, ok := detectBase64ImageMediaType(data); ok {
		return "data:" + detected + ";base64," + data, true
	}
	return raw, true
}

func compactBase64(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch r {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func detectBase64ImageMediaType(raw string) (string, bool) {
	compact := compactBase64(raw)
	if len(compact) < 8 || strings.Contains(compact, "://") {
		return "", false
	}
	sample := compact
	if len(sample) > 512 {
		sample = sample[:512]
	}
	if rem := len(sample) % 4; rem != 0 {
		sample = sample[:len(sample)-rem]
	}
	if len(sample) < 8 {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(sample)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(sample, "="))
	}
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	mediaType := strings.ToLower(strings.TrimSpace(http.DetectContentType(decoded)))
	if strings.HasPrefix(mediaType, "image/") {
		return mediaType, true
	}
	return "", false
}

func imageMediaTypeFromData(data []byte, rawMediaType string) (string, error) {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(rawMediaType, ";", 2)[0]))
	if strings.HasPrefix(mediaType, "image/") {
		return mediaType, nil
	}
	detected := strings.ToLower(strings.TrimSpace(http.DetectContentType(data)))
	if strings.HasPrefix(detected, "image/") {
		return detected, nil
	}
	if mediaType == "" {
		mediaType = detected
	}
	return "", fmt.Errorf("unsupported image MIME type %q; upload an image file or send a data URL like data:image/png;base64,...", mediaType)
}

func multipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	data, err := readMultipartFileHeaderWithLimit(fileHeader, openAICompatImagesMaxUploadFileBytes)
	if err != nil {
		return "", err
	}

	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	normalizedMediaType, errMedia := imageMediaTypeFromData(data, mediaType)
	if errMedia != nil {
		return "", errMedia
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	return "data:" + normalizedMediaType + ";base64," + b64, nil
}

func readMultipartFileHeaderWithLimit(fileHeader *multipart.FileHeader, maxBytes int64) ([]byte, error) {
	if fileHeader == nil {
		return nil, fmt.Errorf("upload file is nil")
	}
	if maxBytes > 0 && fileHeader.Size > maxBytes {
		return nil, fmt.Errorf("upload file %q exceeds %d bytes: %w", fileHeader.Filename, maxBytes, handlers.NewRequestBodyLimitError(maxBytes, false))
	}
	f, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("open upload file failed: %w", err)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("openai images: close upload file error: %v", errClose)
		}
	}()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upload file failed: %w", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("upload file %q exceeds %d bytes: %w", fileHeader.Filename, maxBytes, handlers.NewRequestBodyLimitError(maxBytes, false))
	}
	return data, nil
}

func buildOpenAICompatImagesJSONRequest(rawJSON []byte, imageModel string, stream bool) []byte {
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return rawJSON
	}
	model := strings.TrimSpace(imageModel)
	payload := make([]byte, 0, len(rawJSON)+len(model)+24)
	payload = append(payload, '{')
	first := true
	modelWritten := false
	streamWritten := false
	appendField := func(name string, rawValue string) {
		if !first {
			payload = append(payload, ',')
		}
		first = false
		payload = strconv.AppendQuote(payload, name)
		payload = append(payload, ':')
		payload = append(payload, rawValue...)
	}
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		switch name {
		case "tool_choice", "tools", "parallel_tool_calls":
			return true
		case "model":
			modelWritten = true
			if model != "" {
				encoded := strconv.AppendQuote(nil, model)
				appendField(name, string(encoded))
				return true
			}
		case "stream":
			if !stream {
				return true
			}
			streamWritten = true
			appendField(name, "true")
			return true
		}
		appendField(name, value.Raw)
		return true
	})
	if model != "" && !modelWritten {
		appendField("model", string(strconv.AppendQuote(nil, model)))
	}
	if stream && !streamWritten {
		appendField("stream", "true")
	}
	return append(payload, '}')
}

func buildOpenAICompatImagesEditJSONRequestFromMultipart(form *multipart.Form, imageModel string, prompt string, images []string, mask string, stream bool) []byte {
	type request struct {
		Model             string            `json:"model"`
		Prompt            string            `json:"prompt"`
		Images            []imageURLPayload `json:"images,omitempty"`
		Mask              *imageURLPayload  `json:"mask,omitempty"`
		Size              string            `json:"size,omitempty"`
		Quality           string            `json:"quality,omitempty"`
		Background        string            `json:"background,omitempty"`
		OutputFormat      string            `json:"output_format,omitempty"`
		InputFidelity     string            `json:"input_fidelity,omitempty"`
		Moderation        string            `json:"moderation,omitempty"`
		ResponseFormat    string            `json:"response_format,omitempty"`
		OutputCompression *int64            `json:"output_compression,omitempty"`
		PartialImages     *int64            `json:"partial_images,omitempty"`
		Stream            bool              `json:"stream,omitempty"`
	}
	payload := request{Model: imageModel, Prompt: prompt, Stream: stream}
	payload.Images = make([]imageURLPayload, 0, min(len(images), maxImagesEditReferences))
	for _, img := range images {
		if strings.TrimSpace(img) == "" {
			continue
		}
		payload.Images = append(payload.Images, imageURLPayload{ImageURL: img})
	}
	if strings.TrimSpace(mask) != "" {
		payload.Mask = &imageURLPayload{ImageURL: mask}
	}
	if form != nil {
		payload.Size = openAICompatImagesFormValue(form, "size")
		payload.Quality = openAICompatImagesFormValue(form, "quality")
		payload.Background = openAICompatImagesFormValue(form, "background")
		payload.OutputFormat = openAICompatImagesFormValue(form, "output_format")
		payload.InputFidelity = openAICompatImagesFormValue(form, "input_fidelity")
		payload.Moderation = openAICompatImagesFormValue(form, "moderation")
		payload.ResponseFormat = openAICompatImagesFormValue(form, "response_format")
		payload.OutputCompression = parseOptionalImageInteger(openAICompatImagesFormValue(form, "output_compression"))
		payload.PartialImages = parseOptionalImageInteger(openAICompatImagesFormValue(form, "partial_images"))
	}
	out, _ := json.Marshal(payload)
	return out
}

func parseOptionalImageInteger(value string) *int64 {
	parsed, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if errParse != nil {
		return nil
	}
	return &parsed
}

func openAICompatImagesFormValue(form *multipart.Form, key string) string {
	if form == nil || len(form.Value[key]) == 0 {
		return ""
	}
	return strings.TrimSpace(form.Value[key][0])
}

var openAICompatImagesToolControlFields = []string{
	"tool_choice",
	"tools",
	"parallel_tool_calls",
}

func isOpenAICompatImagesToolControlField(key string) bool {
	key = strings.TrimSpace(strings.ToLower(key))
	for _, field := range openAICompatImagesToolControlFields {
		if key == field || strings.HasPrefix(key, field+".") || strings.HasPrefix(key, field+"[") {
			return true
		}
	}
	return false
}

func cloneMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func buildOpenAICompatImagesMultipartRequest(form *multipart.Form, imageModel string, stream bool) ([]byte, string, error) {
	if form == nil {
		return nil, "", fmt.Errorf("multipart form is nil")
	}
	if totalBytes := multipartFileHeaderTotalSize(form.File); openAICompatImagesMaxMultipartBodyBytes > 0 && totalBytes > openAICompatImagesMaxMultipartBodyBytes {
		return nil, "", fmt.Errorf("multipart upload exceeds %d bytes: %w", openAICompatImagesMaxMultipartBodyBytes, handlers.NewRequestBodyLimitError(openAICompatImagesMaxMultipartBodyBytes, false))
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	var copiedTotal int64

	if errWrite := writer.WriteField("model", imageModel); errWrite != nil {
		return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		if isOpenAICompatImagesToolControlField(key) {
			continue
		}
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}

	for key, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			if openAICompatImagesMaxUploadFileBytes > 0 && fileHeader.Size > openAICompatImagesMaxUploadFileBytes {
				return nil, "", fmt.Errorf("upload file %q exceeds %d bytes: %w", fileHeader.Filename, openAICompatImagesMaxUploadFileBytes, handlers.NewRequestBodyLimitError(openAICompatImagesMaxUploadFileBytes, false))
			}
			header := cloneMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			src, errOpen := fileHeader.Open()
			if errOpen != nil {
				return nil, "", fmt.Errorf("open upload file failed: %w", errOpen)
			}
			written, errCopy := io.Copy(part, io.LimitReader(src, openAICompatImagesMaxUploadFileBytes+1))
			if errClose := src.Close(); errClose != nil {
				log.Errorf("openai images: close upload file error: %v", errClose)
				if errCopy == nil {
					errCopy = errClose
				}
			}
			if errCopy != nil {
				return nil, "", fmt.Errorf("copy upload file failed: %w", errCopy)
			}
			if openAICompatImagesMaxUploadFileBytes > 0 && written > openAICompatImagesMaxUploadFileBytes {
				return nil, "", fmt.Errorf("upload file %q exceeds %d bytes: %w", fileHeader.Filename, openAICompatImagesMaxUploadFileBytes, handlers.NewRequestBodyLimitError(openAICompatImagesMaxUploadFileBytes, false))
			}
			copiedTotal += written
			if openAICompatImagesMaxMultipartBodyBytes > 0 && copiedTotal > openAICompatImagesMaxMultipartBodyBytes {
				return nil, "", fmt.Errorf("multipart upload exceeds %d bytes: %w", openAICompatImagesMaxMultipartBodyBytes, handlers.NewRequestBodyLimitError(openAICompatImagesMaxMultipartBodyBytes, false))
			}
		}
	}

	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func multipartFileHeaderTotalSize(files map[string][]*multipart.FileHeader) int64 {
	var total int64
	for _, list := range files {
		for _, fileHeader := range list {
			if fileHeader == nil || fileHeader.Size <= 0 {
				continue
			}
			total += fileHeader.Size
		}
	}
	return total
}

func parseIntField(raw string, fallback int64) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

func parseBoolField(raw string, fallback bool) bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func (h *OpenAIAPIHandler) ImagesGenerations(c *gin.Context) {
	if h != nil && h.BaseAPIHandler != nil && h.BaseAPIHandler.Cfg != nil && h.BaseAPIHandler.Cfg.DisableImageGeneration == internalconfig.DisableImageGenerationAll {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		handlers.WriteRequestBodyError(c, err)
		return
	}
	if !json.Valid(rawJSON) {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: body must be valid JSON",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if rejectUnsupportedImagesModel(c, imageModel) {
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: imagesPromptRequiredMessage(imageModel, imagesGenerationsPath),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if rejectUnsafeImagePrompt(c, prompt) {
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawJSON, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := shouldStreamImagesRequest(c, gjson.GetBytes(rawJSON, "stream").Bool())
	if isMiniMaxImageModelName(imageModel) {
		miniMaxReq, _, err := buildMiniMaxImageGenerationRequest(rawJSON, imageModel, prompt, nil)
		if err != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		if stream {
			h.streamMiniMaxImages(c, miniMaxReq, imageModel, "image_generation")
			return
		}
		h.collectMiniMaxImages(c, miniMaxReq, imageModel)
		return
	}

	if isCodexImagesToolModel(imageModel) {
		imageReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleRoutedImages(c, imageReq, imageModel, stream)
		return
	}
	if isXAIImagesModel(imageModel) {
		xaiReq := buildXAIImagesGenerationsRequest(rawJSON, imageModel, responseFormat)
		h.handleXAIImages(c, xaiReq, responseFormat, "image_generation", stream)
		return
	}
	if isOpenAICompatImagesModel(imageModel) {
		compatReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleOpenAICompatImages(c, compatReq, imageModel, responseFormat, "image_generation", stream)
		return
	}

	tool := []byte(`{"type":"image_generation","action":"generate"}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)

	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "size", v)
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "quality").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "quality", v)
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "background").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "background", v)
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "output_format").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "output_format", v)
	}
	if v := gjson.GetBytes(rawJSON, "output_compression"); v.Exists() {
		if v.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, "output_compression", v.Int())
		}
	}
	if v := gjson.GetBytes(rawJSON, "partial_images"); v.Exists() {
		if v.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, "partial_images", v.Int())
		}
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "moderation").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "moderation", v)
	}

	responsesReq := buildImagesResponsesRequest(prompt, nil, tool)
	if stream {
		h.streamImagesFromResponses(c, responsesReq, responseFormat, "image_generation")
		return
	}
	h.collectImagesFromResponses(c, responsesReq, responseFormat)
}

func (h *OpenAIAPIHandler) ImagesEdits(c *gin.Context) {
	if h != nil && h.BaseAPIHandler != nil && h.BaseAPIHandler.Cfg != nil && h.BaseAPIHandler.Cfg.DisableImageGeneration == internalconfig.DisableImageGenerationAll {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "application/json") {
		h.imagesEditsFromJSON(c)
		return
	}
	if strings.HasPrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesEditsFromMultipart(c)
		return
	}

	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Invalid request: unsupported Content-Type %q", contentType),
			Type:    "invalid_request_error",
		},
	})
}

func (h *OpenAIAPIHandler) imagesEditsFromMultipart(c *gin.Context) {
	form, err := handlers.ParseMultipartFormWithPolicy(c, openAICompatImagesMaxMultipartBodyBytes, 8<<20, openAICompatImagesMaxUploadFileBytes)
	if err != nil {
		writeImagesMultipartRequestError(c, err)
		return
	}
	uploadedFiles := 0
	if form != nil {
		for _, files := range form.File {
			uploadedFiles += len(files)
			if uploadedFiles > maxImagesEditReferences+1 {
				c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: image edits support at most %d images and one mask", maxImagesEditReferences),
					Type:    "invalid_request_error",
				}})
				return
			}
		}
	}

	imageModel := strings.TrimSpace(c.PostForm("model"))
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if rejectUnsupportedImagesModel(c, imageModel) {
		return
	}

	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if prompt == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: imagesPromptRequiredMessage(imageModel, imagesEditsPath),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if rejectUnsafeImagePrompt(c, prompt) {
		return
	}

	var imageFiles []*multipart.FileHeader
	if files := form.File["image[]"]; len(files) > 0 {
		imageFiles = files
	} else if files := form.File["image"]; len(files) > 0 {
		imageFiles = files
	}
	if len(imageFiles) == 0 {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: image is required",
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if len(imageFiles) > maxImagesEditReferences {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("Invalid request: image edits support at most %d images", maxImagesEditReferences),
			Type:    "invalid_request_error",
		}})
		return
	}

	responseFormat := strings.TrimSpace(c.PostForm("response_format"))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := parseBoolField(c.PostForm("stream"), false)

	if isCodexImagesToolModel(imageModel) {
		imageReq, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, imageModel, stream)
		if errBuild != nil {
			writeImagesMultipartRequestError(c, errBuild)
			return
		}
		c.Request.Header.Set("Content-Type", contentType)
		h.handleRoutedImages(c, imageReq, imageModel, stream)
		return
	}
	if !isXAIImagesModel(imageModel) && isOpenAICompatImagesModel(imageModel) {
		compatReq, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, imageModel, stream)
		if errBuild != nil {
			writeImagesMultipartRequestError(c, errBuild)
			return
		}
		c.Request.Header.Set("Content-Type", contentType)
		h.handleOpenAICompatImages(c, compatReq, imageModel, responseFormat, "image_edit", stream)
		return
	}

	images := make([]string, 0, len(imageFiles))
	for _, fh := range imageFiles {
		dataURL, err := multipartFileToDataURL(fh)
		if err != nil {
			writeImagesMultipartRequestError(c, err)
			return
		}
		images = append(images, dataURL)
	}

	if isXAIImagesModel(imageModel) {
		aspectRatio := xaiImagesAspectRatio(c.PostForm("aspect_ratio"), "")
		aspectRatio = xaiImagesAspectRatioFromSize(c.PostForm("size"), aspectRatio)
		resolution := xaiImagesResolution(c.PostForm("resolution"), c.PostForm("size"), "")
		n := parseIntField(c.PostForm("n"), 0)
		xaiReq := buildXAIImagesEditRequest(imageModel, prompt, images, responseFormat, aspectRatio, resolution, n)
		h.handleXAIImages(c, xaiReq, responseFormat, "image_edit", stream)
		return
	}
	var maskDataURL *string
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, err := multipartFileToDataURL(maskFiles[0])
		if err != nil {
			writeImagesMultipartRequestError(c, err)
			return
		}
		maskDataURL = &dataURL
	}

	tool := []byte(`{"type":"image_generation","action":"edit"}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)

	if v := strings.TrimSpace(c.PostForm("size")); v != "" {
		tool, _ = sjson.SetBytes(tool, "size", v)
	}
	if v := strings.TrimSpace(c.PostForm("quality")); v != "" {
		tool, _ = sjson.SetBytes(tool, "quality", v)
	}
	if v := strings.TrimSpace(c.PostForm("background")); v != "" {
		tool, _ = sjson.SetBytes(tool, "background", v)
	}
	if v := strings.TrimSpace(c.PostForm("output_format")); v != "" {
		tool, _ = sjson.SetBytes(tool, "output_format", v)
	}
	if v := strings.TrimSpace(c.PostForm("input_fidelity")); v != "" {
		tool, _ = sjson.SetBytes(tool, "input_fidelity", v)
	}
	if v := strings.TrimSpace(c.PostForm("moderation")); v != "" {
		tool, _ = sjson.SetBytes(tool, "moderation", v)
	}

	if v := strings.TrimSpace(c.PostForm("output_compression")); v != "" {
		tool, _ = sjson.SetBytes(tool, "output_compression", parseIntField(v, 0))
	}
	if v := strings.TrimSpace(c.PostForm("partial_images")); v != "" {
		tool, _ = sjson.SetBytes(tool, "partial_images", parseIntField(v, 0))
	}

	if maskDataURL != nil && strings.TrimSpace(*maskDataURL) != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", strings.TrimSpace(*maskDataURL))
	}

	responsesReq := buildImagesResponsesRequest(prompt, images, tool)
	if stream {
		h.streamImagesFromResponses(c, responsesReq, responseFormat, "image_edit")
		return
	}
	h.collectImagesFromResponses(c, responsesReq, responseFormat)
}

func writeImagesMultipartRequestError(c *gin.Context, err error) {
	handlers.WriteRequestBodyError(c, err)
}

func (h *OpenAIAPIHandler) imagesEditsFromJSON(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		handlers.WriteRequestBodyError(c, err)
		return
	}
	if !json.Valid(rawJSON) {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: body must be valid JSON",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if rejectUnsupportedImagesModel(c, imageModel) {
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: imagesPromptRequiredMessage(imageModel, imagesEditsPath),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if rejectUnsafeImagePrompt(c, prompt) {
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawJSON, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := shouldStreamImagesRequest(c, gjson.GetBytes(rawJSON, "stream").Bool())
	images, errImages := collectXAIImagesFromJSON(rawJSON)
	if errImages != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", errImages),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	if isCodexImagesToolModel(imageModel) {
		imageReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleRoutedImages(c, imageReq, imageModel, stream)
		return
	}
	if isXAIImagesModel(imageModel) {
		if len(images) == 0 {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: image is required",
					Type:    "invalid_request_error",
				},
			})
			return
		}
		aspectRatio, resolution, n := xaiImagesEditOptionsFromJSON(rawJSON)
		xaiReq := buildXAIImagesEditRequest(imageModel, prompt, images, responseFormat, aspectRatio, resolution, n)
		h.handleXAIImages(c, xaiReq, responseFormat, "image_edit", stream)
		return
	}
	if isMiniMaxImageModelName(imageModel) {
		if len(images) == 0 {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: image is required",
					Type:    "invalid_request_error",
				},
			})
			return
		}
		miniMaxReq, _, errBuild := buildMiniMaxImageGenerationRequest(rawJSON, imageModel, prompt, images)
		if errBuild != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", errBuild),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		if stream {
			h.streamMiniMaxImages(c, miniMaxReq, imageModel, "image_edit")
			return
		}
		h.collectMiniMaxImages(c, miniMaxReq, imageModel)
		return
	}
	if isOpenAICompatImagesModel(imageModel) {
		compatReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleOpenAICompatImages(c, compatReq, imageModel, responseFormat, "image_edit", stream)
		return
	}
	if len(images) == 0 {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: images[].image_url is required (file_id is not supported)",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	var maskDataURL *string
	if mask := gjson.GetBytes(rawJSON, "mask.image_url"); mask.Exists() {
		url := strings.TrimSpace(mask.String())
		if url != "" {
			maskDataURL = &url
		}
	} else if mask := gjson.GetBytes(rawJSON, "mask.file_id"); mask.Exists() {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: mask.file_id is not supported (use mask.image_url instead)",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	toolPayload := imageGenerationToolPayload{
		Type:          "image_generation",
		Action:        "edit",
		Model:         imageModel,
		Size:          strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String()),
		Quality:       strings.TrimSpace(gjson.GetBytes(rawJSON, "quality").String()),
		Background:    strings.TrimSpace(gjson.GetBytes(rawJSON, "background").String()),
		OutputFormat:  strings.TrimSpace(gjson.GetBytes(rawJSON, "output_format").String()),
		InputFidelity: strings.TrimSpace(gjson.GetBytes(rawJSON, "input_fidelity").String()),
		Moderation:    strings.TrimSpace(gjson.GetBytes(rawJSON, "moderation").String()),
	}
	if value := gjson.GetBytes(rawJSON, "output_compression"); value.Exists() && value.Type == gjson.Number {
		parsed := value.Int()
		toolPayload.OutputCompression = &parsed
	}
	if value := gjson.GetBytes(rawJSON, "partial_images"); value.Exists() && value.Type == gjson.Number {
		parsed := value.Int()
		toolPayload.PartialImages = &parsed
	}
	if maskDataURL != nil && strings.TrimSpace(*maskDataURL) != "" {
		toolPayload.InputImageMask = &imageURLPayload{ImageURL: strings.TrimSpace(*maskDataURL)}
	}
	tool, _ := json.Marshal(toolPayload)

	responsesReq := buildImagesResponsesRequest(prompt, images, tool)
	if stream {
		h.streamImagesFromResponses(c, responsesReq, responseFormat, "image_edit")
		return
	}
	h.collectImagesFromResponses(c, responsesReq, responseFormat)
}

func buildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	mainModel := defaultImagesMainModel
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		toolModel := strings.TrimSpace(gjson.GetBytes(toolJSON, "model").String())
		if idx := strings.LastIndex(toolModel, "/"); idx > 0 && idx < len(toolModel)-1 {
			prefix := strings.TrimSpace(toolModel[:idx])
			if prefix != "" {
				mainModel = prefix + "/" + defaultImagesMainModel
			}
		}
	}
	req, _ = sjson.SetBytes(req, "model", mainModel)

	inputText := []byte(`{"type":"input_text","text":""}`)
	inputText, _ = sjson.SetBytes(inputText, "text", prompt)
	content := make([][]byte, 0, min(len(images), maxImagesEditReferences)+1)
	content = append(content, inputText)
	for _, img := range images {
		if strings.TrimSpace(img) == "" {
			continue
		}
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", img)
		content = append(content, part)
	}
	message := []byte(`{"type":"message","role":"user","content":[]}`)
	message, _ = sjson.SetRawBytes(message, "content", internalpayload.BuildRaw(content))
	req, _ = sjson.SetRawBytes(req, "input", internalpayload.BuildRaw([][]byte{message}))

	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		req, _ = sjson.SetRawBytes(req, "tools", internalpayload.BuildRaw([][]byte{toolJSON}))
	} else {
		req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	}
	return req
}

func extractXAIImagesResponse(payload []byte) (results []xaiImageResult, createdAt int64, usageRaw []byte, err error) {
	if !json.Valid(payload) {
		return nil, 0, nil, fmt.Errorf("upstream returned invalid image response JSON")
	}

	createdAt = gjson.GetBytes(payload, "created").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	data := gjson.GetBytes(payload, "data")
	if data.IsArray() {
		for _, item := range data.Array() {
			result := xaiImageResult{
				B64JSON:       strings.TrimSpace(item.Get("b64_json").String()),
				URL:           strings.TrimSpace(item.Get("url").String()),
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				MimeType:      strings.TrimSpace(item.Get("mime_type").String()),
			}
			if result.MimeType == "" {
				result.MimeType = mimeTypeFromOutputFormat(strings.TrimSpace(item.Get("output_format").String()))
			}
			if result.MimeType == "" {
				result.MimeType = "image/png"
			}
			if result.B64JSON == "" && result.URL == "" {
				continue
			}
			results = append(results, result)
		}
	}
	if len(results) == 0 {
		return nil, 0, nil, fmt.Errorf("upstream did not return image output")
	}

	if usage := gjson.GetBytes(payload, "usage"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}

	return results, createdAt, usageRaw, nil
}

func buildImagesAPIResponseFromXAI(payload []byte, responseFormat string) ([]byte, error) {
	results, createdAt, usageRaw, err := extractXAIImagesResponse(payload)
	if err != nil {
		return nil, err
	}

	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)
	responseFormat = normalizeImagesResponseFormat(responseFormat)

	items := make([][]byte, 0, len(results))
	for _, img := range results {
		item := []byte(`{}`)
		if responseFormat == "url" {
			if img.URL != "" {
				item, _ = sjson.SetBytes(item, "url", img.URL)
			} else {
				item, _ = sjson.SetBytes(item, "url", "data:"+mimeTypeFromOutputFormat(img.MimeType)+";base64,"+img.B64JSON)
			}
		} else if img.B64JSON != "" {
			item, _ = sjson.SetBytes(item, "b64_json", img.B64JSON)
		} else {
			item, _ = sjson.SetBytes(item, "url", img.URL)
		}
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		items = append(items, item)
	}
	out, _ = sjson.SetRawBytes(out, "data", internalpayload.BuildRaw(items))

	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}

	return out, nil
}

func (h *OpenAIAPIHandler) handleXAIImages(c *gin.Context, xaiReq []byte, responseFormat string, streamPrefix string, stream bool) {
	handlers.MarkRequestWithoutToolCompatibility(c)
	if stream {
		h.streamXAIImages(c, xaiReq, responseFormat, streamPrefix)
		return
	}
	h.collectXAIImages(c, xaiReq, responseFormat)
}

func (h *OpenAIAPIHandler) handleOpenAICompatImages(c *gin.Context, compatReq []byte, imageModel string, responseFormat string, streamPrefix string, stream bool) {
	handlers.MarkRequestWithoutToolCompatibility(c)
	if stream {
		h.streamOpenAICompatImages(c, compatReq, imageModel)
		return
	}
	h.collectImagesWithModel(c, compatReq, imageModel, responseFormat)
}

func (h *OpenAIAPIHandler) handleRoutedImages(c *gin.Context, imageReq []byte, imageModel string, stream bool) {
	handlers.MarkRequestWithoutToolCompatibility(c)
	if stream {
		h.streamRoutedImages(c, imageReq, imageModel)
		return
	}
	h.collectRoutedImages(c, imageReq, imageModel)
}

func (h *OpenAIAPIHandler) collectRoutedImages(c *gin.Context, imageReq []byte, imageModel string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	model := strings.TrimSpace(imageModel)
	resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, xaiImagesHandlerType, model, imageReq, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel(nil)
}

func (h *OpenAIAPIHandler) streamRoutedImages(c *gin.Context, imageReq []byte, imageModel string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	model := strings.TrimSpace(imageModel)
	execution, streamStarted, canceled := h.waitImagesStreamExecution(c, flusher, func() imagesStreamExecutionResult {
		dataChan, upstreamHeaders, errChan := h.ExecuteImageStreamWithAuthManager(cliCtx, xaiImagesHandlerType, model, imageReq, "")
		return imagesStreamExecutionResult{Data: dataChan, UpstreamHeaders: upstreamHeaders, Errs: errChan}
	})
	if canceled {
		cliCancel(c.Request.Context().Err())
		return
	}
	dataChan := execution.Data
	upstreamHeaders := execution.UpstreamHeaders
	errChan := execution.Errs
	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	stopKeepAlive := func() {
		if keepAlive != nil {
			keepAlive.Stop()
			keepAlive = nil
			keepAliveC = nil
		}
	}
	defer stopKeepAlive()

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if streamStarted {
				writeImagesStreamErrorEvent(c, errMsg)
				flusher.Flush()
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				stopKeepAlive()
				setImagesSSEHeaders(c)
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				cliCancel(nil)
				return
			}

			stopKeepAlive()
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			_, _ = c.Writer.Write(chunk)
			flusher.Flush()
			streamStarted = true
			h.forwardRawImageStream(cliCtx, c, func(err error) { cliCancel(err) }, dataChan, errChan)
			return
		case <-keepAliveC:
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			writeImagesStreamKeepAlive(c, flusher)
			streamStarted = true
		}
	}
}

func (h *OpenAIAPIHandler) forwardRawImageStream(ctx context.Context, c *gin.Context, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	defer func() {
		if keepAlive != nil {
			keepAlive.Stop()
		}
	}()

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case <-ctx.Done():
			cancel(ctx.Err())
			return
		case errMsg, ok := <-errs:
			if ok && errMsg != nil {
				writeImagesStreamErrorEvent(c, errMsg)
				if flusher, ok := c.Writer.(http.Flusher); ok {
					flusher.Flush()
				}
				cancel(errMsg.Error)
				return
			}
			errs = nil
		case chunk, ok := <-data:
			if !ok {
				cancel(nil)
				return
			}
			_, _ = c.Writer.Write(chunk)
			if flusher, ok := c.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-keepAliveC:
			if flusher, ok := c.Writer.(http.Flusher); ok {
				writeImagesStreamKeepAlive(c, flusher)
			}
		}
	}
}

func (h *OpenAIAPIHandler) streamOpenAICompatImages(c *gin.Context, compatReq []byte, imageModel string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	model := strings.TrimSpace(imageModel)
	execution, streamStarted, canceled := h.waitImagesStreamExecution(c, flusher, func() imagesStreamExecutionResult {
		dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, xaiImagesHandlerType, model, compatReq, "")
		return imagesStreamExecutionResult{Data: dataChan, UpstreamHeaders: upstreamHeaders, Errs: errChan}
	})
	if canceled {
		cliCancel(c.Request.Context().Err())
		return
	}
	dataChan := execution.Data
	upstreamHeaders := execution.UpstreamHeaders
	errChan := execution.Errs
	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	stopKeepAlive := func() {
		if keepAlive != nil {
			keepAlive.Stop()
			keepAlive = nil
			keepAliveC = nil
		}
	}
	defer stopKeepAlive()

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if streamStarted {
				writeImagesStreamErrorEvent(c, errMsg)
				flusher.Flush()
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				stopKeepAlive()
				setImagesSSEHeaders(c)
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				flusher.Flush()
				cliCancel(nil)
				return
			}

			stopKeepAlive()
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			_, _ = c.Writer.Write(chunk)
			flusher.Flush()
			streamStarted = true
			h.ForwardStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, handlers.StreamForwardOptions{
				SummaryContext: cliCtx,
				WriteChunk: func(w handlers.StreamBodyWriter, next []byte) {
					_, _ = w.Write(next)
				},
				WriteTerminalError: func(w handlers.StreamBodyWriter, errMsg *interfaces.ErrorMessage) {
					writeImagesStreamErrorEvent(c, errMsg)
				},
			})
			return
		case <-keepAliveC:
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			writeImagesStreamKeepAlive(c, flusher)
			streamStarted = true
		}
	}
}

func (h *OpenAIAPIHandler) collectXAIImages(c *gin.Context, xaiReq []byte, responseFormat string) {
	model := strings.TrimSpace(gjson.GetBytes(xaiReq, "model").String())
	h.collectImagesWithModel(c, xaiReq, model, responseFormat)
}

func (h *OpenAIAPIHandler) collectImagesWithModel(c *gin.Context, imageReq []byte, model string, responseFormat string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	model = strings.TrimSpace(model)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, xaiImagesHandlerType, model, imageReq, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
		return
	}

	out, err := buildImagesAPIResponseFromXAI(resp, responseFormat)
	if err != nil {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
		h.WriteErrorResponse(c, errMsg)
		cliCancel(err)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel(nil)
}

func (h *OpenAIAPIHandler) streamXAIImages(c *gin.Context, xaiReq []byte, responseFormat string, streamPrefix string) {
	model := strings.TrimSpace(gjson.GetBytes(xaiReq, "model").String())
	h.streamImagesWithModel(c, xaiReq, model, responseFormat, streamPrefix)
}

func (h *OpenAIAPIHandler) streamImagesWithModel(c *gin.Context, imageReq []byte, model string, responseFormat string, streamPrefix string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	model = strings.TrimSpace(model)
	type imageStreamResult struct {
		resp            []byte
		upstreamHeaders http.Header
		errMsg          *interfaces.ErrorMessage
	}
	resultChan := make(chan imageStreamResult, 1)
	go func() {
		resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, xaiImagesHandlerType, model, imageReq, "")
		resultChan <- imageStreamResult{resp: resp, upstreamHeaders: upstreamHeaders, errMsg: errMsg}
	}()

	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	stopKeepAlive := func() {
		if keepAlive != nil {
			keepAlive.Stop()
			keepAlive = nil
			keepAliveC = nil
		}
	}
	defer stopKeepAlive()
	streamStarted := false
	writeError := func(errMsg *interfaces.ErrorMessage) {
		if streamStarted {
			writeImagesStreamErrorEvent(c, errMsg)
			flusher.Flush()
		} else {
			h.WriteErrorResponse(c, errMsg)
		}
		if errMsg != nil && errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case <-keepAliveC:
			setImagesSSEHeaders(c)
			writeImagesStreamKeepAlive(c, flusher)
			streamStarted = true
		case result := <-resultChan:
			stopKeepAlive()
			if result.errMsg != nil {
				writeError(result.errMsg)
				return
			}

			results, _, usageRaw, err := extractXAIImagesResponse(result.resp)
			if err != nil {
				writeError(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
				return
			}

			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), result.upstreamHeaders)

			eventName := streamPrefix + ".completed"
			responseFormat = normalizeImagesResponseFormat(responseFormat)
			for _, img := range results {
				data := []byte(`{"type":""}`)
				data, _ = sjson.SetBytes(data, "type", eventName)
				if responseFormat == "url" {
					if img.URL != "" {
						data, _ = sjson.SetBytes(data, "url", img.URL)
					} else {
						data, _ = sjson.SetBytes(data, "url", "data:"+mimeTypeFromOutputFormat(img.MimeType)+";base64,"+img.B64JSON)
					}
				} else if img.B64JSON != "" {
					data, _ = sjson.SetBytes(data, "b64_json", img.B64JSON)
				} else {
					data, _ = sjson.SetBytes(data, "url", img.URL)
				}
				if len(usageRaw) > 0 && json.Valid(usageRaw) {
					data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
				}
				if strings.TrimSpace(eventName) != "" {
					_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
				}
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(data))
				flusher.Flush()
				streamStarted = true
			}
			cliCancel(nil)
			return
		}
	}
}

func (h *OpenAIAPIHandler) collectImagesFromResponses(c *gin.Context, responsesReq []byte, responseFormat string) {
	c.Header("Content-Type", "application/json")
	handlers.MarkBuiltinImageGenerationToolCompatibility(c)

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	mainModel := strings.TrimSpace(gjson.GetBytes(responsesReq, "model").String())
	if mainModel == "" {
		mainModel = defaultImagesMainModel
	}
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, "openai-response", mainModel, responsesReq, "")

	out, errMsg := collectImagesFromResponsesStream(cliCtx, dataChan, errChan, responseFormat)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel()
}

func collectImagesFromResponsesStream(ctx context.Context, data <-chan []byte, errs <-chan *interfaces.ErrorMessage, responseFormat string) ([]byte, *interfaces.ErrorMessage) {
	acc := &sseFrameAccumulator{}

	processFrame := func(frame []byte) ([]byte, bool, *interfaces.ErrorMessage) {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
			if len(trimmed) == 0 {
				continue
			}
			if !bytes.HasPrefix(trimmed, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			if !json.Valid(payload) {
				return nil, false, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("invalid SSE data JSON")}
			}

			if gjson.GetBytes(payload, "type").String() != "response.completed" {
				continue
			}

			results, createdAt, usageRaw, firstMeta, err := extractImagesFromResponsesCompleted(payload)
			if err != nil {
				return nil, false, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
			}
			if len(results) == 0 {
				return nil, false, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("%s", imageEmptyOutputMessage(payload))}
			}
			out, err := buildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, responseFormat)
			if err != nil {
				return nil, false, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}
			}
			return out, true, nil
		}
		return nil, false, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, &interfaces.ErrorMessage{StatusCode: http.StatusRequestTimeout, Error: ctx.Err()}
		case errMsg, ok := <-errs:
			if ok && errMsg != nil {
				return nil, errMsg
			}
			errs = nil
		case chunk, ok := <-data:
			if !ok {
				for _, frame := range acc.Flush() {
					if out, done, errMsg := processFrame(frame); errMsg != nil {
						return nil, errMsg
					} else if done {
						return out, nil
					}
				}
				return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("stream disconnected before completion")}
			}
			for _, frame := range acc.AddChunk(chunk) {
				if out, done, errMsg := processFrame(frame); errMsg != nil {
					return nil, errMsg
				} else if done {
					return out, nil
				}
			}
		}
	}
}

func extractImagesFromResponsesCompleted(payload []byte) (results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, err error) {
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return nil, 0, nil, imageCallResult{}, fmt.Errorf("unexpected event type")
	}

	createdAt = gjson.GetBytes(payload, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		for _, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			res := strings.TrimSpace(item.Get("result").String())
			if res == "" {
				continue
			}
			entry := imageCallResult{
				Result:        res,
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
				Size:          strings.TrimSpace(item.Get("size").String()),
				Background:    strings.TrimSpace(item.Get("background").String()),
				Quality:       strings.TrimSpace(item.Get("quality").String()),
			}
			if len(results) == 0 {
				firstMeta = entry
			}
			results = append(results, entry)
		}
	}

	if usage := gjson.GetBytes(payload, "response.tool_usage.image_gen"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}

	return results, createdAt, usageRaw, firstMeta, nil
}

func imageEmptyOutputMessage(payload []byte) string {
	message := "upstream did not return image output"
	status := strings.TrimSpace(gjson.GetBytes(payload, "response.status").String())
	if gjson.GetBytes(payload, "response.error").Exists() || gjson.GetBytes(payload, "response.status_details.error").Exists() {
		message = "upstream image generation failed"
	} else if status != "" && !strings.EqualFold(status, "completed") {
		message = "upstream image generation did not complete"
	}
	for _, item := range gjson.GetBytes(payload, "response.output").Array() {
		if item.Get("type").String() != "image_generation_call" {
			continue
		}
		if item.Get("error").Exists() || item.Get("status_details.error").Exists() {
			message = "upstream image generation call failed"
			status = strings.TrimSpace(item.Get("status").String())
			break
		}
		itemStatus := strings.TrimSpace(item.Get("status").String())
		if itemStatus != "" && !strings.EqualFold(itemStatus, "completed") {
			message = "upstream image generation call did not complete"
			status = itemStatus
			break
		}
	}
	if message == "upstream did not return image output" && gjson.GetBytes(payload, "response.output.#.content.#.text").Exists() {
		message = "upstream returned text instead of image output"
	}
	switch strings.ToLower(status) {
	case "failed", "incomplete", "cancelled", "canceled":
		message += ": status=" + strings.ToLower(status)
	}
	return message + ": " + internalpayload.SummarizeBodyMetadata(payload, "application/json")
}

func buildImagesAPIResponse(results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, responseFormat string) ([]byte, error) {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)

	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}

	items := make([][]byte, 0, len(results))
	for _, img := range results {
		item := []byte(`{}`)
		if responseFormat == "url" {
			mt := mimeTypeFromOutputFormat(img.OutputFormat)
			item, _ = sjson.SetBytes(item, "url", "data:"+mt+";base64,"+img.Result)
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", img.Result)
		}
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		items = append(items, item)
	}
	out, _ = sjson.SetRawBytes(out, "data", internalpayload.BuildRaw(items))

	if firstMeta.Background != "" {
		out, _ = sjson.SetBytes(out, "background", firstMeta.Background)
	}
	if firstMeta.OutputFormat != "" {
		out, _ = sjson.SetBytes(out, "output_format", firstMeta.OutputFormat)
	}
	if firstMeta.Quality != "" {
		out, _ = sjson.SetBytes(out, "quality", firstMeta.Quality)
	}
	if firstMeta.Size != "" {
		out, _ = sjson.SetBytes(out, "size", firstMeta.Size)
	}

	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}

	return out, nil
}

func (h *OpenAIAPIHandler) streamImagesFromResponses(c *gin.Context, responsesReq []byte, responseFormat string, streamPrefix string) {
	handlers.MarkBuiltinImageGenerationToolCompatibility(c)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	mainModel := strings.TrimSpace(gjson.GetBytes(responsesReq, "model").String())
	if mainModel == "" {
		mainModel = defaultImagesMainModel
	}
	execution, streamStarted, canceled := h.waitImagesStreamExecution(c, flusher, func() imagesStreamExecutionResult {
		dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, "openai-response", mainModel, responsesReq, "")
		return imagesStreamExecutionResult{Data: dataChan, UpstreamHeaders: upstreamHeaders, Errs: errChan}
	})
	if canceled {
		cliCancel(c.Request.Context().Err())
		return
	}
	dataChan := execution.Data
	upstreamHeaders := execution.UpstreamHeaders
	errChan := execution.Errs
	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	stopKeepAlive := func() {
		if keepAlive != nil {
			keepAlive.Stop()
			keepAlive = nil
			keepAliveC = nil
		}
	}
	defer stopKeepAlive()

	writeEvent := func(eventName string, dataJSON []byte) {
		if strings.TrimSpace(eventName) != "" {
			_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(dataJSON))
		flusher.Flush()
	}

	// Peek for the first chunk/error while still allowing configured SSE heartbeats.
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if streamStarted {
				writeImagesStreamErrorEvent(c, errMsg)
				flusher.Flush()
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				stopKeepAlive()
				setImagesSSEHeaders(c)
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				cliCancel(nil)
				return
			}

			stopKeepAlive()
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

			h.forwardImagesStream(cliCtx, c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, chunk, responseFormat, streamPrefix, writeEvent)
			return
		case <-keepAliveC:
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			writeImagesStreamKeepAlive(c, flusher)
			streamStarted = true
		}
	}
}

func (h *OpenAIAPIHandler) forwardImagesStream(ctx context.Context, c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, firstChunk []byte, responseFormat string, streamPrefix string, writeEvent func(string, []byte)) {
	acc := &sseFrameAccumulator{}

	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	defer func() {
		if keepAlive != nil {
			keepAlive.Stop()
		}
	}()

	emitError := func(errMsg *interfaces.ErrorMessage) {
		writeImagesStreamErrorEvent(c, errMsg)
		flusher.Flush()
	}

	processFrame := func(frame []byte) (done bool) {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
			if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
				continue
			}

			switch gjson.GetBytes(payload, "type").String() {
			case "response.image_generation_call.partial_image":
				b64 := strings.TrimSpace(gjson.GetBytes(payload, "partial_image_b64").String())
				if b64 == "" {
					continue
				}
				outputFormat := strings.TrimSpace(gjson.GetBytes(payload, "output_format").String())
				index := gjson.GetBytes(payload, "partial_image_index").Int()
				eventName := streamPrefix + ".partial_image"
				data := []byte(`{"type":"","partial_image_index":0}`)
				data, _ = sjson.SetBytes(data, "type", eventName)
				data, _ = sjson.SetBytes(data, "partial_image_index", index)
				if responseFormat == "url" {
					mt := mimeTypeFromOutputFormat(outputFormat)
					data, _ = sjson.SetBytes(data, "url", "data:"+mt+";base64,"+b64)
				} else {
					data, _ = sjson.SetBytes(data, "b64_json", b64)
				}
				writeEvent(eventName, data)
			case "response.completed":
				results, _, usageRaw, _, err := extractImagesFromResponsesCompleted(payload)
				if err != nil {
					emitError(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
					return true
				}
				if len(results) == 0 {
					emitError(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("%s", imageEmptyOutputMessage(payload))})
					return true
				}
				eventName := streamPrefix + ".completed"
				for _, img := range results {
					data := []byte(`{"type":""}`)
					data, _ = sjson.SetBytes(data, "type", eventName)
					if responseFormat == "url" {
						mt := mimeTypeFromOutputFormat(img.OutputFormat)
						data, _ = sjson.SetBytes(data, "url", "data:"+mt+";base64,"+img.Result)
					} else {
						data, _ = sjson.SetBytes(data, "b64_json", img.Result)
					}
					if len(usageRaw) > 0 && json.Valid(usageRaw) {
						data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
					}
					writeEvent(eventName, data)
				}
				return true
			}
		}
		return false
	}

	for _, frame := range acc.AddChunk(firstChunk) {
		if processFrame(frame) {
			cancel(nil)
			return
		}
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errs:
			if ok && errMsg != nil {
				emitError(errMsg)
				cancel(errMsg.Error)
				return
			}
			errs = nil
		case chunk, ok := <-data:
			if !ok {
				for _, frame := range acc.Flush() {
					if processFrame(frame) {
						cancel(nil)
						return
					}
				}
				cancel(nil)
				return
			}
			for _, frame := range acc.AddChunk(chunk) {
				if processFrame(frame) {
					cancel(nil)
					return
				}
			}
		case <-keepAliveC:
			writeImagesStreamKeepAlive(c, flusher)
		}
	}
}
