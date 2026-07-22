package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

type stubHomeRequestLogClient struct {
	heartbeatOK bool
	pushed      [][]byte
}

func (c *stubHomeRequestLogClient) HeartbeatOK() bool { return c.heartbeatOK }

func (c *stubHomeRequestLogClient) RPushRequestLog(_ context.Context, payload []byte) error {
	c.pushed = append(c.pushed, bytes.Clone(payload))
	return nil
}

func assertFileBodySourceCleaned(t *testing.T, partPaths []string) {
	t.Helper()

	dirs := make(map[string]struct{}, len(partPaths))
	for _, path := range partPaths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("expected part %s to be removed, stat err=%v", path, errStat)
		}
		dirs[filepath.Dir(path)] = struct{}{}
	}
	for dir := range dirs {
		if _, errStat := os.Stat(dir); !os.IsNotExist(errStat) {
			t.Fatalf("expected part dir %s to be removed, stat err=%v", dir, errStat)
		}
	}
}

func TestFileBodySource_RecreatesPartDirAfterManualCleanup(t *testing.T) {
	logsDir := t.TempDir()
	source, errSource := NewFileBodySourceInDir(logsDir, "websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendPart([]byte("before manual cleanup")); errAppend != nil {
		t.Fatalf("AppendPart before cleanup: %v", errAppend)
	}
	if errRemove := os.RemoveAll(logsDir); errRemove != nil {
		t.Fatalf("RemoveAll logs dir: %v", errRemove)
	}
	if errAppend := source.AppendPart([]byte("after manual cleanup")); errAppend != nil {
		t.Fatalf("AppendPart after cleanup: %v", errAppend)
	}

	raw, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes after cleanup: %v", errBytes)
	}
	if bytes.Contains(raw, []byte("before manual cleanup")) {
		t.Fatalf("expected manually removed part to be skipped, got %q", string(raw))
	}
	if !bytes.Contains(raw, []byte("after manual cleanup")) {
		t.Fatalf("expected recreated part content, got %q", string(raw))
	}

	partPaths := source.Paths()
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
	assertFileBodySourceCleaned(t, partPaths)
}

func TestFileBodySource_EnforcesByteLimitAndCleansParts(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "byte-limit")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	secretTail := []byte("unique-secret-after-file-source-limit")
	payload := append(bytes.Repeat([]byte("x"), fileBodySourceMaxBytes+1), secretTail...)
	if errAppend := source.AppendBytes(payload); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}

	raw, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes: %v", errBytes)
	}
	if got := len(raw); got > fileBodySourceMaxBytes {
		t.Fatalf("source bytes = %d, limit = %d", got, fileBodySourceMaxBytes)
	}
	if got := strings.Count(string(raw), fileBodySourceTruncationMarker); got != 1 {
		t.Fatalf("truncation marker count = %d, want 1", got)
	}
	if bytes.Contains(raw, secretTail) {
		t.Fatal("source retained bytes after the hard limit")
	}

	partPaths := source.Paths()
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
	assertFileBodySourceCleaned(t, partPaths)
}

func TestFileBodySource_EnforcesPartLimit(t *testing.T) {
	source, errSource := NewFileBodySourceInDir(t.TempDir(), "part-limit")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	for i := 0; i < fileBodySourceMaxParts+1; i++ {
		part, errPart := source.CreatePart("part")
		if errPart != nil {
			t.Fatalf("CreatePart(%d): %v", i, errPart)
		}
		if _, errWrite := part.Write([]byte("x")); errWrite != nil {
			t.Fatalf("Write(%d): %v", i, errWrite)
		}
		if errClose := part.Close(); errClose != nil {
			t.Fatalf("Close(%d): %v", i, errClose)
		}
	}

	partPaths := source.Paths()
	if got := len(partPaths); got != fileBodySourceMaxParts {
		t.Fatalf("part count = %d, want %d", got, fileBodySourceMaxParts)
	}
	raw, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes: %v", errBytes)
	}
	if got := strings.Count(string(raw), fileBodySourceTruncationMarker); got != 1 {
		t.Fatalf("truncation marker count = %d, want 1", got)
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
	assertFileBodySourceCleaned(t, partPaths)
}

func TestFileRequestLogger_HomeEnabled_ForwardsWhenRequestLogEnabled(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	secretMarker := "unique-log-secret-8ff3bff6"
	requestHeaders := map[string][]string{
		"Content-Type":        {"application/json"},
		"Authorization":       {"Bearer " + secretMarker},
		"Proxy-Authorization": {"Basic " + secretMarker},
		"Cookie":              {"session=" + secretMarker},
		"X-Api-Key":           {secretMarker},
		"X-Access-Token":      {secretMarker},
		"X-Client-Secret":     {secretMarker},
		"X-Trace-ID":          {"trace-safe-value"},
	}

	errLog := logger.LogRequest(
		"/v1/chat/completions",
		http.MethodPost,
		requestHeaders,
		[]byte(`{"input":"`+secretMarker+`"}`),
		http.StatusOK,
		map[string][]string{
			"Content-Type": {"application/json"},
			"Set-Cookie":   {"session=" + secretMarker},
			"X-Trace-ID":   {"response-trace-safe-value"},
		},
		[]byte(`{"result":"`+secretMarker+`"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		"req-1",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequest error: %v", errLog)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no local request log files, got entries: %+v", entries)
	}

	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		Headers    map[string][]string `json:"headers"`
		RequestID  string              `json:"request_id"`
		RequestLog string              `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.Headers == nil || got.Headers["Content-Type"][0] != "application/json" {
		t.Fatalf("headers.content-type = %+v, want application/json", got.Headers["Content-Type"])
	}
	if got.Headers == nil || got.Headers["Authorization"][0] != RedactedHeaderValue {
		t.Fatalf("headers.authorization = %+v, want redacted", got.Headers["Authorization"])
	}
	for _, key := range []string{"Proxy-Authorization", "Cookie", "X-Api-Key", "X-Access-Token", "X-Client-Secret"} {
		if got.Headers == nil || len(got.Headers[key]) != 1 || got.Headers[key][0] != RedactedHeaderValue {
			t.Fatalf("headers.%s = %+v, want redacted", key, got.Headers[key])
		}
	}
	if got.Headers["X-Trace-ID"][0] != "trace-safe-value" || !strings.Contains(got.RequestLog, "response-trace-safe-value") {
		t.Fatalf("harmless headers were not preserved: payload=%s", stub.pushed[0])
	}
	if got.RequestID != "req-1" {
		t.Fatalf("request_id = %q, want req-1", got.RequestID)
	}
	if got.RequestLog == "" {
		t.Fatalf("request_log empty, want non-empty")
	}
	if strings.Contains(string(stub.pushed[0]), secretMarker) || strings.Contains(got.RequestLog, `{"input":`) || strings.Contains(got.RequestLog, `{"result":`) {
		t.Fatalf("home log leaked raw secret/body: %s", stub.pushed[0])
	}
	if !strings.Contains(got.RequestLog, "Set-Cookie: "+RedactedHeaderValue) {
		t.Fatalf("response Set-Cookie was not redacted: %s", got.RequestLog)
	}
}

func TestFileRequestLogger_LocalLogRedactsHeadersAndBodies(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	secret := "unique-local-log-secret"
	errLog := logger.LogRequest(
		"/v1/responses",
		http.MethodPost,
		map[string][]string{
			"Authorization":       {"Bearer " + secret},
			"Proxy-Authorization": {"Basic " + secret},
			"Cookie":              {"session=" + secret},
			"X-Api-Key":           {secret},
			"X-Access-Token":      {secret},
			"X-Client-Secret":     {secret},
			"X-Trace-ID":          {"request-trace-safe"},
		},
		[]byte(`{"input":"`+secret+`"}`),
		http.StatusOK,
		map[string][]string{"Set-Cookie": {"session=" + secret}, "X-Trace-ID": {"response-trace-safe"}},
		[]byte(`{"output":"`+secret+`"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		"local-redaction",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequest: %v", errLog)
	}

	entries, errReadDir := os.ReadDir(logsDir)
	if errReadDir != nil {
		t.Fatalf("ReadDir: %v", errReadDir)
	}
	var raw []byte
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var errRead error
		raw, errRead = os.ReadFile(filepath.Join(logsDir, entry.Name()))
		if errRead != nil {
			t.Fatalf("ReadFile: %v", errRead)
		}
		break
	}
	if len(raw) == 0 {
		t.Fatal("local request log missing")
	}
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte(`{"input":`)) || bytes.Contains(raw, []byte(`{"output":`)) {
		t.Fatalf("local log leaked raw secret/body: %s", raw)
	}
	if !bytes.Contains(raw, []byte("Authorization: "+RedactedHeaderValue)) || !bytes.Contains(raw, []byte("Set-Cookie: "+RedactedHeaderValue)) {
		t.Fatalf("local log did not redact sensitive headers: %s", raw)
	}
	if !bytes.Contains(raw, []byte("X-Trace-ID: request-trace-safe")) || !bytes.Contains(raw, []byte("X-Trace-ID: response-trace-safe")) {
		t.Fatalf("local log did not preserve harmless headers: %s", raw)
	}
}

func TestHomeStreamingLogWriter_StreamsSourceIntoMetadataAndCleansParts(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() { currentHomeRequestLogClient = original }()
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return &stubHomeRequestLogClient{heartbeatOK: false}
	}

	source, errSource := NewFileBodySourceInDir(t.TempDir(), "home-source")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	secretMarker := []byte("unique-home-source-secret")
	payload := append(bytes.Repeat([]byte("y"), homeAPISectionMaxBytes+1), secretMarker...)
	if errAppend := source.AppendBytes(payload); errAppend != nil {
		t.Fatalf("AppendBytes: %v", errAppend)
	}
	partPaths := source.Paths()

	writer := newHomeStreamingLogWriter("/v1/responses", http.MethodPost, nil, nil, "source-metadata")
	if errWrite := writer.WriteAPIResponseSource(source); errWrite != nil {
		t.Fatalf("WriteAPIResponseSource: %v", errWrite)
	}
	if got := len(writer.apiResponse); got > 4096 {
		t.Fatalf("api response metadata bytes = %d, want bounded metadata", got)
	}
	if !bytes.HasPrefix(writer.apiResponse, []byte(bodyMetadataPrefix)) || !bytes.Contains(writer.apiResponse, []byte(`"truncated":true`)) {
		t.Fatalf("source metadata missing truncation: %s", writer.apiResponse)
	}
	if bytes.Contains(writer.apiResponse, secretMarker) {
		t.Fatalf("source metadata leaked raw content: %s", writer.apiResponse)
	}
	assertFileBodySourceCleaned(t, partPaths)
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close: %v", errClose)
	}
}

func TestFileRequestLogger_LogRequestWithSourcesWritesLocalLogAndCleansParts(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)

	timelineSource, errSource := logger.NewFileBodySource("websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("logger.NewFileBodySource: %v", errSource)
	}
	if errAppend := timelineSource.AppendPart([]byte("Timestamp: 2026-05-25T12:00:00Z\nEvent: websocket.request\n{}")); errAppend != nil {
		t.Fatalf("AppendPart request: %v", errAppend)
	}
	if errAppend := timelineSource.AppendPart([]byte("Timestamp: 2026-05-25T12:00:01Z\nEvent: websocket.response\n{}")); errAppend != nil {
		t.Fatalf("AppendPart response: %v", errAppend)
	}
	partPaths := timelineSource.Paths()
	for _, path := range partPaths {
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("part path %s is not under logs dir %s", path, logsDir)
		}
	}

	errLog := logger.LogRequestWithOptionsAndSources(
		"/v1/responses/ws",
		http.MethodGet,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		http.StatusSwitchingProtocols,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		nil,
		timelineSource,
		nil,
		nil,
		nil,
		nil,
		nil,
		false,
		"ws-req-1",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndSources error: %v", errLog)
	}

	assertFileBodySourceCleaned(t, partPaths)

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	var logPath string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		logPath = logsDir + string(os.PathSeparator) + entry.Name()
		break
	}
	if logPath == "" {
		t.Fatal("expected local request log file")
	}
	raw, errReadLog := os.ReadFile(logPath)
	if errReadLog != nil {
		t.Fatalf("read log file: %v", errReadLog)
	}
	if !bytes.Contains(raw, []byte("=== WEBSOCKET TIMELINE ===")) {
		t.Fatalf("websocket timeline section missing: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(bodyMetadataPrefix)) || bytes.Contains(raw, []byte("Event: websocket.request")) || bytes.Contains(raw, []byte("Event: websocket.response")) {
		t.Fatalf("websocket source was not reduced to metadata: %s", string(raw))
	}
}

func TestFileRequestLogger_HomeEnabled_ForwardsSourceLogAndCleansParts(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	timelineSource, errSource := logger.NewFileBodySource("home-websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("logger.NewFileBodySource: %v", errSource)
	}
	if errAppend := timelineSource.AppendPart([]byte("Timestamp: 2026-05-25T12:00:00Z\nEvent: websocket.request\n{}")); errAppend != nil {
		t.Fatalf("AppendPart request: %v", errAppend)
	}
	partPaths := timelineSource.Paths()
	for _, path := range partPaths {
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("part path %s is not under logs dir %s", path, logsDir)
		}
	}

	errLog := logger.LogRequestWithOptionsAndSources(
		"/v1/responses/ws",
		http.MethodGet,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		http.StatusSwitchingProtocols,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		nil,
		timelineSource,
		nil,
		nil,
		nil,
		nil,
		nil,
		false,
		"home-ws-req-1",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndSources error: %v", errLog)
	}
	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		RequestID  string `json:"request_id"`
		RequestLog string `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.RequestID != "home-ws-req-1" {
		t.Fatalf("request_id = %q, want home-ws-req-1", got.RequestID)
	}
	if !strings.Contains(got.RequestLog, bodyMetadataPrefix) || strings.Contains(got.RequestLog, "Event: websocket.request") {
		t.Fatalf("forwarded websocket log was not reduced to metadata: %s", got.RequestLog)
	}
	assertFileBodySourceCleaned(t, partPaths)
}

func TestFileRequestLogger_HomeEnabled_ForwardsStreamingRequestID(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	writer, errLog := logger.LogStreamingRequest(
		"/v1/responses",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		"stream-req-1",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}

	if errStatus := writer.WriteStatus(http.StatusOK, map[string][]string{"Content-Type": {"text/event-stream"}}); errStatus != nil {
		t.Fatalf("WriteStatus error: %v", errStatus)
	}
	writer.WriteChunkAsync([]byte("data: ok\n\n"))
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error: %v", errClose)
	}

	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		RequestID  string `json:"request_id"`
		RequestLog string `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.RequestID != "stream-req-1" {
		t.Fatalf("request_id = %q, want stream-req-1", got.RequestID)
	}
	if got.RequestLog == "" {
		t.Fatalf("request_log empty, want non-empty")
	}
}

func TestHomeStreamingLogWriter_BoundsResponseWithSingleMarker(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient { return stub }

	writer := newHomeStreamingLogWriter("/v1/responses", http.MethodPost, nil, nil, "bounded-stream")
	chunk := bytes.Repeat([]byte("x"), homeStreamingChunkMaxBytes)
	for i := 0; i < homeStreamingResponseMaxBytes/homeStreamingChunkMaxBytes+homeStreamingChunkQueueCapacity+1; i++ {
		writer.WriteChunkAsync(chunk)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error: %v", errClose)
	}

	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var payload homeRequestLogPayload
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v", errUnmarshal)
	}
	if !strings.Contains(payload.RequestLog, bodyMetadataPrefix) || !strings.Contains(payload.RequestLog, `"truncated":true`) {
		t.Fatalf("streaming response metadata missing truncation: %s", payload.RequestLog)
	}
	if got := len(payload.RequestLog); got > homeRequestLogMaxBytes {
		t.Fatalf("home request log bytes = %d, limit = %d", got, homeRequestLogMaxBytes)
	}
}

func TestHomeStreamingLogWriter_BoundsStoredSections(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return &stubHomeRequestLogClient{heartbeatOK: false}
	}

	writer := newHomeStreamingLogWriter(
		"/v1/responses",
		http.MethodPost,
		nil,
		bytes.Repeat([]byte("r"), homeRequestBodyMaxBytes+1),
		"bounded-sections",
	)
	if errWrite := writer.WriteAPIRequest(bytes.Repeat([]byte("q"), homeAPISectionMaxBytes+1)); errWrite != nil {
		t.Fatalf("WriteAPIRequest: %v", errWrite)
	}
	if errWrite := writer.WriteAPIResponse(bytes.Repeat([]byte("s"), homeAPISectionMaxBytes+1)); errWrite != nil {
		t.Fatalf("WriteAPIResponse: %v", errWrite)
	}
	if errWrite := writer.WriteAPIWebsocketTimeline(bytes.Repeat([]byte("w"), homeAPISectionMaxBytes+1)); errWrite != nil {
		t.Fatalf("WriteAPIWebsocketTimeline: %v", errWrite)
	}

	checks := []struct {
		name    string
		payload []byte
	}{
		{name: "request body", payload: writer.requestBody},
		{name: "api request", payload: writer.apiRequest},
		{name: "api response", payload: writer.apiResponse},
		{name: "api websocket timeline", payload: writer.apiWebsocketTime},
	}
	for _, check := range checks {
		if got := len(check.payload); got > 4096 {
			t.Errorf("%s metadata bytes = %d, want bounded metadata", check.name, got)
		}
		if !bytes.HasPrefix(check.payload, []byte(bodyMetadataPrefix)) {
			t.Errorf("%s is not metadata-only: %q", check.name, check.payload)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error: %v", errClose)
	}
}

func TestHomeStreamingLogWriter_CloseStopsWriterWhenHomeBecomesUnhealthy(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return &stubHomeRequestLogClient{heartbeatOK: false}
	}

	writer := newHomeStreamingLogWriter("/v1/responses", http.MethodPost, nil, nil, "unhealthy-home")
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 100; j++ {
				writer.WriteChunkAsync([]byte("data: ok\n\n"))
			}
		}()
	}

	closed := make(chan error, 1)
	go func() { closed <- writer.Close() }()
	writers.Wait()
	select {
	case errClose := <-closed:
		if errClose != nil {
			t.Fatalf("Close error: %v", errClose)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the async writer")
	}
	select {
	case <-writer.doneChan:
	default:
		t.Fatal("async writer goroutine is still running after Close")
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("second Close error: %v", errClose)
	}
}

func TestFileStreamingLogWriter_DropsFullQueueBeforeCloning(t *testing.T) {
	writer := &FileStreamingLogWriter{chunkChan: make(chan []byte, 1)}
	writer.chunkChan <- []byte("occupied")
	chunk := make([]byte, 2<<20)

	before := internalpayload.CurrentLargeCloneMetrics()
	writer.WriteChunkAsync(chunk)
	after := internalpayload.CurrentLargeCloneMetrics()
	if after.Count != before.Count || after.Bytes != before.Bytes || after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("large clone metrics changed for dropped chunk: before=%+v after=%+v", before, after)
	}
}

func TestFileStreamingLogWriter_EnforcesChunkAndQueueByteLimits(t *testing.T) {
	writer := &FileStreamingLogWriter{chunkChan: make(chan []byte, 100)}
	chunk := bytes.Repeat([]byte("q"), streamingLogChunkMaxBytes+1)
	for i := 0; i < 100; i++ {
		writer.WriteChunkAsync(chunk)
	}

	writer.chunkMu.Lock()
	queuedBytes := writer.queuedBytes
	queuedCount := len(writer.chunkChan)
	writer.chunkMu.Unlock()
	if queuedBytes > streamingLogQueueMaxBytes {
		t.Fatalf("queued bytes = %d, budget = %d", queuedBytes, streamingLogQueueMaxBytes)
	}
	if queuedCount != streamingLogQueueMaxBytes/streamingLogChunkMaxBytes {
		t.Fatalf("queued chunks = %d, want %d", queuedCount, streamingLogQueueMaxBytes/streamingLogChunkMaxBytes)
	}
	if !writer.responseTruncated.Load() {
		t.Fatal("expected queue overflow to mark the stream truncated")
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close: %v", errClose)
	}
	writer.chunkMu.Lock()
	defer writer.chunkMu.Unlock()
	if writer.queuedBytes != 0 || writer.chunkChan != nil {
		t.Fatalf("queue retained after close: bytes=%d channel_nil=%t", writer.queuedBytes, writer.chunkChan == nil)
	}
}

func TestFileStreamingLogWriterTracksRetainedLargeClones(t *testing.T) {
	before := internalpayload.CurrentLargeCloneMetrics()
	writer := &FileStreamingLogWriter{}
	large := make([]byte, 1<<20)
	if err := writer.WriteAPIRequest(large); err != nil {
		t.Fatalf("WriteAPIRequest() error = %v", err)
	}
	if err := writer.WriteAPIResponse(large); err != nil {
		t.Fatalf("WriteAPIResponse() error = %v", err)
	}
	during := internalpayload.CurrentLargeCloneMetrics()
	if during.ActiveScopedCount != before.ActiveScopedCount || during.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("raw api bodies should be summarized before retention: before=%+v during=%+v", before, during)
	}

	if err := writer.WriteAPIRequest(large); err != nil {
		t.Fatalf("replace WriteAPIRequest() error = %v", err)
	}
	replaced := internalpayload.CurrentLargeCloneMetrics()
	if replaced.ActiveScopedCount != during.ActiveScopedCount || replaced.ActiveScopedBytes != during.ActiveScopedBytes {
		t.Fatalf("replacement leaked retained clone: during=%+v replaced=%+v", during, replaced)
	}

	writer.releaseScopedClones()
	after := internalpayload.CurrentLargeCloneMetrics()
	if after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("retained clones not released: before=%+v after=%+v", before, after)
	}
}

func TestFileRequestLogger_HomeEnabled_DoesNotForwardForcedErrorLogsWhenRequestLogDisabled(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	errLog := logger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"req-2",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions error: %v", errLog)
	}

	if len(stub.pushed) != 0 {
		t.Fatalf("home pushed records = %d, want 0", len(stub.pushed))
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Name() != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected local forced error log file when request-log disabled")
	}
}
