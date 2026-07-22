package middleware

import (
	"bytes"
	"crypto/sha256"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestExtractRequestBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{
		requestInfo: &RequestInfo{Body: []byte("original-body")},
	}

	body := wrapper.extractRequestBody(c)
	if !strings.Contains(string(body), "[BODY METADATA v1]") || strings.Contains(string(body), "original-body") {
		t.Fatalf("request body is not metadata-only: %q", body)
	}

	c.Set(requestBodyOverrideContextKey, []byte("override-body"))
	body = wrapper.extractRequestBody(c)
	if strings.Contains(string(body), "override-body") {
		t.Fatalf("request override leaked into metadata: %q", body)
	}
}

func TestExtractRequestBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	c.Set(requestBodyOverrideContextKey, "override-as-string")

	body := wrapper.extractRequestBody(c)
	if strings.Contains(string(body), "override-as-string") {
		t.Fatalf("request override leaked: %q", body)
	}
}

func TestExtractResponseBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{responseDigest: sha256.New()}
	wrapper.captureResponseBytes([]byte("original-response"))

	body := wrapper.extractResponseBody(c)
	if !strings.Contains(string(body), "[BODY METADATA v1]") || strings.Contains(string(body), "original-response") {
		t.Fatalf("response body is not metadata-only: %q", body)
	}

	c.Set(responseBodyOverrideContextKey, []byte("override-response"))
	body = wrapper.extractResponseBody(c)
	if strings.Contains(string(body), "override-response") {
		t.Fatalf("response override leaked: %q", body)
	}
}

func TestExtractResponseBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	c.Set(responseBodyOverrideContextKey, "override-response-as-string")

	body := wrapper.extractResponseBody(c)
	if strings.Contains(string(body), "override-response-as-string") {
		t.Fatalf("response override leaked: %q", body)
	}
}

func TestExtractBodyOverrideClonesBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	override := []byte("body-override")
	c.Set(requestBodyOverrideContextKey, override)

	body := extractBodyOverride(c, requestBodyOverrideContextKey)
	if bytes.Contains(body, override) || !strings.Contains(string(body), "[BODY METADATA v1]") {
		t.Fatalf("body override is not metadata-only: %q", body)
	}
}

func TestExtractWebsocketTimelineUsesOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	if got := wrapper.extractWebsocketTimeline(c); got != nil {
		t.Fatalf("expected nil websocket timeline, got %q", string(got))
	}

	c.Set(websocketTimelineOverrideContextKey, []byte("timeline"))
	body := wrapper.extractWebsocketTimeline(c)
	if strings.Contains(string(body), "timeline") || !strings.Contains(string(body), "[BODY METADATA v1]") {
		t.Fatalf("websocket timeline is not metadata-only: %q", body)
	}
}

func TestFinalizeStreamingWritesAPIWebsocketTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	streamWriter := &testStreamingLogWriter{}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         &testRequestLogger{enabled: true},
		requestInfo: &RequestInfo{
			URL:       "/v1/responses",
			Method:    "POST",
			Headers:   map[string][]string{"Content-Type": {"application/json"}},
			RequestID: "req-1",
			Timestamp: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
		},
		isStreaming:  true,
		streamWriter: streamWriter,
	}

	c.Set("API_WEBSOCKET_TIMELINE", []byte("Timestamp: 2026-04-01T12:00:00Z\nEvent: api.websocket.request\n{}"))

	if err := wrapper.Finalize(c); err != nil {
		t.Fatalf("Finalize error: %v", err)
	}
	if bytes.Contains(streamWriter.apiWebsocketTimeline, []byte("api.websocket.request")) || !strings.Contains(string(streamWriter.apiWebsocketTimeline), "[BODY METADATA v1]") {
		t.Fatalf("stream writer websocket timeline is not metadata-only: %q", streamWriter.apiWebsocketTimeline)
	}
	if !streamWriter.closed {
		t.Fatal("expected stream writer to be closed")
	}
}

func TestResponseWriterWrapper_StreamingChunkKeepsClientBodyAndBoundsQueue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	wrapper := NewResponseWriterWrapper(c.Writer, &testRequestLogger{enabled: true}, &RequestInfo{})
	wrapper.isStreaming = true
	wrapper.chunkChannel = make(chan []byte, streamLogQueueCapacity)

	payload := bytes.Repeat([]byte("client-visible"), (streamLogChunkMaxBytes/len("client-visible"))+128)
	written, errWrite := wrapper.Write(payload)
	if errWrite != nil {
		t.Fatalf("Write: %v", errWrite)
	}
	if written != len(payload) || !bytes.Equal(recorder.Body.Bytes(), payload) {
		t.Fatalf("client response changed: written=%d want=%d body=%d", written, len(payload), recorder.Body.Len())
	}
	if got := wrapper.queuedChunkBytes; got > streamLogChunkMaxBytes {
		t.Fatalf("queued chunk bytes = %d, per-chunk limit = %d", got, streamLogChunkMaxBytes)
	}
	queued := <-wrapper.chunkChannel
	if len(queued) > streamLogChunkMaxBytes || !bytes.Contains(queued, []byte(streamLogChunkMarker)) {
		t.Fatalf("queued chunk was not bounded: bytes=%d", len(queued))
	}
	wrapper.queuedChunkBytes -= len(queued)
	wrapper.closeStreamingQueue()
}

func TestResponseWriterWrapper_StreamingQueueHasByteBudget(t *testing.T) {
	wrapper := &ResponseWriterWrapper{chunkChannel: make(chan []byte, streamLogQueueCapacity)}
	chunk := bytes.Repeat([]byte("z"), streamLogChunkMaxBytes+1)
	for i := 0; i < streamLogQueueCapacity+8; i++ {
		wrapper.enqueueStreamingChunk(chunk, "")
	}

	wrapper.chunkMu.Lock()
	queuedBytes := wrapper.queuedChunkBytes
	dropped := wrapper.streamQueueDropped
	channel := wrapper.chunkChannel
	wrapper.chunkMu.Unlock()
	if queuedBytes > streamLogQueueMaxBytes {
		t.Fatalf("queued bytes = %d, budget = %d", queuedBytes, streamLogQueueMaxBytes)
	}
	if !dropped {
		t.Fatal("expected queue overflow to set the truncation flag")
	}
	wrapper.closeStreamingQueue()
	for queued := range channel {
		if len(queued) > streamLogChunkMaxBytes {
			t.Fatalf("queued chunk bytes = %d, limit = %d", len(queued), streamLogChunkMaxBytes)
		}
	}
	if wrapper.queuedChunkBytes != 0 {
		t.Fatalf("queued bytes after close = %d, want 0", wrapper.queuedChunkBytes)
	}
}

func TestResponseWriterWrapper_AbortIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	streamWriter := &countingCloseStreamingLogWriter{}
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		logger:         &testRequestLogger{enabled: true},
		requestInfo:    &RequestInfo{},
		isStreaming:    true,
		streamWriter:   streamWriter,
	}

	var callers sync.WaitGroup
	for i := 0; i < 16; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if errAbort := wrapper.Abort(c); errAbort != nil {
				t.Errorf("Abort: %v", errAbort)
			}
		}()
	}
	callers.Wait()
	if got := streamWriter.closeCalls.Load(); got != 1 {
		t.Fatalf("stream writer close calls = %d, want 1", got)
	}
}

func TestResponseWriterWrapper_RepeatedWriteHeaderStartsOneStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	streamWriter := &countingCloseStreamingLogWriter{}
	logger := &testRequestLogger{enabled: true, streamWriter: streamWriter}
	wrapper := NewResponseWriterWrapper(c.Writer, logger, &RequestInfo{})
	wrapper.Header().Set("Content-Type", "text/event-stream")

	wrapper.WriteHeader(200)
	wrapper.WriteHeader(201)
	if errFinalize := wrapper.Finalize(c); errFinalize != nil {
		t.Fatalf("Finalize: %v", errFinalize)
	}
	if got := logger.streamCalls.Load(); got != 1 {
		t.Fatalf("stream initializations = %d, want 1", got)
	}
	if got := streamWriter.closeCalls.Load(); got != 1 {
		t.Fatalf("stream closes = %d, want 1", got)
	}
}

type testRequestLogger struct {
	enabled      bool
	streamWriter logging.StreamingLogWriter
	streamCalls  atomic.Int32
}

func (l *testRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (l *testRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	l.streamCalls.Add(1)
	if l.streamWriter != nil {
		return l.streamWriter, nil
	}
	return &testStreamingLogWriter{}, nil
}

func (l *testRequestLogger) IsEnabled() bool {
	return l.enabled
}

type testStreamingLogWriter struct {
	apiWebsocketTimeline []byte
	closed               bool
}

type countingCloseStreamingLogWriter struct {
	closeCalls atomic.Int32
}

func (w *countingCloseStreamingLogWriter) WriteChunkAsync([]byte) {}
func (w *countingCloseStreamingLogWriter) WriteStatus(int, map[string][]string) error {
	return nil
}
func (w *countingCloseStreamingLogWriter) WriteAPIRequest([]byte) error  { return nil }
func (w *countingCloseStreamingLogWriter) WriteAPIResponse([]byte) error { return nil }
func (w *countingCloseStreamingLogWriter) WriteAPIWebsocketTimeline([]byte) error {
	return nil
}
func (w *countingCloseStreamingLogWriter) SetFirstChunkTimestamp(time.Time) {}
func (w *countingCloseStreamingLogWriter) Close() error {
	w.closeCalls.Add(1)
	return nil
}

func (w *testStreamingLogWriter) WriteChunkAsync([]byte) {}

func (w *testStreamingLogWriter) WriteStatus(int, map[string][]string) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIRequest([]byte) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIResponse([]byte) error {
	return nil
}

func (w *testStreamingLogWriter) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	w.apiWebsocketTimeline = bytes.Clone(apiWebsocketTimeline)
	return nil
}

func (w *testStreamingLogWriter) SetFirstChunkTimestamp(time.Time) {}

func (w *testStreamingLogWriter) Close() error {
	w.closed = true
	return nil
}
