package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexOpenAIImageSourceFormat = "openai-image"
	codexImagesGenerationsPath   = "/v1/images/generations"
	codexImagesEditsPath         = "/v1/images/edits"
	codexDirectImagesGenerations = "/images/generations"
	codexDirectImagesEdit        = "/images/edit"
	codexGPTImage15Model         = "gpt-image-1.5"
	codexOpenAIImagesMainModel   = "gpt-5.4-mini"
	codexOpenAIImageMaxAttempts  = 2
)

type codexOpenAIImagePreparedRequest struct {
	Body           []byte
	ResponseFormat string
	StreamPrefix   string
}

type codexImageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

type codexOpenAIImageToolPayload struct {
	Type              string                   `json:"type"`
	Action            string                   `json:"action"`
	Model             string                   `json:"model"`
	Size              string                   `json:"size,omitempty"`
	Quality           string                   `json:"quality,omitempty"`
	Background        string                   `json:"background,omitempty"`
	OutputFormat      string                   `json:"output_format,omitempty"`
	InputFidelity     string                   `json:"input_fidelity,omitempty"`
	Moderation        string                   `json:"moderation,omitempty"`
	OutputCompression *int64                   `json:"output_compression,omitempty"`
	PartialImages     *int64                   `json:"partial_images,omitempty"`
	InputImageMask    *codexOpenAIImageURLPart `json:"input_image_mask,omitempty"`
}

type codexOpenAIImageURLPart struct {
	ImageURL string `json:"image_url"`
}

var (
	codexOpenAIImageHTTP11Transport     *http.Transport
	codexOpenAIImageHTTP11TransportOnce sync.Once
)

func initCodexOpenAIImageHTTP11Transport() {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	codexOpenAIImageHTTP11Transport = cloneTransportWithHTTP11(base)
}

func newCodexOpenAIImageHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, timeout)
	if client.Transport == nil {
		codexOpenAIImageHTTP11TransportOnce.Do(initCodexOpenAIImageHTTP11Transport)
		client.Transport = codexOpenAIImageHTTP11Transport
		return client
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		client.Transport = cloneTransportWithHTTP11(transport)
	}
	return client
}

func isCodexOpenAIImageRequest(opts cliproxyexecutor.Options) bool {
	if !strings.EqualFold(strings.TrimSpace(opts.SourceFormat.String()), codexOpenAIImageSourceFormat) {
		return false
	}
	return codexIsImagesEndpointPath(helps.PayloadRequestPath(opts))
}

func codexIsImagesEndpointPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == codexImagesGenerationsPath || path == codexImagesEditsPath {
		return true
	}
	return strings.HasSuffix(path, codexImagesGenerationsPath) || strings.HasSuffix(path, codexImagesEditsPath)
}

func codexOpenAIImageStreamStatusErr(err error) error {
	if err == nil {
		return nil
	}
	if !codexOpenAIImageIsTransientStreamErr(err) {
		return err
	}
	return statusErr{
		code: http.StatusGatewayTimeout,
		msg:  fmt.Sprintf("stream error: upstream stream disconnected before completion: %v", err),
	}
}

func codexOpenAIImageIsTransientStreamErr(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "stream id") ||
		strings.Contains(message, "internal_error") ||
		strings.Contains(message, "received from peer") ||
		strings.Contains(message, "http2: stream closed") ||
		strings.Contains(message, "unexpected eof")
}

func codexOpenAIImageShouldRetry(err error, attempt int) bool {
	if attempt+1 >= codexOpenAIImageMaxAttempts || err == nil {
		return false
	}
	if codexOpenAIImageIsTransientStreamErr(err) {
		return true
	}
	if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		return se.StatusCode() == http.StatusGatewayTimeout
	}
	return false
}

func (e *CodexExecutor) resolveGPTImage2BaseModel() string {
	if e == nil || e.cfg == nil {
		return codexOpenAIImagesMainModel
	}
	model := strings.TrimSpace(e.cfg.GPTImage2BaseModel)
	if model == "" {
		return codexOpenAIImagesMainModel
	}
	if strings.HasPrefix(strings.ToLower(model), "gpt-") {
		return model
	}
	return codexOpenAIImagesMainModel
}

func (e *CodexExecutor) executeOpenAIImage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if directEndpoint := codexDirectOpenAIImageEndpoint(req, opts); directEndpoint != "" {
		return e.executeDirectOpenAIImage(ctx, auth, req, opts, directEndpoint)
	}

	prepared, errPrepare := codexPrepareOpenAIImageRequest(req, opts)
	if errPrepare != nil {
		return resp, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	mainModel := e.resolveGPTImage2BaseModel()
	reporter := helps.NewExecutorUsageReporter(ctx, e, mainModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	body, errBuild := e.prepareCodexOpenAIImageBody(prepared.Body, req, opts, mainModel)
	if errBuild != nil {
		return resp, errBuild
	}
	reporter.SetTranslatedReasoningEffort(body, "codex")

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpClient := newCodexOpenAIImageHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	for attempt := 0; attempt < codexOpenAIImageMaxAttempts; attempt++ {
		var identityState codexIdentityConfuseState
		httpReq, upstreamBody, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
		if errCache != nil {
			return resp, errCache
		}
		applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
		applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
		recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), upstreamBody)

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errDo)
			err = codexOpenAIImageStreamStatusErr(errDo)
			if codexOpenAIImageShouldRetry(err, attempt) {
				helps.LogWithRequestID(ctx).Warnf("codex openai images: retrying after upstream stream failure: %v", err)
				continue
			}
			return resp, err
		}

		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		data, errRead := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			err = codexOpenAIImageStreamStatusErr(errRead)
			if codexOpenAIImageShouldRetry(err, attempt) {
				helps.LogWithRequestID(ctx).Warnf("codex openai images: retrying after upstream stream read failure: %v", err)
				continue
			}
			return resp, err
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
			err = newCodexStatusErr(httpResp.StatusCode, data)
			return resp, err
		}

		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		for _, line := range bytes.Split(data, []byte("\n")) {
			if !bytes.HasPrefix(line, dataTag) {
				continue
			}
			eventData := bytes.TrimSpace(line[len(dataTag):])
			switch gjson.GetBytes(eventData, "type").String() {
			case "response.output_item.done":
				collectCodexOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
			case "response.completed":
				if detail, ok := helps.ParseCodexUsage(eventData); ok {
					reporter.Publish(ctx, detail)
				}
				publishCodexImageToolUsage(ctx, reporter, upstreamBody, eventData)
				results, createdAt, usageRaw, firstMeta, errExtract := codexExtractImageResults(eventData, outputItemsByIndex, outputItemsFallback)
				if errExtract != nil {
					return resp, errExtract
				}
				if len(results) == 0 {
					return resp, codexOpenAIImageEmptyOutputErr(eventData)
				}
				out, errOutput := codexBuildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, prepared.ResponseFormat)
				if errOutput != nil {
					return resp, errOutput
				}
				return cliproxyexecutor.Response{Payload: out, Headers: decodedResponseHeaders(httpResp.Header)}, nil
			}
		}

		err = statusErr{code: http.StatusGatewayTimeout, msg: "stream error: stream disconnected before completion"}
		if codexOpenAIImageShouldRetry(err, attempt) {
			helps.LogWithRequestID(ctx).Warnf("codex openai images: retrying after incomplete upstream stream: %v", err)
			continue
		}
		return resp, err
	}

	err = statusErr{code: http.StatusGatewayTimeout, msg: "stream error: stream disconnected before completion"}
	return resp, err
}

func (e *CodexExecutor) executeOpenAIImageStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if directEndpoint := codexDirectOpenAIImageEndpoint(req, opts); directEndpoint != "" {
		return e.executeDirectOpenAIImageStream(ctx, auth, req, opts, directEndpoint)
	}

	prepared, errPrepare := codexPrepareOpenAIImageRequest(req, opts)
	if errPrepare != nil {
		return nil, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	mainModel := e.resolveGPTImage2BaseModel()
	reporter := helps.NewExecutorUsageReporter(ctx, e, mainModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	body, errBuild := e.prepareCodexOpenAIImageBody(prepared.Body, req, opts, mainModel)
	if errBuild != nil {
		return nil, errBuild
	}
	reporter.SetTranslatedReasoningEffort(body, "codex")

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpClient := newCodexOpenAIImageHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	for attempt := 0; attempt < codexOpenAIImageMaxAttempts; attempt++ {
		var identityState codexIdentityConfuseState
		httpReq, upstreamBody, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
		if errCache != nil {
			return nil, errCache
		}
		applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
		applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
		recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), upstreamBody)

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errDo)
			err = codexOpenAIImageStreamStatusErr(errDo)
			if codexOpenAIImageShouldRetry(err, attempt) {
				helps.LogWithRequestID(ctx).Warnf("codex openai images: retrying stream setup after upstream failure: %v", err)
				continue
			}
			return nil, err
		}
		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			data, errRead := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
			if errRead != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errRead)
				err = codexOpenAIImageStreamStatusErr(errRead)
				if codexOpenAIImageShouldRetry(err, attempt) {
					helps.LogWithRequestID(ctx).Warnf("codex openai images: retrying stream setup after upstream read failure: %v", err)
					continue
				}
				return nil, err
			}
			data = applyCodexIdentityConfuseResponsePayload(data, identityState)
			helps.AppendAPIResponseChunk(ctx, e.cfg, data)
			helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
			err = newCodexStatusErr(httpResp.StatusCode, data)
			if codexOpenAIImageShouldRetry(err, attempt) {
				helps.LogWithRequestID(ctx).Warnf("codex openai images: retrying stream setup after upstream status failure: %v", err)
				continue
			}
			return nil, err
		}

		sseStream, errStream := helps.NewBoundedUpstreamHTTPResponseSSEStream(httpResp, 0)
		if errStream != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errStream)
			err = codexOpenAIImageStreamStatusErr(errStream)
			if codexOpenAIImageShouldRetry(err, attempt) {
				helps.LogWithRequestID(ctx).Warnf("codex openai images: retrying stream setup after upstream decode failure: %v", err)
				continue
			}
			return nil, err
		}
		streamCtx, cancelStream := context.WithCancel(ctx)
		closeResponse := closeHTTPResponseBodyOnce(cancelStream, sseStream, "codex openai images executor")
		out := make(chan cliproxyexecutor.StreamChunk)
		go func(identityState codexIdentityConfuseState, upstreamBody []byte) {
			defer close(out)
			defer closeResponse()

			sendPayload := func(payload []byte) bool {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
					return true
				case <-streamCtx.Done():
					return false
				}
			}
			sendError := func(errSend error) bool {
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errSend}:
					return true
				case <-streamCtx.Done():
					return false
				}
			}

			outputItemsByIndex := make(map[int64][]byte)
			var outputItemsFallback [][]byte
			for {
				event, errRead := sseStream.ReadEvent()
				if errRead != nil {
					if streamCtx.Err() != nil || errors.Is(errRead, io.EOF) {
						return
					}
					errRead = codexOpenAIImageStreamStatusErr(errRead)
					helps.RecordAPIResponseError(ctx, e.cfg, errRead)
					reporter.PublishFailure(ctx, errRead)
					sendError(errRead)
					return
				}
				for _, rawLine := range bytes.FieldsFunc(event, func(value rune) bool { return value == '\r' || value == '\n' }) {
					line := applyCodexIdentityConfuseResponsePayload(rawLine, identityState)
					helps.AppendAPIResponseChunk(ctx, e.cfg, line)
					if !bytes.HasPrefix(line, dataTag) {
						continue
					}
					eventData := bytes.TrimSpace(line[len(dataTag):])
					switch gjson.GetBytes(eventData, "type").String() {
					case "response.output_item.done":
						collectCodexOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
					case "response.image_generation_call.partial_image":
						frame := codexBuildImagePartialFrame(eventData, prepared.ResponseFormat, prepared.StreamPrefix)
						if len(frame) > 0 && !sendPayload(frame) {
							return
						}
					case "response.completed":
						if detail, ok := helps.ParseCodexUsage(eventData); ok {
							reporter.Publish(ctx, detail)
						}
						publishCodexImageToolUsage(ctx, reporter, upstreamBody, eventData)
						results, _, usageRaw, _, errExtract := codexExtractImageResults(eventData, outputItemsByIndex, outputItemsFallback)
						if errExtract != nil {
							sendError(errExtract)
							return
						}
						if len(results) == 0 {
							sendError(codexOpenAIImageEmptyOutputErr(eventData))
							return
						}
						for _, img := range results {
							frame := codexBuildImageCompletedFrame(img, usageRaw, prepared.ResponseFormat, prepared.StreamPrefix)
							if len(frame) > 0 && !sendPayload(frame) {
								return
							}
						}
						return
					}
				}
			}
		}(identityState, upstreamBody)
		return &cliproxyexecutor.StreamResult{Headers: decodedResponseHeaders(httpResp.Header), Chunks: out, Cancel: closeResponse}, nil
	}

	err = statusErr{code: http.StatusGatewayTimeout, msg: "stream error: stream disconnected before completion"}
	return nil, err
}

func (e *CodexExecutor) executeDirectOpenAIImage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	body, contentType, model, errPrepare := codexPrepareDirectOpenAIImageBody(req, opts, false)
	if errPrepare != nil {
		return resp, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(body, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	var identityState codexIdentityConfuseState
	httpReq, body, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
	if errCache != nil {
		return resp, errCache
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, errRead := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	data = applyCodexIdentityConfuseResponsePayload(data, identityState)
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return resp, err
	}

	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: data, Headers: decodedResponseHeaders(httpResp.Header)}, nil
}

func (e *CodexExecutor) executeDirectOpenAIImageStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	body, contentType, model, errPrepare := codexPrepareDirectOpenAIImageBody(req, opts, true)
	if errPrepare != nil {
		return nil, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(body, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	var identityState codexIdentityConfuseState
	httpReq, body, identityState, errCache := e.cacheHelper(ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body)
	if errCache != nil {
		return nil, errCache
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordCodexOpenAIImageRequest(ctx, e.cfg, e.Identifier(), auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return nil, err
	}

	sseStream, errStream := helps.NewBoundedUpstreamHTTPResponseSSEStream(httpResp, 0)
	if errStream != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errStream)
		return nil, errStream
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	closeResponse := closeHTTPResponseBodyOnce(cancelStream, sseStream, "codex direct openai images executor")
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			closeResponse()
			reporter.EnsurePublished(ctx)
		}()

		for {
			event, errRead := sseStream.ReadEvent()
			if errRead == nil {
				chunk := applyCodexIdentityConfuseResponsePayload(terminatedSSEEvent(event), identityState)
				helps.AppendAPIResponseChunk(ctx, e.cfg, chunk)
				for _, line := range bytes.FieldsFunc(chunk, func(value rune) bool { return value == '\r' || value == '\n' }) {
					if detail, ok := helps.ParseOpenAIStreamUsage(bytes.TrimSpace(line)); ok {
						reporter.Publish(ctx, detail)
					}
				}
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-streamCtx.Done():
					return
				}
			}
			if errRead != nil {
				if streamCtx.Err() == nil && !errors.Is(errRead, io.EOF) {
					helps.RecordAPIResponseError(ctx, e.cfg, errRead)
					reporter.PublishFailure(ctx, errRead)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
					case <-streamCtx.Done():
					}
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: decodedResponseHeaders(httpResp.Header), Chunks: out, Cancel: closeResponse}, nil
}

func codexDirectOpenAIImageEndpoint(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if codexDirectOpenAIImageModel(req) == "" {
		return ""
	}
	path := helps.PayloadRequestPath(opts)
	if strings.HasSuffix(strings.TrimSpace(path), codexImagesGenerationsPath) {
		return codexDirectImagesGenerations
	}
	if strings.HasSuffix(strings.TrimSpace(path), codexImagesEditsPath) {
		return codexDirectImagesEdit
	}
	return ""
}

func codexPrepareDirectOpenAIImageBody(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, string, string, error) {
	model := codexDirectOpenAIImageModel(req)
	if model == "" {
		return nil, "", "", fmt.Errorf("unsupported direct OpenAI image model %q", req.Model)
	}
	body, contentType, errPrepare := codexPrepareDirectOpenAIImagePayload(req, opts, model, stream)
	if errPrepare != nil {
		return nil, "", "", errPrepare
	}
	return body, contentType, model, nil
}

func codexPrepareDirectOpenAIImagePayload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string, stream bool) ([]byte, string, error) {
	contentType := opts.Headers.Get("Content-Type")
	path := strings.TrimSpace(helps.PayloadRequestPath(opts))
	if strings.HasSuffix(path, codexImagesEditsPath) {
		return codexPrepareDirectOpenAIImageEditPayload(req.Payload, model, contentType, stream)
	}
	return prepareOpenAICompatImagesPayload(req.Payload, model, contentType, stream)
}

func codexPrepareDirectOpenAIImageEditPayload(payload []byte, model string, contentType string, stream bool) ([]byte, string, error) {
	if json.Valid(payload) {
		return prepareOpenAICompatImagesPayload(payload, model, contentType, stream)
	}

	mediaType, params, errParse := mime.ParseMediaType(strings.TrimSpace(contentType))
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return nil, "", fmt.Errorf("unsupported OpenAI image edit Content-Type %q", contentType)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return codexRewriteOpenAIImageEditMultipartToJSON(payload, model, boundary, stream)
}

func codexRewriteOpenAIImageEditMultipartToJSON(payload []byte, model string, boundary string, stream bool) ([]byte, string, error) {
	if err := helps.ValidateMultipartPayloadSize(payload, helps.DefaultMultipartBodyBytes); err != nil {
		return nil, "", err
	}
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, errRead := reader.ReadForm(openAICompatMultipartMemory)
	if errRead != nil {
		return nil, "", fmt.Errorf("read multipart form failed: %w", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("codex openai images: remove multipart form files error: %v", errRemove)
		}
	}()
	if errValidate := helps.ValidateMultipartFormFiles(form, helps.DefaultMultipartFileBytes); errValidate != nil {
		return nil, "", errValidate
	}

	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "model", model)
	if stream {
		out, _ = sjson.SetBytes(out, "stream", true)
	}

	for key, values := range form.Value {
		key = strings.TrimSpace(key)
		if key == "" || key == "model" || key == "stream" {
			continue
		}
		out = codexSetOpenAIImageEditFormValues(out, key, values)
	}

	images := make([]codexOpenAIImageURLPart, 0, len(codexMultipartImageFiles(form)))
	for _, fileHeader := range codexMultipartImageFiles(form) {
		dataURL, errData := codexMultipartFileToDataURL(fileHeader)
		if errData != nil {
			return nil, "", errData
		}
		images = append(images, codexOpenAIImageURLPart{ImageURL: dataURL})
	}
	if len(images) > 0 {
		encoded, errMarshal := json.Marshal(images)
		if errMarshal != nil {
			return nil, "", fmt.Errorf("encode multipart image references: %w", errMarshal)
		}
		out, _ = sjson.SetRawBytes(out, "images", encoded)
	}
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, errData := codexMultipartFileToDataURL(maskFiles[0])
		if errData != nil {
			return nil, "", errData
		}
		out, _ = sjson.SetBytes(out, "mask.image_url", dataURL)
	}

	return out, "application/json", nil
}

func codexSetOpenAIImageEditFormValues(out []byte, key string, values []string) []byte {
	if len(values) == 0 {
		return out
	}
	path := codexOpenAIImageEditFormJSONPath(key)
	if path == "" {
		return out
	}
	if len(values) == 1 {
		return codexSetOpenAIImageEditFormValue(out, path, values[0])
	}
	items := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		items = append(items, json.RawMessage(codexOpenAIImageEditFormJSONValue(key, value)))
	}
	encoded, errMarshal := json.Marshal(items)
	if errMarshal == nil {
		out, _ = sjson.SetRawBytes(out, path, encoded)
	}
	return out
}

func codexSetOpenAIImageEditFormValue(out []byte, path string, value string) []byte {
	item := codexOpenAIImageEditFormJSONValue(path, value)
	out, _ = sjson.SetRawBytes(out, path, item)
	return out
}

func codexOpenAIImageEditFormJSONValue(key string, value string) []byte {
	value = strings.TrimSpace(value)
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "n", "output_compression", "partial_images":
		if parsed, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
			raw, _ := json.Marshal(parsed)
			return raw
		}
	}
	raw, _ := json.Marshal(value)
	return raw
}

func codexOpenAIImageEditFormJSONPath(key string) string {
	key = strings.TrimSpace(key)
	switch key {
	case "mask[file_id]":
		return "mask.file_id"
	case "mask[image_url]":
		return "mask.image_url"
	default:
		return key
	}
}

func codexDirectOpenAIImageModel(req cliproxyexecutor.Request) string {
	for _, model := range []string{gjson.GetBytes(req.Payload, "model").String(), req.Model} {
		baseModel := codexOpenAIImageBaseModel(model)
		if codexIsDirectOpenAIImageModel(baseModel) {
			return baseModel
		}
	}
	return ""
}

func codexOpenAIImageBaseModel(model string) string {
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		model = strings.TrimSpace(model[idx+1:])
	}
	return strings.ToLower(strings.TrimSpace(model))
}

func codexIsDirectOpenAIImageModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case codexGPTImage15Model, codexDefaultImageToolModel:
		return true
	default:
		return false
	}
}

func (e *CodexExecutor) prepareCodexOpenAIImageBody(body []byte, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, mainModel string) ([]byte, error) {
	out := body
	mainModel = strings.TrimSpace(mainModel)
	if mainModel == "" {
		mainModel = codexOpenAIImagesMainModel
	}
	var errThinking error
	out, errThinking = thinking.ApplyThinking(out, mainModel, codexOpenAIImageSourceFormat, "codex", e.Identifier())
	if errThinking != nil {
		return nil, errThinking
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	out = helps.ApplyPayloadConfigWithRequest(e.cfg, mainModel, "codex", codexOpenAIImageSourceFormat, "", out, body, requestedModel, requestPath, opts.Headers)
	out, _ = sjson.SetBytes(out, "model", mainModel)
	out, _ = sjson.SetBytes(out, "stream", true)
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	out, _ = sjson.DeleteBytes(out, "prompt_cache_retention")
	out, _ = sjson.DeleteBytes(out, "safety_identifier")
	out, _ = sjson.DeleteBytes(out, "stream_options")
	return normalizeCodexInstructions(out), nil
}

func recordCodexOpenAIImageRequest(ctx context.Context, cfg *config.Config, provider string, auth *cliproxyauth.Auth, url string, headers http.Header, body []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   headers,
		Body:      body,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func codexPrepareOpenAIImageRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (codexOpenAIImagePreparedRequest, error) {
	path := helps.PayloadRequestPath(opts)
	if strings.HasSuffix(path, codexImagesGenerationsPath) {
		return codexPrepareOpenAIImageGenerationJSON(req.Payload, req.Model)
	}
	if !strings.HasSuffix(path, codexImagesEditsPath) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("unsupported OpenAI image endpoint path %q", path)
	}

	contentType := codexImageContentType(opts.Headers)
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return codexPrepareOpenAIImageEditMultipart(req.Payload, req.Model, contentType)
	}
	return codexPrepareOpenAIImageEditJSON(req.Payload, req.Model)
}

func codexPrepareOpenAIImageGenerationJSON(rawJSON []byte, routeModel string) (codexOpenAIImagePreparedRequest, error) {
	if !json.Valid(rawJSON) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("invalid OpenAI image generation request JSON")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	tool := codexBuildOpenAIImageTool(rawJSON, routeModel, "generate", []string{"size", "quality", "background", "output_format", "moderation"}, []string{"output_compression", "partial_images"})
	body := codexBuildImagesResponsesRequest(prompt, nil, tool)
	return codexOpenAIImagePreparedRequest{
		Body:           body,
		ResponseFormat: codexOpenAIImageResponseFormatFromJSON(rawJSON),
		StreamPrefix:   "image_generation",
	}, nil
}

func codexPrepareOpenAIImageEditJSON(rawJSON []byte, routeModel string) (codexOpenAIImagePreparedRequest, error) {
	if !json.Valid(rawJSON) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("invalid OpenAI image edit request JSON")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	images := make([]string, 0)
	appendImage := func(raw string) {
		if ref := codexNormalizeImageReference(raw); ref != "" {
			images = append(images, ref)
		}
	}
	if imageResult := gjson.GetBytes(rawJSON, "image"); imageResult.Exists() {
		appendImage(codexImageReferenceFromResult(imageResult))
	}
	if imagesResult := gjson.GetBytes(rawJSON, "images"); imagesResult.IsArray() {
		for _, img := range imagesResult.Array() {
			appendImage(codexImageReferenceFromResult(img))
		}
	}
	tool := codexBuildOpenAIImageTool(rawJSON, routeModel, "edit", []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"}, []string{"output_compression", "partial_images"})
	if mask := codexNormalizeImageReference(codexImageReferenceFromResult(gjson.GetBytes(rawJSON, "mask"))); mask != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", mask)
	}
	body := codexBuildImagesResponsesRequest(prompt, images, tool)
	return codexOpenAIImagePreparedRequest{
		Body:           body,
		ResponseFormat: codexOpenAIImageResponseFormatFromJSON(rawJSON),
		StreamPrefix:   "image_edit",
	}, nil
}

func codexPrepareOpenAIImageEditMultipart(rawBody []byte, routeModel string, contentType string) (codexOpenAIImagePreparedRequest, error) {
	if err := helps.ValidateMultipartPayloadSize(rawBody, helps.DefaultMultipartBodyBytes); err != nil {
		return codexOpenAIImagePreparedRequest{}, err
	}
	_, params, errMedia := mime.ParseMediaType(contentType)
	if errMedia != nil {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("parse multipart content type failed: %w", errMedia)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("multipart boundary is required")
	}
	reader := multipart.NewReader(bytes.NewReader(rawBody), boundary)
	form, errForm := reader.ReadForm(openAICompatMultipartMemory)
	if errForm != nil {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("parse multipart form failed: %w", errForm)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("codex openai images: remove multipart temp files error: %v", errRemove)
		}
	}()
	if errValidate := helps.ValidateMultipartFormFiles(form, helps.DefaultMultipartFileBytes); errValidate != nil {
		return codexOpenAIImagePreparedRequest{}, errValidate
	}

	prompt := strings.TrimSpace(codexFormValue(form, "prompt"))
	responseFormat := codexNormalizeImageResponseFormat(codexFormValue(form, "response_format"))
	toolPayload := codexOpenAIImageToolPayload{
		Type:          "image_generation",
		Action:        "edit",
		Model:         codexOpenAIImageToolModel(codexFormValue(form, "model"), routeModel),
		Size:          strings.TrimSpace(codexFormValue(form, "size")),
		Quality:       strings.TrimSpace(codexFormValue(form, "quality")),
		Background:    strings.TrimSpace(codexFormValue(form, "background")),
		OutputFormat:  strings.TrimSpace(codexFormValue(form, "output_format")),
		InputFidelity: strings.TrimSpace(codexFormValue(form, "input_fidelity")),
		Moderation:    strings.TrimSpace(codexFormValue(form, "moderation")),
	}
	toolPayload.OutputCompression = codexParseOptionalImageInteger(codexFormValue(form, "output_compression"))
	toolPayload.PartialImages = codexParseOptionalImageInteger(codexFormValue(form, "partial_images"))

	images := make([]string, 0)
	for _, fh := range codexMultipartImageFiles(form) {
		dataURL, errData := codexMultipartFileToDataURL(fh)
		if errData != nil {
			return codexOpenAIImagePreparedRequest{}, errData
		}
		images = append(images, dataURL)
	}
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, errData := codexMultipartFileToDataURL(maskFiles[0])
		if errData != nil {
			return codexOpenAIImagePreparedRequest{}, errData
		}
		toolPayload.InputImageMask = &codexOpenAIImageURLPart{ImageURL: dataURL}
	}
	tool, _ := json.Marshal(toolPayload)

	body := codexBuildImagesResponsesRequest(prompt, images, tool)
	return codexOpenAIImagePreparedRequest{
		Body:           body,
		ResponseFormat: responseFormat,
		StreamPrefix:   "image_edit",
	}, nil
}

func codexImageContentType(headers http.Header) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get("Content-Type"))
}

func codexOpenAIImageResponseFormatFromJSON(rawJSON []byte) string {
	return codexNormalizeImageResponseFormat(gjson.GetBytes(rawJSON, "response_format").String())
}

func codexNormalizeImageResponseFormat(responseFormat string) string {
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		return "url"
	}
	return "b64_json"
}

func codexOpenAIImageToolModel(requestModel string, routeModel string) string {
	model := strings.TrimSpace(requestModel)
	if model == "" {
		model = strings.TrimSpace(routeModel)
	}
	if model == "" {
		model = codexDefaultImageToolModel
	}
	return model
}

func codexBuildOpenAIImageTool(rawJSON []byte, routeModel string, action string, stringFields []string, numberFields []string) []byte {
	var source map[string]json.RawMessage
	_ = json.Unmarshal(rawJSON, &source)
	tool := codexOpenAIImageToolPayload{
		Type:   "image_generation",
		Action: action,
		Model:  codexOpenAIImageToolModel(codexImageRawString(source["model"]), routeModel),
	}
	for _, field := range stringFields {
		value := codexImageRawString(source[field])
		switch field {
		case "size":
			tool.Size = value
		case "quality":
			tool.Quality = value
		case "background":
			tool.Background = value
		case "output_format":
			tool.OutputFormat = value
		case "input_fidelity":
			tool.InputFidelity = value
		case "moderation":
			tool.Moderation = value
		}
	}
	for _, field := range numberFields {
		value := gjson.ParseBytes(source[field])
		if value.Type != gjson.Number {
			continue
		}
		parsed := value.Int()
		switch field {
		case "output_compression":
			tool.OutputCompression = &parsed
		case "partial_images":
			tool.PartialImages = &parsed
		}
	}
	out, _ := json.Marshal(tool)
	return out
}

func codexImageRawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.ParseBytes(raw).String())
}

func codexParseOptionalImageInteger(value string) *int64 {
	parsed, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if errParse != nil {
		return nil
	}
	return &parsed
}

func codexBuildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	type inputContent struct {
		Type     string  `json:"type"`
		Text     *string `json:"text,omitempty"`
		ImageURL *string `json:"image_url,omitempty"`
	}
	type inputMessage struct {
		Type    string         `json:"type"`
		Role    string         `json:"role"`
		Content []inputContent `json:"content"`
	}
	type reasoning struct {
		Effort  string `json:"effort"`
		Summary string `json:"summary"`
	}
	type toolChoice struct {
		Type string `json:"type"`
	}
	type responsesRequest struct {
		Instructions      string            `json:"instructions"`
		Stream            bool              `json:"stream"`
		Reasoning         reasoning         `json:"reasoning"`
		ParallelToolCalls bool              `json:"parallel_tool_calls"`
		Include           []string          `json:"include"`
		Model             string            `json:"model"`
		Store             bool              `json:"store"`
		ToolChoice        toolChoice        `json:"tool_choice"`
		Input             []inputMessage    `json:"input"`
		Tools             []json.RawMessage `json:"tools"`
	}
	content := make([]inputContent, 0, len(images)+1)
	content = append(content, inputContent{Type: "input_text", Text: &prompt})
	for _, img := range images {
		if strings.TrimSpace(img) == "" {
			continue
		}
		imageURL := img
		content = append(content, inputContent{Type: "input_image", ImageURL: &imageURL})
	}
	tools := make([]json.RawMessage, 0, 1)
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		tools = append(tools, json.RawMessage(toolJSON))
	}
	req, _ := json.Marshal(responsesRequest{
		Stream:            true,
		Reasoning:         reasoning{Effort: "medium", Summary: "auto"},
		ParallelToolCalls: true,
		Include:           []string{"reasoning.encrypted_content"},
		Model:             codexOpenAIImagesMainModel,
		ToolChoice:        toolChoice{Type: "image_generation"},
		Input:             []inputMessage{{Type: "message", Role: "user", Content: content}},
		Tools:             tools,
	})
	return req
}

func codexFormValue(form *multipart.Form, key string) string {
	if form == nil || len(form.Value[key]) == 0 {
		return ""
	}
	return strings.TrimSpace(form.Value[key][0])
}

func codexMultipartImageFiles(form *multipart.Form) []*multipart.FileHeader {
	if form == nil {
		return nil
	}
	if files := form.File["image[]"]; len(files) > 0 {
		return files
	}
	return form.File["image"]
}

func codexMultipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	data, errRead := helps.ReadMultipartFile(fileHeader, helps.DefaultMultipartFileBytes)
	if errRead != nil {
		return "", errRead
	}
	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0])), "image/") {
		if extType := strings.TrimSpace(mime.TypeByExtension(strings.ToLower(filepath.Ext(fileHeader.Filename)))); strings.HasPrefix(strings.ToLower(extType), "image/") {
			mediaType = extType
		}
	}
	normalizedMediaType, errMedia := codexImageMediaTypeFromData(data, mediaType)
	if errMedia != nil {
		return "", errMedia
	}
	return "data:" + normalizedMediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func codexImageReferenceFromResult(result gjson.Result) string {
	if !result.Exists() {
		return ""
	}
	if result.Type == gjson.String {
		return result.String()
	}
	for _, path := range []string{
		"image_url.url",
		"image_url",
		"url",
		"data_url",
		"b64_json",
		"base64",
	} {
		value := strings.TrimSpace(result.Get(path).String())
		if value != "" {
			return value
		}
	}
	return ""
}

func codexNormalizeImageReference(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if normalized, ok := codexNormalizeImageDataURL(raw); ok {
		return normalized
	}
	if mediaType, ok := codexDetectBase64ImageMediaType(raw); ok {
		return "data:" + mediaType + ";base64," + codexCompactBase64(raw)
	}
	return raw
}

func codexNormalizeImageDataURL(raw string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "data:") {
		return "", false
	}
	comma := strings.Index(raw, ",")
	if comma < 0 {
		return raw, true
	}
	meta := strings.TrimSpace(raw[len("data:"):comma])
	data := codexCompactBase64(raw[comma+1:])
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
	if detected, ok := codexDetectBase64ImageMediaType(data); ok {
		return "data:" + detected + ";base64," + data, true
	}
	return raw, true
}

func codexCompactBase64(raw string) string {
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

func codexDetectBase64ImageMediaType(raw string) (string, bool) {
	compact := codexCompactBase64(raw)
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

func codexImageMediaTypeFromData(data []byte, rawMediaType string) (string, error) {
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

// codexExtractImageResults extracts image generation results directly from the
// completed event and the items collected from response.output_item.done events,
// without rebuilding the full completed JSON.
//
// It prefers image_generation_call items already present in the completed event's
// response.output and only falls back to the collected items when that output is
// empty, mirroring the semantics of patchCodexCompletedOutput + the previous
// extractor. Skipping the concatenate-and-reparse step avoids two large copies of
// the base64 payload, which matters for multi-megabyte generated images.
func codexExtractImageResults(completed []byte, itemsByIndex map[int64][]byte, fallback [][]byte) (results []codexImageCallResult, createdAt int64, usageRaw []byte, firstMeta codexImageCallResult, err error) {
	if gjson.GetBytes(completed, "type").String() != "response.completed" {
		return nil, 0, nil, codexImageCallResult{}, fmt.Errorf("unexpected event type")
	}
	createdAt = gjson.GetBytes(completed, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	appendItem := func(item gjson.Result) {
		if item.Get("type").String() != "image_generation_call" {
			return
		}
		res := strings.TrimSpace(item.Get("result").String())
		if res == "" {
			return
		}
		entry := codexImageCallResult{
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

	var outputItems []gjson.Result
	if output := gjson.GetBytes(completed, "response.output"); output.Exists() && output.IsArray() {
		outputItems = output.Array()
	}
	if len(outputItems) > 0 {
		// Completed event already carries the output; extract from it in place.
		results = make([]codexImageCallResult, 0, len(outputItems))
		for _, item := range outputItems {
			appendItem(item)
		}
	} else if len(itemsByIndex) > 0 || len(fallback) > 0 {
		// Completed output was empty; extract directly from the collected items,
		// preserving their original output_index ordering.
		results = make([]codexImageCallResult, 0, len(itemsByIndex)+len(fallback))
		if len(itemsByIndex) > 0 {
			indexes := make([]int64, 0, len(itemsByIndex))
			for idx := range itemsByIndex {
				indexes = append(indexes, idx)
			}
			sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
			for _, idx := range indexes {
				appendItem(gjson.ParseBytes(itemsByIndex[idx]))
			}
		}
		for _, raw := range fallback {
			appendItem(gjson.ParseBytes(raw))
		}
	}

	if usage := gjson.GetBytes(completed, "response.tool_usage.image_gen"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}
	return results, createdAt, usageRaw, firstMeta, nil
}

func codexOpenAIImageEmptyOutputErr(payload []byte) statusErr {
	message, _ := safeUpstreamFailureMessage("application/json", payload)
	return statusErr{code: http.StatusBadGateway, msg: message, errorCode: "codex_image_empty_output"}
}

func codexBuildImagesAPIResponse(results []codexImageCallResult, createdAt int64, usageRaw []byte, firstMeta codexImageCallResult, responseFormat string) ([]byte, error) {
	type urlImageData struct {
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	type b64ImageData struct {
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	responseFormat = codexNormalizeImageResponseFormat(responseFormat)
	var data any
	if responseFormat == "url" {
		items := make([]urlImageData, 0, len(results))
		for _, img := range results {
			items = append(items, urlImageData{
				URL:           "data:" + codexMimeTypeFromOutputFormat(img.OutputFormat) + ";base64," + img.Result,
				RevisedPrompt: img.RevisedPrompt,
			})
		}
		data = items
	} else {
		items := make([]b64ImageData, 0, len(results))
		for _, img := range results {
			items = append(items, b64ImageData{B64JSON: img.Result, RevisedPrompt: img.RevisedPrompt})
		}
		data = items
	}
	var usage json.RawMessage
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		usage = json.RawMessage(usageRaw)
	}
	out, errMarshal := json.Marshal(struct {
		Created      int64           `json:"created"`
		Data         any             `json:"data"`
		Background   string          `json:"background,omitempty"`
		OutputFormat string          `json:"output_format,omitempty"`
		Quality      string          `json:"quality,omitempty"`
		Size         string          `json:"size,omitempty"`
		Usage        json.RawMessage `json:"usage,omitempty"`
	}{
		Created:      createdAt,
		Data:         data,
		Background:   firstMeta.Background,
		OutputFormat: firstMeta.OutputFormat,
		Quality:      firstMeta.Quality,
		Size:         firstMeta.Size,
		Usage:        usage,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode codex image response: %w", errMarshal)
	}
	return out, nil
}

func codexBuildImagePartialFrame(payload []byte, responseFormat string, streamPrefix string) []byte {
	b64 := strings.TrimSpace(gjson.GetBytes(payload, "partial_image_b64").String())
	if b64 == "" {
		return nil
	}
	outputFormat := strings.TrimSpace(gjson.GetBytes(payload, "output_format").String())
	eventName := strings.TrimSpace(streamPrefix) + ".partial_image"
	data := []byte(`{"type":"","partial_image_index":0}`)
	data, _ = sjson.SetBytes(data, "type", eventName)
	data, _ = sjson.SetBytes(data, "partial_image_index", gjson.GetBytes(payload, "partial_image_index").Int())
	if codexNormalizeImageResponseFormat(responseFormat) == "url" {
		data, _ = sjson.SetBytes(data, "url", "data:"+codexMimeTypeFromOutputFormat(outputFormat)+";base64,"+b64)
	} else {
		data, _ = sjson.SetBytes(data, "b64_json", b64)
	}
	return codexBuildSSEFrame(eventName, data)
}

func codexBuildImageCompletedFrame(img codexImageCallResult, usageRaw []byte, responseFormat string, streamPrefix string) []byte {
	eventName := strings.TrimSpace(streamPrefix) + ".completed"
	data := []byte(`{"type":""}`)
	data, _ = sjson.SetBytes(data, "type", eventName)
	if codexNormalizeImageResponseFormat(responseFormat) == "url" {
		data, _ = sjson.SetBytes(data, "url", "data:"+codexMimeTypeFromOutputFormat(img.OutputFormat)+";base64,"+img.Result)
	} else {
		data, _ = sjson.SetBytes(data, "b64_json", img.Result)
	}
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
	}
	return codexBuildSSEFrame(eventName, data)
}

func codexBuildSSEFrame(eventName string, data []byte) []byte {
	var buf bytes.Buffer
	if strings.TrimSpace(eventName) != "" {
		buf.WriteString("event: ")
		buf.WriteString(eventName)
		buf.WriteString("\n")
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

func codexMimeTypeFromOutputFormat(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}
