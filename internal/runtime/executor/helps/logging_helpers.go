package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	log "github.com/sirupsen/logrus"
)

const (
	apiAttemptsKey          = "API_UPSTREAM_ATTEMPTS"
	apiRequestKey           = "API_REQUEST"
	apiResponseKey          = "API_RESPONSE"
	apiResponseDirtyKey     = "API_RESPONSE_DIRTY"
	apiWebsocketTimelineKey = "API_WEBSOCKET_TIMELINE"
	creditsUsedKey          = "__antigravity_credits_used__"
)

// UpstreamRequestLog captures the outbound upstream request details for logging.
type UpstreamRequestLog struct {
	URL       string
	Method    string
	Headers   http.Header
	Body      []byte
	Provider  string
	AuthID    string
	AuthLabel string
	AuthType  string
	AuthValue string
}

type upstreamAttempt struct {
	index                int
	request              string
	response             *strings.Builder
	responseTrailer      *strings.Builder
	responseSource       *logging.FileBodySource
	bodyCapture          *boundedAPIResponseBodyCapture
	responseIntroWritten bool
	statusWritten        bool
	headersWritten       bool
	bodyStarted          bool
	bodyHasContent       bool
	bodySummaryWritten   bool
	errorWritten         bool
}

type boundedAPIResponseBodyCapture struct {
	totalBytes int64
	chunks     int
	digest     hash.Hash
}

// APIResponseLogRuntime caches the latest request-log attempt state so stream
// loops can append chunks without repeatedly resolving gin/request-log state
// from context on every payload.
type APIResponseLogRuntime struct {
	ginCtx   *gin.Context
	attempts []*upstreamAttempt
	attempt  *upstreamAttempt
}

func requestLogCaptureEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.RequestLog && !cfg.CommercialMode
}

// RecordAPIRequest stores the upstream request metadata in Gin context for request logging.
func RecordAPIRequest(ctx context.Context, cfg *config.Config, info UpstreamRequestLog) {
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	attempts := getAttempts(ginCtx)
	index := len(attempts) + 1

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("=== API REQUEST %d ===\n", index))
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	if info.URL != "" {
		builder.WriteString(fmt.Sprintf("Upstream URL: %s\n", sanitizeUpstreamURLForLog(info.URL)))
	} else {
		builder.WriteString("Upstream URL: <unknown>\n")
	}
	if info.Method != "" {
		builder.WriteString(fmt.Sprintf("HTTP Method: %s\n", info.Method))
	}
	if auth := formatAuthInfo(info); auth != "" {
		builder.WriteString(fmt.Sprintf("Auth: %s\n", auth))
	}
	builder.WriteString("\nHeaders:\n")
	writeHeaders(builder, info.Headers)
	builder.WriteString("\nBody:\n")
	bodyMetadata := logging.SummarizeBodyForLog(info.Body, info.Headers.Get("Content-Type"))

	requestText := ""
	if source, ok := apiRequestSource(ginCtx); ok {
		if errWrite := source.AppendBytes([]byte(builder.String())); errWrite == nil {
			if errBody := source.AppendBytes(bodyMetadata); errBody != nil {
				log.WithError(errBody).Warn("failed to append api request body metadata")
			}
			if errEnd := source.AppendBytes([]byte("\n\n")); errEnd != nil {
				log.WithError(errEnd).Warn("failed to append api request log terminator")
			}
		} else {
			log.WithError(errWrite).Warn("failed to append api request log part")
			builder.Write(bodyMetadata)
			builder.WriteString("\n\n")
			requestText = builder.String()
		}
	} else {
		builder.Write(bodyMetadata)
		builder.WriteString("\n\n")
		requestText = builder.String()
	}

	attempt := &upstreamAttempt{
		index:          index,
		request:        requestText,
		response:       &strings.Builder{},
		responseSource: apiResponseSourceOrNil(ginCtx),
	}
	attempts = append(attempts, attempt)
	ginCtx.Set(apiAttemptsKey, attempts)
	if requestText != "" {
		updateAggregatedRequest(ginCtx, attempts)
	}
}

// RecordAPIResponseMetadata captures upstream response status/header information for the latest attempt.
func RecordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	logging.SetResponseHeaders(ctx, headers)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	attempts, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(ginCtx, attempt)

	if status > 0 && !attempt.statusWritten {
		writeAttemptResponse(ginCtx, attempt, []byte(fmt.Sprintf("Status: %d\n", status)))
		attempt.statusWritten = true
	}
	if !attempt.headersWritten {
		builder := &strings.Builder{}
		builder.WriteString("Headers:\n")
		writeHeaders(builder, headers)
		writeAttemptResponse(ginCtx, attempt, []byte(builder.String()))
		attempt.headersWritten = true
		writeAttemptResponse(ginCtx, attempt, []byte("\n"))
	}

	updateAggregatedResponseIfMemoryBacked(ginCtx, attempts)
}

// RecordAPIResponseError adds an error entry for the latest attempt when no HTTP response is available.
func RecordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	if !requestLogCaptureEnabled(cfg) || err == nil {
		return
	}
	runtime := NewAPIResponseLogRuntime(ctx, cfg)
	if runtime == nil {
		return
	}
	runtime.RecordError(err)
}

// NewAPIResponseLogRuntime resolves and caches the latest request-log attempt
// for a response. Callers should acquire it once per upstream response and
// reuse it inside per-chunk streaming loops.
func NewAPIResponseLogRuntime(ctx context.Context, cfg *config.Config) *APIResponseLogRuntime {
	if !requestLogCaptureEnabled(cfg) {
		return nil
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return nil
	}
	attempts, attempt := ensureAttempt(ginCtx)
	return &APIResponseLogRuntime{
		ginCtx:   ginCtx,
		attempts: attempts,
		attempt:  attempt,
	}
}

// RecordError appends an upstream response error to the cached attempt.
func (r *APIResponseLogRuntime) RecordError(err error) {
	if r == nil || err == nil || r.ginCtx == nil || r.attempt == nil {
		return
	}
	attempt := r.attempt
	ensureResponseIntro(r.ginCtx, attempt)

	if attempt.bodyStarted && !attempt.bodyHasContent {
		// Ensure body does not stay empty marker if error arrives first.
		attempt.bodyStarted = false
		attempt.bodyCapture = nil
	}
	if attempt.bodyStarted && !responseFileBacked(r.ginCtx, attempt) {
		if attempt.responseTrailer == nil {
			attempt.responseTrailer = &strings.Builder{}
		}
		if attempt.errorWritten {
			attempt.responseTrailer.WriteString("\n")
		}
		attempt.responseTrailer.WriteString("Error: upstream request failed\n")
		attempt.errorWritten = true
		r.ginCtx.Set(apiResponseDirtyKey, true)
		return
	}

	if attempt.errorWritten {
		writeAttemptResponse(r.ginCtx, attempt, []byte("\n"))
	}
	writeAttemptResponse(r.ginCtx, attempt, []byte("Error: upstream request failed\n"))
	attempt.errorWritten = true
	updateAggregatedResponseIfMemoryBacked(r.ginCtx, r.attempts)
}

// AppendAPIResponseChunk appends an upstream response chunk to Gin context for request logging.
func AppendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	runtime := NewAPIResponseLogRuntime(ctx, cfg)
	if runtime == nil {
		return
	}
	runtime.AppendChunk(chunk)
}

// AppendChunk appends an upstream response chunk to the cached attempt.
func (r *APIResponseLogRuntime) AppendChunk(chunk []byte) {
	if r == nil || r.ginCtx == nil || r.attempt == nil {
		return
	}
	data := bytes.TrimSpace(chunk)
	if len(data) == 0 {
		return
	}
	attempt := r.attempt
	ensureResponseIntro(r.ginCtx, attempt)

	if !attempt.headersWritten {
		builder := &strings.Builder{}
		builder.WriteString("Headers:\n")
		writeHeaders(builder, nil)
		writeAttemptResponse(r.ginCtx, attempt, []byte(builder.String()))
		attempt.headersWritten = true
		writeAttemptResponse(r.ginCtx, attempt, []byte("\n"))
	}
	if !attempt.bodyStarted {
		attempt.bodyStarted = true
		if attempt.bodyCapture == nil {
			attempt.bodyCapture = newBoundedAPIResponseBodyCapture()
		}
	}
	if attempt.bodyCapture != nil {
		attempt.bodyCapture.Append(data)
	}
	attempt.bodyHasContent = true
	r.ginCtx.Set(apiResponseDirtyKey, true)
}

// RecordAPIWebsocketRequest stores an upstream websocket request event in Gin context.
func RecordAPIWebsocketRequest(ctx context.Context, cfg *config.Config, info UpstreamRequestLog) {
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.request\n")
	if info.URL != "" {
		builder.WriteString(fmt.Sprintf("Upstream URL: %s\n", sanitizeUpstreamURLForLog(info.URL)))
	}
	if auth := formatAuthInfo(info); auth != "" {
		builder.WriteString(fmt.Sprintf("Auth: %s\n", auth))
	}
	builder.WriteString("Headers:\n")
	writeHeaders(builder, info.Headers)
	builder.WriteString("\nBody:\n")
	builder.Write(logging.SummarizeBodyForLog(info.Body, info.Headers.Get("Content-Type")))
	builder.WriteString("\n")

	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

// RecordAPIWebsocketHandshake stores the upstream websocket handshake response metadata.
func RecordAPIWebsocketHandshake(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	logging.SetResponseHeaders(ctx, headers)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.handshake\n")
	if status > 0 {
		builder.WriteString(fmt.Sprintf("Status: %d\n", status))
	}
	builder.WriteString("Headers:\n")
	writeHeaders(builder, headers)
	builder.WriteString("\n")

	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

// RecordAPIWebsocketUpgradeRejection stores a rejected websocket upgrade as an HTTP attempt.
func RecordAPIWebsocketUpgradeRejection(ctx context.Context, cfg *config.Config, info UpstreamRequestLog, status int, headers http.Header, body []byte) {
	logging.SetResponseHeaders(ctx, headers)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	RecordAPIRequest(ctx, cfg, info)
	RecordAPIResponseMetadata(ctx, cfg, status, headers)
	AppendAPIResponseChunk(ctx, cfg, body)
}

// WebsocketUpgradeRequestURL converts a websocket URL back to its HTTP handshake URL for logging.
func WebsocketUpgradeRequestURL(rawURL string) string {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL == "" {
		return ""
	}
	parsed, err := url.Parse(trimmedURL)
	if err != nil {
		return trimmedURL
	}
	switch strings.ToLower(parsed.Scheme) {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	}
	return parsed.String()
}

// AppendAPIWebsocketResponse stores an upstream websocket response frame in Gin context.
func AppendAPIWebsocketResponse(ctx context.Context, cfg *config.Config, payload []byte) {
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	data := bytes.TrimSpace(payload)
	if len(data) == 0 {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	markAPIResponseTimestamp(ginCtx)

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.response\n")
	builder.Write(logging.SummarizeBodyForLog(data, "application/websocket-frame"))
	builder.WriteString("\n")

	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

// RecordAPIWebsocketError stores an upstream websocket error event in Gin context.
func RecordAPIWebsocketError(ctx context.Context, cfg *config.Config, stage string, err error) {
	if !requestLogCaptureEnabled(cfg) || err == nil {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	markAPIResponseTimestamp(ginCtx)

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.error\n")
	if trimmed := strings.TrimSpace(stage); trimmed != "" {
		builder.WriteString(fmt.Sprintf("Stage: %s\n", trimmed))
	}
	builder.WriteString("Error: upstream websocket failed\n")

	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

func ginContextFrom(ctx context.Context) *gin.Context {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return ginCtx
}

func getAttempts(ginCtx *gin.Context) []*upstreamAttempt {
	if ginCtx == nil {
		return nil
	}
	if value, exists := ginCtx.Get(apiAttemptsKey); exists {
		if attempts, ok := value.([]*upstreamAttempt); ok {
			return attempts
		}
	}
	return nil
}

func ensureAttempt(ginCtx *gin.Context) ([]*upstreamAttempt, *upstreamAttempt) {
	attempts := getAttempts(ginCtx)
	if len(attempts) == 0 {
		attempt := &upstreamAttempt{
			index:          1,
			response:       &strings.Builder{},
			responseSource: apiResponseSourceOrNil(ginCtx),
		}
		if source, ok := apiRequestSource(ginCtx); ok {
			if errWrite := source.AppendBytes([]byte("=== API REQUEST 1 ===\n<missing>\n\n")); errWrite != nil {
				log.WithError(errWrite).Warn("failed to append missing api request log part")
				attempt.request = "=== API REQUEST 1 ===\n<missing>\n\n"
			}
		} else {
			attempt.request = "=== API REQUEST 1 ===\n<missing>\n\n"
		}
		attempts = []*upstreamAttempt{attempt}
		ginCtx.Set(apiAttemptsKey, attempts)
		if attempt.request != "" {
			updateAggregatedRequest(ginCtx, attempts)
		}
	}
	return attempts, attempts[len(attempts)-1]
}

func ensureResponseIntro(ginCtx *gin.Context, attempt *upstreamAttempt) {
	if attempt == nil || attempt.response == nil || attempt.responseIntroWritten {
		return
	}
	writeAttemptResponse(ginCtx, attempt, []byte(fmt.Sprintf("=== API RESPONSE %d ===\n", attempt.index)))
	writeAttemptResponse(ginCtx, attempt, []byte(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano))))
	writeAttemptResponse(ginCtx, attempt, []byte("\n"))
	attempt.responseIntroWritten = true
}

func writeAttemptResponse(ginCtx *gin.Context, attempt *upstreamAttempt, payload []byte) {
	if attempt == nil || len(payload) == 0 {
		return
	}
	if attempt.responseSource == nil {
		attempt.responseSource = apiResponseSourceOrNil(ginCtx)
	}
	if attempt.responseSource != nil {
		if errWrite := attempt.responseSource.AppendBytes(payload); errWrite == nil {
			if ginCtx != nil {
				ginCtx.Set(logging.APIResponseCapturedContextKey, true)
			}
			return
		} else {
			log.WithError(errWrite).Warn("failed to append api response log part")
			attempt.responseSource = nil
		}
	}
	if attempt.response == nil {
		attempt.response = &strings.Builder{}
	}
	attempt.response.Write(payload)
}

func responseFileBacked(ginCtx *gin.Context, attempt *upstreamAttempt) bool {
	if attempt == nil {
		return false
	}
	if attempt.responseSource == nil {
		attempt.responseSource = apiResponseSourceOrNil(ginCtx)
	}
	return attempt.responseSource != nil
}

func updateAggregatedRequest(ginCtx *gin.Context, attempts []*upstreamAttempt) {
	if ginCtx == nil {
		return
	}
	var builder strings.Builder
	for _, attempt := range attempts {
		builder.WriteString(attempt.request)
	}
	ginCtx.Set(apiRequestKey, []byte(builder.String()))
}

func updateAggregatedResponseIfMemoryBacked(ginCtx *gin.Context, attempts []*upstreamAttempt) {
	if apiResponseSourceOrNil(ginCtx) != nil {
		return
	}
	updateAggregatedResponse(ginCtx, attempts)
}

func updateAggregatedResponse(ginCtx *gin.Context, attempts []*upstreamAttempt) {
	if ginCtx == nil {
		return
	}
	var builder strings.Builder
	for idx, attempt := range attempts {
		responseText := renderAttemptResponse(attempt)
		if responseText == "" {
			continue
		}
		builder.WriteString(responseText)
		if !strings.HasSuffix(responseText, "\n") {
			builder.WriteString("\n")
		}
		if idx < len(attempts)-1 {
			builder.WriteString("\n")
		}
	}
	ginCtx.Set(apiResponseKey, []byte(builder.String()))
	ginCtx.Set(apiResponseDirtyKey, false)
}

// MaterializeAPIResponse returns the latest aggregated API response text,
// rendering deferred request-log body updates on demand when needed.
func MaterializeAPIResponse(ginCtx *gin.Context) []byte {
	if ginCtx == nil {
		return nil
	}
	attempts := getAttempts(ginCtx)
	for _, attempt := range attempts {
		if attempt == nil || attempt.responseSource == nil || !attempt.bodyStarted || attempt.bodySummaryWritten {
			continue
		}
		if errWrite := attempt.responseSource.AppendBytes([]byte("Body:\n" + attempt.bodyCapture.Render() + "\n")); errWrite != nil {
			log.WithError(errWrite).Warn("failed to append api response body metadata")
		} else {
			attempt.bodySummaryWritten = true
		}
	}
	if value, exists := ginCtx.Get(apiResponseDirtyKey); exists {
		if dirty, ok := value.(bool); ok && dirty {
			updateAggregatedResponse(ginCtx, attempts)
		}
	}
	apiResponse, exists := ginCtx.Get(apiResponseKey)
	if !exists {
		return nil
	}
	data, ok := apiResponse.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	return data
}

// MaterializeAPIResponseFromContext renders and returns the aggregated API response
// attached to a context-backed Gin request, when one exists.
func MaterializeAPIResponseFromContext(ctx context.Context) []byte {
	return MaterializeAPIResponse(ginContextFrom(ctx))
}

func newBoundedAPIResponseBodyCapture() *boundedAPIResponseBodyCapture {
	return &boundedAPIResponseBodyCapture{
		digest: sha256.New(),
	}
}

func (c *boundedAPIResponseBodyCapture) Append(data []byte) {
	c.appendBytes(data, true)
}

func (c *boundedAPIResponseBodyCapture) appendBytes(data []byte, countChunk bool) {
	if c == nil || len(data) == 0 {
		return
	}
	if countChunk {
		c.chunks++
	}
	c.totalBytes += int64(len(data))
	_, _ = c.digest.Write(data)

}

func (c *boundedAPIResponseBodyCapture) Render() string {
	if c == nil || c.totalBytes == 0 {
		return string(logging.SummarizeBodyForLog(nil, ""))
	}
	digest := hex.EncodeToString(c.digest.Sum(nil))
	return string(logging.EncodeBodyLogMetadata(logging.BodyLogMetadata{
		Bytes:  c.totalBytes,
		SHA256: digest,
		Chunks: int64(c.chunks),
	}))
}

func renderAttemptResponse(attempt *upstreamAttempt) string {
	if attempt == nil || attempt.response == nil {
		return ""
	}
	prefix := attempt.response.String()
	trailer := ""
	if attempt.responseTrailer != nil {
		trailer = attempt.responseTrailer.String()
	}
	if !attempt.bodyStarted {
		return prefix + trailer
	}

	body := "<empty>"
	if attempt.bodyCapture != nil {
		body = attempt.bodyCapture.Render()
	}

	var builder strings.Builder
	builder.Grow(len(prefix) + len(body) + len(trailer) + 16)
	builder.WriteString(prefix)
	builder.WriteString("Body:\n")
	builder.WriteString(body)
	if trailer != "" {
		if !strings.HasSuffix(body, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString(trailer)
	}
	return builder.String()
}

func apiRequestSource(ginCtx *gin.Context) (*logging.FileBodySource, bool) {
	return fileBodySourceFromGin(ginCtx, logging.APIRequestSourceContextKey)
}

func apiResponseSourceOrNil(ginCtx *gin.Context) *logging.FileBodySource {
	source, ok := fileBodySourceFromGin(ginCtx, logging.APIResponseSourceContextKey)
	if !ok {
		return nil
	}
	return source
}

func appendAPIWebsocketTimeline(ginCtx *gin.Context, chunk []byte) {
	if ginCtx == nil {
		return
	}
	data := bytes.TrimSpace(chunk)
	if len(data) == 0 {
		return
	}
	if source, ok := apiWebsocketTimelineSource(ginCtx); ok {
		if errAppend := source.AppendPart(data); errAppend == nil {
			return
		} else {
			log.WithError(errAppend).Warn("failed to append api websocket timeline log part")
		}
	}
	if existing, exists := ginCtx.Get(apiWebsocketTimelineKey); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+2)
			combined = append(combined, existingBytes...)
			if !bytes.HasSuffix(existingBytes, []byte("\n")) {
				combined = append(combined, '\n')
			}
			combined = append(combined, '\n')
			combined = append(combined, data...)
			ginCtx.Set(apiWebsocketTimelineKey, combined)
			return
		}
	}
	ginCtx.Set(apiWebsocketTimelineKey, internalpayload.CloneBytes(data))
}

func apiWebsocketTimelineSource(ginCtx *gin.Context) (*logging.FileBodySource, bool) {
	return fileBodySourceFromGin(ginCtx, logging.APIWebsocketTimelineSourceContextKey)
}

func fileBodySourceFromGin(ginCtx *gin.Context, key string) (*logging.FileBodySource, bool) {
	if ginCtx == nil {
		return nil, false
	}
	value, exists := ginCtx.Get(key)
	if !exists {
		return nil, false
	}
	source, ok := value.(*logging.FileBodySource)
	return source, ok && source != nil
}

func markAPIResponseTimestamp(ginCtx *gin.Context) {
	if ginCtx == nil {
		return
	}
	if _, exists := ginCtx.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	ginCtx.Set("API_RESPONSE_TIMESTAMP", time.Now())
}

func writeHeaders(builder *strings.Builder, headers http.Header) {
	if builder == nil {
		return
	}
	if len(headers) == 0 {
		builder.WriteString("<none>\n")
		return
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := headers[key]
		if len(values) == 0 {
			builder.WriteString(fmt.Sprintf("%s:\n", key))
			continue
		}
		for _, value := range values {
			masked := logging.RedactHeaderValue(key, value)
			builder.WriteString(fmt.Sprintf("%s: %s\n", key, masked))
		}
	}
}

func sanitizeUpstreamURLForLog(rawURL string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil {
		return "<invalid>"
	}
	if parsed.User != nil {
		parsed.User = url.User(logging.RedactedHeaderValue)
	}
	query := parsed.Query()
	for key, values := range query {
		redacted := make([]string, len(values))
		for index := range redacted {
			redacted[index] = logging.RedactedHeaderValue
		}
		query[key] = redacted
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String()
}

func formatAuthInfo(info UpstreamRequestLog) string {
	var parts []string
	if trimmed := strings.TrimSpace(info.Provider); trimmed != "" {
		parts = append(parts, fmt.Sprintf("provider=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthID); trimmed != "" {
		parts = append(parts, fmt.Sprintf("auth_id=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthLabel); trimmed != "" {
		parts = append(parts, fmt.Sprintf("label=%s", trimmed))
	}

	authType := strings.ToLower(strings.TrimSpace(info.AuthType))
	authValue := strings.TrimSpace(info.AuthValue)
	switch authType {
	case "api_key":
		if authValue != "" {
			parts = append(parts, fmt.Sprintf("type=api_key value=%s", logging.RedactedHeaderValue))
		} else {
			parts = append(parts, "type=api_key")
		}
	case "oauth":
		parts = append(parts, "type=oauth")
	default:
		if authType != "" {
			if authValue != "" {
				parts = append(parts, fmt.Sprintf("type=%s value=%s", authType, logging.RedactedHeaderValue))
			} else {
				parts = append(parts, fmt.Sprintf("type=%s", authType))
			}
		}
	}

	return strings.Join(parts, ", ")
}

func SummarizeErrorBody(contentType string, body []byte) string {
	return string(logging.SummarizeBodyForLog(body, contentType))
}

// logWithRequestID returns a logrus Entry with request_id field populated from context.
// If no request ID is found in context, it returns the standard logger.
func LogWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	requestID := logging.GetRequestID(ctx)
	if requestID == "" {
		return log.NewEntry(log.StandardLogger())
	}
	return log.WithField("request_id", requestID)
}

// MarkCreditsUsed flags the request as having used AI credits for billing.
func MarkCreditsUsed(ctx context.Context) {
	ginCtx := ginContextFrom(ctx)
	if ginCtx != nil {
		ginCtx.Set(creditsUsedKey, true)
	}
}

// CreditsUsed returns true if the request used AI credits.
func CreditsUsed(ctx context.Context) bool {
	ginCtx := ginContextFrom(ctx)
	if ginCtx != nil {
		if val, exists := ginCtx.Get(creditsUsedKey); exists {
			if b, ok := val.(bool); ok {
				return b
			}
		}
	}
	return false
}
