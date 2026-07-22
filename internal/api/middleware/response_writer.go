// Package middleware provides Gin HTTP middleware for the CLI Proxy API server.
// It includes a sophisticated response writer wrapper designed to capture and log request and response data,
// including support for streaming responses, without impacting latency.
package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
)

const requestBodyOverrideContextKey = "REQUEST_BODY_OVERRIDE"
const responseBodyOverrideContextKey = "RESPONSE_BODY_OVERRIDE"
const websocketTimelineOverrideContextKey = "WEBSOCKET_TIMELINE_OVERRIDE"

const (
	streamLogChunkMaxBytes   = 64 << 10
	streamLogQueueMaxBytes   = 2 << 20
	streamLogQueueCapacity   = 32
	streamLogChunkMarker     = "\n[TRUNCATED STREAM CHUNK: size limit reached]\n"
	streamLogQueueDropMarker = "\n[TRUNCATED STREAM LOG QUEUE: byte limit reached]\n"
)

// RequestInfo holds essential details of an incoming HTTP request for logging purposes.
type RequestInfo struct {
	URL         string              // URL is the request URL.
	Method      string              // Method is the HTTP method (e.g., GET, POST).
	Headers     map[string][]string // Headers contains the request headers.
	Body        []byte              // Body is accepted for compatibility and summarized before logging.
	RequestID   string              // RequestID is the unique identifier for the request.
	Timestamp   time.Time           // Timestamp is when the request was received.
	bodyCapture *requestBodyMetadataCapture
}

// ResponseWriterWrapper wraps the standard gin.ResponseWriter to intercept and log response data.
// It is designed to handle both standard and streaming responses, ensuring that logging operations do not block the client response.
type ResponseWriterWrapper struct {
	gin.ResponseWriter
	responseDigest      hash.Hash
	responseBytes       int64
	responseChunks      int64
	responseMu          sync.Mutex
	isStreaming         bool                       // isStreaming indicates whether the response is a streaming type (e.g., text/event-stream).
	streamWriter        logging.StreamingLogWriter // streamWriter is a writer for handling streaming log entries.
	chunkChannel        chan []byte                // chunkChannel is a channel for asynchronously passing response chunks to the logger.
	chunkMu             sync.Mutex
	queuedChunkBytes    int
	streamQueueDropped  bool
	streamClosed        bool
	streamDone          chan struct{}         // streamDone signals when the streaming goroutine completes.
	logger              logging.RequestLogger // logger is the instance of the request logger service.
	requestInfo         *RequestInfo          // requestInfo holds the details of the original request.
	statusCode          int                   // statusCode stores the HTTP status code of the response.
	headers             map[string][]string   // headers stores the response headers.
	logOnErrorOnly      bool                  // logOnErrorOnly enables logging only when an error response is detected.
	firstChunkTimestamp time.Time             // firstChunkTimestamp captures TTFB for streaming responses.
	finishMu            sync.Mutex
	finishOnce          sync.Once
	finishDone          chan struct{}
	finishErr           error
}

// NewResponseWriterWrapper creates and initializes a new ResponseWriterWrapper.
// It takes the original gin.ResponseWriter, a logger instance, and request information.
//
// Parameters:
//   - w: The original gin.ResponseWriter to wrap.
//   - logger: The logging service to use for recording requests.
//   - requestInfo: The pre-captured information about the incoming request.
//
// Returns:
//   - A pointer to a new ResponseWriterWrapper.
func NewResponseWriterWrapper(w gin.ResponseWriter, logger logging.RequestLogger, requestInfo *RequestInfo) *ResponseWriterWrapper {
	return &ResponseWriterWrapper{
		ResponseWriter: w,
		responseDigest: sha256.New(),
		logger:         logger,
		requestInfo:    requestInfo,
		headers:        make(map[string][]string),
	}
}

// Write wraps the underlying ResponseWriter's Write method to capture response data.
// For non-streaming responses, it writes to an internal buffer. For streaming responses,
// it sends data chunks to a non-blocking channel for asynchronous logging.
// CRITICAL: This method prioritizes writing to the client to ensure zero latency,
// handling logging operations subsequently.
func (w *ResponseWriterWrapper) Write(data []byte) (int, error) {
	// Ensure headers are captured before first write
	// This is critical because Write() may trigger WriteHeader() internally
	w.ensureHeadersCaptured()

	// CRITICAL: Write to client first (zero latency)
	n, err := w.ResponseWriter.Write(data)

	// THEN: Handle logging based on response type
	if w.isStreaming {
		// Capture TTFB on first chunk (synchronous, before async channel send)
		if w.firstChunkTimestamp.IsZero() {
			w.firstChunkTimestamp = time.Now()
		}
		// For streaming responses: Send to async logging channel (non-blocking)
		w.enqueueStreamingChunk(data, "")
		return n, err
	}

	if w.shouldBufferResponseBody() {
		w.captureResponseBytes(data[:max(0, min(n, len(data)))])
	}

	return n, err
}

func (w *ResponseWriterWrapper) shouldBufferResponseBody() bool {
	if w.logger != nil && w.logger.IsEnabled() {
		return true
	}
	if !w.logOnErrorOnly {
		return false
	}
	status := w.statusCode
	if status == 0 {
		if statusWriter, ok := w.ResponseWriter.(interface{ Status() int }); ok && statusWriter != nil {
			status = statusWriter.Status()
		} else {
			status = http.StatusOK
		}
	}
	return status >= http.StatusBadRequest
}

// WriteString wraps the underlying ResponseWriter's WriteString method to capture response data.
// Some handlers (and fmt/io helpers) write via io.StringWriter; without this override, those writes
// bypass Write() and would be missing from request logs.
func (w *ResponseWriterWrapper) WriteString(data string) (int, error) {
	w.ensureHeadersCaptured()

	// CRITICAL: Write to client first (zero latency)
	n, err := w.ResponseWriter.WriteString(data)

	// THEN: Capture for logging
	if w.isStreaming {
		// Capture TTFB on first chunk (synchronous, before async channel send)
		if w.firstChunkTimestamp.IsZero() {
			w.firstChunkTimestamp = time.Now()
		}
		w.enqueueStreamingChunk(nil, data)
		return n, err
	}

	if w.shouldBufferResponseBody() {
		w.captureResponseString(data[:max(0, min(n, len(data)))])
	}
	return n, err
}

func (w *ResponseWriterWrapper) captureResponseString(value string) {
	for len(value) > 0 {
		chunkLen := min(len(value), 64<<10)
		w.captureResponseBytes([]byte(value[:chunkLen]))
		value = value[chunkLen:]
	}
}

func (w *ResponseWriterWrapper) captureResponseBytes(payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.responseMu.Lock()
	defer w.responseMu.Unlock()
	if w.responseDigest == nil {
		w.responseDigest = sha256.New()
	}
	_, _ = w.responseDigest.Write(payload)
	w.responseBytes += int64(len(payload))
	w.responseChunks++
}

func (w *ResponseWriterWrapper) enqueueStreamingChunk(payload []byte, text string) {
	sourceLen := len(payload)
	if payload == nil {
		sourceLen = len(text)
	}
	if sourceLen == 0 {
		return
	}
	chunkLen := min(sourceLen, streamLogChunkMaxBytes)
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if w.streamClosed || w.chunkChannel == nil || len(w.chunkChannel) >= cap(w.chunkChannel) || w.queuedChunkBytes+chunkLen > streamLogQueueMaxBytes {
		w.streamQueueDropped = true
		return
	}
	chunk := make([]byte, 0, chunkLen)
	if sourceLen > streamLogChunkMaxBytes {
		dataLimit := max(0, streamLogChunkMaxBytes-len(streamLogChunkMarker))
		if payload != nil {
			chunk = append(chunk, payload[:dataLimit]...)
		} else {
			chunk = append(chunk, text[:dataLimit]...)
		}
		chunk = append(chunk, streamLogChunkMarker...)
	} else if payload != nil {
		chunk = append(chunk, payload...)
	} else {
		chunk = append(chunk, text...)
	}
	w.queuedChunkBytes += len(chunk)
	w.chunkChannel <- chunk
}

// WriteHeader wraps the underlying ResponseWriter's WriteHeader method.
// It captures the status code, detects if the response is streaming based on the Content-Type header,
// and initializes the appropriate logging mechanism (standard or streaming).
func (w *ResponseWriterWrapper) WriteHeader(statusCode int) {
	if w == nil || w.ResponseWriter == nil || w.ResponseWriter.Written() {
		return
	}
	w.statusCode = statusCode

	// Capture response headers using the new method
	w.captureCurrentHeaders()

	// Detect streaming based on Content-Type
	contentType := w.ResponseWriter.Header().Get("Content-Type")
	w.isStreaming = w.detectStreaming(contentType)

	// If streaming, initialize streaming log writer
	if w.isStreaming && w.logger.IsEnabled() && w.streamWriter == nil && w.chunkChannel == nil && !w.streamClosed {
		streamWriter, err := w.logger.LogStreamingRequest(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			w.requestInfo.Body,
			w.requestInfo.RequestID,
		)
		if err == nil && streamWriter != nil {
			w.streamWriter = streamWriter
			w.chunkChannel = make(chan []byte, streamLogQueueCapacity)
			doneChan := make(chan struct{})
			w.streamDone = doneChan

			// Start async chunk processor
			go w.processStreamingChunks(streamWriter, w.chunkChannel, doneChan)

			// Write status immediately
			_ = streamWriter.WriteStatus(statusCode, w.headers)
		}
	}

	// Call original WriteHeader
	w.ResponseWriter.WriteHeader(statusCode)
}

// ensureHeadersCaptured is a helper function to make sure response headers are captured.
// It is safe to call this method multiple times; it will always refresh the headers
// with the latest state from the underlying ResponseWriter.
func (w *ResponseWriterWrapper) ensureHeadersCaptured() {
	// Always capture the current headers to ensure we have the latest state
	w.captureCurrentHeaders()
}

// captureCurrentHeaders reads all headers from the underlying ResponseWriter and stores them
// in the wrapper's headers map. It creates copies of the header values to prevent race conditions.
func (w *ResponseWriterWrapper) captureCurrentHeaders() {
	// Initialize headers map if needed
	if w.headers == nil {
		w.headers = make(map[string][]string)
	}

	// Capture all current headers from the underlying ResponseWriter
	for key, values := range w.ResponseWriter.Header() {
		// Make a copy of the values slice to avoid reference issues
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		w.headers[key] = headerValues
	}
}

// detectStreaming determines if a response should be treated as a streaming response.
// It checks for a "text/event-stream" Content-Type or a '"stream": true'
// field in the original request body.
func (w *ResponseWriterWrapper) detectStreaming(contentType string) bool {
	// Check Content-Type for Server-Sent Events
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	// If a concrete Content-Type is already set (e.g., application/json for error responses),
	// treat it as non-streaming instead of inferring from the request payload.
	if strings.TrimSpace(contentType) != "" {
		return false
	}

	return false
}

// processStreamingChunks runs in a separate goroutine to process response chunks from the chunkChannel.
// It asynchronously writes each chunk to the streaming log writer.
func (w *ResponseWriterWrapper) processStreamingChunks(streamWriter logging.StreamingLogWriter, chunks <-chan []byte, done chan struct{}) {
	if done == nil {
		return
	}

	defer close(done)

	if streamWriter == nil || chunks == nil {
		return
	}

	for chunk := range chunks {
		w.chunkMu.Lock()
		w.queuedChunkBytes -= len(chunk)
		if w.queuedChunkBytes < 0 {
			w.queuedChunkBytes = 0
		}
		w.chunkMu.Unlock()
		streamWriter.WriteChunkAsync(chunk)
	}
	w.chunkMu.Lock()
	dropped := w.streamQueueDropped
	w.chunkMu.Unlock()
	if dropped {
		streamWriter.WriteChunkAsync([]byte(streamLogQueueDropMarker))
	}
}

// Finalize completes the logging process for the request and response.
// For streaming responses, it closes the chunk channel and the stream writer.
// For non-streaming responses, it logs the complete request and response details,
// including any API-specific request/response data stored in the Gin context.
func (w *ResponseWriterWrapper) Finalize(c *gin.Context) error {
	return w.finish(c, false)
}

// Abort releases logging resources during panic unwinding without writing a partial log.
func (w *ResponseWriterWrapper) Abort(c *gin.Context) error {
	return w.finish(c, true)
}

func (w *ResponseWriterWrapper) finish(c *gin.Context, abort bool) error {
	if w == nil {
		return nil
	}
	w.finishMu.Lock()
	if w.finishDone == nil {
		w.finishDone = make(chan struct{})
	}
	done := w.finishDone
	w.finishMu.Unlock()
	w.finishOnce.Do(func() {
		defer close(done)
		if abort {
			w.finishErr = w.abort(c)
			return
		}
		w.finishErr = w.finalize(c)
	})
	<-done
	return w.finishErr
}

func (w *ResponseWriterWrapper) abort(c *gin.Context) error {
	if w == nil {
		return nil
	}
	websocketTimelineSource := w.extractWebsocketTimelineSource(c)
	apiRequestSource := w.extractAPIRequestSource(c)
	apiResponseSource := w.extractAPIResponseSource(c)
	apiWebsocketTimelineSource := w.extractAPIWebsocketTimelineSource(c)
	defer cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource)
	w.closeStreamingQueue()
	if w.streamWriter == nil {
		return nil
	}
	errClose := w.streamWriter.Close()
	w.streamWriter = nil
	return errClose
}

func (w *ResponseWriterWrapper) closeStreamingQueue() {
	w.chunkMu.Lock()
	if !w.streamClosed {
		w.streamClosed = true
		if w.chunkChannel != nil {
			close(w.chunkChannel)
		}
	}
	done := w.streamDone
	w.chunkMu.Unlock()
	if done != nil {
		<-done
	}
	w.chunkMu.Lock()
	w.chunkChannel = nil
	w.streamDone = nil
	w.queuedChunkBytes = 0
	w.chunkMu.Unlock()
}

func (w *ResponseWriterWrapper) finalize(c *gin.Context) error {
	if w.logger == nil {
		return nil
	}

	finalStatusCode := w.statusCode
	if finalStatusCode == 0 {
		if statusWriter, ok := w.ResponseWriter.(interface{ Status() int }); ok {
			finalStatusCode = statusWriter.Status()
		} else {
			finalStatusCode = 200
		}
	}

	var slicesAPIResponseError []*interfaces.ErrorMessage
	apiResponseError, isExist := c.Get("API_RESPONSE_ERROR")
	if isExist {
		if apiErrors, ok := apiResponseError.([]*interfaces.ErrorMessage); ok {
			slicesAPIResponseError = apiErrors
		}
	}

	hasAPIError := len(slicesAPIResponseError) > 0 || finalStatusCode >= http.StatusBadRequest
	forceLog := w.logOnErrorOnly && hasAPIError && !w.logger.IsEnabled()
	websocketTimelineSource := w.extractWebsocketTimelineSource(c)
	apiRequestSource := w.extractAPIRequestSource(c)
	apiResponseSource := w.extractAPIResponseSource(c)
	apiWebsocketTimelineSource := w.extractAPIWebsocketTimelineSource(c)
	if !w.logger.IsEnabled() && !forceLog {
		cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource)
		return nil
	}

	if w.isStreaming && w.streamWriter != nil {
		w.closeStreamingQueue()

		w.streamWriter.SetFirstChunkTimestamp(w.firstChunkTimestamp)

		// Write API Request and Response to the streaming log before closing
		apiRequest := w.extractAPIRequest(c)
		apiResponse := w.extractAPIResponse(c)
		if sourceWriter, ok := w.streamWriter.(interface {
			WriteAPIRequestSource(*logging.FileBodySource) error
			WriteAPIResponseSource(*logging.FileBodySource) error
			WriteAPIWebsocketTimelineSource(*logging.FileBodySource) error
		}); ok {
			if len(apiRequest) > 0 {
				_ = w.streamWriter.WriteAPIRequest(apiRequest)
			}
			if apiRequestSource != nil && apiRequestSource.HasPayload() {
				_ = sourceWriter.WriteAPIRequestSource(apiRequestSource)
			}
			if len(apiResponse) > 0 {
				_ = w.streamWriter.WriteAPIResponse(apiResponse)
			}
			if apiResponseSource != nil && apiResponseSource.HasPayload() {
				_ = sourceWriter.WriteAPIResponseSource(apiResponseSource)
			}
		} else {
			var errMerge error
			apiRequest, errMerge = mergeFileBodySource(apiRequest, apiRequestSource)
			if errMerge != nil {
				cleanupFileBodySources(websocketTimelineSource, apiResponseSource, apiWebsocketTimelineSource)
				return errMerge
			}
			apiResponse, errMerge = mergeFileBodySource(apiResponse, apiResponseSource)
			if errMerge != nil {
				cleanupFileBodySources(websocketTimelineSource, apiWebsocketTimelineSource)
				return errMerge
			}
			if len(apiRequest) > 0 {
				_ = w.streamWriter.WriteAPIRequest(apiRequest)
			}
			if len(apiResponse) > 0 {
				_ = w.streamWriter.WriteAPIResponse(apiResponse)
			}
		}
		apiWebsocketTimeline := w.extractAPIWebsocketTimeline(c)
		if len(apiWebsocketTimeline) > 0 {
			_ = w.streamWriter.WriteAPIWebsocketTimeline(apiWebsocketTimeline)
		}
		if sourceWriter, ok := w.streamWriter.(interface {
			WriteAPIWebsocketTimelineSource(*logging.FileBodySource) error
		}); ok && apiWebsocketTimelineSource != nil && apiWebsocketTimelineSource.HasPayload() {
			_ = sourceWriter.WriteAPIWebsocketTimelineSource(apiWebsocketTimelineSource)
		} else {
			cleanupFileBodySources(apiWebsocketTimelineSource)
		}
		if err := w.streamWriter.Close(); err != nil {
			w.streamWriter = nil
			cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource)
			return err
		}
		w.streamWriter = nil
		cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource)
		return nil
	}

	return w.logRequest(w.extractRequestBody(c), finalStatusCode, w.cloneHeaders(), w.extractResponseBody(c), w.extractWebsocketTimeline(c), websocketTimelineSource, w.extractAPIRequest(c), apiRequestSource, w.extractAPIResponse(c), apiResponseSource, w.extractAPIWebsocketTimeline(c), apiWebsocketTimelineSource, w.extractAPIResponseTimestamp(c), slicesAPIResponseError, forceLog)
}

func (w *ResponseWriterWrapper) cloneHeaders() map[string][]string {
	w.ensureHeadersCaptured()

	finalHeaders := make(map[string][]string, len(w.headers))
	for key, values := range w.headers {
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		finalHeaders[key] = headerValues
	}

	return finalHeaders
}

func (w *ResponseWriterWrapper) extractAPIRequest(c *gin.Context) []byte {
	apiRequest, isExist := c.Get("API_REQUEST")
	if !isExist {
		return nil
	}
	data, ok := apiRequest.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	return logging.SummarizeBodyForLog(data, "text/plain")
}

func (w *ResponseWriterWrapper) extractAPIResponse(c *gin.Context) []byte {
	return logging.SummarizeBodyForLog(helps.MaterializeAPIResponse(c), "text/plain")
}

func (w *ResponseWriterWrapper) extractAPIRequestSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.APIRequestSourceContextKey)
}

func (w *ResponseWriterWrapper) extractAPIResponseSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.APIResponseSourceContextKey)
}

func (w *ResponseWriterWrapper) extractAPIWebsocketTimeline(c *gin.Context) []byte {
	apiTimeline, isExist := c.Get("API_WEBSOCKET_TIMELINE")
	if !isExist {
		return nil
	}
	data, ok := apiTimeline.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	return logging.SummarizeBodyForLog(data, "text/plain")
}

func (w *ResponseWriterWrapper) extractAPIWebsocketTimelineSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.APIWebsocketTimelineSourceContextKey)
}

func (w *ResponseWriterWrapper) extractAPIResponseTimestamp(c *gin.Context) time.Time {
	ts, isExist := c.Get("API_RESPONSE_TIMESTAMP")
	if !isExist {
		return time.Time{}
	}
	if t, ok := ts.(time.Time); ok {
		return t
	}
	return time.Time{}
}

func (w *ResponseWriterWrapper) extractRequestBody(c *gin.Context) []byte {
	_ = c
	if w.requestInfo == nil {
		return logging.SummarizeBodyForLog(nil, "")
	}
	if w.requestInfo.bodyCapture != nil {
		return w.requestInfo.bodyCapture.metadata()
	}
	return logging.SummarizeBodyForLog(w.requestInfo.Body, headerValue(w.requestInfo.Headers, "Content-Type"))
}

func (w *ResponseWriterWrapper) extractResponseBody(c *gin.Context) []byte {
	_ = c
	w.responseMu.Lock()
	defer w.responseMu.Unlock()
	if w.responseDigest == nil {
		w.responseDigest = sha256.New()
	}
	return logging.EncodeBodyLogMetadata(logging.BodyLogMetadata{
		Bytes:       w.responseBytes,
		SHA256:      hex.EncodeToString(w.responseDigest.Sum(nil)),
		Chunks:      w.responseChunks,
		ContentType: headerValue(w.headers, "Content-Type"),
	})
}

func (w *ResponseWriterWrapper) extractWebsocketTimeline(c *gin.Context) []byte {
	return extractBodyOverride(c, websocketTimelineOverrideContextKey)
}

func (w *ResponseWriterWrapper) extractWebsocketTimelineSource(c *gin.Context) *logging.FileBodySource {
	return extractFileBodySource(c, logging.WebsocketTimelineSourceContextKey)
}

func extractFileBodySource(c *gin.Context, key string) *logging.FileBodySource {
	if c == nil {
		return nil
	}
	value, exists := c.Get(key)
	if !exists {
		return nil
	}
	source, ok := value.(*logging.FileBodySource)
	if !ok || source == nil {
		return nil
	}
	return source
}

func extractBodyOverride(c *gin.Context, key string) []byte {
	if c == nil {
		return nil
	}
	bodyOverride, isExist := c.Get(key)
	if !isExist {
		return nil
	}
	switch value := bodyOverride.(type) {
	case []byte:
		if len(value) > 0 {
			return logging.SummarizeBodyForLog(value, "text/plain")
		}
	case string:
		if strings.TrimSpace(value) != "" {
			digest := sha256.Sum256([]byte(value))
			return logging.EncodeBodyLogMetadata(logging.BodyLogMetadata{
				Bytes:       int64(len(value)),
				SHA256:      hex.EncodeToString(digest[:]),
				ContentType: "text/plain",
			})
		}
	}
	return nil
}

func (w *ResponseWriterWrapper) logRequest(requestBody []byte, statusCode int, headers map[string][]string, body, websocketTimeline []byte, websocketTimelineSource *logging.FileBodySource, apiRequestBody []byte, apiRequestSource *logging.FileBodySource, apiResponseBody []byte, apiResponseSource *logging.FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *logging.FileBodySource, apiResponseTimestamp time.Time, apiResponseErrors []*interfaces.ErrorMessage, forceLog bool) error {
	if w.requestInfo == nil {
		cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource)
		return nil
	}

	if loggerWithAllSources, ok := w.logger.(interface {
		LogRequestWithOptionsAndAllSources(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, *logging.FileBodySource, []byte, *logging.FileBodySource, []byte, *logging.FileBodySource, []byte, *logging.FileBodySource, []*interfaces.ErrorMessage, bool, string, time.Time, time.Time) error
	}); ok {
		return loggerWithAllSources.LogRequestWithOptionsAndAllSources(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			requestBody,
			statusCode,
			headers,
			body,
			websocketTimeline,
			websocketTimelineSource,
			apiRequestBody,
			apiRequestSource,
			apiResponseBody,
			apiResponseSource,
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
			apiResponseErrors,
			forceLog,
			w.requestInfo.RequestID,
			w.requestInfo.Timestamp,
			apiResponseTimestamp,
		)
	}

	if loggerWithSources, ok := w.logger.(interface {
		LogRequestWithOptionsAndSources(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, *logging.FileBodySource, []byte, []byte, []byte, *logging.FileBodySource, []*interfaces.ErrorMessage, bool, string, time.Time, time.Time) error
	}); ok {
		var errMerge error
		apiRequestBody, errMerge = mergeFileBodySource(apiRequestBody, apiRequestSource)
		if errMerge != nil {
			cleanupFileBodySources(websocketTimelineSource, apiResponseSource, apiWebsocketTimelineSource)
			return errMerge
		}
		apiResponseBody, errMerge = mergeFileBodySource(apiResponseBody, apiResponseSource)
		if errMerge != nil {
			cleanupFileBodySources(websocketTimelineSource, apiWebsocketTimelineSource)
			return errMerge
		}
		return loggerWithSources.LogRequestWithOptionsAndSources(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			requestBody,
			statusCode,
			headers,
			body,
			websocketTimeline,
			websocketTimelineSource,
			apiRequestBody,
			apiResponseBody,
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
			apiResponseErrors,
			forceLog,
			w.requestInfo.RequestID,
			w.requestInfo.Timestamp,
			apiResponseTimestamp,
		)
	}

	var errMerge error
	websocketTimeline, errMerge = mergeFileBodySource(websocketTimeline, websocketTimelineSource)
	if errMerge != nil {
		cleanupFileBodySources(apiRequestSource, apiResponseSource, apiWebsocketTimelineSource)
		return errMerge
	}
	apiRequestBody, errMerge = mergeFileBodySource(apiRequestBody, apiRequestSource)
	if errMerge != nil {
		cleanupFileBodySources(apiResponseSource, apiWebsocketTimelineSource)
		return errMerge
	}
	apiResponseBody, errMerge = mergeFileBodySource(apiResponseBody, apiResponseSource)
	if errMerge != nil {
		cleanupFileBodySources(apiWebsocketTimelineSource)
		return errMerge
	}
	apiWebsocketTimeline, errMerge = mergeFileBodySource(apiWebsocketTimeline, apiWebsocketTimelineSource)
	if errMerge != nil {
		return errMerge
	}

	if loggerWithOptions, ok := w.logger.(interface {
		LogRequestWithOptions(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage, bool, string, time.Time, time.Time) error
	}); ok {
		return loggerWithOptions.LogRequestWithOptions(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			requestBody,
			statusCode,
			headers,
			body,
			websocketTimeline,
			apiRequestBody,
			apiResponseBody,
			apiWebsocketTimeline,
			apiResponseErrors,
			forceLog,
			w.requestInfo.RequestID,
			w.requestInfo.Timestamp,
			apiResponseTimestamp,
		)
	}

	return w.logger.LogRequest(
		w.requestInfo.URL,
		w.requestInfo.Method,
		w.requestInfo.Headers,
		requestBody,
		statusCode,
		headers,
		body,
		websocketTimeline,
		apiRequestBody,
		apiResponseBody,
		apiWebsocketTimeline,
		apiResponseErrors,
		w.requestInfo.RequestID,
		w.requestInfo.Timestamp,
		apiResponseTimestamp,
	)
}

func mergeFileBodySource(payload []byte, source *logging.FileBodySource) ([]byte, error) {
	if source == nil {
		return payload, nil
	}
	defer cleanupFileBodySources(source)
	if !source.HasPayload() {
		return payload, nil
	}
	metadata, errMetadata := source.Metadata("text/plain")
	if errMetadata != nil {
		return nil, errMetadata
	}
	if len(payload) == 0 {
		return metadata, nil
	}
	return []byte(string(normalizeMiddlewareBodyMetadata(payload)) + "\n" + string(metadata)), nil
}

func normalizeMiddlewareBodyMetadata(payload []byte) []byte {
	return logging.SummarizeBodyForLog(payload, "text/plain")
}

func headerValue(headers map[string][]string, target string) string {
	for key, values := range headers {
		if strings.EqualFold(strings.TrimSpace(key), target) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func cleanupFileBodySources(sources ...*logging.FileBodySource) {
	for _, source := range sources {
		if source == nil {
			continue
		}
		if errCleanup := source.Cleanup(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up log part files")
		}
	}
}
