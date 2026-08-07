package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAICompatImageHandlerType                    = "openai-image"
	openAICompatImagesGenerationsPath               = "/images/generations"
	openAICompatImagesEditsPath                     = "/images/edits"
	openAICompatDefaultImageEndpoint                = openAICompatImagesGenerationsPath
	openAICompatMultipartMemory               int64 = 8 << 20
	executorHTTPRequestBodyBytes              int64 = 256 << 20
	openAICompatRequestPlanTransformStage           = "request_plan.openai_compat"
	openAICompatProviderResolveTransformStage       = "request_plan.openai_compat.provider_resolve"
	openAICompatProviderConfigTransformStage        = "request_plan.openai_compat.provider_config"
	openAICompatToolHistoryTransformStage           = "request_plan.openai_compat.tool_history"
	openAICompatUserPayloadConfigStage              = "compat/" + string(compat.ApplyUserPayloadConfig)
	openAICompatProviderFinalizationStage           = "request_plan.openai_compat.provider_finalization"
	openAICompatFinalSanitizeTransformStage         = "request_plan.openai_compat.final_sanitize"
	openAICompatProviderResolvePolicy               = "openai_compat.provider_resolve"
	openAICompatProviderConfigPolicy                = "openai_compat.provider_config"
	openAICompatToolHistoryPolicy                   = "openai_compat.tool_history"
	openAICompatUserPayloadConfigPolicy             = "openai_compat.user_payload_config"
	openAICompatProviderFinalizationPolicy          = "openai_compat.provider_finalization"
	openAICompatFinalSanitizePolicy                 = "openai_compat.final_sanitize"
	openAICompatInlineRemoteImagesPolicy            = "openai_compat.inline_remote_images"
	openAICompatMetadataRemovedDowngrade            = "openai_compat.metadata_removed"
	openAICompatStoreRemovedDowngrade               = "openai_compat.store_removed"
	openAICompatParallelToolsRemovedDowngrade       = "openai_compat.parallel_tools_removed"
	openAICompatReasoningRemovedDowngrade           = "openai_compat.reasoning_controls_removed"
	openAICompatStreamUsageRemovedDowngrade         = "openai_compat.stream_usage_removed"
	openAICompatCompactStreamRemovedDowngrade       = "openai_compat.compact_stream_removed"
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

func closeHTTPResponseBodyOnce(cancel context.CancelFunc, body io.Closer, label string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if cancel != nil {
				cancel()
			}
			if body == nil {
				return
			}
			if errClose := body.Close(); errClose != nil {
				log.Errorf("%s: close response body error: %v", label, errClose)
			}
		})
	}
}

func readAndCloseExecutorHTTPRequestBody(req *http.Request, label string) ([]byte, error) {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	source := req.Body
	defer func() {
		if errClose := source.Close(); errClose != nil {
			log.Errorf("%s: request body close error: %v", label, errClose)
		}
	}()
	if req.ContentLength > executorHTTPRequestBodyBytes {
		return nil, executorHTTPRequestBodyTooLarge()
	}
	body, errRead := io.ReadAll(io.LimitReader(source, executorHTTPRequestBodyBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(body)) > executorHTTPRequestBodyBytes {
		return nil, executorHTTPRequestBodyTooLarge()
	}
	return body, nil
}

func executorHTTPRequestBodyTooLarge() error {
	cause := fmt.Errorf("request body exceeds %d bytes", executorHTTPRequestBodyBytes)
	return &failurecontract.Failure{
		Kind:          failurecontract.RequestTooLarge,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusRequestEntityTooLarge,
		ProviderCode:  "request_too_large",
		Cause:         cause,
		PublicMessage: cause.Error(),
	}
}

func decodedResponseHeaders(headers http.Header) http.Header {
	cloned := headers.Clone()
	for _, name := range []string{"Content-Encoding", "Content-Length", "Content-MD5", "Digest", "ETag"} {
		cloned.Del(name)
	}
	return cloned
}

func isEventStreamResponse(headers http.Header) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(headers.Get("Content-Type"), ";", 2)[0]))
	return mediaType == "text/event-stream"
}

func terminatedSSEEvent(event []byte) []byte {
	out := internalpayload.CloneBytes(event)
	if len(out) == 0 {
		return []byte("\n")
	}
	switch out[len(out)-1] {
	case '\n':
		return append(out, '\n')
	case '\r':
		return append(out, '\r')
	default:
		return append(out, '\n', '\n')
	}
}

func translateOpenAICompatStreamLine(ctx context.Context, upstreamFormat, downstreamFormat sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	rawCopy := internalpayload.CloneBytes(rawJSON)
	if upstreamFormat == downstreamFormat {
		return [][]byte{rawCopy}
	}
	return sdktranslator.TranslateStream(ctx, upstreamFormat, downstreamFormat, model, originalRequestRawJSON, requestRawJSON, rawCopy, param)
}

func openAICompatTargetFormatAndEndpoint(from sdktranslator.Format, opts cliproxyexecutor.Options, profile openAICompatProfile, model string) (sdktranslator.Format, string) {
	to := sdktranslator.FromString("openai")
	endpoint := "/chat/completions"
	if opts.Alt == "responses/compact" && profile.SupportsResponses {
		return sdktranslator.FromString("openai-response"), "/responses/compact"
	}
	if opts.Alt == "" && profile.SupportsNativeResponses && openAICompatModelSupportsNativeResponses(profile, model) &&
		strings.EqualFold(strings.TrimSpace(from.String()), "openai-response") {
		return sdktranslator.FromString("openai-response"), "/responses"
	}
	return to, endpoint
}

func openAICompatModelSupportsNativeResponses(profile openAICompatProfile, model string) bool {
	if config.NormalizeOpenAICompatibilityKind(profile.Kind) != "deepseek" {
		return true
	}
	return strings.HasPrefix(normalizedOpenAICompatPolicyModelName(model), "deepseek-v4-flash")
}

func openAICompatPolicyRequestMatch(from, to sdktranslator.Format, endpoint string, stream bool) compat.MatchContext {
	mode := compat.ExecutionMode("non-stream")
	if stream {
		mode = "stream"
	}
	endpointKind := compat.EndpointKind(strings.Trim(endpoint, "/"))
	switch endpoint {
	case "/chat/completions":
		endpointKind = "chat"
	case "/responses":
		endpointKind = "responses"
	case "/responses/compact":
		endpointKind = "compact"
	}
	return compat.MatchContext{
		Endpoint:     endpointKind,
		Mode:         mode,
		SourceFormat: compat.Format(from.String()),
		TargetFormat: compat.Format(to.String()),
	}
}

func applyOpenAICompatRequestCorrelationHeaders(ctx context.Context, req *http.Request, source http.Header) {
	if req == nil {
		return
	}
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	if requestID == "" {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("X-Cliproxy-Request-Id", requestID)
	misc.EnsureHeader(req.Header, source, "X-Request-Id", requestID)
	misc.EnsureHeader(req.Header, source, "X-Client-Request-Id", requestID)
}

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	profile := e.resolveProfile(auth)
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	applyOpenAICompatDefaultHeaders(req, profile)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	profile := e.resolveProfile(auth)
	baseURL, _ := e.resolveCredentials(auth)
	if err := sanitizeOpenAICompatHTTPRequestBody(httpReq, profile, baseURL); err != nil {
		return nil, err
	}
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func sanitizeOpenAICompatHTTPRequestBody(req *http.Request, profile openAICompatProfile, baseURL string) error {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	body, errRead := readAndCloseExecutorHTTPRequestBody(req, "openai compat executor")
	if errRead != nil {
		return errRead
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	if errReject := rejectDeepSeekUnsupportedResponsesTools(req.Context(), body, profile, model, path, path, openAICompatSourceFormatForPath(path)); errReject != nil {
		return errReject
	}
	if errReject := rejectLargeOpenAICompatToolHistory(req.Context(), body, profile, model, path); errReject != nil {
		return errReject
	}
	updated := scrubOpenAICompatPayloadForModel(body, profile, model, baseURL)
	inlinedImages := false
	if inlined, changed := inlineMiniMaxM3RemoteImageURLs(req.Context(), updated, profile, model); changed {
		updated = inlined
		inlinedImages = true
	}
	if errValidate := validateOpenAICompatOutboundJSON(updated); errValidate != nil {
		return errValidate
	}
	if errGuard := internalpayload.EnforceRequestTransform(req.Context(), "request_plan.openai_compat.http", int64(len(body)), int64(len(updated)), miniMaxM3InlineAmplificationOverride(inlinedImages)); errGuard != nil {
		return errGuard
	}
	req.Body = io.NopCloser(bytes.NewReader(updated))
	req.ContentLength = int64(len(updated))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(updated)), nil
	}
	if req.Header != nil {
		req.Header.Set("Content-Length", strconv.Itoa(len(updated)))
	}
	return nil
}

const (
	largeOpenAICompatToolHistoryPayloadBytes            = 10 * 1024 * 1024
	largeOpenAICompatToolOutputMessages                 = 300
	largeOpenAICompatLongContextToolHistoryPayloadBytes = 30 * 1024 * 1024
	largeOpenAICompatLongContextToolOutputMessages      = 600
)

func rejectLargeOpenAICompatToolHistory(ctx context.Context, body []byte, profile openAICompatProfile, model, path string) error {
	payloadBytesLimit, toolOutputMessagesLimit := largeOpenAICompatToolHistoryLimits(model)
	if len(body) < payloadBytesLimit || !hasOpenAICompatToolOutputMarker(body) {
		return nil
	}
	toolOutputs := countOpenAICompatToolOutputMessages(body)
	if toolOutputs < toolOutputMessagesLimit {
		return nil
	}
	fields := log.Fields{
		"event":                      "openai_compat_tool_history_guard",
		"model":                      model,
		"compat_kind":                config.NormalizeOpenAICompatibilityKind(profile.Kind),
		"request_path":               path,
		"payload_bytes":              len(body),
		"payload_bytes_limit":        payloadBytesLimit,
		"tool_output_messages":       toolOutputs,
		"tool_output_messages_limit": toolOutputMessagesLimit,
	}
	helps.LogWithRequestID(ctx).WithFields(fields).Warn("large OpenAI-compatible tool history rejected before compat repair")
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: "request_feature_unsupported",
		msg:       largeOpenAICompatToolHistoryUserMessage(),
	}
}

func largeOpenAICompatToolHistoryLimits(model string) (payloadBytes int, toolOutputMessages int) {
	baseModel, _ := thinking.StripPublicModelHint(model)
	baseModel = normalizedOpenAICompatPolicyModelName(baseModel)
	if strings.HasPrefix(baseModel, "mimo-v2.5") ||
		strings.HasPrefix(baseModel, "kimi-k3") ||
		strings.HasPrefix(baseModel, "glm-5.2") ||
		strings.HasPrefix(baseModel, "deepseek-v4-pro") ||
		strings.HasPrefix(baseModel, "deepseek-v4-flash") {
		return largeOpenAICompatLongContextToolHistoryPayloadBytes,
			largeOpenAICompatLongContextToolOutputMessages
	}
	return largeOpenAICompatToolHistoryPayloadBytes, largeOpenAICompatToolOutputMessages
}

func rejectDeepSeekUnsupportedChatResponseFormat(ctx context.Context, sourceBody, body []byte, profile openAICompatProfile, model, endpoint, path string) error {
	if config.NormalizeOpenAICompatibilityKind(profile.Kind) != "deepseek" || endpoint != "/chat/completions" {
		return nil
	}
	responseFormatType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "response_format.type").String()))
	if responseFormatType == "" {
		responseFormatType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(sourceBody, "response_format.type").String()))
	}
	if responseFormatType == "" {
		responseFormatType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(sourceBody, "text.format.type").String()))
	}
	if responseFormatType != "json_schema" {
		return nil
	}
	fields := log.Fields{
		"event":                "openai_compat_response_format_guard",
		"model":                model,
		"compat_kind":          "deepseek",
		"request_path":         path,
		"upstream_endpoint":    endpoint,
		"payload_bytes":        len(body),
		"response_format_type": responseFormatType,
	}
	helps.LogWithRequestID(ctx).WithFields(fields).Warn("DeepSeek Chat rejected unsupported response format before upstream request")
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: "request_feature_unsupported",
		msg:       deepSeekChatJSONSchemaUserMessage(),
	}
}

func deepSeekChatJSONSchemaUserMessage() string {
	return "request_feature_unsupported: deepseek_chat_json_schema. DeepSeek 官方 Chat Completions 仅支持 response_format.type=text 或 json_object，不支持 json_schema。如果必须保证 JSON Schema，请改用 deepseek-v4-flash 的 /v1/responses；否则请将 Chat 请求改为 response_format={\"type\":\"json_object\"}，并在提示词中明确要求输出 JSON。CPA 不会静默降级 json_schema。"
}

func rejectDeepSeekUnsupportedImageInput(ctx context.Context, body []byte, profile openAICompatProfile, model, path string) error {
	if config.NormalizeOpenAICompatibilityKind(profile.Kind) != "deepseek" {
		return nil
	}
	imageParts := countOpenAICompatImageParts(body)
	if imageParts == 0 {
		return nil
	}
	fields := log.Fields{
		"event":         "openai_compat_image_guard",
		"model":         model,
		"compat_kind":   "deepseek",
		"request_path":  path,
		"payload_bytes": len(body),
		"image_parts":   imageParts,
	}
	helps.LogWithRequestID(ctx).WithFields(fields).Warn("DeepSeek official route rejected image content before upstream request")
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: "request_feature_unsupported",
		msg:       deepSeekOfficialImageInputUserMessage(),
	}
}

func deepSeekOfficialImageInputUserMessage() string {
	return "request_feature_unsupported: deepseek_official_image_input. DeepSeek 官方当前不支持图片输入。请移除当前请求和历史消息里的 image_url / input_image，仅保留文本内容后重试；如果必须传图，请切换到支持图像输入的模型或路由。原样重复提交不会提高成功率。"
}

func rejectDeepSeekUnsupportedResponsesTools(ctx context.Context, body []byte, profile openAICompatProfile, model, path, endpoint, sourceFormat string) error {
	return rejectDeepSeekUnsupportedResponsesToolsForKind(ctx, body, profile.Kind, model, path, endpoint, sourceFormat, "OpenAICompatExecutor")
}

func rejectDeepSeekUnsupportedResponsesToolsForKind(ctx context.Context, body []byte, compatKind, model, path, endpoint, sourceFormat, executor string) error {
	if config.NormalizeOpenAICompatibilityKind(compatKind) != "deepseek" ||
		!strings.EqualFold(strings.TrimSpace(sourceFormat), "openai-response") {
		return nil
	}
	toolCount, unsupportedCount, unsupportedTypes := deepSeekUnsupportedResponsesToolTypes(body)
	if unsupportedCount == 0 {
		return nil
	}
	fields := log.Fields{
		"event":                  "deepseek_responses_tool_guard",
		"executor":               executor,
		"model":                  model,
		"compat_kind":            "deepseek",
		"request_path":           path,
		"upstream_endpoint":      endpoint,
		"source_format":          sourceFormat,
		"tool_definition_count":  toolCount,
		"unsupported_tool_count": unsupportedCount,
		"unsupported_tool_types": unsupportedTypes,
	}
	helps.LogWithRequestID(ctx).WithFields(fields).Warn("DeepSeek route rejected unsupported Responses tool types before upstream request")
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: "request_feature_unsupported",
		msg:       deepSeekResponsesToolsUserMessage(unsupportedTypes),
	}
}

func deepSeekUnsupportedResponsesToolTypes(body []byte) (toolCount, unsupportedCount int, unsupportedTypes []string) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0, 0, nil
	}
	seen := make(map[string]struct{})
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		toolCount++
		toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		if toolType == "function" {
			continue
		}
		unsupportedCount++
		toolType = safeDeepSeekToolTypeLabel(toolType)
		if _, ok := seen[toolType]; ok {
			continue
		}
		seen[toolType] = struct{}{}
		if len(unsupportedTypes) < 8 {
			unsupportedTypes = append(unsupportedTypes, toolType)
		}
	}
	return toolCount, unsupportedCount, unsupportedTypes
}

func safeDeepSeekToolTypeLabel(toolType string) string {
	if toolType == "" {
		return "missing"
	}
	if len(toolType) > 64 {
		return "other"
	}
	for _, char := range toolType {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return "other"
	}
	return toolType
}

func deepSeekResponsesToolsUserMessage(unsupportedTypes []string) string {
	typeSummary := "non-function"
	if len(unsupportedTypes) > 0 {
		typeSummary = strings.Join(unsupportedTypes, ", ")
	}
	return fmt.Sprintf("request_feature_unsupported: deepseek_responses_non_function_tools. DeepSeek 官方当前仅能安全承载 function 工具，当前 Responses 请求包含不支持的工具类型：%s。CPA 不会静默删除、降级或改写这些工具。请仅保留 function 工具，或切换到原生支持这些 Responses 工具的模型/渠道；原样重复提交不会提高成功率。", typeSummary)
}

func openAICompatSourceFormatForPath(path string) string {
	normalized := strings.TrimSuffix(strings.TrimSpace(path), "/")
	if normalized == "/responses" || strings.HasSuffix(normalized, "/v1/responses") {
		return "openai-response"
	}
	return ""
}

func hasOpenAICompatToolOutputMarker(body []byte) bool {
	return bytes.Contains(body, []byte(`"tool`)) ||
		bytes.Contains(body, []byte(`"function_call_output"`)) ||
		bytes.Contains(body, []byte(`"custom_tool_call_output"`))
}

func largeOpenAICompatToolHistoryUserMessage() string {
	return "request_feature_unsupported: large_openai_tool_history. 历史工具调用过多、文件工具结果过多或上下文过大，当前 GPT/OpenAI-compatible 路由继续携带这些工具结果会显著拖慢或中断。请新开会话、把历史工具调用/文件结果压缩成普通文本摘要、减少重复文件提交，或切换到更适合长文件上下文的模型；原样重复提交不会提高成功率。"
}

func countOpenAICompatImageParts(body []byte) int {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0
	}
	count := 0
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			switch partType {
			case "image", "image_url", "input_image":
				count++
			}
		}
	}
	return count
}

func countOpenAICompatToolOutputMessages(body []byte) int {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0
	}
	count := 0
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		if strings.TrimSpace(msg.Get("role").String()) == "tool" {
			count++
		}
	}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call_output", "custom_tool_call_output":
			count++
		}
	}
	return count
}

func validateOpenAICompatOutboundJSON(payload []byte) error {
	if gjson.ValidBytes(payload) {
		return nil
	}
	return statusErr{
		code:      http.StatusBadRequest,
		msg:       "invalid JSON body after OpenAI-compatible normalization",
		errorCode: "invalid_json_body",
	}
}

func logOpenAICompatCompatibilityDiagnostic(ctx context.Context, diagnostic openAICompatPayloadDiagnostic, statusCode int, headers http.Header, body []byte) {
	if statusCode != http.StatusBadRequest || !diagnostic.relevant() {
		return
	}
	if diagnostic.UpstreamRequestID == "" {
		diagnostic.UpstreamRequestID = firstHeaderValue(headers, "X-Tt-Logid", "X-Volc-Request-Id", "X-Request-Id", "X-Request-ID", "X-Requestid", "Request-Id")
	}
	fields := log.Fields{
		"event":              "compatibility_diagnostic",
		"provider":           "openai-compatibility",
		"compat_kind":        diagnostic.CompatKind,
		"compat_kind_source": diagnostic.CompatKindSource,
		"compat_mapping":     diagnostic.CompatMapping,
		"model":              diagnostic.Model,
		"endpoint":           diagnostic.Endpoint,
		"payload_bytes":      diagnostic.PayloadSize,
		"status":             statusCode,
	}
	if diagnostic.RequestPath != "" {
		fields["request_path"] = diagnostic.RequestPath
	}
	if diagnostic.Channel != "" {
		fields["channel"] = diagnostic.Channel
	}
	if diagnostic.AuthID != "" {
		fields["auth_id"] = diagnostic.AuthID
	}
	if diagnostic.CompatName != "" {
		fields["compat_name"] = diagnostic.CompatName
	}
	if diagnostic.UpstreamRequestID != "" {
		fields["upstream_request_id"] = diagnostic.UpstreamRequestID
	}
	if len(diagnostic.PayloadFields) > 0 {
		fields["payload_fields"] = diagnostic.PayloadFields
	}
	if diagnostic.MessageCount > 0 {
		fields["message_count"] = diagnostic.MessageCount
	}
	if len(diagnostic.MessageRoles) > 0 {
		fields["message_roles"] = diagnostic.MessageRoles
	}
	if diagnostic.MessageRoleSequence != "" {
		fields["message_role_sequence"] = diagnostic.MessageRoleSequence
	}
	if len(diagnostic.MessageContentKinds) > 0 {
		fields["message_content_kinds"] = diagnostic.MessageContentKinds
	}
	if len(diagnostic.ContentPartTypes) > 0 {
		fields["content_part_types"] = diagnostic.ContentPartTypes
	}
	if diagnostic.ToolDefinitionCount > 0 {
		fields["tool_definition_count"] = diagnostic.ToolDefinitionCount
	}
	if len(diagnostic.ToolTypes) > 0 {
		fields["tool_types"] = diagnostic.ToolTypes
	}
	if diagnostic.ToolCallCount > 0 {
		fields["tool_call_count"] = diagnostic.ToolCallCount
	}
	if diagnostic.AssistantToolCalls > 0 {
		fields["assistant_tool_call_messages"] = diagnostic.AssistantToolCalls
	}
	if diagnostic.ToolResultMessages > 0 {
		fields["tool_result_messages"] = diagnostic.ToolResultMessages
	}
	if diagnostic.ReasoningMessages > 0 {
		fields["reasoning_messages"] = diagnostic.ReasoningMessages
	}
	if diagnostic.MaxContentParts > 0 {
		fields["max_content_parts"] = diagnostic.MaxContentParts
	}
	if diagnostic.ToolChoiceType != "" {
		fields["tool_choice_type"] = diagnostic.ToolChoiceType
	}
	if diagnostic.ThinkingType != "" {
		fields["thinking_type"] = diagnostic.ThinkingType
	}
	if diagnostic.ReasoningEffort != "" {
		fields["reasoning_effort"] = diagnostic.ReasoningEffort
	}
	if diagnostic.Temperature != "" {
		fields["payload_temperature"] = diagnostic.Temperature
	}
	if diagnostic.TopP != "" {
		fields["payload_top_p"] = diagnostic.TopP
	}
	if diagnostic.ResponseFormatType != "" {
		fields["response_format_type"] = diagnostic.ResponseFormatType
	}
	if diagnostic.ParallelToolCalls != "" {
		fields["parallel_tool_calls"] = diagnostic.ParallelToolCalls
	}
	if diagnostic.MaxTokens > 0 {
		fields["payload_max_tokens"] = diagnostic.MaxTokens
	}
	if diagnostic.MaxCompletionTokens > 0 {
		fields["payload_max_completion_tokens"] = diagnostic.MaxCompletionTokens
	}
	if diagnostic.MaxOutputTokens > 0 {
		fields["payload_max_output_tokens"] = diagnostic.MaxOutputTokens
	}
	if diagnostic.ThinkingBudget > 0 {
		fields["payload_thinking_budget"] = diagnostic.ThinkingBudget
	}
	if diagnostic.StopCount > 0 {
		fields["payload_stop_count"] = diagnostic.StopCount
	}
	if len(diagnostic.InputItemTypes) > 0 {
		fields["input_item_types"] = diagnostic.InputItemTypes
	}
	if len(diagnostic.AddedFields) > 0 {
		fields["added_fields"] = diagnostic.AddedFields
	}
	if len(diagnostic.RemovedFields) > 0 {
		fields["removed_fields"] = diagnostic.RemovedFields
	}
	if len(diagnostic.ModifiedFields) > 0 {
		fields["modified_fields"] = diagnostic.ModifiedFields
	}
	if errorCode := firstNonEmptyJSONValue(body, "error.code", "code", "error.type", "type", "error.err_code"); errorCode != "" {
		fields["upstream_error_code"] = errorCode
	}
	helps.LogWithRequestID(ctx).WithFields(fields).Warn("openai compat compatibility diagnostic")
}

type openAICompatRequestPlan struct {
	upstreamFormat   sdktranslator.Format
	responseFormat   sdktranslator.Format
	endpoint         string
	requestPath      string
	requestSource    []byte
	body             []byte
	logBody          []byte
	diagnosticSource []byte
	failureCtx       context.Context
}

func openAICompatCompatibilityDowngrades(input, output []byte) []string {
	fields := []struct {
		path string
		id   string
	}{
		{path: "metadata", id: openAICompatMetadataRemovedDowngrade},
		{path: "store", id: openAICompatStoreRemovedDowngrade},
		{path: "parallel_tool_calls", id: openAICompatParallelToolsRemovedDowngrade},
		{path: "reasoning", id: openAICompatReasoningRemovedDowngrade},
		{path: "reasoning_effort", id: openAICompatReasoningRemovedDowngrade},
		{path: "stream_options", id: openAICompatStreamUsageRemovedDowngrade},
		{path: "stream", id: openAICompatCompactStreamRemovedDowngrade},
	}
	downgrades := make([]string, 0, len(fields))
	for _, field := range fields {
		if !gjson.GetBytes(input, field.path).Exists() || gjson.GetBytes(output, field.path).Exists() {
			continue
		}
		duplicate := false
		for _, existing := range downgrades {
			if existing == field.id {
				duplicate = true
				break
			}
		}
		if !duplicate {
			downgrades = append(downgrades, field.id)
		}
	}
	return downgrades
}

func (e *OpenAICompatExecutor) prepareOpenAICompatRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseURL, baseModel string, profile openAICompatProfile, stream bool) (plan openAICompatRequestPlan, err error) {
	plan.failureCtx = ctx
	from := opts.SourceFormat
	plan.responseFormat = cliproxyexecutor.ResponseFormatOrSource(opts)
	plan.upstreamFormat, plan.endpoint = openAICompatTargetFormatAndEndpoint(from, opts, profile, baseModel)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	payloadSource := req.Payload
	if repaired, ok := helps.RepairInvalidJSONStringEscapes(originalPayloadSource); ok {
		originalPayloadSource = repaired
	}
	if repaired, ok := helps.RepairInvalidJSONStringEscapes(payloadSource); ok {
		payloadSource = repaired
	}
	if from.String() == "claude" {
		originalPayloadSource = downgradeClaudeToolSearchForCompat(baseURL, originalPayloadSource)
		payloadSource = downgradeClaudeToolSearchForCompat(baseURL, payloadSource)
	}
	plan.requestSource = payloadSource

	translationStream := opts.Stream || stream
	originalTranslated, body, err := helps.TranslateRequestPairGuarded(ctx, "legacy.translate.openai_compat", from, plan.upstreamFormat, baseModel, originalPayloadSource, payloadSource, translationStream, internalpayload.AmplificationOverride{})
	if err != nil {
		return plan, err
	}
	providerResolveStarted := time.Now()
	providerResolveInput := body
	thinkingProviderKey := profile.KindOrFallback(auth)
	body = normalizeOpenAICompatRouteReasoningEffort(body, opts, baseModel, thinkingProviderKey, baseURL, profile.Kind)
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), plan.upstreamFormat.String(), thinkingProviderKey)
	if err != nil {
		return plan, err
	}
	if err = helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatProviderResolveTransformStage,
		providerResolveInput,
		body,
		providerResolveStarted,
		[]string{openAICompatProviderResolvePolicy},
		nil,
		internalpayload.AmplificationOverride{},
	); err != nil {
		return plan, err
	}

	historyStarted := time.Now()
	historyInput := body
	var xiaomiHistoryDowngraded bool
	body, xiaomiHistoryDowngraded, err = normalizeXiaomiMimoThinkingHistory(body, profile.Kind, baseModel)
	if err != nil {
		return plan, err
	}
	var historyReport thinkingHistoryTransformReport
	body, _, _, historyReport, err = normalizeThinkingHistoryForModelWithReport(body, "openai", baseModel)
	if err != nil {
		return plan, err
	}
	if xiaomiHistoryDowngraded {
		historyReport.InputBytes = len(historyInput)
		historyReport.OutputBytes = len(body)
		historyReport.DowngradeReason = thinkingHistoryUnrepairableReason
	}
	if err = enforceThinkingHistoryTransform(ctx, "openai", historyReport, time.Since(historyStarted)); err != nil {
		return plan, err
	}

	providerConfigStarted := time.Now()
	providerConfigInput := body
	body = e.overrideModel(body, baseModel)
	plan.diagnosticSource = body
	if err = helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatProviderConfigTransformStage,
		providerConfigInput,
		body,
		providerConfigStarted,
		[]string{openAICompatProviderConfigPolicy},
		nil,
		internalpayload.AmplificationOverride{},
	); err != nil {
		return plan, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	plan.requestPath = helps.PayloadRequestPath(opts)
	toolHistoryStarted := time.Now()
	toolHistoryInput := body
	if err = rejectLargeOpenAICompatToolHistory(ctx, body, profile, baseModel, plan.requestPath); err != nil {
		return plan, err
	}
	if err = helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatToolHistoryTransformStage,
		toolHistoryInput,
		body,
		toolHistoryStarted,
		[]string{openAICompatToolHistoryPolicy},
		nil,
		internalpayload.AmplificationOverride{},
	); err != nil {
		return plan, err
	}

	policyMatch := openAICompatPolicyRequestMatch(from, plan.upstreamFormat, plan.endpoint, stream)
	body, err = scrubOpenAICompatPayloadForModelWithPolicies(ctx, body, profile, baseModel, baseURL, policyMatch)
	if err != nil {
		return plan, err
	}

	payloadConfigStarted := time.Now()
	payloadConfigInput := body
	configuredBody := helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, plan.upstreamFormat.String(), from.String(), "", body, originalTranslated, requestedModel, plan.requestPath, opts.Headers)
	if err = helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatUserPayloadConfigStage,
		payloadConfigInput,
		configuredBody,
		payloadConfigStarted,
		[]string{openAICompatUserPayloadConfigPolicy},
		nil,
		internalpayload.AmplificationOverride{},
	); err != nil {
		return plan, err
	}
	if bytes.Equal(configuredBody, body) {
		body = configuredBody
	} else {
		body, err = revalidateOpenAICompatPayloadAfterConfig(ctx, configuredBody, profile, baseModel, baseURL, policyMatch)
		if err != nil {
			return plan, err
		}
	}
	providerFinalizationStarted := time.Now()
	providerFinalizationInput := body
	body, _, err = normalizeXiaomiMimoThinkingHistory(body, profile.Kind, baseModel)
	if err != nil {
		return plan, err
	}
	if stream {
		if profile.SupportsStreamUsage && plan.endpoint == "/chat/completions" {
			body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
		}
	} else if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(body, "stream"); errDelete == nil {
			body = updated
		}
		body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "openai compat executor", body)
	}
	if err = validateOpenAICompatOutboundJSON(body); err != nil {
		return plan, err
	}
	if err = helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatProviderFinalizationStage,
		providerFinalizationInput,
		body,
		providerFinalizationStarted,
		[]string{openAICompatProviderFinalizationPolicy},
		openAICompatCompatibilityDowngrades(providerFinalizationInput, body),
		internalpayload.AmplificationOverride{},
	); err != nil {
		return plan, err
	}

	diagnostic := newOpenAICompatPayloadDiagnostic(plan.diagnosticSource, body, profile, auth, baseModel, plan.endpoint, plan.requestPath, opts.Headers, nil)
	plan.failureCtx = cliproxyusage.WithFailureDiagnostic(ctx, diagnostic.failureDiagnostic())
	finalSanitizeStarted := time.Now()
	finalSanitizeInput := body
	if err = rejectDeepSeekUnsupportedChatResponseFormat(ctx, plan.requestSource, body, profile, baseModel, plan.endpoint, plan.requestPath); err != nil {
		return plan, err
	}
	if err = rejectDeepSeekUnsupportedResponsesTools(ctx, plan.requestSource, profile, baseModel, plan.requestPath, plan.endpoint, from.String()); err != nil {
		return plan, err
	}
	if err = rejectDeepSeekUnsupportedImageInput(ctx, body, profile, baseModel, plan.requestPath); err != nil {
		return plan, err
	}
	plan.logBody = body
	inlinedImages := false
	if inlined, changed := inlineMiniMaxM3RemoteImageURLs(ctx, body, profile, baseModel); changed {
		body = inlined
		inlinedImages = true
		plan.logBody = redactOpenAICompatImageDataURLsForLog(body)
		if err = validateOpenAICompatOutboundJSON(body); err != nil {
			return plan, err
		}
	}
	finalPolicies := []string{openAICompatFinalSanitizePolicy}
	if inlinedImages {
		finalPolicies = append(finalPolicies, openAICompatInlineRemoteImagesPolicy)
	}
	if err = helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatFinalSanitizeTransformStage,
		finalSanitizeInput,
		body,
		finalSanitizeStarted,
		finalPolicies,
		nil,
		miniMaxM3InlineAmplificationOverride(inlinedImages),
	); err != nil {
		return plan, err
	}
	plan.body = body
	return plan, nil
}

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImages(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	profile := e.resolveProfile(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	failureCtx := ctx
	defer func() {
		reporter.TrackFailure(failureCtx, &err)
	}()

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}
	if isOpenAICompatMiniMaxImageGeneration(opts, profile, baseURL, baseModel) {
		return e.executeMiniMaxImageGeneration(ctx, auth, req, baseURL, profile, reporter)
	}

	plan, err := e.prepareOpenAICompatRequest(ctx, auth, req, opts, baseURL, baseModel, profile, false)
	failureCtx = plan.failureCtx
	if err != nil {
		return resp, err
	}
	reporter.SetTranslatedReasoningEffort(plan.body, plan.upstreamFormat.String())

	url := strings.TrimSuffix(baseURL, "/") + plan.endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(plan.body))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	applyOpenAICompatRequestCorrelationHeaders(ctx, httpReq, opts.Headers)
	applyOpenAICompatDefaultHeaders(httpReq, profile)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
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
		Body:      plan.logBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		err = helps.NormalizeUpstreamReadError(err)
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	body, err := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		compatDiagnostic := newOpenAICompatPayloadDiagnostic(plan.diagnosticSource, plan.body, profile, auth, baseModel, plan.endpoint, plan.requestPath, opts.Headers, httpResp.Header)
		failureCtx = cliproxyusage.WithFailureDiagnostic(failureCtx, compatDiagnostic.failureDiagnostic())
		logOpenAICompatCompatibilityDiagnostic(ctx, compatDiagnostic, httpResp.StatusCode, httpResp.Header, body)
		err = normalizeOpenAICompatImageGenerationPermissionError(
			newOpenAICompatStatusErr(profile, auth, req.Model, httpResp.StatusCode, httpResp.Header, httpResp.Header.Get("Content-Type"), body),
			plan.body,
		)
		return resp, err
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.EnsurePublished(ctx)
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(ctx, plan.upstreamFormat, plan.responseFormat, req.Model, opts.OriginalRequest, plan.body, body, &param)
	responseHeaders := decodedResponseHeaders(httpResp.Header)
	resp = cliproxyexecutor.Response{Payload: out, Headers: responseHeaders}
	return resp, nil
}

func (e *OpenAICompatExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	planStarted := time.Now()
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	profile := e.resolveProfile(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return resp, err
	}

	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(req.Payload, baseModel, opts.Headers.Get("Content-Type"), false)
	if errPrepare != nil {
		err = errPrepare
		return resp, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	if errGuard := internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       openAICompatRequestPlanTransformStage,
		InputBytes:  int64(len(req.Payload)),
		OutputBytes: int64(len(payload)),
		Duration:    time.Since(planStarted),
	}, internalpayload.AmplificationOverride{}); errGuard != nil {
		return resp, errGuard
	}
	reporter.SetTranslatedReasoningEffort(payload, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	applyOpenAICompatRequestCorrelationHeaders(ctx, httpReq, opts.Headers)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
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
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	body, errRead := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		err = errRead
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		err = newOpenAICompatStatusErr(profile, auth, req.Model, httpResp.StatusCode, httpResp.Header, httpResp.Header.Get("Content-Type"), body)
		return resp, err
	}

	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	reporter.EnsurePublished(ctx)
	resp = cliproxyexecutor.Response{Payload: body, Headers: decodedResponseHeaders(httpResp.Header)}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImagesStream(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	profile := e.resolveProfile(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	failureCtx := ctx
	defer func() {
		reporter.TrackFailure(failureCtx, &err)
	}()

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	plan, err := e.prepareOpenAICompatRequest(ctx, auth, req, opts, baseURL, baseModel, profile, true)
	failureCtx = plan.failureCtx
	if err != nil {
		return nil, err
	}
	reporter.SetTranslatedReasoningEffort(plan.body, plan.upstreamFormat.String())

	url := strings.TrimSuffix(baseURL, "/") + plan.endpoint
	requestCtx, cancelRequest := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(plan.body))
	if err != nil {
		cancelRequest()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	applyOpenAICompatRequestCorrelationHeaders(ctx, httpReq, opts.Headers)
	applyOpenAICompatDefaultHeaders(httpReq, profile)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
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
		Body:      plan.logBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		cancelRequest()
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	responseLog := helps.NewAPIResponseLogRuntime(ctx, e.cfg)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, errRead := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
		cancelRequest()
		if errRead != nil {
			if responseLog != nil {
				responseLog.RecordError(errRead)
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		if responseLog != nil {
			responseLog.AppendChunk(b)
		}
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		compatDiagnostic := newOpenAICompatPayloadDiagnostic(plan.diagnosticSource, plan.body, profile, auth, baseModel, plan.endpoint, plan.requestPath, opts.Headers, httpResp.Header)
		failureCtx = cliproxyusage.WithFailureDiagnostic(failureCtx, compatDiagnostic.failureDiagnostic())
		logOpenAICompatCompatibilityDiagnostic(ctx, compatDiagnostic, httpResp.StatusCode, httpResp.Header, b)
		err = normalizeOpenAICompatImageGenerationPermissionError(
			newOpenAICompatStatusErr(profile, auth, req.Model, httpResp.StatusCode, httpResp.Header, httpResp.Header.Get("Content-Type"), b),
			plan.body,
		)
		return nil, err
	}
	sseStream, errStream := helps.NewBoundedUpstreamHTTPResponseSSEStream(httpResp, 0)
	if errStream != nil {
		cancelRequest()
		helps.RecordAPIResponseError(ctx, e.cfg, errStream)
		return nil, errStream
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	closeResponse := closeHTTPResponseBodyOnce(cancelRequest, sseStream, "openai compat executor")
	go func() {
		defer close(out)
		defer closeResponse()
		var param any
		cleanEOF := false
		for {
			event, errRead := sseStream.ReadEvent()
			if errRead != nil {
				if requestCtx.Err() != nil {
					return
				}
				if errors.Is(errRead, io.EOF) {
					cleanEOF = true
					break
				}
				if responseLog != nil {
					responseLog.RecordError(errRead)
				}
				reporter.PublishFailure(failureCtx, errRead)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
				case <-requestCtx.Done():
				}
				break
			}
			for _, line := range bytes.FieldsFunc(event, func(value rune) bool { return value == '\r' || value == '\n' }) {
				if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
				}
				trimmedLine := bytes.TrimSpace(line)
				if len(trimmedLine) == 0 {
					continue
				}

				if !bytes.HasPrefix(trimmedLine, []byte("data:")) {
					if bytes.HasPrefix(trimmedLine, []byte(":")) || bytes.HasPrefix(trimmedLine, []byte("event:")) ||
						bytes.HasPrefix(trimmedLine, []byte("id:")) || bytes.HasPrefix(trimmedLine, []byte("retry:")) {
						continue
					}
					if bytes.HasPrefix(trimmedLine, []byte("{")) || bytes.HasPrefix(trimmedLine, []byte("[")) {
						streamErr := openAICompatMalformedSSEEventError(trimmedLine)
						helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
						reporter.PublishFailure(failureCtx, streamErr)
						select {
						case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
						case <-requestCtx.Done():
						}
						return
					}
					continue
				}

				// OpenAI-compatible streams must use SSE data lines.
				chunks := sdktranslator.TranslateStream(ctx, plan.upstreamFormat, plan.responseFormat, req.Model, opts.OriginalRequest, plan.body, internalpayload.CloneBytes(trimmedLine), &param)
				for i := range chunks {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
					case <-requestCtx.Done():
						return
					}
				}
			}
			if responseLog != nil {
				responseLog.AppendChunk(event)
			}
		}
		if cleanEOF && plan.upstreamFormat.String() != "openai-response" {
			// In case the upstream close the stream without a terminal [DONE] marker.
			// Feed a synthetic done marker through the translator so pending
			// response.completed events are still emitted exactly once.
			chunks := sdktranslator.TranslateStream(ctx, plan.upstreamFormat, plan.responseFormat, req.Model, opts.OriginalRequest, plan.body, []byte("data: [DONE]"), &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-requestCtx.Done():
					return
				}
			}
		}
		// Ensure we record the request if no usage chunk was ever seen
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: decodedResponseHeaders(httpResp.Header), Chunks: out, Cancel: closeResponse}, nil
}

func (e *OpenAICompatExecutor) executeImagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	planStarted := time.Now()
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	profile := e.resolveProfile(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(req.Payload, baseModel, opts.Headers.Get("Content-Type"), true)
	if errPrepare != nil {
		err = errPrepare
		return nil, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	if errGuard := internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       openAICompatRequestPlanTransformStage,
		InputBytes:  int64(len(req.Payload)),
		OutputBytes: int64(len(payload)),
		Duration:    time.Since(planStarted),
	}, internalpayload.AmplificationOverride{}); errGuard != nil {
		return nil, errGuard
	}
	reporter.SetTranslatedReasoningEffort(payload, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	requestCtx, cancelRequest := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		cancelRequest()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	applyOpenAICompatRequestCorrelationHeaders(ctx, httpReq, opts.Headers)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
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
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		cancelRequest()
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	responseLog := helps.NewAPIResponseLogRuntime(ctx, e.cfg)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, errRead := helps.ReadBoundedUpstreamHTTPResponse(httpResp, helps.UpstreamBodyLimits{})
		cancelRequest()
		if errRead != nil {
			if responseLog != nil {
				responseLog.RecordError(errRead)
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		if responseLog != nil {
			responseLog.AppendChunk(body)
		}
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		return nil, newOpenAICompatStatusErr(profile, auth, req.Model, httpResp.StatusCode, httpResp.Header, httpResp.Header.Get("Content-Type"), body)
	}
	if isEventStreamResponse(httpResp.Header) {
		sseStream, errStream := helps.NewBoundedUpstreamHTTPResponseSSEStream(httpResp, 0)
		if errStream != nil {
			cancelRequest()
			helps.RecordAPIResponseError(ctx, e.cfg, errStream)
			return nil, errStream
		}
		out := make(chan cliproxyexecutor.StreamChunk)
		closeResponse := closeHTTPResponseBodyOnce(cancelRequest, sseStream, "openai compat image executor")
		go func() {
			defer close(out)
			defer closeResponse()
			defer reporter.EnsurePublished(ctx)
			for {
				event, errRead := sseStream.ReadEvent()
				if errRead != nil {
					if requestCtx.Err() != nil || errors.Is(errRead, io.EOF) {
						return
					}
					if responseLog != nil {
						responseLog.RecordError(errRead)
					}
					reporter.PublishFailure(ctx, errRead)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
					case <-requestCtx.Done():
					}
					return
				}
				chunk := terminatedSSEEvent(event)
				if responseLog != nil {
					responseLog.AppendChunk(chunk)
				}
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-requestCtx.Done():
					return
				}
			}
		}()
		return &cliproxyexecutor.StreamResult{Headers: decodedResponseHeaders(httpResp.Header), Chunks: out, Cancel: closeResponse}, nil
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	closeResponse := closeHTTPResponseBodyOnce(cancelRequest, httpResp.Body, "openai compat executor")
	go func() {
		defer close(out)
		defer closeResponse()
		defer reporter.EnsurePublished(ctx)
		buffer := make([]byte, 32*1024)
		for {
			n, errRead := httpResp.Body.Read(buffer)
			if n > 0 {
				chunk := internalpayload.CloneBytes(buffer[:n])
				if responseLog != nil {
					responseLog.AppendChunk(chunk)
				}
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-requestCtx.Done():
					return
				}
			}
			if errRead != nil {
				if requestCtx.Err() != nil {
					return
				}
				if errRead != io.EOF {
					errRead = helps.NormalizeUpstreamReadError(errRead)
					if responseLog != nil {
						responseLog.RecordError(errRead)
					}
					reporter.PublishFailure(ctx, errRead)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
					case <-requestCtx.Done():
					}
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out, Cancel: closeResponse}, nil
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	started := time.Now()
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	profile := e.resolveProfile(auth)
	baseURL, _ := e.resolveCredentials(auth)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("openai")
	translated, err := helps.TranslateRequestGuarded(ctx, "legacy.translate.openai_compat.count", from, to, baseModel, req.Payload, false, internalpayload.AmplificationOverride{})
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	modelForCounting := baseModel

	thinkingProviderKey := profile.KindOrFallback(auth)
	if openAICompatCountTokensNeedsThinking(req, opts, translated, baseModel) {
		translated = normalizeOpenAICompatRouteReasoningEffort(translated, opts, modelForCounting, thinkingProviderKey, baseURL, profile.Kind)
		translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), thinkingProviderKey)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
	}
	translated = e.overrideModel(translated, modelForCounting)
	translated = scrubOpenAICompatPayloadForModel(translated, profile, baseModel, baseURL)
	if err = internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       openAICompatRequestPlanTransformStage,
		InputBytes:  int64(len(req.Payload)),
		OutputBytes: int64(len(translated)),
		Duration:    time.Since(started),
	}, internalpayload.AmplificationOverride{}); err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	logCountTokensSummary(ctx, newCountTokensSummaryLogMeta(opts, helps.PayloadRequestedModel(opts, req.Model), modelForCounting, e.Identifier(), "OpenAICompatExecutor", translated), count, time.Since(started))
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

func openAICompatCountTokensNeedsThinking(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payload []byte, baseModel string) bool {
	if strings.TrimSpace(req.Model) != strings.TrimSpace(baseModel) {
		return true
	}
	if openAICompatMetadataString(opts.Metadata, cliproxyexecutor.ReasoningEffortOriginalMetadataKey) != "" {
		return true
	}
	return bytes.Contains(payload, []byte(`"reasoning"`)) ||
		bytes.Contains(payload, []byte(`"reasoning_effort"`)) ||
		bytes.Contains(payload, []byte(`"thinking"`))
}

// Refresh is a no-op for API-key based compatibility providers.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func openAICompatImageEndpointPath(opts cliproxyexecutor.Options) string {
	if opts.SourceFormat.String() != openAICompatImageHandlerType {
		return ""
	}
	path := helps.PayloadRequestPath(opts)
	if strings.HasSuffix(path, "/images/edits") {
		return openAICompatImagesEditsPath
	}
	if strings.HasSuffix(path, "/images/generations") {
		return openAICompatImagesGenerationsPath
	}
	return openAICompatDefaultImageEndpoint
}

func normalizeOpenAICompatRouteReasoningEffort(payload []byte, opts cliproxyexecutor.Options, finalModel string, providerKey string, baseURL string, compatKind string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	original := openAICompatMetadataString(opts.Metadata, cliproxyexecutor.ReasoningEffortOriginalMetadataKey)
	if original == "" {
		return payload
	}

	requestedModel := helps.PayloadRequestedModel(opts, finalModel)
	clientProfile := openAICompatMetadataString(opts.Metadata, cliproxyexecutor.ClientProfileMetadataKey)
	deepSeekOfficial := requiresDeepSeekThinkingBudgetCompatibility(finalModel, baseURL, compatKind)
	if deepSeekOfficial && openAICompatReasoningDisabled(original) {
		if updated, err := sjson.SetBytes(payload, "thinking.type", "disabled"); err == nil {
			payload = updated
		}
		return stripOpenAICompatReasoningEffort(payload)
	}
	if !deepSeekOfficial && !thinking.ShouldNormalizeStrongestReasoningIntent(requestedModel, clientProfile, original) {
		return payload
	}

	modelInfo := registry.LookupModelInfo(strings.TrimSpace(finalModel), strings.TrimSpace(providerKey))
	var support *registry.ThinkingSupport
	if modelInfo != nil {
		support = modelInfo.Thinking
	}
	normalized := thinking.NormalizeReasoningEffortForTarget(original, support, deepSeekOfficial)
	if deepSeekOfficial {
		normalized.Normalized = thinking.NormalizeDeepSeekOfficialReasoningEffortForModel(finalModel, original)
		normalized.Stripped = false
	}
	if normalized.Stripped {
		return stripOpenAICompatReasoningEffort(payload)
	}
	if normalized.Normalized == "" {
		return payload
	}

	updated, err := sjson.SetBytes(payload, "reasoning_effort", normalized.Normalized)
	if err != nil {
		return payload
	}
	if cleaned, errDelete := sjson.DeleteBytes(updated, "thinking.reasoning_effort"); errDelete == nil {
		updated = cleaned
	}
	return updated
}

func openAICompatReasoningDisabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "off", "disabled", "disable", "false":
		return true
	default:
		return false
	}
}

func stripOpenAICompatReasoningEffort(payload []byte) []byte {
	return mutateOpenAICompatJSON(payload, []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"}, nil)
}

func openAICompatMetadataString(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func prepareOpenAICompatImagesPayload(payload []byte, model string, contentType string, stream bool) ([]byte, string, error) {
	model = strings.TrimSpace(model)
	contentType = strings.TrimSpace(contentType)
	if json.Valid(payload) {
		if model != "" {
			payload, _ = sjson.SetBytes(payload, "model", model)
		}
		if stream {
			payload, _ = sjson.SetBytes(payload, "stream", true)
		} else {
			payload, _ = sjson.DeleteBytes(payload, "stream")
		}
		return payload, "application/json", nil
	}

	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return payload, contentType, nil
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return rewriteOpenAICompatImagesMultipartPayload(payload, model, boundary, stream)
}

func cloneOpenAICompatMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func rewriteOpenAICompatImagesMultipartPayload(payload []byte, model string, boundary string, stream bool) ([]byte, string, error) {
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
			log.Errorf("openai compat executor: remove multipart form files error: %v", errRemove)
		}
	}()
	if errValidate := helps.ValidateMultipartFormFiles(form, helps.DefaultMultipartFileBytes); errValidate != nil {
		return nil, "", errValidate
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if model != "" {
		if errWrite := writer.WriteField("model", model); errWrite != nil {
			return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
		}
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
			header := cloneOpenAICompatMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			if errCopy := helps.CopyMultipartFile(part, fileHeader, helps.DefaultMultipartFileBytes); errCopy != nil {
				return nil, "", errCopy
			}
		}
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) resolveCompatConfig(auth *cliproxyauth.Auth) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
			candidates = append(candidates, v)
		}
		if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
			candidates = append(candidates, v)
		}
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range e.cfg.OpenAICompatibility {
		compat := &e.cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "model", model)
	return payload
}

type statusErr struct {
	code               int
	providerStatusCode int
	msg                string
	errorCode          string
	retryAfter         *time.Duration
	headers            http.Header
	failure            *failurecontract.Failure
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int { return e.code }
func (e statusErr) ProviderStatusCode() int {
	if e.providerStatusCode > 0 {
		return e.providerStatusCode
	}
	return e.code
}
func (e statusErr) ErrorCode() string          { return e.errorCode }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
func (e statusErr) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}
func (e statusErr) Unwrap() error { return e.failure }

func normalizeOpenAICompatImageGenerationPermissionError(err statusErr, requestPayload []byte) statusErr {
	if err.ProviderStatusCode() != http.StatusForbidden || !codexHasImageGenerationTool(requestPayload) {
		return err
	}
	original := err
	unsupported := codexUnsupportedImageGenerationToolError()
	unsupported.providerStatusCode = err.ProviderStatusCode()
	unsupported.headers = err.Headers()
	unsupported.failure = &failurecontract.Failure{
		Kind:          failurecontract.UnsupportedFeature,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    unsupported.code,
		ProviderCode:  unsupported.errorCode,
		Retryable:     false,
		Cause:         original,
		PublicMessage: unsupported.msg,
	}
	return unsupported
}

func classifyOpenAICompatFailure(statusCode, providerStatusCode int, message, errorCode string, retryAfter *time.Duration) *failurecontract.Failure {
	kind, scope, retryable := openAICompatFailureSemantics(statusCode, providerStatusCode, message, errorCode)
	return &failurecontract.Failure{
		Kind:          kind,
		Scope:         scope,
		HTTPStatus:    statusCode,
		ProviderCode:  errorCode,
		RetryAfter:    retryAfter,
		Retryable:     retryable,
		PublicMessage: message,
	}
}

func openAICompatFailureSemantics(statusCode, providerStatusCode int, message, errorCode string) (failurecontract.Kind, failurecontract.Scope, bool) {
	if openAICompatContentSafetyFailure(providerStatusCode, message, errorCode) {
		return failurecontract.ContentSafetyBlocked, failurecontract.ScopeRequest, false
	}
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return failurecontract.InvalidRequest, failurecontract.ScopeRequest, false
	case http.StatusRequestEntityTooLarge:
		return failurecontract.RequestTooLarge, failurecontract.ScopeRequest, false
	case http.StatusUnauthorized:
		return failurecontract.AuthenticationFailed, failurecontract.ScopeCredential, false
	case http.StatusForbidden:
		if openAICompatAuthenticationFailure(message, errorCode) {
			return failurecontract.AuthenticationFailed, failurecontract.ScopeCredential, false
		}
		return "", "", false
	case http.StatusPaymentRequired:
		return failurecontract.QuotaExceeded, failurecontract.ScopeCredential, true
	case http.StatusNotFound:
		if openAICompatRequestScopedNotFound(message) {
			return failurecontract.InvalidRequest, failurecontract.ScopeRequest, false
		}
		if openAICompatModelUnavailableFailure(message, errorCode) {
			return failurecontract.ModelUnavailable, failurecontract.ScopeModel, false
		}
		return "", "", false
	case http.StatusTooManyRequests:
		if providerStatusCode == http.StatusPaymentRequired ||
			openAICompatAccountQuotaLikeMessage(strings.ToLower(message)) ||
			openAICompatQuotaErrorCode(errorCode) {
			return failurecontract.QuotaExceeded, failurecontract.ScopeCredential, true
		}
		return failurecontract.RateLimited, failurecontract.ScopeCredential, true
	case http.StatusRequestTimeout, http.StatusBadGateway, http.StatusGatewayTimeout, 524:
		return failurecontract.TransportError, failurecontract.ScopeProvider, true
	default:
		if statusCode >= http.StatusInternalServerError {
			return failurecontract.ProviderUnavailable, failurecontract.ScopeProvider, true
		}
		return "", "", false
	}
}

func openAICompatRequestScopedNotFound(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func openAICompatContentSafetyFailure(providerStatusCode int, message, errorCode string) bool {
	switch providerStatusCode {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusUnavailableForLegalReasons:
	default:
		return false
	}
	code := strings.Trim(strings.ToLower(strings.TrimSpace(errorCode)), `"'(),:;[]{}<>`)
	switch code {
	case "content_policy_violation", "data_inspection_failed", "datainspectionfailed", "1026", "1027", "1301":
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "content_policy_violation") ||
		strings.Contains(lower, "data_inspection_failed") ||
		strings.Contains(lower, "data may contain inappropriate content") ||
		(strings.Contains(lower, "request was rejected") && strings.Contains(lower, "high risk")) ||
		(strings.Contains(lower, "content") && strings.Contains(lower, "blocked"))
}

func openAICompatAuthenticationFailure(message, errorCode string) bool {
	code := strings.Trim(strings.ToLower(strings.TrimSpace(errorCode)), `"'(),:;[]{}<>`)
	switch code {
	case "invalid_api_key", "authentication_error", "unauthorized", "forbidden", "access_denied", "permission_denied":
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	return containsAny(lower,
		"api key", "api_key", "authentication", "unauthorized",
		"invalid token", "access denied", "permission denied",
	)
}

func openAICompatModelUnavailableFailure(message, errorCode string) bool {
	code := strings.Trim(strings.ToLower(strings.TrimSpace(errorCode)), `"'(),:;[]{}<>`)
	switch code {
	case "model_not_found", "model_not_available", "unsupported_model", "model_decommissioned":
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "model") && containsAny(lower,
		"not found", "does not exist", "not available", "unsupported",
	)
}

func openAICompatQuotaErrorCode(errorCode string) bool {
	switch strings.ToLower(strings.TrimSpace(errorCode)) {
	case "insufficient_quota", "quota_exhausted", "billing_cycle_quota":
		return true
	default:
		return false
	}
}
