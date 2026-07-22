// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers (OpenAI, Claude, Gemini).
package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"golang.org/x/net/context"
)

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`
}

const idempotencyKeyMetadataKey = "idempotency_key"

const (
	defaultStreamingKeepAliveSeconds = 0
	defaultStreamingBootstrapRetries = 0
	// Stream interceptor history is intentionally bounded and not configurable in the first SDK surface.
	maxStreamInterceptorHistoryChunks = 64
	maxStreamInterceptorHistoryBytes  = 1 << 20
)

type pinnedAuthContextKey struct{}
type selectedAuthCallbackContextKey struct{}
type executionSessionContextKey struct{}
type disallowFreeAuthContextKey struct{}

// PluginInterceptorHost applies plugin interceptors around handler execution.
type PluginInterceptorHost interface {
	InterceptRequestBeforeAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
	InterceptResponse(context.Context, pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse
	InterceptStreamChunk(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse
}

type pluginInterceptorSkipHost interface {
	InterceptRequestBeforeAuthExcept(context.Context, pluginapi.RequestInterceptRequest, string) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuthExcept(context.Context, pluginapi.RequestInterceptRequest, string) pluginapi.RequestInterceptResponse
	InterceptResponseExcept(context.Context, pluginapi.ResponseInterceptRequest, string) pluginapi.ResponseInterceptResponse
	InterceptStreamChunkExcept(context.Context, pluginapi.StreamChunkInterceptRequest, string) pluginapi.StreamChunkInterceptResponse
}

type streamInterceptorDetector interface {
	HasStreamInterceptors() bool
}

type requestInterceptorDetector interface {
	HasRequestInterceptors() bool
}

// PluginModelRouterHost routes matching requests to a plugin executor, the router's own executor,
// or a built-in provider before model-to-provider resolution and auth selection.
type PluginModelRouterHost interface {
	RouteModel(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool)
}

// PluginExecutorHost executes a routed request with a specific plugin executor.
type PluginExecutorHost interface {
	ExecutePluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)
	ExecutePluginExecutorStream(context.Context, string, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error)
	CountPluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)
}

type pluginExecutorFormatResolver interface {
	PluginExecutorRequestToFormat(string, coreexecutor.Request, coreexecutor.Options) sdktranslator.Format
}

type pluginModelRouterSkipHost interface {
	RouteModelExcept(context.Context, pluginapi.ModelRouteRequest, string) (pluginapi.ModelRouteResponse, bool)
}

type modelRouterDetector interface {
	HasModelRouters() bool
}

type modelRouterSkipDetector interface {
	HasModelRoutersExcept(string) bool
}

// WithPinnedAuthID returns a child context that requests execution on a specific auth ID.
func WithPinnedAuthID(ctx context.Context, authID string) context.Context {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinnedAuthContextKey{}, authID)
}

// WithSelectedAuthIDCallback returns a child context that receives the selected auth ID.
func WithSelectedAuthIDCallback(ctx context.Context, callback func(string)) context.Context {
	if callback == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectedAuthCallbackContextKey{}, callback)
}

// WithExecutionSessionID returns a child context tagged with a long-lived execution session ID.
func WithExecutionSessionID(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionSessionContextKey{}, sessionID)
}

// WithDisallowFreeAuth returns a child context that requests skipping known free-tier credentials.
func WithDisallowFreeAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, disallowFreeAuthContextKey{}, true)
}

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// Known user-facing error families may be normalized before raw upstream JSON is passed through.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}
	status = NormalizeKnownUserErrorStatus(status, errText)
	if body, ok := BuildContentSafetyErrorBody(status, errText); ok {
		return body
	}
	if body, ok := BuildContextWindowExceededErrorBody(status, errText); ok {
		return body
	}
	if body, ok := BuildRequestFeatureUnsupportedErrorBody(status, errText); ok {
		return body
	}
	if body, ok := BuildClientHintErrorBody(status, errText); ok {
		return body
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}

// NormalizeKnownUserErrorStatus maps deterministic client/request-shape errors away from 5xx.
func NormalizeKnownUserErrorStatus(status int, errText string) int {
	normalized, _ := normalizeKnownUserErrorStatus(status, errText)
	return normalized
}

// NormalizeKnownUserError also checks a previously captured upstream body for hidden error details.
func NormalizeKnownUserError(status int, errText string, captured []byte) (int, string) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}
	if normalized, ok := normalizeKnownUserErrorStatus(status, errText); ok {
		return normalized, errText
	}
	capturedText := strings.TrimSpace(string(captured))
	if capturedText == "" {
		return status, errText
	}
	if normalized, ok := normalizeKnownUserErrorStatus(status, capturedText); ok {
		return normalized, capturedText
	}
	combined := strings.TrimSpace(errText + "\n" + capturedText)
	if normalized, ok := normalizeKnownUserErrorStatus(status, combined); ok {
		return normalized, combined
	}
	return status, errText
}

func normalizeKnownUserErrorStatus(status int, errText string) (int, bool) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if _, ok := contentSafetyDirection(status, errText); ok {
		return http.StatusBadRequest, true
	}
	if IsContextWindowExceededError(status, errText) {
		return http.StatusBadRequest, true
	}
	if IsRequestFeatureUnsupportedError(status, errText) {
		return http.StatusBadRequest, true
	}
	if isInvalidRequestParameterError(status, errText) {
		return http.StatusBadRequest, true
	}
	if isQuotaOrRateLimitError(status, errText) {
		return http.StatusTooManyRequests, true
	}
	return status, false
}

// KnownUserErrorDetail returns the user-facing detail for known, high-volume error families.
func KnownUserErrorDetail(status int, errText string) (ErrorDetail, bool) {
	if direction, ok := contentSafetyDirection(status, errText); ok {
		return ErrorDetail{
			Message: UserFacingContentSafetyMessage(direction),
			Type:    "invalid_request_error",
			Code:    contentPolicyViolationErrorCode,
		}, true
	}
	if detail, ok := contextWindowExceededErrorDetail(status, errText); ok {
		return detail, true
	}
	if detail, ok := requestFeatureUnsupportedErrorDetail(status, errText); ok {
		return detail, true
	}
	return clientHintErrorDetail(status, errText)
}

// BuildClientHintErrorBody builds a short Chinese hint for common generic upstream errors.
func BuildClientHintErrorBody(status int, errText string) ([]byte, bool) {
	detail, ok := clientHintErrorDetail(status, errText)
	if !ok {
		return nil, false
	}
	payload, err := json.Marshal(ErrorResponse{Error: detail})
	if err != nil {
		return nil, false
	}
	return payload, true
}

func clientHintErrorDetail(status int, errText string) (ErrorDetail, bool) {
	if isInvalidRequestParameterError(status, errText) {
		return ErrorDetail{
			Message: "请求参数不兼容或格式不正确。请检查模型、接口路径和工具调用历史；移除当前模型不支持的 temperature、reasoning、tool_choice、max_tokens 等参数，或清理失配的 tool_result 后重试。原样重复提交不会提高成功率。",
			Type:    "invalid_request_error",
			Code:    "invalid_request_parameters",
		}, true
	}
	if isQuotaOrRateLimitError(status, errText) {
		return ErrorDetail{
			Message: "当前模型上游通道额度或速率受限。系统会优先尝试其它可用通道；如果仍失败，请稍后重试或切换模型。",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
		}, true
	}
	if isUpstreamTemporaryError(status, errText) {
		return ErrorDetail{
			Message: "上游模型通道临时异常或超时。系统已尝试可用通道后仍失败，请稍后重试或切换模型；这通常不是提示词格式问题。",
			Type:    "server_error",
			Code:    "upstream_error",
		}, true
	}
	return ErrorDetail{}, false
}

func isInvalidRequestParameterError(status int, errText string) bool {
	if status > 0 && status < http.StatusBadRequest {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(errText))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"invalidparameter",
		"invalid parameter",
		"param incorrect",
		"invalid params",
		"invalid temperature",
		"only 1 is allowed for this model",
		"tool choice",
		"not found in 'tools'",
		"tool result's tool id",
		"out of supported range",
		"prompt is required",
		"does not support image input",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isQuotaOrRateLimitError(status int, errText string) bool {
	lower := strings.ToLower(strings.TrimSpace(errText))
	return status == http.StatusTooManyRequests ||
		strings.Contains(lower, "accountquotaexceeded") ||
		strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "quota exhausted") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests")
}

func isUpstreamTemporaryError(status int, errText string) bool {
	if status < http.StatusInternalServerError {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(errText))
	if lower == "" {
		return true
	}
	for _, marker := range []string{
		"upstream_error",
		"upstream error",
		"do_request_failed",
		"api_error",
		"openai_error",
		"bad gateway",
		"service unavailable",
		"gateway timeout",
		"timeout",
		"socket",
		"connection reset",
		"unexpected eof",
		"internal_server_error",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// StreamingKeepAliveInterval returns the SSE keep-alive interval for this server.
// Returning 0 disables keep-alives (default when unset).
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := defaultStreamingKeepAliveSeconds
	if cfg != nil {
		seconds = cfg.Streaming.KeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// StreamingFlushInterval returns the maximum delay before flushing buffered stream chunks.
func StreamingFlushInterval(cfg *config.SDKConfig) time.Duration {
	milliseconds := 0
	if cfg != nil {
		milliseconds = cfg.Streaming.FlushIntervalMilliseconds
	}
	if milliseconds <= 0 {
		return 0
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// Only include it if the client explicitly provides it.
	key := ""
	requestPath := ""
	clientProfile := ""
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
			requestPath = strings.TrimSpace(ginCtx.FullPath())
			if requestPath == "" && ginCtx.Request.URL != nil {
				requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
			}
			clientProfile = inferClientProfileFromHeaders(ginCtx.Request.Header)
		}
	}
	meta := make(map[string]any)
	if clientProfile != "" {
		meta[coreexecutor.ClientProfileMetadataKey] = clientProfile
	}
	if key != "" {
		meta[idempotencyKeyMetadataKey] = key
	}
	if requestPath != "" {
		meta[coreexecutor.RequestPathMetadataKey] = requestPath
	}
	if pinnedAuthID := pinnedAuthIDFromContext(ctx); pinnedAuthID != "" {
		meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
	}
	if selectedCallback := selectedAuthIDCallbackFromContext(ctx); selectedCallback != nil {
		meta[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
	}
	if disallowFreeAuthFromContext(ctx) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
	}
	return meta
}

func inferClientProfileFromHeaders(headers http.Header) string {
	for _, name := range []string{"User-Agent", "X-Client-Name", "X-App-Name", "X-Title"} {
		value := strings.ToLower(headers.Get(name))
		if strings.Contains(value, "workbuddy") || strings.Contains(value, "codebuddy") {
			return "workbuddy"
		}
	}
	return ""
}

func setReasoningEffortMetadata(meta map[string]any, handlerType, model string, rawJSON []byte) {
	if meta == nil {
		return
	}
	effort := thinking.ExtractReasoningEffort(rawJSON, handlerType, model)
	if effort == "" {
		return
	}
	meta[coreexecutor.ReasoningEffortMetadataKey] = effort
	meta[coreexecutor.ReasoningEffortOriginalMetadataKey] = effort
}

func setServiceTierMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil {
		return
	}
	serviceTier := coreusage.DefaultServiceTier
	node := gjson.GetBytes(rawJSON, "service_tier")
	if node.Exists() {
		value := strings.TrimSpace(node.String())
		if value != "" {
			serviceTier = value
		}
	}
	meta[coreexecutor.ServiceTierMetadataKey] = serviceTier
}

// headersFromContext extracts the original HTTP request headers from the gin context
// embedded in the provided context. This allows session affinity selectors to read
// client-provided session headers.
func headersFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.Request.Header.Clone()
	}
	return nil
}

// queryFromContext extracts the original HTTP request query parameters from the
// gin context embedded in the provided context. Mirrors headersFromContext so
// model routers can observe inbound query parameters for plain HTTP requests,
// where execOptions.Query is not populated by callers.
func queryFromContext(ctx context.Context) url.Values {
	if ctx == nil {
		return nil
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil && ginCtx.Request.URL != nil {
		return ginCtx.Request.URL.Query()
	}
	return nil
}

func pinnedAuthIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(pinnedAuthContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func selectedAuthIDCallbackFromContext(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(selectedAuthCallbackContextKey{})
	if callback, ok := raw.(func(string)); ok && callback != nil {
		return callback
	}
	return nil
}

func executionSessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(executionSessionContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func disallowFreeAuthFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw, ok := ctx.Value(disallowFreeAuthContextKey{}).(bool)
	return ok && raw
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig

	// PluginHost optionally applies plugin interceptors around upstream execution.
	PluginHost PluginInterceptorHost

	// ModelRouterHost optionally routes matching requests to a plugin executor, the router's own
	// executor, or a built-in provider before model-to-provider resolution and auth selection.
	ModelRouterHost PluginModelRouterHost

	admissionUpdateMu sync.Mutex
	admission         atomic.Pointer[admissionController]
	payloadBodyLimit  atomic.Pointer[payloadBodyLimitPolicy]
	amplification     atomic.Pointer[amplificationGuardPolicy]
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	handler := &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
	}
	handler.updateAdmissionController(cfg)
	handler.updatePayloadBodyLimitPolicy(cfg)
	handler.updateAmplificationGuardPolicy(cfg)
	return handler
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) {
	if h == nil {
		return
	}
	h.Cfg = cfg
	h.updateAdmissionController(cfg)
	h.updatePayloadBodyLimitPolicy(cfg)
	h.updateAmplificationGuardPolicy(cfg)
}

// SetPluginHost configures the optional plugin interceptor host.
func (h *BaseAPIHandler) SetPluginHost(host PluginInterceptorHost) {
	if h == nil {
		return
	}
	if isNilPluginInterceptorHost(host) {
		h.PluginHost = nil
		return
	}
	h.PluginHost = host
}

// SetModelRouterHost configures the optional plugin model router host.
func (h *BaseAPIHandler) SetModelRouterHost(host PluginModelRouterHost) {
	if h == nil {
		return
	}
	if isNilPluginModelRouterHost(host) {
		h.ModelRouterHost = nil
		return
	}
	h.ModelRouterHost = host
}

func isNilPluginInterceptorHost(host PluginInterceptorHost) bool {
	return isNilInterface(host)
}

func isNilPluginModelRouterHost(host PluginModelRouterHost) bool {
	return isNilInterface(host)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	// A typed nil pointer stored in an interface is not equal to nil.
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context and the API handler into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc) {
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}

	if requestCtx != nil && logging.GetRequestID(parentCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		} else if requestID = logging.GetGinRequestID(c); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		}
	}
	newCtx, cancel := context.WithCancel(parentCtx)

	endpointMethod := ""
	endpointPath := ""
	if c != nil && c.Request != nil {
		path := strings.TrimSpace(c.FullPath())
		if path == "" && c.Request.URL != nil {
			path = strings.TrimSpace(c.Request.URL.Path)
		}
		if path != "" {
			method := strings.TrimSpace(c.Request.Method)
			endpointMethod = method
			endpointPath = path
		}
	}
	if endpointMethod != "" || endpointPath != "" {
		newCtx = logging.WithEndpointParts(newCtx, endpointMethod, endpointPath)
	}
	newCtx = coreauth.WithStreamSummaryTracking(newCtx)
	newCtx = logging.WithToolStreamRepairTracking(newCtx)
	newCtx = logging.WithResponseStatusHolder(newCtx)
	newCtx = logging.WithResponseHeadersHolder(newCtx)

	cancelCtx := newCtx
	if requestCtx != nil && requestCtx != parentCtx {
		go func() {
			select {
			case <-requestCtx.Done():
				cancel()
			case <-cancelCtx.Done():
			}
		}()
	}
	newCtx = context.WithValue(newCtx, "gin", c)
	newCtx = context.WithValue(newCtx, "handler", handler)
	return newCtx, func(params ...interface{}) {
		defer coreauth.MarkStreamSummaryDownstreamDone(cancelCtx)
		if c != nil {
			logging.SetResponseStatus(cancelCtx, c.Writer.Status())
		}
		if h.Cfg.RequestLog && len(params) == 1 {
			if captured, exists := c.Get(logging.APIResponseCapturedContextKey); exists {
				if capturedBool, ok := captured.(bool); ok && capturedBool {
					cancel()
					return
				}
			}
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(bytes.TrimSpace(existingBytes)) > 0 {
					switch params[0].(type) {
					case error, string:
						cancel()
						return
					}
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				if existingBytes := currentAPIResponse(c); len(existingBytes) > 0 {
					trimmedPayload := bytes.TrimSpace(payload)
					if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
						cancel()
						return
					}
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

const admissionKeepAliveGateKey = "admission_keepalive_gate"

type admissionKeepAliveGate struct {
	ready chan struct{}
	once  sync.Once
}

func newAdmissionKeepAliveGate() *admissionKeepAliveGate {
	return &admissionKeepAliveGate{ready: make(chan struct{})}
}

func (g *admissionKeepAliveGate) allow() {
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.ready) })
}

func allowAdmissionKeepAlive(ctx context.Context) {
	if ctx == nil {
		return
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return
	}
	value, exists := ginCtx.Get(admissionKeepAliveGateKey)
	if !exists {
		return
	}
	gate, _ := value.(*admissionKeepAliveGate)
	gate.allow()
}

// StartNonStreamingKeepAlive emits blank lines at the configured interval while waiting for a non-streaming response.
// The first write is held until local admission succeeds so a queue rejection can still return its HTTP status.
// It returns a stop function that must be called before writing the final response.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	if h == nil || c == nil {
		return func() {}
	}
	interval := NonStreamingKeepAliveInterval(h.Cfg)
	if interval <= 0 {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	gate := newAdmissionKeepAliveGate()
	c.Set(admissionKeepAliveGateKey, gate)

	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case <-gate.ready:
				default:
					continue
				}
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
		wg.Wait()
	}
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	// Capture timestamp on first API response
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); !exists {
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
	}

	if existingBytes := currentAPIResponse(c); len(existingBytes) > 0 {
		combined := make([]byte, 0, len(existingBytes)+len(data)+1)
		combined = append(combined, existingBytes...)
		if existingBytes[len(existingBytes)-1] != '\n' {
			combined = append(combined, '\n')
		}
		combined = append(combined, data...)
		c.Set("API_RESPONSE", combined)
		return
	}

	c.Set("API_RESPONSE", internalpayload.CloneBytes(data))
}

func currentAPIResponse(c *gin.Context) []byte {
	return helps.MaterializeAPIResponse(c)
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageWithAuthManager executes an OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

func (h *BaseAPIHandler) executeWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeWithAuthManagerFormats(ctx, handlerType, handlerType, modelName, rawJSON, alt, allowImageModel, modelExecutionOptions{})
}

func (h *BaseAPIHandler) executeWithAuthManagerFormats(ctx context.Context, entryProtocol, exitProtocol, modelName string, rawJSON []byte, alt string, allowImageModel bool, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	execOptions.complexityDimensions = executionComplexityDimensions(entryProtocol, alt, false, allowImageModel, false)
	var release func()
	var errAdmission error
	ctx, release, errAdmission = h.inspectAndAcquireAdmission(ctx, rawJSON, &execOptions)
	if errAdmission != nil {
		return nil, nil, admissionErrorMessage(errAdmission)
	}
	defer release()

	originalRequestedModel := modelName
	routeDecision := h.applyModelRouter(ctx, entryProtocol, modelName, rawJSON, false, execOptions)
	responseProtocol := modelExecutionResponseProtocol(entryProtocol, exitProtocol)
	if routeDecision.ExecutorPluginID != "" {
		return h.executeWithPluginExecutor(ctx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, routeDecision.ExecutorPluginID, execOptions)
	}
	providers, normalizedModel, errMsg := h.providersForExecution(modelName, originalRequestedModel, allowImageModel, routeDecision)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	providers = filterProvidersByToolCompatibility(providers, execOptions.complexity)
	if len(providers) == 0 {
		return nil, nil, toolCompatibilitySelectionError(modelName, execOptions.complexity)
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	addModelExecutionSourceMetadata(reqMeta, execOptions.InternalSource)
	setExecutionRequestShapeMetadata(reqMeta, rawJSON, execOptions)
	setReasoningEffortMetadata(reqMeta, entryProtocol, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	opts := coreexecutor.Options{
		Stream:                      false,
		Alt:                         alt,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(entryProtocol),
		ResponseFormat:              sdktranslator.FromString(responseProtocol),
		Headers:                     modelExecutionHeaders(ctx, execOptions.Headers),
		Query:                       modelExecutionQuery(ctx, execOptions.Query),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, execOptions.SkipInterceptorPluginID),
	}
	opts.Metadata = reqMeta
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, entryProtocol, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	resp, err := h.AuthManager.Execute(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		status := statusFromError(err)
		if status <= 0 {
			status = http.StatusInternalServerError
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		return nil, nil, &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
	}
	executedReq, executedOpts := afterAuthCapture.apply(req, opts)
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(ctx, responseProtocol, normalizedModel, originalRequestedModel, executedOpts, rawResponseHeaders, responseHeaders, executedOpts.OriginalRequest, executedReq.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	return body, responseHeaders, nil
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	return h.executeCountWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, modelExecutionOptions{})
}

func (h *BaseAPIHandler) executeCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	execOptions.complexityDimensions = executionComplexityDimensions(handlerType, alt, false, false, true)
	var release func()
	var errAdmission error
	ctx, release, errAdmission = h.inspectAndAcquireAdmission(ctx, rawJSON, &execOptions)
	if errAdmission != nil {
		return nil, nil, admissionErrorMessage(errAdmission)
	}
	defer release()

	originalRequestedModel := modelName
	routeDecision := h.applyModelRouter(ctx, handlerType, modelName, rawJSON, false, execOptions)
	if routeDecision.ExecutorPluginID != "" {
		return h.countWithPluginExecutor(ctx, handlerType, modelName, originalRequestedModel, rawJSON, alt, routeDecision.ExecutorPluginID, execOptions)
	}
	providers, normalizedModel, errMsg := h.providersForExecution(modelName, originalRequestedModel, false, routeDecision)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	providers = filterProvidersByToolCompatibility(providers, execOptions.complexity)
	if len(providers) == 0 {
		return nil, nil, toolCompatibilitySelectionError(modelName, execOptions.complexity)
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	setExecutionRequestShapeMetadata(reqMeta, rawJSON, execOptions)
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	opts := coreexecutor.Options{
		Stream:                      false,
		Alt:                         alt,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(handlerType),
		Headers:                     modelExecutionHeaders(ctx, execOptions.Headers),
		Query:                       modelExecutionQuery(ctx, execOptions.Query),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, execOptions.SkipInterceptorPluginID),
	}
	opts.Metadata = reqMeta
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, handlerType, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	resp, err := h.AuthManager.ExecuteCount(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		status := statusFromError(err)
		if status <= 0 {
			status = http.StatusInternalServerError
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		return nil, nil, &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
	}
	executedReq, executedOpts := afterAuthCapture.apply(req, opts)
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(ctx, handlerType, normalizedModel, originalRequestedModel, executedOpts, rawResponseHeaders, responseHeaders, executedOpts.OriginalRequest, executedReq.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	return body, responseHeaders, nil
}

func (h *BaseAPIHandler) executeWithPluginExecutor(ctx context.Context, entryProtocol, responseProtocol, modelName, originalRequestedModel string, rawJSON []byte, alt, executorPluginID string, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	host := h.pluginExecutorHost()
	if host == nil {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor host is unavailable")}
	}
	req, opts := h.pluginExecutorRequest(ctx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, false, execOptions)
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, entryProtocol, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	req, opts = h.applyRequestInterceptorsAfterPluginExecutorRoute(ctx, host, executorPluginID, entryProtocol, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	resp, errExecute := host.ExecutePluginExecutor(ctx, executorPluginID, req, opts)
	if errExecute != nil {
		return nil, nil, executionErrorMessage(errExecute)
	}
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(ctx, responseProtocol, modelName, originalRequestedModel, opts, rawResponseHeaders, responseHeaders, opts.OriginalRequest, req.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	return body, responseHeaders, nil
}

func (h *BaseAPIHandler) countWithPluginExecutor(ctx context.Context, handlerType, modelName, originalRequestedModel string, rawJSON []byte, alt, executorPluginID string, execOptions modelExecutionOptions) ([]byte, http.Header, *interfaces.ErrorMessage) {
	host := h.pluginExecutorHost()
	if host == nil {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor host is unavailable")}
	}
	req, opts := h.pluginExecutorRequest(ctx, handlerType, handlerType, modelName, originalRequestedModel, rawJSON, alt, false, execOptions)
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, handlerType, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	req, opts = h.applyRequestInterceptorsAfterPluginExecutorRoute(ctx, host, executorPluginID, handlerType, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	resp, errCount := host.CountPluginExecutor(ctx, executorPluginID, req, opts)
	if errCount != nil {
		return nil, nil, executionErrorMessage(errCount)
	}
	rawResponseHeaders := cloneHeader(resp.Headers)
	responseHeaders := downstreamHeadersFromExecutor(rawResponseHeaders, PassthroughHeadersEnabled(h.Cfg))
	body, responseHeaders := h.applyResponseInterceptors(ctx, handlerType, modelName, originalRequestedModel, opts, rawResponseHeaders, responseHeaders, opts.OriginalRequest, req.Payload, resp.Payload, http.StatusOK, execOptions.SkipInterceptorPluginID)
	return body, responseHeaders, nil
}

func (h *BaseAPIHandler) pluginExecutorRequest(ctx context.Context, entryProtocol, responseProtocol, modelName, originalRequestedModel string, rawJSON []byte, alt string, stream bool, execOptions modelExecutionOptions) (coreexecutor.Request, coreexecutor.Options) {
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	addModelExecutionSourceMetadata(reqMeta, execOptions.InternalSource)
	setExecutionRequestShapeMetadata(reqMeta, rawJSON, execOptions)
	setReasoningEffortMetadata(reqMeta, entryProtocol, modelName, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{Model: modelName, Payload: payload}
	opts := coreexecutor.Options{
		Stream:          stream,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(entryProtocol),
		ResponseFormat:  sdktranslator.FromString(responseProtocol),
		Headers:         modelExecutionHeaders(ctx, execOptions.Headers),
		Query:           modelExecutionQuery(ctx, execOptions.Query),
		Metadata:        reqMeta,
	}
	return req, opts
}

func (h *BaseAPIHandler) applyRequestInterceptorsAfterPluginExecutorRoute(ctx context.Context, host PluginExecutorHost, executorPluginID, entryProtocol, originalRequestedModel string, req coreexecutor.Request, opts coreexecutor.Options, skipPluginID string) (coreexecutor.Request, coreexecutor.Options) {
	if !requestInterceptorsEnabled(h.interceptorHost()) {
		return req, opts
	}
	toFormat := sdktranslator.FromString(entryProtocol)
	if resolver, ok := host.(pluginExecutorFormatResolver); ok && resolver != nil {
		if resolved := resolver.PluginExecutorRequestToFormat(executorPluginID, req, opts); resolved != "" {
			toFormat = resolved
		}
	}
	resp := h.applyRequestInterceptorsAfterAuth(ctx, coreexecutor.RequestAfterAuthInterceptRequest{
		SourceFormat:   opts.SourceFormat,
		ToFormat:       toFormat,
		Model:          req.Model,
		RequestedModel: originalRequestedModel,
		Stream:         opts.Stream,
		Headers:        cloneHeader(opts.Headers),
		Body:           cloneBytes(req.Payload),
		Metadata:       opts.Metadata,
	}, skipPluginID)
	opts.Headers = mergeRequestInterceptorHeaders(opts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		req.Payload = cloneBytes(resp.Body)
		opts.OriginalRequest = cloneBytes(resp.Body)
	}
	return req, opts
}

func executionErrorMessage(err error) *interfaces.ErrorMessage {
	status := statusFromError(err)
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	var addon http.Header
	if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
		if hdr := he.Headers(); hdr != nil {
			addon = hdr.Clone()
		}
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
}

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
// The returned http.Header carries upstream response headers captured before streaming begins.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageStreamWithAuthManager executes a streaming OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

func (h *BaseAPIHandler) streamWithPluginExecutor(ctx context.Context, entryProtocol, responseProtocol, modelName, originalRequestedModel string, rawJSON []byte, alt, executorPluginID string, execOptions modelExecutionOptions, releaseAdmission func()) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	host := h.pluginExecutorHost()
	if host == nil {
		releaseAdmission()
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor host is unavailable")}
		close(errChan)
		return nil, nil, errChan
	}
	req, opts := h.pluginExecutorRequest(ctx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, true, execOptions)
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, entryProtocol, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	req, opts = h.applyRequestInterceptorsAfterPluginExecutorRoute(ctx, host, executorPluginID, entryProtocol, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	streamResult, errStream := host.ExecutePluginExecutorStream(ctx, executorPluginID, req, opts)
	if errStream != nil {
		releaseAdmission()
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- executionErrorMessage(errStream)
		close(errChan)
		return nil, nil, errChan
	}
	if streamResult == nil {
		releaseAdmission()
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor returned nil stream")}
		close(errChan)
		return nil, nil, errChan
	}

	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.Cfg)
	interceptorHost := h.interceptorHost()
	streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)
	rawStreamHeaders := cloneHeader(streamResult.Headers)
	baseStreamHeaders := cloneHeader(streamResult.Headers)
	upstreamHeaders := downstreamHeadersFromExecutor(rawStreamHeaders, passthroughHeadersEnabled)
	if upstreamHeaders == nil && (passthroughHeadersEnabled || streamInterceptorsActive) {
		upstreamHeaders = make(http.Header)
	}
	streamHeadersCommitted := false
	applyStreamHeaders := func(headers http.Header) {
		rawStreamHeaders = finalInterceptorHeaders(rawStreamHeaders, headers)
		if streamHeadersCommitted || upstreamHeaders == nil {
			return
		}
		nextHeaders := downstreamHeadersAfterInterceptors(baseStreamHeaders, rawStreamHeaders, passthroughHeadersEnabled)
		replaceHeader(upstreamHeaders, nextHeaders)
	}
	if streamInterceptorsActive {
		intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
			SourceFormat:    responseProtocol,
			Model:           modelName,
			RequestedModel:  originalRequestedModel,
			RequestHeaders:  cloneHeader(opts.Headers),
			ResponseHeaders: cloneHeader(rawStreamHeaders),
			OriginalRequest: cloneBytes(opts.OriginalRequest),
			RequestBody:     cloneBytes(req.Payload),
			ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
			Metadata:        opts.Metadata,
		}, execOptions.SkipInterceptorPluginID)
		applyStreamHeaders(intercepted.Headers)
	}

	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	headersReady := make(chan struct{})
	var headersReadyOnce sync.Once
	markHeadersReady := func() {
		headersReadyOnce.Do(func() {
			close(headersReady)
		})
	}
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	chunks := streamResult.Chunks
	if chunks == nil {
		closed := make(chan coreexecutor.StreamChunk)
		close(closed)
		chunks = closed
	}
	go func() {
		defer releaseAdmission()
		defer close(dataChan)
		defer close(errChan)
		defer markHeadersReady()
		chunkIndex := 0
		var historyChunks [][]byte
		for {
			chunk, ok, canceled := nextStreamChunk(ctx, nil, nil, chunks)
			if canceled {
				return
			}
			if !ok {
				return
			}
			if chunk.Err != nil {
				select {
				case errChan <- executionErrorMessage(chunk.Err):
				case <-done:
				}
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			payload := cloneBytes(chunk.Payload)
			if streamInterceptorsActive {
				intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
					SourceFormat:    responseProtocol,
					Model:           modelName,
					RequestedModel:  originalRequestedModel,
					RequestHeaders:  cloneHeader(opts.Headers),
					ResponseHeaders: cloneHeader(rawStreamHeaders),
					OriginalRequest: cloneBytes(opts.OriginalRequest),
					RequestBody:     cloneBytes(req.Payload),
					Body:            payload,
					HistoryChunks:   cloneByteSlices(historyChunks),
					ChunkIndex:      chunkIndex,
					Metadata:        opts.Metadata,
				}, execOptions.SkipInterceptorPluginID)
				applyStreamHeaders(intercepted.Headers)
				if len(intercepted.Body) > 0 {
					payload = cloneBytes(intercepted.Body)
				}
				chunkIndex++
				if intercepted.DropChunk {
					continue
				}
			} else {
				chunkIndex++
			}
			if responseProtocol == "openai-response" {
				if errValidate := validateSSEDataJSON(payload); errValidate != nil {
					select {
					case errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errValidate}:
					case <-done:
					}
					return
				}
			}
			streamHeadersCommitted = true
			// Freeze the returned header map before publishing the first payload.
			markHeadersReady()
			select {
			case dataChan <- payload:
				if streamInterceptorsActive {
					historyChunks = appendStreamInterceptorHistory(historyChunks, payload)
				}
			case <-done:
				return
			}
		}
	}()
	if streamInterceptorsActive {
		<-headersReady
	}
	return dataChan, upstreamHeaders, errChan
}

func (h *BaseAPIHandler) executeStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManagerFormats(ctx, handlerType, handlerType, modelName, rawJSON, alt, allowImageModel, modelExecutionOptions{})
}

func (h *BaseAPIHandler) executeStreamWithAuthManagerFormats(ctx context.Context, entryProtocol, exitProtocol, modelName string, rawJSON []byte, alt string, allowImageModel bool, execOptions modelExecutionOptions) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	execOptions.complexityDimensions = executionComplexityDimensions(entryProtocol, alt, true, allowImageModel, false)
	var release func()
	var errAdmission error
	ctx, release, errAdmission = h.inspectAndAcquireAdmission(ctx, rawJSON, &execOptions)
	if errAdmission != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- admissionErrorMessage(errAdmission)
		close(errChan)
		return nil, nil, errChan
	}
	owned := true
	defer func() {
		if owned {
			release()
		}
	}()
	dataChan, headers, errChan := h.executeAdmittedStreamWithAuthManagerFormats(ctx, entryProtocol, exitProtocol, modelName, rawJSON, alt, allowImageModel, execOptions, release)
	owned = false
	return dataChan, headers, errChan
}

func (h *BaseAPIHandler) executeAdmittedStreamWithAuthManagerFormats(ctx context.Context, entryProtocol, exitProtocol, modelName string, rawJSON []byte, alt string, allowImageModel bool, execOptions modelExecutionOptions, releaseAdmission func()) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	originalRequestedModel := modelName
	routeDecision := h.applyModelRouter(ctx, entryProtocol, modelName, rawJSON, true, execOptions)
	responseProtocol := modelExecutionResponseProtocol(entryProtocol, exitProtocol)
	if routeDecision.ExecutorPluginID != "" {
		return h.streamWithPluginExecutor(ctx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, routeDecision.ExecutorPluginID, execOptions, releaseAdmission)
	}
	providers, normalizedModel, errMsg := h.providersForExecution(modelName, originalRequestedModel, allowImageModel, routeDecision)
	if errMsg != nil {
		releaseAdmission()
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	providers = filterProvidersByToolCompatibility(providers, execOptions.complexity)
	if len(providers) == 0 {
		releaseAdmission()
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- toolCompatibilitySelectionError(modelName, execOptions.complexity)
		close(errChan)
		return nil, nil, errChan
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	addModelExecutionSourceMetadata(reqMeta, execOptions.InternalSource)
	setExecutionRequestShapeMetadata(reqMeta, rawJSON, execOptions)
	setReasoningEffortMetadata(reqMeta, entryProtocol, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	opts := coreexecutor.Options{
		Stream:                      true,
		Alt:                         alt,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(entryProtocol),
		ResponseFormat:              sdktranslator.FromString(responseProtocol),
		Headers:                     modelExecutionHeaders(ctx, execOptions.Headers),
		Query:                       modelExecutionQuery(ctx, execOptions.Query),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, execOptions.SkipInterceptorPluginID),
	}
	opts.Metadata = reqMeta
	req, opts = h.applyRequestInterceptorsBeforeAuth(ctx, entryProtocol, originalRequestedModel, req, opts, execOptions.SkipInterceptorPluginID)
	streamResult, err := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		releaseAdmission()
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		status := statusFromError(err)
		if status <= 0 {
			status = http.StatusInternalServerError
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		errChan <- &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
		close(errChan)
		return nil, nil, errChan
	}
	executedRequest := func() (coreexecutor.Request, coreexecutor.Options) {
		return afterAuthCapture.apply(req, opts)
	}
	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.Cfg)
	interceptorHost := h.interceptorHost()
	streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)
	// Capture upstream headers from the initial connection synchronously before the goroutine starts.
	// Keep a mutable map so bootstrap retries can replace it before first payload is sent.
	rawStreamHeaders := cloneHeader(streamResult.Headers)
	baseStreamHeaders := cloneHeader(streamResult.Headers)
	upstreamHeaders := downstreamHeadersFromExecutor(rawStreamHeaders, passthroughHeadersEnabled)
	if upstreamHeaders == nil && (passthroughHeadersEnabled || streamInterceptorsActive) {
		upstreamHeaders = make(http.Header)
	}
	chunks := streamResult.Chunks
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	headersReady := make(chan struct{})
	var headersReadyOnce sync.Once
	markHeadersReady := func() {
		headersReadyOnce.Do(func() {
			close(headersReady)
		})
	}
	streamHeaderInitialized := false
	streamHeadersCommitted := false

	applyStreamHeaders := func(headers http.Header) {
		rawStreamHeaders = finalInterceptorHeaders(rawStreamHeaders, headers)
		if streamHeadersCommitted {
			return
		}
		nextHeaders := downstreamHeadersAfterInterceptors(baseStreamHeaders, rawStreamHeaders, passthroughHeadersEnabled)
		replaceHeader(upstreamHeaders, nextHeaders)
	}

	applyStreamHeaderInit := func() {
		if !streamInterceptorsActive || streamHeaderInitialized {
			return
		}
		executedReq, executedOpts := executedRequest()
		intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
			SourceFormat:    responseProtocol,
			Model:           normalizedModel,
			RequestedModel:  originalRequestedModel,
			RequestHeaders:  cloneHeader(executedOpts.Headers),
			ResponseHeaders: cloneHeader(rawStreamHeaders),
			OriginalRequest: cloneBytes(executedOpts.OriginalRequest),
			RequestBody:     cloneBytes(executedReq.Payload),
			ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
			Metadata:        executedOpts.Metadata,
		}, execOptions.SkipInterceptorPluginID)
		applyStreamHeaders(intercepted.Headers)
		streamHeaderInitialized = true
	}

	pendingChunks := make([]coreexecutor.StreamChunk, 0, 1)
	streamClosedBeforeRead := false
	streamCanceledBeforeRead := false
	readInitialStreamChunks := func() {
		for {
			var chunk coreexecutor.StreamChunk
			var ok bool
			if ctx != nil {
				select {
				case <-ctx.Done():
					streamCanceledBeforeRead = true
					return
				case chunk, ok = <-chunks:
				}
			} else {
				chunk, ok = <-chunks
			}
			if !ok {
				streamClosedBeforeRead = true
				applyStreamHeaderInit()
				return
			}
			pendingChunks = append(pendingChunks, chunk)
			if chunk.Err != nil {
				return
			}
			if len(chunk.Payload) > 0 {
				applyStreamHeaderInit()
				return
			}
		}
	}
	readInitialStreamChunks()

	go func() {
		defer releaseAdmission()
		defer close(dataChan)
		defer close(errChan)
		defer markHeadersReady()
		if streamCanceledBeforeRead {
			return
		}
		sentPayload := false
		bootstrapRetries := 0
		chunkIndex := 0
		var historyChunks [][]byte
		maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)

		sendErr := func(msg *interfaces.ErrorMessage) bool {
			if ctx == nil {
				errChan <- msg
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case errChan <- msg:
				return true
			}
		}

		sendData := func(chunk []byte) bool {
			if ctx == nil {
				dataChan <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case dataChan <- chunk:
				return true
			}
		}

		bootstrapEligible := func(err error) bool {
			status := statusFromError(err)
			if status == 0 {
				return true
			}
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
				http.StatusRequestTimeout, http.StatusTooManyRequests:
				return true
			default:
				return status >= http.StatusInternalServerError
			}
		}

	outer:
		for {
			for {
				chunk, ok, canceled := nextStreamChunk(ctx, &pendingChunks, &streamClosedBeforeRead, chunks)
				if canceled {
					return
				}
				if !ok {
					applyStreamHeaderInit()
					return
				}
				if chunk.Err != nil {
					streamErr := chunk.Err
					// Safe bootstrap recovery: if the upstream fails before any payload bytes are sent,
					// retry a few times (to allow auth rotation / transient recovery) and then attempt model fallback.
					if !sentPayload {
						if bootstrapRetries < maxBootstrapRetries && bootstrapEligible(streamErr) {
							bootstrapRetries++
							retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
							if retryErr == nil {
								rawStreamHeaders = cloneHeader(retryResult.Headers)
								baseStreamHeaders = cloneHeader(retryResult.Headers)
								replaceHeader(upstreamHeaders, downstreamHeadersFromExecutor(rawStreamHeaders, passthroughHeadersEnabled))
								streamHeaderInitialized = false
								streamHeadersCommitted = false
								pendingChunks = nil
								streamClosedBeforeRead = false
								chunks = retryResult.Chunks
								continue outer
							}
							streamErr = enrichAuthSelectionError(retryErr, providers, normalizedModel)
						}
					}

					_ = sendErr(executionErrorMessage(streamErr))
					return
				}
				if len(chunk.Payload) > 0 {
					applyStreamHeaderInit()
					payload := cloneBytes(chunk.Payload)
					if streamInterceptorsActive {
						executedReq, executedOpts := executedRequest()
						intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
							SourceFormat:    responseProtocol,
							Model:           normalizedModel,
							RequestedModel:  originalRequestedModel,
							RequestHeaders:  cloneHeader(executedOpts.Headers),
							ResponseHeaders: cloneHeader(rawStreamHeaders),
							OriginalRequest: cloneBytes(executedOpts.OriginalRequest),
							RequestBody:     cloneBytes(executedReq.Payload),
							Body:            payload,
							HistoryChunks:   cloneByteSlices(historyChunks),
							ChunkIndex:      chunkIndex,
							Metadata:        executedOpts.Metadata,
						}, execOptions.SkipInterceptorPluginID)
						applyStreamHeaders(intercepted.Headers)
						if len(intercepted.Body) > 0 {
							payload = cloneBytes(intercepted.Body)
						}
						chunkIndex++
						if intercepted.DropChunk {
							continue
						}
					} else {
						chunkIndex++
					}
					if responseProtocol == "openai-response" {
						if errValidate := validateSSEDataJSON(payload); errValidate != nil {
							_ = sendErr(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errValidate})
							return
						}
					}
					sentPayload = true
					streamHeadersCommitted = true
					// Freeze the returned header map before publishing the first payload.
					// sendData may block until the caller receives the returned channels.
					markHeadersReady()
					if okSendData := sendData(payload); !okSendData {
						return
					}
					if streamInterceptorsActive {
						historyChunks = appendStreamInterceptorHistory(historyChunks, payload)
					}
				}
			}
		}
	}()
	<-headersReady
	return dataChan, upstreamHeaders, errChan
}

func validateSSEDataJSON(chunk []byte) error {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if json.Valid(data) {
			continue
		}
		return invalidSSEDataJSONError(data)
	}
	return nil
}

func statusFromError(err error) int {
	return failurecontract.HTTPStatusOf(err)
}

func (h *BaseAPIHandler) getRequestDetails(modelName string) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	return h.getRequestDetailsWithOptions(modelName, false)
}

func ResolveProvidersForModel(modelName string, authManager *coreauth.Manager) []string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return nil
	}

	providers := util.GetProviderName(modelName)
	if len(providers) > 0 || authManager == nil {
		return providers
	}
	return authManager.ResolveConfiguredProviders(modelName)
}

func NormalizePublicModelHint(modelName string) (baseModel string, hint string) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || !strings.HasSuffix(modelName, "]") {
		return modelName, ""
	}
	open := strings.LastIndex(modelName, "[")
	if open <= 0 || open >= len(modelName)-1 {
		return modelName, ""
	}
	hint = strings.TrimSpace(modelName[open+1 : len(modelName)-1])
	if hint == "" {
		return modelName, ""
	}
	baseModel = strings.TrimSpace(modelName[:open])
	if baseModel == "" {
		return modelName, ""
	}
	return baseModel, hint
}

// providersForExecution resolves the providers and normalized model for a request. When a model
// router selected a built-in provider, it skips model->provider resolution and uses the router's
// provider (with an optional target model); otherwise it falls back to the registry-based path.
func (h *BaseAPIHandler) providersForExecution(modelName, originalRequestedModel string, allowImageModel bool, routeDecision modelRouteDecision) ([]string, string, *interfaces.ErrorMessage) {
	if routeDecision.Provider != "" {
		normalizedModel := originalRequestedModel
		if routeDecision.Model != "" {
			normalizedModel = routeDecision.Model
		}
		baseModel := strings.TrimSpace(thinking.ParseSuffix(normalizedModel).ModelName)
		if baseModel == "" {
			baseModel = strings.TrimSpace(normalizedModel)
		}
		if isImageOnlyModelForTextEndpoint(baseModel) && !allowImageModel {
			return nil, "", &interfaces.ErrorMessage{
				StatusCode: http.StatusServiceUnavailable,
				Error:      fmt.Errorf("model %s is only supported on /v1/images/generations and /v1/images/edits", routeModelBaseName(baseModel)),
			}
		}
		return []string{routeDecision.Provider}, normalizedModel, nil
	}
	return h.getRequestDetailsWithOptions(modelName, allowImageModel)
}

func (h *BaseAPIHandler) getRequestDetailsWithOptions(modelName string, allowImageModel bool) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
			if initialSuffix.HasSuffix {
				resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
			} else {
				resolvedModelName = resolvedBase
			}
		}
	} else {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedModelName = util.ResolveAutoModel(modelName)
		}
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)

	if errMsg := h.validateImageOnlyModel(baseModel, allowImageModel); errMsg != nil {
		return nil, "", errMsg
	}

	if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return []string{"home"}, resolvedModelName, nil
	}

	providers = ResolveProvidersForModel(baseModel, h.AuthManager)
	// Fallback: if baseModel has no provider but differs from resolvedModelName,
	// try using the full model name. This handles edge cases where custom models
	// may be registered with their full suffixed name (e.g., "my-model(8192)").
	// Evaluated in Story 11.8: This fallback is intentionally preserved to support
	// custom model registrations that include thinking suffixes.
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = ResolveProvidersForModel(resolvedModelName, h.AuthManager)
	}
	if len(providers) == 0 {
		if hintedBase, _ := NormalizePublicModelHint(baseModel); hintedBase != "" && hintedBase != baseModel {
			providers = ResolveProvidersForModel(hintedBase, h.AuthManager)
		}
	}
	if len(providers) == 0 {
		if hintedBase, _ := NormalizePublicModelHint(resolvedModelName); hintedBase != "" && hintedBase != resolvedModelName {
			providers = ResolveProvidersForModel(hintedBase, h.AuthManager)
		}
	}

	if len(providers) == 0 {
		return nil, "", &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("unknown provider for model %s", modelName)}
	}

	// The thinking suffix is preserved in the model name itself, so no
	// metadata-based configuration passing is needed.
	return providers, resolvedModelName, nil
}

func filterProvidersByToolCompatibility(providers []string, compatibility *complexityVector) []string {
	if len(providers) == 0 {
		return nil
	}
	if compatibility == nil {
		return providers
	}

	if payloadHasBuiltinImageGenerationTool(compatibility) {
		filtered := make([]string, 0, len(providers))
		for _, provider := range providers {
			if !providerSupportsBuiltinImageGeneration(provider) {
				continue
			}
			filtered = append(filtered, provider)
		}
		providers = filtered
	}

	if !(compatibility.hasSearchTool && compatibility.hasNonSearchTool) {
		return providers
	}

	filtered := make([]string, 0, len(providers))
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(provider), "antigravity") {
			continue
		}
		filtered = append(filtered, provider)
	}
	return filtered
}

func payloadHasBuiltinImageGenerationTool(compatibility *complexityVector) bool {
	return compatibility != nil && compatibility.hasBuiltinImageGeneration
}

func providerSupportsBuiltinImageGeneration(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
}

func toolCompatibilitySelectionError(modelName string, compatibility *complexityVector) *interfaces.ErrorMessage {
	if payloadHasBuiltinImageGenerationTool(compatibility) {
		return &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("the image_generation builtin tool currently requires a codex-compatible channel for model %s", modelName),
		}
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      fmt.Errorf("mixed search and non-search tools are not supported by available providers for model %s", modelName),
	}
}

func imageModelEndpointHelpMessage(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = "this image model"
	}
	return fmt.Sprintf("请求路径错误：%s 是图片生成/编辑模型，不能通过文字对话接口调用，例如 /v1/chat/completions、/v1/responses 或 /v1/messages。请使用正确的 Images API URL：如果你的 base_url 是 https://your-domain/v1，生图请调用 POST https://your-domain/v1/images/generations，请求 JSON 至少包含 model=%s 和 prompt；图片编辑/参考图请调用 POST https://your-domain/v1/images/edits，使用 multipart/form-data，并包含 model=%s、prompt 和 image 文件。不要继续使用 chat/messages/responses 接口调用这个图片模型。", modelName, modelName, modelName)
}

func (h *BaseAPIHandler) validateImageOnlyModel(modelName string, allowImageModel bool) *interfaces.ErrorMessage {
	baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
	if baseModel == "" {
		baseModel = strings.TrimSpace(modelName)
	}
	if isImageOnlyModelForTextEndpoint(baseModel) && !allowImageModel {
		return &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      errors.New(imageModelEndpointHelpMessage(routeModelBaseName(baseModel))),
		}
	}
	return nil
}

func routeModelBaseName(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[idx+1:])
	}
	return model
}

func isImageOnlyModelForTextEndpoint(model string) bool {
	switch strings.ToLower(routeModelBaseName(model)) {
	case "gpt-image-2", "image-01", "image-01-live":
		return true
	default:
		return false
	}
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func cloneBytes(src []byte) []byte {
	return internalpayload.CloneBytes(src)
}

func cloneByteSlices(src [][]byte) [][]byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([][]byte, 0, len(src))
	for _, item := range src {
		dst = append(dst, cloneBytes(item))
	}
	return dst
}

func nextStreamChunk(ctx context.Context, pending *[]coreexecutor.StreamChunk, closed *bool, chunks <-chan coreexecutor.StreamChunk) (coreexecutor.StreamChunk, bool, bool) {
	if pending != nil && len(*pending) > 0 {
		chunk := (*pending)[0]
		(*pending)[0] = coreexecutor.StreamChunk{}
		*pending = (*pending)[1:]
		return chunk, true, false
	}
	if closed != nil && *closed {
		return coreexecutor.StreamChunk{}, false, false
	}
	var chunk coreexecutor.StreamChunk
	var ok bool
	if ctx != nil {
		select {
		case <-ctx.Done():
			return coreexecutor.StreamChunk{}, false, true
		case chunk, ok = <-chunks:
		}
	} else {
		chunk, ok = <-chunks
	}
	if !ok && closed != nil {
		*closed = true
	}
	return chunk, ok, false
}

func appendStreamInterceptorHistory(history [][]byte, chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return history
	}
	history = append(history, cloneBytes(chunk))
	for len(history) > maxStreamInterceptorHistoryChunks || byteSlicesSize(history) > maxStreamInterceptorHistoryBytes {
		history[0] = nil
		history = history[1:]
	}
	if len(history) == 0 {
		return nil
	}
	return history
}

func byteSlicesSize(items [][]byte) int {
	total := 0
	for _, item := range items {
		total += len(item)
	}
	return total
}

func replaceHeader(dst http.Header, src http.Header) {
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

func finalInterceptorHeaders(current, intercepted http.Header) http.Header {
	if intercepted == nil {
		return current
	}
	if len(intercepted) == 0 {
		return nil
	}
	return cloneHeader(intercepted)
}

func downstreamHeadersFromExecutor(headers http.Header, passthrough bool) http.Header {
	if !passthrough {
		return nil
	}
	return FilterUpstreamHeaders(headers)
}

func downstreamHeadersAfterInterceptors(baseRaw, finalRaw http.Header, passthrough bool) http.Header {
	if passthrough {
		return FilterUpstreamHeaders(finalRaw)
	}
	return FilterUpstreamHeaders(diffHeaders(baseRaw, finalRaw))
}

func diffHeaders(base, next http.Header) http.Header {
	if len(next) == 0 {
		return nil
	}
	baseValues := make(map[string][]string, len(base))
	for key, values := range base {
		baseValues[http.CanonicalHeaderKey(key)] = values
	}
	out := make(http.Header)
	for key, values := range next {
		canonicalKey := http.CanonicalHeaderKey(key)
		if stringSlicesEqual(baseValues[canonicalKey], values) {
			continue
		}
		out[canonicalKey] = append([]string(nil), values...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (h *BaseAPIHandler) interceptorHost() PluginInterceptorHost {
	if h == nil {
		return nil
	}
	return h.PluginHost
}

func (h *BaseAPIHandler) modelRouterHost() PluginModelRouterHost {
	if h == nil {
		return nil
	}
	if !isNilPluginModelRouterHost(h.ModelRouterHost) {
		return h.ModelRouterHost
	}
	host := h.interceptorHost()
	if host == nil {
		return nil
	}
	router, ok := host.(PluginModelRouterHost)
	if !ok {
		return nil
	}
	return router
}

func (h *BaseAPIHandler) pluginExecutorHost() PluginExecutorHost {
	if h == nil {
		return nil
	}
	if executorHost, ok := h.ModelRouterHost.(PluginExecutorHost); ok && executorHost != nil {
		return executorHost
	}
	if executorHost, ok := h.PluginHost.(PluginExecutorHost); ok && executorHost != nil {
		return executorHost
	}
	return nil
}

type modelRouteDecision struct {
	ExecutorPluginID string
	Provider         string
	Model            string
}

func routeModel(ctx context.Context, host PluginModelRouterHost, req pluginapi.ModelRouteRequest, skipPluginID string) (pluginapi.ModelRouteResponse, bool) {
	if host == nil {
		return pluginapi.ModelRouteResponse{}, false
	}
	skipPluginID = strings.TrimSpace(skipPluginID)
	if skipPluginID != "" {
		if skipper, ok := host.(pluginModelRouterSkipHost); ok {
			return skipper.RouteModelExcept(ctx, req, skipPluginID)
		}
		return pluginapi.ModelRouteResponse{}, false
	}
	return host.RouteModel(ctx, req)
}

func modelRoutersEnabled(host PluginModelRouterHost, skipPluginID string) bool {
	if host == nil {
		return false
	}
	skipPluginID = strings.TrimSpace(skipPluginID)
	if skipPluginID != "" {
		if _, ok := host.(pluginModelRouterSkipHost); !ok {
			return false
		}
		if detector, ok := host.(modelRouterSkipDetector); ok {
			return detector.HasModelRoutersExcept(skipPluginID)
		}
	}
	if detector, ok := host.(modelRouterDetector); ok {
		return detector.HasModelRouters()
	}
	// No detector: treat routing as disabled (same conservative default as before any
	// ModelRouter existed). Hosts that route must implement HasModelRouters (pluginhost.Host does).
	return false
}

func (h *BaseAPIHandler) applyModelRouter(ctx context.Context, handlerType, modelName string, rawJSON []byte, stream bool, execOptions modelExecutionOptions) modelRouteDecision {
	var decision modelRouteDecision
	host := h.modelRouterHost()
	if host == nil || !modelRoutersEnabled(host, execOptions.SkipRouterPluginID) {
		return decision
	}
	meta := requestExecutionMetadata(ctx)
	meta[coreexecutor.RequestedModelMetadataKey] = modelName
	addModelExecutionSourceMetadata(meta, execOptions.InternalSource)
	setExecutionRequestShapeMetadata(meta, rawJSON, execOptions)
	resp, ok := routeModel(ctx, host, pluginapi.ModelRouteRequest{
		SourceFormat:   handlerType,
		RequestedModel: modelName,
		Stream:         stream,
		Headers:        modelExecutionHeaders(ctx, execOptions.Headers),
		Query:          modelExecutionQuery(ctx, execOptions.Query),
		Body:           cloneBytes(rawJSON),
		Metadata:       meta,
	}, execOptions.SkipRouterPluginID)
	if !ok || !resp.Handled {
		return decision
	}
	switch resp.TargetKind {
	case pluginapi.ModelRouteTargetSelf, pluginapi.ModelRouteTargetExecutor:
		decision.ExecutorPluginID = strings.TrimSpace(resp.Target)
	case pluginapi.ModelRouteTargetProvider:
		decision.Provider = strings.ToLower(strings.TrimSpace(resp.Target))
		decision.Model = strings.TrimSpace(resp.TargetModel)
	}
	return decision
}

func streamInterceptorsEnabled(host PluginInterceptorHost) bool {
	if host == nil {
		return false
	}
	if detector, ok := host.(streamInterceptorDetector); ok {
		return detector.HasStreamInterceptors()
	}
	return true
}

func requestInterceptorsEnabled(host PluginInterceptorHost) bool {
	if host == nil {
		return false
	}
	if detector, ok := host.(requestInterceptorDetector); ok {
		return detector.HasRequestInterceptors()
	}
	return true
}

type requestAfterAuthCapture struct {
	mu                      sync.Mutex
	set                     bool
	headers                 http.Header
	body                    []byte
	originalRequest         []byte
	originalRequestReplaced bool
}

func (c *requestAfterAuthCapture) record(req coreexecutor.RequestAfterAuthInterceptRequest, resp coreexecutor.RequestAfterAuthInterceptResponse) {
	if c == nil {
		return
	}
	headers := mergeRequestInterceptorHeaders(req.Headers, resp.Headers, resp.ClearHeaders)
	body := cloneBytes(req.Body)
	var originalRequest []byte
	originalRequestReplaced := false
	if len(resp.Body) > 0 {
		body = cloneBytes(resp.Body)
		originalRequest = cloneBytes(resp.Body)
		originalRequestReplaced = true
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.set = true
	c.headers = headers
	c.body = body
	c.originalRequest = originalRequest
	c.originalRequestReplaced = originalRequestReplaced
}

func (c *requestAfterAuthCapture) apply(req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Request, coreexecutor.Options) {
	if c == nil {
		return req, opts
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.set {
		return req, opts
	}
	req.Payload = cloneBytes(c.body)
	opts.Headers = cloneHeader(c.headers)
	if c.originalRequestReplaced {
		opts.OriginalRequest = cloneBytes(c.originalRequest)
	}
	return req, opts
}

func mergeRequestInterceptorHeaders(current, updates http.Header, clear []string) http.Header {
	if updates == nil && len(clear) == 0 {
		return cloneHeader(current)
	}
	out := cloneHeader(current)
	if out == nil && (len(updates) > 0 || len(clear) > 0) {
		out = make(http.Header)
	}
	for _, key := range clear {
		out.Del(key)
	}
	for key, values := range updates {
		out.Del(key)
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

func interceptRequestBeforeAuth(ctx context.Context, host PluginInterceptorHost, req pluginapi.RequestInterceptRequest, skipPluginID string) pluginapi.RequestInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptRequestBeforeAuthExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptRequestBeforeAuth(ctx, req)
}

func interceptRequestAfterAuth(ctx context.Context, host PluginInterceptorHost, req pluginapi.RequestInterceptRequest, skipPluginID string) pluginapi.RequestInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptRequestAfterAuthExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptRequestAfterAuth(ctx, req)
}

func interceptResponse(ctx context.Context, host PluginInterceptorHost, req pluginapi.ResponseInterceptRequest, skipPluginID string) pluginapi.ResponseInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptResponseExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptResponse(ctx, req)
}

func interceptStreamChunk(ctx context.Context, host PluginInterceptorHost, req pluginapi.StreamChunkInterceptRequest, skipPluginID string) pluginapi.StreamChunkInterceptResponse {
	if skipPluginID != "" {
		if skipper, ok := host.(pluginInterceptorSkipHost); ok {
			return skipper.InterceptStreamChunkExcept(ctx, req, skipPluginID)
		}
	}
	return host.InterceptStreamChunk(ctx, req)
}

func (h *BaseAPIHandler) applyRequestInterceptorsBeforeAuth(ctx context.Context, handlerType, requestedModel string, req coreexecutor.Request, opts coreexecutor.Options, skipPluginID string) (coreexecutor.Request, coreexecutor.Options) {
	host := h.interceptorHost()
	if host == nil {
		return req, opts
	}
	resp := interceptRequestBeforeAuth(ctx, host, pluginapi.RequestInterceptRequest{
		SourceFormat:   handlerType,
		Model:          req.Model,
		RequestedModel: requestedModel,
		Stream:         opts.Stream,
		Headers:        cloneHeader(opts.Headers),
		Body:           cloneBytes(req.Payload),
		Metadata:       opts.Metadata,
	}, skipPluginID)
	opts.Headers = finalInterceptorHeaders(opts.Headers, resp.Headers)
	if len(resp.Body) > 0 {
		req.Payload = cloneBytes(resp.Body)
		opts.OriginalRequest = cloneBytes(resp.Body)
	}
	return req, opts
}

func (h *BaseAPIHandler) requestAfterAuthInterceptor(capture *requestAfterAuthCapture, skipPluginID string) coreexecutor.RequestAfterAuthInterceptor {
	if !requestInterceptorsEnabled(h.interceptorHost()) {
		return nil
	}
	return func(ctx context.Context, req coreexecutor.RequestAfterAuthInterceptRequest) coreexecutor.RequestAfterAuthInterceptResponse {
		resp := h.applyRequestInterceptorsAfterAuth(ctx, req, skipPluginID)
		if capture != nil {
			capture.record(req, resp)
		}
		return resp
	}
}

func (h *BaseAPIHandler) applyRequestInterceptorsAfterAuth(ctx context.Context, req coreexecutor.RequestAfterAuthInterceptRequest, skipPluginID string) coreexecutor.RequestAfterAuthInterceptResponse {
	host := h.interceptorHost()
	if !requestInterceptorsEnabled(host) {
		return coreexecutor.RequestAfterAuthInterceptResponse{}
	}
	resp := interceptRequestAfterAuth(ctx, host, pluginapi.RequestInterceptRequest{
		SourceFormat:   req.SourceFormat.String(),
		ToFormat:       req.ToFormat.String(),
		Model:          req.Model,
		RequestedModel: req.RequestedModel,
		Stream:         req.Stream,
		Headers:        cloneHeader(req.Headers),
		Body:           cloneBytes(req.Body),
		Metadata:       req.Metadata,
	}, skipPluginID)
	return coreexecutor.RequestAfterAuthInterceptResponse{
		Headers:      resp.Headers,
		Body:         resp.Body,
		ClearHeaders: resp.ClearHeaders,
	}
}

func (h *BaseAPIHandler) applyResponseInterceptors(ctx context.Context, handlerType, normalizedModel, requestedModel string, opts coreexecutor.Options, rawResponseHeaders, responseHeaders http.Header, originalRequest, requestBody, body []byte, statusCode int, skipPluginID string) ([]byte, http.Header) {
	host := h.interceptorHost()
	if host == nil {
		return body, responseHeaders
	}
	resp := interceptResponse(ctx, host, pluginapi.ResponseInterceptRequest{
		SourceFormat:    handlerType,
		Model:           normalizedModel,
		RequestedModel:  requestedModel,
		Stream:          false,
		RequestHeaders:  cloneHeader(opts.Headers),
		ResponseHeaders: cloneHeader(rawResponseHeaders),
		OriginalRequest: cloneBytes(originalRequest),
		RequestBody:     cloneBytes(requestBody),
		Body:            cloneBytes(body),
		StatusCode:      statusCode,
		Metadata:        opts.Metadata,
	}, skipPluginID)
	responseHeaders = downstreamHeadersAfterInterceptors(rawResponseHeaders, finalInterceptorHeaders(rawResponseHeaders, resp.Headers), PassthroughHeadersEnabled(h.Cfg))
	if len(resp.Body) > 0 {
		body = cloneBytes(resp.Body)
	}
	return body, responseHeaders
}

func enrichAuthSelectionError(err error, providers []string, model string) error {
	if err == nil {
		return nil
	}

	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return err
	}

	code := strings.TrimSpace(authErr.Code)
	if code != "auth_not_found" && code != "auth_unavailable" {
		return err
	}

	baseMessage := strings.TrimSpace(authErr.Message)
	if baseMessage == "" || strings.EqualFold(baseMessage, "no auth available") {
		baseMessage = "requested route is temporarily unavailable"
	}

	status := authErr.HTTPStatus
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}

	return &coreauth.Error{
		Code:       authErr.Code,
		Message:    baseMessage,
		Retryable:  authErr.Retryable,
		HTTPStatus: status,
	}
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.Addon != nil {
		WriteErrorAddonHeaders(c.Writer.Header(), msg.Addon, PassthroughHeadersEnabled(h.Cfg))
	}

	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}
	var previous []byte
	if existingBytes := currentAPIResponse(c); len(existingBytes) > 0 {
		previous = existingBytes
	}
	status, errText = NormalizeKnownUserError(status, errText, previous)

	LogContextWindowExceededEvent(c, status, errText, h.AuthManager)
	body := BuildErrorResponseBody(status, errText)
	// Append first to preserve upstream response logs, then drop duplicate payloads if already recorded.
	appendAPIResponse(c, body)
	trimmedErrText := strings.TrimSpace(errText)
	trimmedBody := bytes.TrimSpace(body)
	if len(previous) > 0 {
		if (trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
			(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)) {
			c.Set("API_RESPONSE", previous)
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if h.Cfg.RequestLog {
		if ginContext, ok := ctx.Value("gin").(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...interface{})
