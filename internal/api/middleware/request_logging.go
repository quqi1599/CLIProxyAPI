// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request logging middleware that captures comprehensive
// request and response data when enabled through configuration.
package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// RequestLoggingMiddleware creates a Gin middleware that logs HTTP requests and responses.
// It captures detailed information about the request and response, including headers and body,
// and uses the provided RequestLogger to record this data. When full request logging is disabled,
// body capture is limited to small known-size payloads to avoid large per-request memory spikes.
func RequestLoggingMiddleware(logger logging.RequestLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logger == nil {
			c.Next()
			return
		}

		if shouldSkipMethodForRequestLogging(c.Request) {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogRequest(path) {
			c.Next()
			return
		}

		loggerEnabled := logger.IsEnabled()

		// Capture request metadata without retaining raw body bytes.
		requestInfo, err := captureRequestInfo(c)
		if err != nil {
			// Log error but continue processing
			// In a real implementation, you might want to use a proper logger here
			c.Next()
			return
		}

		// Create response writer wrapper
		wrapper := NewResponseWriterWrapper(c.Writer, logger, requestInfo)
		if !loggerEnabled {
			wrapper.logOnErrorOnly = true
		}
		c.Writer = wrapper
		attachRequestLogSources(c, logger, loggerEnabled)

		defer func() {
			if panicValue := recover(); panicValue != nil {
				_ = wrapper.Abort(c)
				panic(panicValue)
			}
			_ = wrapper.Finalize(c)
		}()
		c.Next()
	}
}

type fileBodySourceFactory interface {
	NewFileBodySource(prefix string) (*logging.FileBodySource, error)
}

func attachRequestLogSources(c *gin.Context, logger logging.RequestLogger, loggerEnabled bool) {
	if c == nil || !loggerEnabled {
		return
	}
	factory, ok := logger.(fileBodySourceFactory)
	if !ok || factory == nil {
		return
	}
	if source, errSource := factory.NewFileBodySource("api-request"); errSource == nil {
		c.Set(logging.APIRequestSourceContextKey, source)
	}
	if source, errSource := factory.NewFileBodySource("api-response"); errSource == nil {
		c.Set(logging.APIResponseSourceContextKey, source)
	}
	if !isResponsesWebsocketUpgrade(c.Request) {
		return
	}
	if source, errSource := factory.NewFileBodySource("websocket-timeline"); errSource == nil {
		c.Set(logging.WebsocketTimelineSourceContextKey, source)
	}
	if source, errSource := factory.NewFileBodySource("api-websocket-timeline"); errSource == nil {
		c.Set(logging.APIWebsocketTimelineSourceContextKey, source)
	}
}

func shouldSkipMethodForRequestLogging(req *http.Request) bool {
	if req == nil {
		return true
	}
	if req.Method != http.MethodGet {
		return false
	}
	return !isResponsesWebsocketUpgrade(req)
}

func isResponsesWebsocketUpgrade(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if req.URL.Path != "/v1/responses" && req.URL.Path != "/backend-api/codex/responses" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

// captureRequestInfo extracts relevant information from the incoming HTTP request.
// The request body is wrapped with a streaming digest and is never buffered for logs.
func captureRequestInfo(c *gin.Context) (*RequestInfo, error) {
	// Capture URL with sensitive query parameters masked
	maskedQuery := util.MaskSensitiveQuery(c.Request.URL.RawQuery)
	url := c.Request.URL.Path
	if maskedQuery != "" {
		url += "?" + maskedQuery
	}

	// Capture method
	method := c.Request.Method

	// Capture headers
	headers := make(map[string][]string, len(c.Request.Header))
	for key, values := range c.Request.Header {
		headers[key] = append([]string(nil), values...)
	}

	var bodyCapture *requestBodyMetadataCapture
	if c.Request.Body != nil {
		bodyCapture = &requestBodyMetadataCapture{
			body:          c.Request.Body,
			digest:        sha256.New(),
			contentLength: c.Request.ContentLength,
			contentType:   c.Request.Header.Get("Content-Type"),
		}
		c.Request.Body = bodyCapture
	}

	return &RequestInfo{
		URL:         url,
		Method:      method,
		Headers:     headers,
		bodyCapture: bodyCapture,
		RequestID:   logging.GetGinRequestID(c),
		Timestamp:   time.Now(),
	}, nil
}

type requestBodyMetadataCapture struct {
	mu            sync.Mutex
	body          io.ReadCloser
	digest        hash.Hash
	bytesRead     int64
	contentLength int64
	contentType   string
	complete      bool
}

func (c *requestBodyMetadataCapture) Read(payload []byte) (int, error) {
	if c == nil || c.body == nil {
		return 0, io.EOF
	}
	n, errRead := c.body.Read(payload)
	c.mu.Lock()
	if n > 0 {
		_, _ = c.digest.Write(payload[:n])
		c.bytesRead += int64(n)
	}
	if errRead == io.EOF || (c.contentLength >= 0 && c.bytesRead >= c.contentLength) {
		c.complete = true
	}
	c.mu.Unlock()
	return n, errRead
}

func (c *requestBodyMetadataCapture) Close() error {
	if c == nil || c.body == nil {
		return nil
	}
	return c.body.Close()
}

func (c *requestBodyMetadataCapture) metadata() []byte {
	if c == nil {
		return logging.SummarizeBodyForLog(nil, "")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return logging.EncodeBodyLogMetadata(logging.BodyLogMetadata{
		Bytes:       c.bytesRead,
		SHA256:      hex.EncodeToString(c.digest.Sum(nil)),
		ContentType: c.contentType,
		Truncated:   !c.complete && (c.bytesRead > 0 || c.contentLength > 0),
	})
}

// shouldLogRequest determines whether the request should be logged.
// It skips management endpoints to avoid leaking secrets but allows
// all other routes, including module-provided ones, to honor request-log.
func shouldLogRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}

	return true
}
