package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestShouldSkipMethodForRequestLogging(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		skip bool
	}{
		{
			name: "nil request",
			req:  nil,
			skip: true,
		},
		{
			name: "post request should not skip",
			req: &http.Request{
				Method: http.MethodPost,
				URL:    &url.URL{Path: "/v1/responses"},
			},
			skip: false,
		},
		{
			name: "plain get should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/models"},
				Header: http.Header{},
			},
			skip: true,
		},
		{
			name: "responses websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "codex responses websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/backend-api/codex/responses"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "responses get without upgrade should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{},
			},
			skip: true,
		},
	}

	for i := range tests {
		got := shouldSkipMethodForRequestLogging(tests[i].req)
		if got != tests[i].skip {
			t.Fatalf("%s: got skip=%t, want %t", tests[i].name, got, tests[i].skip)
		}
	}
}

func TestAttachRequestLogSourcesUsesLoggerLogsDir(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 0)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/backend-api/codex/responses", nil)
	c.Request.Header.Set("Upgrade", "websocket")

	attachRequestLogSources(c, logger, true)
	defer cleanupFileBodySourcesFromContext(c)

	for _, key := range []string{
		logging.WebsocketTimelineSourceContextKey,
		logging.APIWebsocketTimelineSourceContextKey,
	} {
		value, exists := c.Get(key)
		if !exists {
			t.Fatalf("expected %s source to be attached", key)
		}
		source, ok := value.(*logging.FileBodySource)
		if !ok || source == nil {
			t.Fatalf("%s source type = %T", key, value)
		}
		file, errPart := source.CreatePart("probe")
		if errPart != nil {
			t.Fatalf("CreatePart(%s): %v", key, errPart)
		}
		path := file.Name()
		if errClose := file.Close(); errClose != nil {
			t.Fatalf("close part: %v", errClose)
		}
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("%s part path %s is not under logs dir %s", key, path, logsDir)
		}
	}
}

func cleanupFileBodySourcesFromContext(c *gin.Context) {
	if c == nil {
		return
	}
	for _, key := range []string{
		logging.WebsocketTimelineSourceContextKey,
		logging.APIWebsocketTimelineSourceContextKey,
	} {
		value, exists := c.Get(key)
		if !exists {
			continue
		}
		if source, ok := value.(*logging.FileBodySource); ok && source != nil {
			_ = source.Cleanup()
		}
	}
}

func TestCaptureRequestInfoDecodesZstdRequestBodyForLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model","stream":true}`)
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	compressedBytes := compressed.Bytes()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedBytes))
	req.Header.Set("Content-Encoding", "zstd")
	c.Request = req

	info, errCapture := captureRequestInfo(c)
	if errCapture != nil {
		t.Fatalf("captureRequestInfo: %v", errCapture)
	}
	if len(info.Body) != 0 || info.bodyCapture == nil {
		t.Fatal("request body should be represented only by streaming metadata")
	}

	restoredBody, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		t.Fatalf("read restored request body: %v", errRead)
	}
	if !bytes.Equal(restoredBody, compressedBytes) {
		t.Fatal("request body was not restored with the original compressed bytes")
	}
}

func TestCaptureRequestInfoDoesNotBufferLargeRequestBody(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("x"), (1<<20)+2)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))

	info, err := captureRequestInfo(c)
	if err != nil {
		t.Fatalf("captureRequestInfo: %v", err)
	}
	if len(info.Body) != 0 {
		t.Fatalf("captured body bytes = %d, want metadata-only", len(info.Body))
	}
	restored, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read restored request body: %v", err)
	}
	if !bytes.Equal(restored, payload) {
		t.Fatal("request body was not fully replayed after bounded capture")
	}
}

func TestRequestLoggingMiddleware_PanicClosesStreamAndCleansSources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streamWriter := &lifecycleStreamingLogWriter{closed: make(chan struct{})}
	logger := &lifecycleRequestLogger{logsDir: t.TempDir(), streamWriter: streamWriter}
	pathsCh := make(chan []string, 1)

	router := gin.New()
	router.Use(gin.Recovery(), RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) {
		var paths []string
		for _, key := range []string{logging.APIRequestSourceContextKey, logging.APIResponseSourceContextKey} {
			value, exists := c.Get(key)
			if !exists {
				t.Fatalf("source %s was not attached", key)
			}
			source, ok := value.(*logging.FileBodySource)
			if !ok || source == nil {
				t.Fatalf("source %s has type %T", key, value)
			}
			if errAppend := source.AppendBytes([]byte("panic-path-secret")); errAppend != nil {
				t.Fatalf("AppendBytes(%s): %v", key, errAppend)
			}
			paths = append(paths, source.Paths()...)
		}
		pathsCh <- paths
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write([]byte("data: client still receives this\n\n"))
		panic("panic after stream start")
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"safe"}`))
	router.ServeHTTP(recorder, req)
	paths := <-pathsCh

	select {
	case <-streamWriter.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream writer was not closed during panic unwinding")
	}
	if got := streamWriter.closeCalls.Load(); got != 1 {
		t.Fatalf("stream close calls = %d, want 1", got)
	}
	if got := streamWriter.chunkCalls.Load(); got == 0 {
		t.Fatal("streaming queue did not drain before close")
	}
	if !strings.Contains(recorder.Body.String(), "client still receives this") {
		t.Fatalf("client stream was changed: %q", recorder.Body.String())
	}
	for _, path := range paths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("panic cleanup left source part %s: %v", path, errStat)
		}
		if _, errStat := os.Stat(filepath.Dir(path)); !os.IsNotExist(errStat) {
			t.Fatalf("panic cleanup left source dir %s: %v", filepath.Dir(path), errStat)
		}
	}
}

type lifecycleRequestLogger struct {
	logsDir      string
	streamWriter *lifecycleStreamingLogWriter
	mu           sync.Mutex
	sources      []*logging.FileBodySource
}

func (l *lifecycleRequestLogger) IsEnabled() bool { return true }

func (l *lifecycleRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (l *lifecycleRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return l.streamWriter, nil
}

func (l *lifecycleRequestLogger) NewFileBodySource(prefix string) (*logging.FileBodySource, error) {
	source, errSource := logging.NewFileBodySourceInDir(l.logsDir, prefix)
	if errSource != nil {
		return nil, errSource
	}
	l.mu.Lock()
	l.sources = append(l.sources, source)
	l.mu.Unlock()
	return source, nil
}

type lifecycleStreamingLogWriter struct {
	closeOnce  sync.Once
	closed     chan struct{}
	closeCalls atomic.Int32
	chunkCalls atomic.Int32
}

func (w *lifecycleStreamingLogWriter) WriteChunkAsync([]byte) { w.chunkCalls.Add(1) }
func (w *lifecycleStreamingLogWriter) WriteStatus(int, map[string][]string) error {
	return nil
}
func (w *lifecycleStreamingLogWriter) WriteAPIRequest([]byte) error  { return nil }
func (w *lifecycleStreamingLogWriter) WriteAPIResponse([]byte) error { return nil }
func (w *lifecycleStreamingLogWriter) WriteAPIWebsocketTimeline([]byte) error {
	return nil
}
func (w *lifecycleStreamingLogWriter) SetFirstChunkTimestamp(time.Time) {}
func (w *lifecycleStreamingLogWriter) Close() error {
	w.closeOnce.Do(func() {
		w.closeCalls.Add(1)
		close(w.closed)
	})
	return nil
}
