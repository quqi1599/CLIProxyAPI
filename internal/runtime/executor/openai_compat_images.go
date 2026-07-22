package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAICompatAltMiniMaxImageGeneration = "minimax/image_generation"

func isOpenAICompatMiniMaxImageGeneration(opts cliproxyexecutor.Options, profile openAICompatProfile, baseURL string, model string) bool {
	if opts.Alt != openAICompatAltMiniMaxImageGeneration {
		return false
	}
	if !isMiniMaxImageGenerationModel(model) {
		return false
	}
	if config.NormalizeOpenAICompatibilityKind(profile.Kind) == "minimax" {
		return true
	}
	return inferOpenAICompatKindFromBaseURL(baseURL) == "minimax"
}

func isMiniMaxImageGenerationModel(model string) bool {
	switch miniMaxImageGenerationBaseModel(model) {
	case "image-01", "image-01-live":
		return true
	default:
		return false
	}
}

func miniMaxImageGenerationBaseModel(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		model = model[idx+1:]
	}
	if idx := strings.Index(model, "("); idx > 0 {
		model = model[:idx]
	}
	return strings.ToLower(strings.TrimSpace(model))
}

func (e *OpenAICompatExecutor) executeMiniMaxImageGeneration(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, baseURL string, profile openAICompatProfile, reporter *helps.UsageReporter) (cliproxyexecutor.Response, error) {
	payload := req.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: "minimax image_generation: request body must be valid JSON"}
	}
	upstreamModel := miniMaxImageGenerationBaseModel(req.Model)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(req.Model)
	}
	payload, _ = sjson.SetBytes(payload, "model", upstreamModel)
	if errGuard := internalpayload.EnforceRequestTransform(
		ctx,
		openAICompatRequestPlanTransformStage,
		int64(len(req.Payload)),
		int64(len(payload)),
		internalpayload.AmplificationOverride{},
	); errGuard != nil {
		return cliproxyexecutor.Response{}, errGuard
	}

	url := strings.TrimSuffix(baseURL, "/") + "/image_generation"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	body, err := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		return cliproxyexecutor.Response{}, newOpenAICompatStatusErr(profile, auth, req.Model, httpResp.StatusCode, httpResp.Header, httpResp.Header.Get("Content-Type"), body)
	}

	out, err := buildOpenAIImagesResponseFromMiniMax(body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(out))
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: out, Headers: decodedResponseHeaders(httpResp.Header)}, nil
}

func buildOpenAIImagesResponseFromMiniMax(body []byte) ([]byte, error) {
	if len(body) == 0 || !json.Valid(body) {
		return nil, statusErr{code: http.StatusBadGateway, msg: "minimax image_generation: invalid JSON response", errorCode: "minimax_invalid_json"}
	}
	if baseResp := gjson.GetBytes(body, "base_resp.status_code"); baseResp.Exists() && baseResp.Int() != 0 {
		status := newUpstreamStatusErr(http.StatusBadGateway, nil, "application/json", body)
		status.providerStatusCode = http.StatusOK
		status.errorCode = "minimax_" + strconv.FormatInt(baseResp.Int(), 10)
		return nil, status
	}

	b64Images := collectMiniMaxImageStrings(body,
		"data.image_base64",
		"data.image_base64s",
		"data.images.#.image_base64",
		"data.images.#.b64_json",
		"data.images.#.base64",
		"image_base64",
	)
	urlImages := collectMiniMaxImageStrings(body,
		"data.image_urls",
		"data.image_url",
		"data.images.#.url",
		"data.images.#.image_url",
		"image_urls",
	)
	revisedPrompt := firstNonEmptyJSONValue(body, "data.revised_prompt", "revised_prompt")
	type imageData struct {
		B64JSON       string `json:"b64_json,omitempty"`
		URL           string `json:"url,omitempty"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	data := make([]imageData, 0, len(b64Images)+len(urlImages))
	for _, b64 := range b64Images {
		data = append(data, imageData{B64JSON: b64, RevisedPrompt: revisedPrompt})
	}
	for _, imageURL := range urlImages {
		data = append(data, imageData{URL: imageURL, RevisedPrompt: revisedPrompt})
	}
	if len(data) == 0 {
		return nil, statusErr{code: http.StatusBadGateway, msg: "minimax image_generation: upstream did not return image output", errorCode: "minimax_empty_output"}
	}
	out, errMarshal := json.Marshal(struct {
		Created int64       `json:"created"`
		Data    []imageData `json:"data"`
	}{Created: time.Now().Unix(), Data: data})
	if errMarshal != nil {
		return nil, fmt.Errorf("minimax image_generation: encode response: %w", errMarshal)
	}
	return out, nil
}

func collectMiniMaxImageStrings(body []byte, paths ...string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, path := range paths {
		result := gjson.GetBytes(body, path)
		if !result.Exists() {
			continue
		}
		if result.IsArray() {
			for _, entry := range result.Array() {
				add(entry.String())
			}
			continue
		}
		add(result.String())
	}
	return out
}
