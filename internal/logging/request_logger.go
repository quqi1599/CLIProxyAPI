// Package logging provides request logging functionality for the CLI Proxy API server.
// It handles capturing and storing detailed HTTP request and response data when enabled
// through configuration, supporting both regular and streaming responses.
package logging

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

var requestLogID atomic.Uint64

const (
	WebsocketTimelineSourceContextKey    = "WEBSOCKET_TIMELINE_SOURCE"
	APIRequestSourceContextKey           = "API_REQUEST_SOURCE"
	APIResponseSourceContextKey          = "API_RESPONSE_SOURCE"
	APIResponseCapturedContextKey        = "API_RESPONSE_CAPTURED"
	APIWebsocketTimelineSourceContextKey = "API_WEBSOCKET_TIMELINE_SOURCE"
)

type homeRequestLogClient interface {
	HeartbeatOK() bool
	RPushRequestLog(ctx context.Context, payload []byte) error
}

var currentHomeRequestLogClient = func() homeRequestLogClient {
	return home.Current()
}

const (
	homeRequestLogMaxBytes          = 16 << 20
	homeRequestBodyMaxBytes         = 2 << 20
	homeAPISectionMaxBytes          = 4 << 20
	homeStreamingResponseMaxBytes   = 8 << 20
	homeStreamingChunkMaxBytes      = 64 << 10
	homeStreamingChunkQueueCapacity = 32
	decompressedLogResponseMaxBytes = 16 << 20
	fileBodySourceMaxBytes          = 16 << 20
	fileBodySourceMaxParts          = 256
	streamingLogChunkMaxBytes       = 64 << 10
	streamingLogQueueMaxBytes       = 2 << 20

	homeRequestLogTruncationMarker        = "\n[TRUNCATED HOME REQUEST LOG: size limit reached]\n"
	homeRequestBodyTruncationMarker       = "\n[TRUNCATED REQUEST BODY: size limit reached]\n"
	homeAPIRequestTruncationMarker        = "\n[TRUNCATED API REQUEST: size limit reached]\n"
	homeAPIResponseTruncationMarker       = "\n[TRUNCATED API RESPONSE: size limit reached]\n"
	homeWebsocketTruncationMarker         = "\n[TRUNCATED WEBSOCKET TIMELINE: size limit reached]\n"
	homeAPIWebsocketTruncationMarker      = "\n[TRUNCATED API WEBSOCKET TIMELINE: size limit reached]\n"
	homeStreamingResponseTruncationMarker = "\n[TRUNCATED STREAMING RESPONSE: size limit reached]\n"
	decompressedResponseTruncationMarker  = "\n[TRUNCATED DECOMPRESSED RESPONSE: size limit reached]\n"
	fileBodySourceTruncationMarker        = "\n[TRUNCATED FILE-BACKED LOG SOURCE: size limit reached]\n"
	streamingLogChunkTruncationMarker     = "\n[TRUNCATED STREAM CHUNK: size limit reached]\n"
	streamingLogQueueTruncationMarker     = "\n[TRUNCATED STREAM LOG QUEUE: byte limit reached]\n"
)

const (
	// RedactedHeaderValue is deliberately fixed so logs never retain a token prefix or suffix.
	RedactedHeaderValue = "[REDACTED]"
	bodyMetadataPrefix  = "[BODY METADATA v1] "
)

// BodyLogMetadata is the only body representation accepted by request logs.
type BodyLogMetadata struct {
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	Chunks      int64  `json:"chunks,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Truncated   bool   `json:"truncated"`
}

// EncodeBodyLogMetadata renders a small, validated metadata envelope without raw body bytes.
func EncodeBodyLogMetadata(metadata BodyLogMetadata) []byte {
	if metadata.Bytes < 0 {
		metadata.Bytes = 0
	}
	if metadata.Chunks < 0 {
		metadata.Chunks = 0
	}
	metadata.ContentType = sanitizeLogMetadataValue(metadata.ContentType, 128)
	if _, errDecode := hex.DecodeString(metadata.SHA256); errDecode != nil || len(metadata.SHA256) != sha256.Size*2 {
		metadata.SHA256 = hex.EncodeToString(sha256.New().Sum(nil))
	}
	raw, _ := json.Marshal(metadata)
	return append([]byte(bodyMetadataPrefix), raw...)
}

// SummarizeBodyForLog replaces raw body bytes with deterministic metadata.
func SummarizeBodyForLog(payload []byte, contentType string) []byte {
	digest := sha256.Sum256(payload)
	return EncodeBodyLogMetadata(BodyLogMetadata{
		Bytes:       int64(len(payload)),
		SHA256:      hex.EncodeToString(digest[:]),
		ContentType: contentType,
	})
}

func normalizeBodyLogMetadata(payload []byte, contentType string) []byte {
	if len(payload) <= 4096 && bytes.HasPrefix(payload, []byte(bodyMetadataPrefix)) {
		var metadata BodyLogMetadata
		decoder := json.NewDecoder(bytes.NewReader(payload[len(bodyMetadataPrefix):]))
		decoder.DisallowUnknownFields()
		if errDecode := decoder.Decode(&metadata); errDecode == nil {
			var extra any
			if errExtra := decoder.Decode(&extra); errExtra == io.EOF {
				if decoded, errDigest := hex.DecodeString(metadata.SHA256); errDigest == nil && len(decoded) == sha256.Size && metadata.Bytes >= 0 && metadata.Chunks >= 0 {
					return EncodeBodyLogMetadata(metadata)
				}
			}
		}
	}
	return SummarizeBodyForLog(payload, contentType)
}

func sanitizeLogMetadataValue(value string, maxBytes int) string {
	value = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
	if len(value) > maxBytes {
		value = value[:maxBytes]
	}
	return value
}

// RedactHeaderValue applies the single fail-safe header policy shared by every log sink.
func RedactHeaderValue(key, value string) string {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if normalized == "authorization" || normalized == "proxy-authorization" || normalized == "cookie" || normalized == "set-cookie" ||
		strings.Contains(normalized, "token") || strings.Contains(normalized, "key") || strings.Contains(normalized, "secret") {
		return RedactedHeaderValue
	}
	return value
}

func headerValue(headers map[string][]string, target string) string {
	for key, values := range headers {
		if strings.EqualFold(strings.TrimSpace(key), target) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// boundedLogBuffer consumes every write while retaining at most limit bytes.
// Once truncated, it emits exactly one marker and discards later writes.
type boundedLogBuffer struct {
	buf       bytes.Buffer
	limit     int
	marker    string
	truncated bool
}

func newBoundedLogBuffer(limit int, marker string) *boundedLogBuffer {
	return &boundedLogBuffer{limit: limit, marker: marker}
}

func (b *boundedLogBuffer) Write(payload []byte) (int, error) {
	originalLen := len(payload)
	if originalLen == 0 || b == nil || b.truncated {
		return originalLen, nil
	}

	dataLimit := b.limit - len(b.marker)
	if dataLimit < 0 {
		dataLimit = 0
	}
	remaining := dataLimit - b.buf.Len()
	if remaining > 0 {
		writeLen := min(remaining, originalLen)
		_, _ = b.buf.Write(payload[:writeLen])
		payload = payload[writeLen:]
	}
	if len(payload) > 0 {
		b.markTruncated()
	}
	return originalLen, nil
}

func (b *boundedLogBuffer) markTruncated() {
	if b == nil || b.truncated {
		return
	}
	dataLimit := b.limit - len(b.marker)
	if dataLimit < 0 {
		dataLimit = 0
	}
	if b.buf.Len() > dataLimit {
		b.buf.Truncate(dataLimit)
	}
	marker := b.marker
	if len(marker) > b.limit {
		marker = marker[:b.limit]
	}
	_, _ = b.buf.WriteString(marker)
	b.truncated = true
}

func (b *boundedLogBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf.Bytes()
}

func (b *boundedLogBuffer) String() string {
	if b == nil {
		return ""
	}
	return b.buf.String()
}

func truncateLogSection(payload []byte, limit int, marker string) []byte {
	if len(payload) <= limit {
		return payload
	}
	dataLimit := limit - len(marker)
	if dataLimit < 0 {
		dataLimit = 0
	}
	truncated := make([]byte, 0, limit)
	truncated = append(truncated, payload[:dataLimit]...)
	if len(marker) > limit {
		marker = marker[:limit]
	}
	return append(truncated, marker...)
}

func cloneBoundedLogSection(payload []byte, limit int, marker string) []byte {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) <= limit {
		return internalpayload.CloneBytes(payload)
	}
	dataLimit := limit - len(marker)
	if dataLimit < 0 {
		dataLimit = 0
	}
	truncated := make([]byte, 0, limit)
	truncated = append(truncated, payload[:dataLimit]...)
	if len(marker) > limit {
		marker = marker[:limit]
	}
	return append(truncated, marker...)
}

// FileBodySource stores large log sections as ordered temp-file parts.
type FileBodySource struct {
	mu           sync.Mutex
	dir          string
	paths        []string
	writtenBytes int64
	truncated    bool
	cleaned      bool
}

// FileBodyPart is an ordered, source-budgeted log part.
type FileBodyPart struct {
	mu     sync.Mutex
	source *FileBodySource
	file   *os.File
}

func (p *FileBodyPart) Name() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file == nil {
		return ""
	}
	return p.file.Name()
}

func (p *FileBodyPart) Write(payload []byte) (int, error) {
	if p == nil || len(payload) == 0 {
		return len(payload), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.source == nil || p.file == nil {
		return len(payload), nil
	}
	p.source.mu.Lock()
	defer p.source.mu.Unlock()
	if p.source.cleaned {
		return 0, fmt.Errorf("file body source has been cleaned")
	}
	return p.source.writeBoundedLocked(p.file, payload)
}

func (p *FileBodyPart) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file == nil {
		return nil
	}
	errClose := p.file.Close()
	p.file = nil
	return errClose
}

// NewFileBodySourceInDir creates a temp-backed source under baseDir.
func NewFileBodySourceInDir(baseDir string, prefix string) (*FileBodySource, error) {
	prefix = sanitizeTempPrefix(prefix)
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return nil, fmt.Errorf("base directory is required")
	}
	if errMkdir := os.MkdirAll(baseDir, 0755); errMkdir != nil {
		return nil, errMkdir
	}
	dir, errCreate := os.MkdirTemp(baseDir, "request-log-parts-"+prefix+"-*")
	if errCreate != nil {
		return nil, errCreate
	}
	return &FileBodySource{dir: dir}, nil
}

func sanitizeTempPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "log"
	}
	var builder strings.Builder
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-_")
	if out == "" {
		return "log"
	}
	return out
}

// CreatePart creates one ordered detail log part.
func (s *FileBodySource) CreatePart(prefix string) (*FileBodyPart, error) {
	if s == nil {
		return nil, fmt.Errorf("file body source is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return nil, fmt.Errorf("file body source has been cleaned")
	}
	if s.truncated || len(s.paths) >= fileBodySourceMaxParts {
		if !s.truncated {
			if errMark := s.markTruncatedLocked(); errMark != nil {
				return nil, errMark
			}
		}
		return &FileBodyPart{source: s}, nil
	}
	prefix = sanitizeTempPrefix(prefix)
	if errMkdir := os.MkdirAll(s.dir, 0755); errMkdir != nil {
		return nil, errMkdir
	}
	file, errCreate := os.CreateTemp(s.dir, prefix+"-*.tmp")
	if errCreate != nil {
		return nil, errCreate
	}
	s.paths = append(s.paths, file.Name())
	return &FileBodyPart{source: s, file: file}, nil
}

// AppendPart appends one complete ordered part to the source.
func (s *FileBodySource) AppendPart(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	file, errCreate := s.CreatePart("part")
	if errCreate != nil {
		return errCreate
	}
	writeErr := writeLogPart(file, data, false)
	if errClose := file.Close(); errClose != nil {
		if writeErr == nil {
			writeErr = errClose
		}
	}
	return writeErr
}

// AppendBytes appends raw bytes to a single ordered part.
func (s *FileBodySource) AppendBytes(data []byte) error {
	if s == nil {
		return fmt.Errorf("file body source is nil")
	}
	if len(data) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return fmt.Errorf("file body source has been cleaned")
	}
	if s.truncated {
		return nil
	}
	if errMkdir := os.MkdirAll(s.dir, 0755); errMkdir != nil {
		return errMkdir
	}

	var file *os.File
	var errOpen error
	if len(s.paths) == 0 {
		file, errOpen = os.CreateTemp(s.dir, "part-*.tmp")
		if errOpen == nil {
			s.paths = append(s.paths, file.Name())
		}
	} else {
		file, errOpen = os.OpenFile(s.paths[len(s.paths)-1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	}
	if errOpen != nil {
		return errOpen
	}

	_, writeErr := s.writeBoundedLocked(file, data)
	if errClose := file.Close(); errClose != nil {
		if writeErr == nil {
			writeErr = errClose
		}
	}
	return writeErr
}

func (s *FileBodySource) writeBoundedLocked(file *os.File, payload []byte) (int, error) {
	originalLen := len(payload)
	if originalLen == 0 || s.truncated {
		return originalLen, nil
	}
	dataLimit := int64(fileBodySourceMaxBytes - len(fileBodySourceTruncationMarker))
	remaining := dataLimit - s.writtenBytes
	if remaining < 0 {
		remaining = 0
	}
	writeLen := min(originalLen, int(remaining))
	if writeLen > 0 {
		written, errWrite := file.Write(payload[:writeLen])
		s.writtenBytes += int64(written)
		if errWrite != nil {
			return written, errWrite
		}
		if written != writeLen {
			return written, io.ErrShortWrite
		}
	}
	if writeLen < originalLen {
		if errMark := s.writeTruncationMarkerLocked(file); errMark != nil {
			return writeLen, errMark
		}
	}
	return originalLen, nil
}

func (s *FileBodySource) markTruncatedLocked() error {
	if s.truncated {
		return nil
	}
	if len(s.paths) == 0 {
		if errMkdir := os.MkdirAll(s.dir, 0755); errMkdir != nil {
			return errMkdir
		}
		file, errCreate := os.CreateTemp(s.dir, "truncated-*.tmp")
		if errCreate != nil {
			return errCreate
		}
		s.paths = append(s.paths, file.Name())
		errMark := s.writeTruncationMarkerLocked(file)
		if errClose := file.Close(); errClose != nil && errMark == nil {
			errMark = errClose
		}
		return errMark
	}
	file, errOpen := os.OpenFile(s.paths[len(s.paths)-1], os.O_WRONLY|os.O_APPEND, 0644)
	if errOpen != nil {
		return errOpen
	}
	errMark := s.writeTruncationMarkerLocked(file)
	if errClose := file.Close(); errClose != nil && errMark == nil {
		errMark = errClose
	}
	return errMark
}

func (s *FileBodySource) writeTruncationMarkerLocked(file *os.File) error {
	if s.truncated {
		return nil
	}
	marker := []byte(fileBodySourceTruncationMarker)
	remaining := int64(fileBodySourceMaxBytes) - s.writtenBytes
	if remaining < int64(len(marker)) {
		marker = marker[:max(0, int(remaining))]
	}
	if len(marker) > 0 {
		written, errWrite := file.Write(marker)
		s.writtenBytes += int64(written)
		if errWrite != nil {
			return errWrite
		}
		if written != len(marker) {
			return io.ErrShortWrite
		}
	}
	s.truncated = true
	return nil
}

// HasPayload reports whether any detail parts were recorded.
func (s *FileBodySource) HasPayload() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths) > 0 && !s.cleaned
}

// Paths returns a copy of the ordered part paths.
func (s *FileBodySource) Paths() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.paths))
	copy(out, s.paths)
	return out
}

// CopyTo merges all ordered parts into w.
func (s *FileBodySource) CopyTo(w io.Writer) error {
	return s.CopyToBounded(w, fileBodySourceMaxBytes, fileBodySourceTruncationMarker)
}

var errLogOutputLimit = fmt.Errorf("log output byte limit reached")

type boundedLogStreamWriter struct {
	writer    io.Writer
	limit     int
	marker    string
	written   int
	truncated bool
}

func (w *boundedLogStreamWriter) Write(payload []byte) (int, error) {
	originalLen := len(payload)
	if originalLen == 0 {
		return 0, nil
	}
	if w.truncated {
		return originalLen, errLogOutputLimit
	}
	dataLimit := max(0, w.limit-len(w.marker))
	remaining := max(0, dataLimit-w.written)
	writeLen := min(originalLen, remaining)
	if writeLen > 0 {
		written, errWrite := w.writer.Write(payload[:writeLen])
		w.written += written
		if errWrite != nil {
			return written, errWrite
		}
		if written != writeLen {
			return written, io.ErrShortWrite
		}
	}
	if writeLen < originalLen {
		marker := w.marker
		if len(marker) > w.limit-w.written {
			marker = marker[:max(0, w.limit-w.written)]
		}
		if marker != "" {
			written, errWrite := io.WriteString(w.writer, marker)
			w.written += written
			if errWrite != nil {
				return writeLen, errWrite
			}
		}
		w.truncated = true
		return originalLen, errLogOutputLimit
	}
	return originalLen, nil
}

// CopyToBounded streams ordered parts into w without materializing them in memory.
func (s *FileBodySource) CopyToBounded(w io.Writer, limit int, marker string) error {
	if s == nil || w == nil {
		return nil
	}
	if limit <= 0 {
		return nil
	}
	paths := s.Paths()
	bounded := &boundedLogStreamWriter{writer: w, limit: limit, marker: marker}
	wrote := false
	for _, path := range paths {
		file, errOpen := os.Open(path)
		if errOpen != nil {
			if os.IsNotExist(errOpen) {
				continue
			}
			return errOpen
		}
		if wrote {
			if _, errWrite := io.WriteString(bounded, "\n"); errWrite != nil {
				if errClose := file.Close(); errClose != nil {
					log.WithError(errClose).Warn("failed to close log part file")
				}
				if errWrite == errLogOutputLimit {
					return nil
				}
				return errWrite
			}
		}
		_, errCopy := io.Copy(bounded, file)
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close log part file")
			if errCopy == nil {
				errCopy = errClose
			}
		}
		if errCopy == errLogOutputLimit {
			return nil
		}
		if errCopy != nil {
			return errCopy
		}
		wrote = true
	}
	return nil
}

// Metadata streams the source through SHA-256 and returns no raw content.
func (s *FileBodySource) Metadata(contentType string) ([]byte, error) {
	digest := sha256.New()
	counter := &countingWriter{writer: digest}
	if errCopy := s.CopyTo(counter); errCopy != nil {
		return nil, errCopy
	}
	s.mu.Lock()
	truncated := s.truncated
	s.mu.Unlock()
	return EncodeBodyLogMetadata(BodyLogMetadata{
		Bytes:       counter.written,
		SHA256:      hex.EncodeToString(digest.Sum(nil)),
		ContentType: contentType,
		Truncated:   truncated,
	}), nil
}

type countingWriter struct {
	writer  hash.Hash
	written int64
}

func (w *countingWriter) Write(payload []byte) (int, error) {
	written, errWrite := w.writer.Write(payload)
	w.written += int64(written)
	return written, errWrite
}

// Bytes merges all ordered parts into memory.
func (s *FileBodySource) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if errWrite := s.CopyTo(&buf); errWrite != nil {
		return nil, errWrite
	}
	return buf.Bytes(), nil
}

// Cleanup removes all temp detail parts and their directory.
func (s *FileBodySource) Cleanup() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.cleaned {
		s.mu.Unlock()
		return nil
	}
	paths := make([]string, len(s.paths))
	copy(paths, s.paths)
	dir := s.dir
	s.paths = nil
	s.cleaned = true
	s.mu.Unlock()

	var firstErr error
	for _, path := range paths {
		if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) && firstErr == nil {
			firstErr = errRemove
		}
	}
	if dir != "" {
		if errRemove := os.RemoveAll(dir); errRemove != nil && firstErr == nil {
			firstErr = errRemove
		}
	}
	return firstErr
}

func cleanupFileBodySources(sources ...*FileBodySource) {
	for _, source := range sources {
		if source == nil {
			continue
		}
		if errCleanup := source.Cleanup(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up log part files")
		}
	}
}

// RequestLogger defines the interface for logging HTTP requests and responses.
// It provides methods for logging both regular and streaming HTTP request/response cycles.
type RequestLogger interface {
	// LogRequest logs a complete non-streaming request/response cycle.
	//
	// Parameters:
	//   - url: The request URL
	//   - method: The HTTP method
	//   - requestHeaders: The request headers
	//   - body: The request body
	//   - statusCode: The response status code
	//   - responseHeaders: The response headers
	//   - response: The raw response data
	//   - websocketTimeline: Optional downstream websocket event timeline
	//   - apiRequest: The API request data
	//   - apiResponse: The API response data
	//   - apiWebsocketTimeline: Optional upstream websocket event timeline
	//   - requestID: Optional request ID for log file naming
	//   - requestTimestamp: When the request was received
	//   - apiResponseTimestamp: When the API response was received
	//
	// Returns:
	//   - error: An error if logging fails, nil otherwise
	LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error

	// LogStreamingRequest initiates logging for a streaming request and returns a writer for chunks.
	//
	// Parameters:
	//   - url: The request URL
	//   - method: The HTTP method
	//   - headers: The request headers
	//   - body: The request body
	//   - requestID: Optional request ID for log file naming
	//
	// Returns:
	//   - StreamingLogWriter: A writer for streaming response chunks
	//   - error: An error if logging initialization fails, nil otherwise
	LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (StreamingLogWriter, error)

	// IsEnabled returns whether request logging is currently enabled.
	//
	// Returns:
	//   - bool: True if logging is enabled, false otherwise
	IsEnabled() bool
}

// StreamingLogWriter handles real-time logging of streaming response chunks.
// It provides methods for writing streaming response data asynchronously.
type StreamingLogWriter interface {
	// WriteChunkAsync writes a response chunk asynchronously (non-blocking).
	//
	// Parameters:
	//   - chunk: The response chunk to write
	WriteChunkAsync(chunk []byte)

	// WriteStatus writes the response status and headers to the log.
	//
	// Parameters:
	//   - status: The response status code
	//   - headers: The response headers
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteStatus(status int, headers map[string][]string) error

	// WriteAPIRequest writes the upstream API request details to the log.
	// This should be called before WriteStatus to maintain proper log ordering.
	//
	// Parameters:
	//   - apiRequest: The API request data (typically includes URL, headers, body sent upstream)
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteAPIRequest(apiRequest []byte) error

	// WriteAPIResponse writes the upstream API response details to the log.
	// This should be called after the streaming response is complete.
	//
	// Parameters:
	//   - apiResponse: The API response data
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteAPIResponse(apiResponse []byte) error

	// WriteAPIWebsocketTimeline writes the upstream websocket timeline to the log.
	// This should be called when upstream communication happened over websocket.
	//
	// Parameters:
	//   - apiWebsocketTimeline: The upstream websocket event timeline
	//
	// Returns:
	//   - error: An error if writing fails, nil otherwise
	WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error

	// SetFirstChunkTimestamp sets the TTFB timestamp captured when first chunk was received.
	//
	// Parameters:
	//   - timestamp: The time when first response chunk was received
	SetFirstChunkTimestamp(timestamp time.Time)

	// Close finalizes the log file and cleans up resources.
	//
	// Returns:
	//   - error: An error if closing fails, nil otherwise
	Close() error
}

// FileRequestLogger implements RequestLogger using file-based storage.
// It provides file-based logging functionality for HTTP requests and responses.
type FileRequestLogger struct {
	// enabled indicates whether request logging is currently enabled.
	enabled atomic.Bool

	// logsDir is the directory where log files are stored.
	logsDir string

	// errorLogsMaxFiles limits the number of error log files retained.
	errorLogsMaxFiles atomic.Int64

	homeEnabled atomic.Bool
}

type homeRequestLogPayload struct {
	Headers    map[string][]string `json:"headers,omitempty"`
	RequestID  string              `json:"request_id,omitempty"`
	RequestLog string              `json:"request_log,omitempty"`
}

func cloneHeaders(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if values == nil {
			out[key] = nil
			continue
		}
		copied := make([]string, len(values))
		for index, value := range values {
			copied[index] = RedactHeaderValue(key, value)
		}
		out[key] = copied
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (l *FileRequestLogger) forwardRequestLogToHome(ctx context.Context, headers map[string][]string, requestID string, logText string) error {
	if l == nil || !l.homeEnabled.Load() {
		return nil
	}
	client := currentHomeRequestLogClient()
	if client == nil || !client.HeartbeatOK() {
		return nil
	}
	if len(logText) > homeRequestLogMaxBytes {
		dataLimit := homeRequestLogMaxBytes - len(homeRequestLogTruncationMarker)
		logText = logText[:dataLimit] + homeRequestLogTruncationMarker
	}
	payload := homeRequestLogPayload{
		Headers:    cloneHeaders(headers),
		RequestID:  strings.TrimSpace(requestID),
		RequestLog: logText,
	}
	raw, errMarshal := json.Marshal(&payload)
	if errMarshal != nil {
		return errMarshal
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return client.RPushRequestLog(ctx, raw)
}

// NewFileRequestLogger creates a new file-based request logger.
//
// Parameters:
//   - enabled: Whether request logging should be enabled
//   - logsDir: The directory where log files should be stored (can be relative)
//   - configDir: The directory of the configuration file; when logsDir is
//     relative, it will be resolved relative to this directory
//   - errorLogsMaxFiles: Maximum number of error log files to retain (0 = no cleanup)
//
// Returns:
//   - *FileRequestLogger: A new file-based request logger instance
func NewFileRequestLogger(enabled bool, logsDir string, configDir string, errorLogsMaxFiles int) *FileRequestLogger {
	// Resolve logsDir relative to the configuration file directory when it's not absolute.
	if !filepath.IsAbs(logsDir) {
		// If configDir is provided, resolve logsDir relative to it.
		if configDir != "" {
			logsDir = filepath.Join(configDir, logsDir)
		}
	}
	logger := &FileRequestLogger{
		logsDir: logsDir,
	}
	logger.enabled.Store(enabled)
	logger.errorLogsMaxFiles.Store(int64(errorLogsMaxFiles))
	return logger
}

// SetHomeEnabled toggles home request-log forwarding.
// When enabled, request logs are not written to disk and are instead forwarded to home via Redis RESP.
func (l *FileRequestLogger) SetHomeEnabled(enabled bool) {
	if l == nil {
		return
	}
	l.homeEnabled.Store(enabled)
}

// IsEnabled returns whether request logging is currently enabled.
//
// Returns:
//   - bool: True if logging is enabled, false otherwise
func (l *FileRequestLogger) IsEnabled() bool {
	return l.enabled.Load()
}

// SetEnabled updates the request logging enabled state.
// This method allows dynamic enabling/disabling of request logging.
//
// Parameters:
//   - enabled: Whether request logging should be enabled
func (l *FileRequestLogger) SetEnabled(enabled bool) {
	l.enabled.Store(enabled)
}

// SetErrorLogsMaxFiles updates the maximum number of error log files to retain.
func (l *FileRequestLogger) SetErrorLogsMaxFiles(maxFiles int) {
	l.errorLogsMaxFiles.Store(int64(maxFiles))
}

// NewFileBodySource creates a temp-backed source under the request log directory.
func (l *FileRequestLogger) NewFileBodySource(prefix string) (*FileBodySource, error) {
	if l == nil {
		return nil, fmt.Errorf("file request logger is nil")
	}
	if errEnsure := l.ensureLogsDir(); errEnsure != nil {
		return nil, errEnsure
	}
	return NewFileBodySourceInDir(l.logsDir, prefix)
}

// LogRequest logs a complete non-streaming request/response cycle to a file.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - requestHeaders: The request headers
//   - body: The request body
//   - statusCode: The response status code
//   - responseHeaders: The response headers
//   - response: The raw response data
//   - apiRequest: The API request data
//   - apiResponse: The API response data
//   - requestID: Optional request ID for log file naming
//   - requestTimestamp: When the request was received
//   - apiResponseTimestamp: When the API response was received
//
// Returns:
//   - error: An error if logging fails, nil otherwise
func (l *FileRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, false, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptions logs a request with optional forced logging behavior.
// The force flag allows writing error logs even when regular request logging is disabled.
func (l *FileRequestLogger) LogRequestWithOptions(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

func (l *FileRequestLogger) logRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptionsAndSources logs a request with optional file-backed large sections.
func (l *FileRequestLogger) LogRequestWithOptionsAndSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, websocketTimelineSource, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptionsAndAllSources logs a request with optional file-backed request and response sections.
func (l *FileRequestLogger) LogRequestWithOptionsAndAllSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

func (l *FileRequestLogger) logRequestWithSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	defer cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource)

	if !l.enabled.Load() && !force {
		return nil
	}

	if l.homeEnabled.Load() && l.enabled.Load() {
		responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
		if decompressErr != nil {
			responseToWrite = response
		}

		body = truncateLogSection(body, homeRequestBodyMaxBytes, homeRequestBodyTruncationMarker)
		websocketTimeline = truncateLogSection(websocketTimeline, homeAPISectionMaxBytes, homeWebsocketTruncationMarker)
		apiRequest = truncateLogSection(apiRequest, homeAPISectionMaxBytes, homeAPIRequestTruncationMarker)
		apiResponse = truncateLogSection(apiResponse, homeAPISectionMaxBytes, homeAPIResponseTruncationMarker)
		apiWebsocketTimeline = truncateLogSection(apiWebsocketTimeline, homeAPISectionMaxBytes, homeAPIWebsocketTruncationMarker)
		responseToWrite = truncateLogSection(responseToWrite, homeStreamingResponseMaxBytes, homeStreamingResponseTruncationMarker)

		buf := newBoundedLogBuffer(homeRequestLogMaxBytes, homeRequestLogTruncationMarker)
		writeErr := l.writeNonStreamingLog(
			buf,
			url,
			method,
			requestHeaders,
			body,
			websocketTimeline,
			websocketTimelineSource,
			apiRequest,
			apiRequestSource,
			apiResponse,
			apiResponseSource,
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
			apiResponseErrors,
			statusCode,
			responseHeaders,
			responseToWrite,
			decompressErr,
			requestTimestamp,
			apiResponseTimestamp,
		)
		if writeErr != nil {
			return fmt.Errorf("failed to build request log content: %w", writeErr)
		}
		return l.forwardRequestLogToHome(context.Background(), requestHeaders, requestID, buf.String())
	}

	// Ensure logs directory exists
	if errEnsure := l.ensureLogsDir(); errEnsure != nil {
		return fmt.Errorf("failed to create logs directory: %w", errEnsure)
	}

	// Generate filename with request ID
	filename := l.generateFilename(url, requestID)
	if force && !l.enabled.Load() {
		filename = l.generateErrorFilename(url, requestID)
	}
	filePath := filepath.Join(l.logsDir, filename)

	logFile, errOpen := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if errOpen != nil {
		return fmt.Errorf("failed to create log file: %w", errOpen)
	}

	writeErr := l.writeNonStreamingLog(
		logFile,
		url,
		method,
		requestHeaders,
		body,
		websocketTimeline,
		websocketTimelineSource,
		apiRequest,
		apiRequestSource,
		apiResponse,
		apiResponseSource,
		apiWebsocketTimeline,
		apiWebsocketTimelineSource,
		apiResponseErrors,
		statusCode,
		responseHeaders,
		response,
		nil,
		requestTimestamp,
		apiResponseTimestamp,
	)
	if errClose := logFile.Close(); errClose != nil {
		log.WithError(errClose).Warn("failed to close request log file")
		if writeErr == nil {
			return errClose
		}
	}
	if writeErr != nil {
		return fmt.Errorf("failed to write log file: %w", writeErr)
	}

	if force && !l.enabled.Load() {
		if errCleanup := l.cleanupOldErrorLogs(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up old error logs")
		}
	}

	return nil
}

// LogStreamingRequest initiates logging for a streaming request.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - headers: The request headers
//   - body: The request body
//   - requestID: Optional request ID for log file naming
//
// Returns:
//   - StreamingLogWriter: A writer for streaming response chunks
//   - error: An error if logging initialization fails, nil otherwise
func (l *FileRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (StreamingLogWriter, error) {
	if !l.enabled.Load() {
		return &NoOpStreamingLogWriter{}, nil
	}

	if l.homeEnabled.Load() {
		client := currentHomeRequestLogClient()
		if client == nil || !client.HeartbeatOK() {
			return &NoOpStreamingLogWriter{}, nil
		}
		return newHomeStreamingLogWriter(url, method, headers, body, requestID), nil
	}

	// Ensure logs directory exists
	if err := l.ensureLogsDir(); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Generate filename with request ID
	filename := l.generateFilename(url, requestID)
	filePath := filepath.Join(l.logsDir, filename)

	requestHeaders := cloneHeaders(headers)

	// Create streaming writer
	writer := &FileStreamingLogWriter{
		logFilePath:    filePath,
		url:            url,
		method:         method,
		timestamp:      time.Now(),
		requestHeaders: requestHeaders,
		requestBody:    normalizeBodyLogMetadata(body, headerValue(headers, "Content-Type")),
		chunkChan:      make(chan []byte, 100), // Buffered channel for async writes
		closeChan:      make(chan struct{}),
		errorChan:      make(chan error, 1),
		closeResult:    make(chan struct{}),
		responseDigest: sha256.New(),
	}

	// Start async writer goroutine
	go writer.asyncWriter()

	return writer, nil
}

// generateErrorFilename creates a filename with an error prefix to differentiate forced error logs.
func (l *FileRequestLogger) generateErrorFilename(url string, requestID ...string) string {
	return fmt.Sprintf("error-%s", l.generateFilename(url, requestID...))
}

// ensureLogsDir creates the logs directory if it doesn't exist.
//
// Returns:
//   - error: An error if directory creation fails, nil otherwise
func (l *FileRequestLogger) ensureLogsDir() error {
	if _, err := os.Stat(l.logsDir); os.IsNotExist(err) {
		return os.MkdirAll(l.logsDir, 0755)
	}
	return nil
}

// generateFilename creates a sanitized filename from the URL path and current timestamp.
// Format: v1-responses-2025-12-23T195811-a1b2c3d4.log
//
// Parameters:
//   - url: The request URL
//   - requestID: Optional request ID to include in filename
//
// Returns:
//   - string: A sanitized filename for the log file
func (l *FileRequestLogger) generateFilename(url string, requestID ...string) string {
	// Extract path from URL
	path := url
	if strings.Contains(url, "?") {
		path = strings.Split(url, "?")[0]
	}

	// Remove leading slash
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}

	// Sanitize path for filename
	sanitized := l.sanitizeForFilename(path)

	// Add timestamp
	timestamp := time.Now().Format("2006-01-02T150405")

	// Use request ID if provided, otherwise use sequential ID
	var idPart string
	if len(requestID) > 0 && requestID[0] != "" {
		idPart = requestID[0]
	} else {
		id := requestLogID.Add(1)
		idPart = fmt.Sprintf("%d", id)
	}

	return fmt.Sprintf("%s-%s-%s.log", sanitized, timestamp, idPart)
}

// sanitizeForFilename replaces characters that are not safe for filenames.
//
// Parameters:
//   - path: The path to sanitize
//
// Returns:
//   - string: A sanitized filename
func (l *FileRequestLogger) sanitizeForFilename(path string) string {
	// Replace slashes with hyphens
	sanitized := strings.ReplaceAll(path, "/", "-")

	// Replace colons with hyphens
	sanitized = strings.ReplaceAll(sanitized, ":", "-")

	// Replace other problematic characters with hyphens
	reg := regexp.MustCompile(`[<>:"|?*\s]`)
	sanitized = reg.ReplaceAllString(sanitized, "-")

	// Remove multiple consecutive hyphens
	reg = regexp.MustCompile(`-+`)
	sanitized = reg.ReplaceAllString(sanitized, "-")

	// Remove leading/trailing hyphens
	sanitized = strings.Trim(sanitized, "-")

	// Handle empty result
	if sanitized == "" {
		sanitized = "root"
	}

	return sanitized
}

// cleanupOldErrorLogs keeps only the newest errorLogsMaxFiles forced error log files.
func (l *FileRequestLogger) cleanupOldErrorLogs() error {
	maxFiles := int(l.errorLogsMaxFiles.Load())
	if maxFiles <= 0 {
		return nil
	}

	entries, errRead := os.ReadDir(l.logsDir)
	if errRead != nil {
		return errRead
	}

	type logFile struct {
		name    string
		modTime time.Time
	}

	var files []logFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "error-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			log.WithError(errInfo).Warn("failed to read error log info")
			continue
		}
		files = append(files, logFile{name: name, modTime: info.ModTime()})
	}

	if len(files) <= maxFiles {
		return nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	for _, file := range files[maxFiles:] {
		if errRemove := os.Remove(filepath.Join(l.logsDir, file.name)); errRemove != nil {
			log.WithError(errRemove).Warnf("failed to remove old error log: %s", file.name)
		}
	}

	return nil
}

func (l *FileRequestLogger) writeNonStreamingLog(
	w io.Writer,
	url, method string,
	requestHeaders map[string][]string,
	requestBody []byte,
	websocketTimeline []byte,
	websocketTimelineSource *FileBodySource,
	apiRequest []byte,
	apiRequestSource *FileBodySource,
	apiResponse []byte,
	apiResponseSource *FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *FileBodySource,
	apiResponseErrors []*interfaces.ErrorMessage,
	statusCode int,
	responseHeaders map[string][]string,
	response []byte,
	decompressErr error,
	requestTimestamp time.Time,
	apiResponseTimestamp time.Time,
) error {
	if requestTimestamp.IsZero() {
		requestTimestamp = time.Now()
	}
	isWebsocketTranscript := hasSectionPayload(websocketTimeline) || hasFileBodySourcePayload(websocketTimelineSource)
	downstreamTransport := inferDownstreamTransport(requestHeaders, websocketTimeline, websocketTimelineSource)
	upstreamTransport := inferUpstreamTransport(apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors)
	if errWrite := writeRequestInfoWithBody(w, url, method, requestHeaders, requestBody, requestTimestamp, downstreamTransport, upstreamTransport, !isWebsocketTranscript); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(w, "=== WEBSOCKET TIMELINE ===\n", "=== WEBSOCKET TIMELINE", websocketTimeline, websocketTimelineSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(w, "=== API WEBSOCKET TIMELINE ===\n", "=== API WEBSOCKET TIMELINE", apiWebsocketTimeline, apiWebsocketTimelineSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(w, "=== API REQUEST ===\n", "=== API REQUEST", apiRequest, apiRequestSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPIErrorResponses(w, apiResponseErrors); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(w, "=== API RESPONSE ===\n", "=== API RESPONSE", apiResponse, apiResponseSource, apiResponseTimestamp); errWrite != nil {
		return errWrite
	}
	if isWebsocketTranscript {
		// Intentionally omit the generic downstream HTTP response section for websocket
		// transcripts. The durable session exchange is captured in WEBSOCKET TIMELINE,
		// and appending a one-off upgrade response snapshot would dilute that transcript.
		return nil
	}
	return writeResponseSection(w, statusCode, true, responseHeaders, response, decompressErr, true)
}

func writeRequestInfoWithBody(
	w io.Writer,
	url, method string,
	headers map[string][]string,
	body []byte,
	timestamp time.Time,
	downstreamTransport string,
	upstreamTransport string,
	includeBody bool,
) error {
	if _, errWrite := io.WriteString(w, "=== REQUEST INFO ===\n"); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Version: %s\n", buildinfo.Version)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("URL: %s\n", url)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Method: %s\n", method)); errWrite != nil {
		return errWrite
	}
	if strings.TrimSpace(downstreamTransport) != "" {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Downstream Transport: %s\n", downstreamTransport)); errWrite != nil {
			return errWrite
		}
	}
	if strings.TrimSpace(upstreamTransport) != "" {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Upstream Transport: %s\n", upstreamTransport)); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
		return errWrite
	}
	if errWrite := writeSectionSpacing(w, 1); errWrite != nil {
		return errWrite
	}

	if _, errWrite := io.WriteString(w, "=== HEADERS ===\n"); errWrite != nil {
		return errWrite
	}
	for key, values := range headers {
		for _, value := range values {
			masked := RedactHeaderValue(key, value)
			if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, masked)); errWrite != nil {
				return errWrite
			}
		}
	}
	if errWrite := writeSectionSpacing(w, 1); errWrite != nil {
		return errWrite
	}

	if !includeBody {
		return nil
	}

	if _, errWrite := io.WriteString(w, "=== REQUEST BODY ===\n"); errWrite != nil {
		return errWrite
	}

	bodyMetadata := normalizeBodyLogMetadata(body, headerValue(headers, "Content-Type"))
	if _, errWrite := w.Write(bodyMetadata); errWrite != nil {
		return errWrite
	}
	if errWrite := writeSectionSpacing(w, countTrailingNewlinesBytes(bodyMetadata)); errWrite != nil {
		return errWrite
	}
	return nil
}

func countTrailingNewlinesBytes(payload []byte) int {
	count := 0
	for i := len(payload) - 1; i >= 0; i-- {
		if payload[i] != '\n' {
			break
		}
		count++
	}
	return count
}

func writeSectionSpacing(w io.Writer, trailingNewlines int) error {
	missingNewlines := 3 - trailingNewlines
	if missingNewlines <= 0 {
		return nil
	}
	_, errWrite := io.WriteString(w, strings.Repeat("\n", missingNewlines))
	return errWrite
}

type trailingNewlineTrackingWriter struct {
	writer           io.Writer
	trailingNewlines int
}

func (t *trailingNewlineTrackingWriter) Write(payload []byte) (int, error) {
	written, errWrite := t.writer.Write(payload)
	if written > 0 {
		writtenPayload := payload[:written]
		trailingNewlines := countTrailingNewlinesBytes(writtenPayload)
		if trailingNewlines == len(writtenPayload) {
			t.trailingNewlines += trailingNewlines
		} else {
			t.trailingNewlines = trailingNewlines
		}
	}
	return written, errWrite
}

func hasSectionPayload(payload []byte) bool {
	return len(bytes.TrimSpace(payload)) > 0
}

func hasFileBodySourcePayload(source *FileBodySource) bool {
	return source != nil && source.HasPayload()
}

func inferDownstreamTransport(headers map[string][]string, websocketTimeline []byte, websocketTimelineSource *FileBodySource) string {
	if hasSectionPayload(websocketTimeline) || hasFileBodySourcePayload(websocketTimelineSource) {
		return "websocket"
	}
	for key, values := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "Upgrade") {
			for _, value := range values {
				if strings.EqualFold(strings.TrimSpace(value), "websocket") {
					return "websocket"
				}
			}
		}
	}
	return "http"
}

func inferUpstreamTransport(apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, _ []*interfaces.ErrorMessage) string {
	hasHTTP := hasSectionPayload(apiRequest) || hasFileBodySourcePayload(apiRequestSource) || hasSectionPayload(apiResponse) || hasFileBodySourcePayload(apiResponseSource)
	hasWS := hasSectionPayload(apiWebsocketTimeline) || hasFileBodySourcePayload(apiWebsocketTimelineSource)
	switch {
	case hasHTTP && hasWS:
		return "websocket+http"
	case hasWS:
		return "websocket"
	case hasHTTP:
		return "http"
	default:
		return ""
	}
}

func writeLogPart(w io.Writer, payload []byte, prependNewline bool) error {
	if w == nil {
		return nil
	}
	if prependNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := w.Write(payload); errWrite != nil {
		return errWrite
	}
	if !bytes.HasSuffix(payload, []byte("\n")) {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeAPISection(w io.Writer, sectionHeader string, sectionPrefix string, payload []byte, timestamp time.Time) error {
	if len(payload) == 0 {
		return nil
	}

	_ = sectionPrefix
	if _, errWrite := io.WriteString(w, sectionHeader); errWrite != nil {
		return errWrite
	}
	if !timestamp.IsZero() {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
			return errWrite
		}
	}
	metadata := normalizeBodyLogMetadata(payload, "text/plain")
	if _, errWrite := w.Write(metadata); errWrite != nil {
		return errWrite
	}

	if errWrite := writeSectionSpacing(w, countTrailingNewlinesBytes(metadata)); errWrite != nil {
		return errWrite
	}
	return nil
}

func writeAPISectionWithSource(w io.Writer, sectionHeader string, sectionPrefix string, payload []byte, source *FileBodySource, timestamp time.Time) error {
	if !hasFileBodySourcePayload(source) {
		return writeAPISection(w, sectionHeader, sectionPrefix, payload, timestamp)
	}
	if _, errWrite := io.WriteString(w, sectionHeader); errWrite != nil {
		return errWrite
	}
	if !timestamp.IsZero() {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
			return errWrite
		}
	}
	if len(payload) > 0 {
		if _, errWrite := io.WriteString(w, "Memory: "); errWrite != nil {
			return errWrite
		}
		if _, errWrite := w.Write(normalizeBodyLogMetadata(payload, "text/plain")); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	metadata, errMetadata := source.Metadata("text/plain")
	if errMetadata != nil {
		return errMetadata
	}
	if _, errWrite := io.WriteString(w, "Source: "); errWrite != nil {
		return errWrite
	}
	if _, errWrite := w.Write(metadata); errWrite != nil {
		return errWrite
	}
	if errWrite := writeSectionSpacing(w, countTrailingNewlinesBytes(metadata)); errWrite != nil {
		return errWrite
	}
	return nil
}

func writePreformattedAPISectionWithSource(w io.Writer, sectionHeader string, sectionPrefix string, payload []byte, source *FileBodySource, timestamp time.Time) error {
	return writeAPISectionWithSource(w, sectionHeader, sectionPrefix, payload, source, timestamp)
}

func writeAPIErrorResponses(w io.Writer, apiResponseErrors []*interfaces.ErrorMessage) error {
	for i := 0; i < len(apiResponseErrors); i++ {
		if apiResponseErrors[i] == nil {
			continue
		}
		if _, errWrite := io.WriteString(w, "=== API ERROR RESPONSE ===\n"); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(w, fmt.Sprintf("HTTP Status: %d\n", apiResponseErrors[i].StatusCode)); errWrite != nil {
			return errWrite
		}
		trailingNewlines := 1
		if apiResponseErrors[i].Error != nil {
			errText := "upstream request failed"
			if _, errWrite := io.WriteString(w, errText); errWrite != nil {
				return errWrite
			}
			if errText != "" {
				trailingNewlines = countTrailingNewlinesBytes([]byte(errText))
			}
		}
		if errWrite := writeSectionSpacing(w, trailingNewlines); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeResponseSection(w io.Writer, statusCode int, statusWritten bool, responseHeaders map[string][]string, response []byte, decompressErr error, trailingNewline bool) error {
	if _, errWrite := io.WriteString(w, "=== RESPONSE ===\n"); errWrite != nil {
		return errWrite
	}
	if statusWritten {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Status: %d\n", statusCode)); errWrite != nil {
			return errWrite
		}
	}

	if responseHeaders != nil {
		for key, values := range responseHeaders {
			for _, value := range values {
				if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, RedactHeaderValue(key, value))); errWrite != nil {
					return errWrite
				}
			}
		}
	}

	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}
	metadata := normalizeBodyLogMetadata(response, headerValue(responseHeaders, "Content-Type"))
	if _, errWrite := w.Write(metadata); errWrite != nil {
		return errWrite
	}
	if decompressErr != nil {
		if _, errWrite := io.WriteString(w, "\ndecompression_failed=true"); errWrite != nil {
			return errWrite
		}
	}

	if trailingNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func responseBodyStartsWithLeadingNewline(reader *bufio.Reader) bool {
	if reader == nil {
		return false
	}
	if peeked, _ := reader.Peek(2); len(peeked) >= 2 && peeked[0] == '\r' && peeked[1] == '\n' {
		return true
	}
	if peeked, _ := reader.Peek(1); len(peeked) >= 1 && peeked[0] == '\n' {
		return true
	}
	return false
}

// decompressResponse decompresses response data based on Content-Encoding header.
//
// Parameters:
//   - responseHeaders: The response headers
//   - response: The response data to decompress
//
// Returns:
//   - []byte: The decompressed response data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressResponse(responseHeaders map[string][]string, response []byte) ([]byte, error) {
	if responseHeaders == nil || len(response) == 0 {
		return response, nil
	}

	// Check Content-Encoding header
	var contentEncoding string
	for key, values := range responseHeaders {
		if strings.ToLower(key) == "content-encoding" && len(values) > 0 {
			contentEncoding = strings.ToLower(values[0])
			break
		}
	}

	switch contentEncoding {
	case "gzip":
		return l.decompressGzip(response)
	case "deflate":
		return l.decompressDeflate(response)
	case "br":
		return l.decompressBrotli(response)
	case "zstd":
		return l.decompressZstd(response)
	default:
		// No compression or unsupported compression
		return response, nil
	}
}

// decompressGzip decompresses gzip-encoded data.
//
// Parameters:
//   - data: The gzip-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressGzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		if errClose := reader.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close gzip reader in request logger")
		}
	}()

	decompressed, err := readDecompressedLogResponse(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress gzip data: %w", err)
	}

	return decompressed, nil
}

// decompressDeflate decompresses deflate-encoded data.
//
// Parameters:
//   - data: The deflate-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressDeflate(data []byte) ([]byte, error) {
	reader := flate.NewReader(bytes.NewReader(data))
	defer func() {
		if errClose := reader.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close deflate reader in request logger")
		}
	}()

	decompressed, err := readDecompressedLogResponse(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress deflate data: %w", err)
	}

	return decompressed, nil
}

// decompressBrotli decompresses brotli-encoded data.
//
// Parameters:
//   - data: The brotli-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressBrotli(data []byte) ([]byte, error) {
	reader := brotli.NewReader(bytes.NewReader(data))

	decompressed, err := readDecompressedLogResponse(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress brotli data: %w", err)
	}

	return decompressed, nil
}

// decompressZstd decompresses zstd-encoded data.
//
// Parameters:
//   - data: The zstd-encoded data to decompress
//
// Returns:
//   - []byte: The decompressed data
//   - error: An error if decompression fails, nil otherwise
func (l *FileRequestLogger) decompressZstd(data []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer decoder.Close()

	decompressed, err := readDecompressedLogResponse(decoder)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress zstd data: %w", err)
	}

	return decompressed, nil
}

func readDecompressedLogResponse(reader io.Reader) ([]byte, error) {
	decompressed, errRead := io.ReadAll(io.LimitReader(reader, decompressedLogResponseMaxBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	return truncateLogSection(decompressed, decompressedLogResponseMaxBytes, decompressedResponseTruncationMarker), nil
}

// FileStreamingLogWriter implements StreamingLogWriter for file-based streaming logs.
// It spools streaming response chunks to a temporary file to avoid retaining large responses in memory.
// The final log file is assembled when Close is called.
type FileStreamingLogWriter struct {
	// logFilePath is the final log file path.
	logFilePath string

	// url is the request URL (masked upstream in middleware).
	url string

	// method is the HTTP method.
	method string

	// timestamp is captured when the streaming log is initialized.
	timestamp time.Time

	// requestHeaders stores the request headers.
	requestHeaders map[string][]string

	// requestBody stores only the validated metadata envelope.
	requestBody []byte

	// chunkChan is a channel for receiving response chunks to spool.
	chunkChan   chan []byte
	chunkMu     sync.Mutex
	queuedBytes int
	closed      bool
	closeOnce   sync.Once
	closeResult chan struct{}
	closeErr    error

	// closeChan is a channel for signaling when the writer is closed.
	closeChan chan struct{}

	// errorChan is a channel for reporting errors during writing.
	errorChan chan error

	responseDigest        hash.Hash
	responseObservedBytes atomic.Int64
	responseCapturedBytes int64
	responseChunks        int64
	responseTruncated     atomic.Bool

	// responseStatus stores the HTTP status code.
	responseStatus int

	// statusWritten indicates whether a non-zero status was recorded.
	statusWritten bool

	// responseHeaders stores the response headers.
	responseHeaders map[string][]string

	// apiRequest stores the upstream API request data.
	apiRequest        []byte
	apiRequestRelease func()

	// apiRequestSource stores file-backed upstream API request data.
	apiRequestSource *FileBodySource

	// apiResponse stores the upstream API response data.
	apiResponse        []byte
	apiResponseRelease func()

	// apiResponseSource stores file-backed upstream API response data.
	apiResponseSource *FileBodySource

	// apiWebsocketTimeline stores the upstream websocket event timeline.
	apiWebsocketTimeline        []byte
	apiWebsocketTimelineRelease func()
	apiWebsocketTimelineSource  *FileBodySource

	// apiResponseTimestamp captures when the API response was received.
	apiResponseTimestamp time.Time
}

// WriteChunkAsync writes a response chunk asynchronously (non-blocking).
//
// Parameters:
//   - chunk: The response chunk to write
func (w *FileStreamingLogWriter) WriteChunkAsync(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	w.responseObservedBytes.Add(int64(len(chunk)))
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	chunkLen := min(len(chunk), streamingLogChunkMaxBytes)
	if len(chunk) > streamingLogChunkMaxBytes {
		chunkLen = streamingLogChunkMaxBytes
		w.responseTruncated.Store(true)
	}
	if w.closed || w.chunkChan == nil || len(w.chunkChan) >= cap(w.chunkChan) || w.queuedBytes+chunkLen > streamingLogQueueMaxBytes {
		w.responseTruncated.Store(true)
		return
	}
	queued := cloneBoundedStreamingChunk(chunk)
	w.queuedBytes += len(queued)
	w.chunkChan <- queued
}

func cloneBoundedStreamingChunk(chunk []byte) []byte {
	if len(chunk) <= streamingLogChunkMaxBytes {
		return internalpayload.CloneBytes(chunk)
	}
	dataLimit := max(0, streamingLogChunkMaxBytes-len(streamingLogChunkTruncationMarker))
	cloned := make([]byte, 0, streamingLogChunkMaxBytes)
	cloned = append(cloned, chunk[:dataLimit]...)
	return append(cloned, streamingLogChunkTruncationMarker...)
}

// WriteStatus buffers the response status and headers for later writing.
//
// Parameters:
//   - status: The response status code
//   - headers: The response headers
//
// Returns:
//   - error: Always returns nil (buffering cannot fail)
func (w *FileStreamingLogWriter) WriteStatus(status int, headers map[string][]string) error {
	if status == 0 {
		return nil
	}

	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if w.closed {
		return nil
	}
	w.responseStatus = status
	w.responseHeaders = cloneHeaders(headers)
	w.statusWritten = true
	return nil
}

// WriteAPIRequest buffers the upstream API request details for later writing.
//
// Parameters:
//   - apiRequest: The API request data (typically includes URL, headers, body sent upstream)
//
// Returns:
//   - error: Always returns nil (buffering cannot fail)
func (w *FileStreamingLogWriter) WriteAPIRequest(apiRequest []byte) error {
	if len(apiRequest) == 0 {
		return nil
	}
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if w.closed {
		return nil
	}
	w.replaceScopedClone(&w.apiRequest, &w.apiRequestRelease, normalizeBodyLogMetadata(apiRequest, "text/plain"), "logging.file.api_request")
	return nil
}

// WriteAPIRequestSource buffers a file-backed upstream API request for final writing.
func (w *FileStreamingLogWriter) WriteAPIRequestSource(apiRequestSource *FileBodySource) error {
	if apiRequestSource == nil || !apiRequestSource.HasPayload() {
		return nil
	}
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if !w.closed {
		w.apiRequestSource = apiRequestSource
	}
	return nil
}

// WriteAPIResponse buffers the upstream API response details for later writing.
//
// Parameters:
//   - apiResponse: The API response data
//
// Returns:
//   - error: Always returns nil (buffering cannot fail)
func (w *FileStreamingLogWriter) WriteAPIResponse(apiResponse []byte) error {
	if len(apiResponse) == 0 {
		return nil
	}
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if w.closed {
		return nil
	}
	w.replaceScopedClone(&w.apiResponse, &w.apiResponseRelease, normalizeBodyLogMetadata(apiResponse, "text/plain"), "logging.file.api_response")
	return nil
}

// WriteAPIResponseSource buffers a file-backed upstream API response for final writing.
func (w *FileStreamingLogWriter) WriteAPIResponseSource(apiResponseSource *FileBodySource) error {
	if apiResponseSource == nil || !apiResponseSource.HasPayload() {
		return nil
	}
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if !w.closed {
		w.apiResponseSource = apiResponseSource
	}
	return nil
}

// WriteAPIWebsocketTimeline buffers the upstream websocket timeline for later writing.
//
// Parameters:
//   - apiWebsocketTimeline: The upstream websocket event timeline
//
// Returns:
//   - error: Always returns nil (buffering cannot fail)
func (w *FileStreamingLogWriter) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	if len(apiWebsocketTimeline) == 0 {
		return nil
	}
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if w.closed {
		return nil
	}
	w.replaceScopedClone(&w.apiWebsocketTimeline, &w.apiWebsocketTimelineRelease, normalizeBodyLogMetadata(apiWebsocketTimeline, "text/plain"), "logging.file.websocket_timeline")
	return nil
}

func (w *FileStreamingLogWriter) WriteAPIWebsocketTimelineSource(source *FileBodySource) error {
	if source == nil || !source.HasPayload() {
		return nil
	}
	w.chunkMu.Lock()
	defer w.chunkMu.Unlock()
	if !w.closed {
		w.apiWebsocketTimelineSource = source
	}
	return nil
}

func (w *FileStreamingLogWriter) replaceScopedClone(destination *[]byte, release *func(), source []byte, hotspot string) {
	if release != nil && *release != nil {
		(*release)()
	}
	cloned, releaseClone := internalpayload.CloneBytesScoped(source, hotspot)
	*destination = cloned
	*release = releaseClone
}

func (w *FileStreamingLogWriter) releaseScopedClones() {
	for _, release := range []func(){w.apiRequestRelease, w.apiResponseRelease, w.apiWebsocketTimelineRelease} {
		if release != nil {
			release()
		}
	}
	w.apiRequestRelease = nil
	w.apiResponseRelease = nil
	w.apiWebsocketTimelineRelease = nil
	w.apiRequest = nil
	w.apiResponse = nil
	w.apiWebsocketTimeline = nil
}

func (w *FileStreamingLogWriter) SetFirstChunkTimestamp(timestamp time.Time) {
	if !timestamp.IsZero() {
		w.chunkMu.Lock()
		defer w.chunkMu.Unlock()
		if w.closed {
			return
		}
		w.apiResponseTimestamp = timestamp
	}
}

// Close finalizes the log file and cleans up resources.
// It writes all buffered data to the file in the correct order:
// API WEBSOCKET TIMELINE -> API REQUEST -> API RESPONSE -> RESPONSE (status, headers, body chunks)
//
// Returns:
//   - error: An error if closing fails, nil otherwise
func (w *FileStreamingLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.chunkMu.Lock()
	if w.closeResult == nil {
		w.closeResult = make(chan struct{})
	}
	closeResult := w.closeResult
	w.chunkMu.Unlock()
	w.closeOnce.Do(func() {
		w.closeErr = w.close()
		close(closeResult)
	})
	<-closeResult
	return w.closeErr
}

func (w *FileStreamingLogWriter) close() error {
	defer w.releaseScopedClones()
	w.chunkMu.Lock()
	if !w.closed && w.chunkChan != nil {
		w.closed = true
		close(w.chunkChan)
	}
	closeChan := w.closeChan
	chunkChan := w.chunkChan
	w.chunkMu.Unlock()

	// Wait for async writer to finish spooling chunks
	if closeChan != nil {
		<-closeChan
	} else if chunkChan != nil {
		for chunk := range chunkChan {
			w.releaseQueuedChunk(len(chunk))
		}
	}
	w.chunkMu.Lock()
	w.chunkChan = nil
	w.chunkMu.Unlock()

	select {
	case errWrite := <-w.errorChan:
		w.cleanupTempFiles()
		return errWrite
	default:
	}

	if w.logFilePath == "" {
		w.cleanupTempFiles()
		return nil
	}

	logFile, errOpen := os.OpenFile(w.logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if errOpen != nil {
		w.cleanupTempFiles()
		return fmt.Errorf("failed to create log file: %w", errOpen)
	}

	writeErr := w.writeFinalLog(logFile)
	if errClose := logFile.Close(); errClose != nil {
		log.WithError(errClose).Warn("failed to close request log file")
		if writeErr == nil {
			writeErr = errClose
		}
	}

	w.cleanupTempFiles()
	return writeErr
}

// asyncWriter runs in a goroutine to buffer chunks from the channel.
// It continuously reads chunks from the channel and appends them to a temp file for later assembly.
func (w *FileStreamingLogWriter) asyncWriter() {
	defer close(w.closeChan)

	for chunk := range w.chunkChan {
		w.releaseQueuedChunk(len(chunk))
		if w.responseDigest == nil {
			w.responseDigest = sha256.New()
		}
		_, _ = w.responseDigest.Write(chunk)
		w.responseCapturedBytes += int64(len(chunk))
		w.responseChunks++
	}
}

func (w *FileStreamingLogWriter) releaseQueuedChunk(size int) {
	w.chunkMu.Lock()
	w.queuedBytes -= size
	if w.queuedBytes < 0 {
		w.queuedBytes = 0
	}
	w.chunkMu.Unlock()
}

func (w *FileStreamingLogWriter) writeFinalLog(logFile *os.File) error {
	if errWrite := writeRequestInfoWithBody(logFile, w.url, w.method, w.requestHeaders, w.requestBody, w.timestamp, "http", inferUpstreamTransport(w.apiRequest, w.apiRequestSource, w.apiResponse, w.apiResponseSource, w.apiWebsocketTimeline, w.apiWebsocketTimelineSource, nil), true); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(logFile, "=== API WEBSOCKET TIMELINE ===\n", "=== API WEBSOCKET TIMELINE", w.apiWebsocketTimeline, w.apiWebsocketTimelineSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(logFile, "=== API REQUEST ===\n", "=== API REQUEST", w.apiRequest, w.apiRequestSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(logFile, "=== API RESPONSE ===\n", "=== API RESPONSE", w.apiResponse, w.apiResponseSource, w.apiResponseTimestamp); errWrite != nil {
		return errWrite
	}

	digest := sha256.New()
	if w.responseDigest != nil {
		digest = w.responseDigest
	}
	metadata := EncodeBodyLogMetadata(BodyLogMetadata{
		Bytes:       w.responseObservedBytes.Load(),
		SHA256:      hex.EncodeToString(digest.Sum(nil)),
		Chunks:      w.responseChunks,
		ContentType: headerValue(w.responseHeaders, "Content-Type"),
		Truncated:   w.responseTruncated.Load() || w.responseCapturedBytes != w.responseObservedBytes.Load(),
	})
	return writeResponseSection(logFile, w.responseStatus, w.statusWritten, w.responseHeaders, metadata, nil, false)
}

func (w *FileStreamingLogWriter) cleanupTempFiles() {
	cleanupFileBodySources(w.apiRequestSource, w.apiResponseSource, w.apiWebsocketTimelineSource)
	w.apiRequestSource = nil
	w.apiResponseSource = nil
	w.apiWebsocketTimelineSource = nil
	w.requestBody = nil
}

// NoOpStreamingLogWriter is a no-operation implementation for when logging is disabled.
// It implements the StreamingLogWriter interface but performs no actual logging operations.
type NoOpStreamingLogWriter struct{}

// WriteChunkAsync is a no-op implementation that does nothing.
//
// Parameters:
//   - chunk: The response chunk (ignored)
func (w *NoOpStreamingLogWriter) WriteChunkAsync(_ []byte) {}

// WriteStatus is a no-op implementation that does nothing and always returns nil.
//
// Parameters:
//   - status: The response status code (ignored)
//   - headers: The response headers (ignored)
//
// Returns:
//   - error: Always returns nil
func (w *NoOpStreamingLogWriter) WriteStatus(_ int, _ map[string][]string) error {
	return nil
}

// WriteAPIRequest is a no-op implementation that does nothing and always returns nil.
//
// Parameters:
//   - apiRequest: The API request data (ignored)
//
// Returns:
//   - error: Always returns nil
func (w *NoOpStreamingLogWriter) WriteAPIRequest(_ []byte) error {
	return nil
}

// WriteAPIResponse is a no-op implementation that does nothing and always returns nil.
//
// Parameters:
//   - apiResponse: The API response data (ignored)
//
// Returns:
//   - error: Always returns nil
func (w *NoOpStreamingLogWriter) WriteAPIResponse(_ []byte) error {
	return nil
}

// WriteAPIWebsocketTimeline is a no-op implementation that does nothing and always returns nil.
//
// Parameters:
//   - apiWebsocketTimeline: The upstream websocket event timeline (ignored)
//
// Returns:
//   - error: Always returns nil
func (w *NoOpStreamingLogWriter) WriteAPIWebsocketTimeline(_ []byte) error {
	return nil
}

func (w *NoOpStreamingLogWriter) SetFirstChunkTimestamp(_ time.Time) {}

// Close is a no-op implementation that does nothing and always returns nil.
//
// Returns:
//   - error: Always returns nil
func (w *NoOpStreamingLogWriter) Close() error { return nil }

type homeStreamingLogWriter struct {
	url       string
	method    string
	timestamp time.Time

	requestHeaders map[string][]string
	requestBody    []byte

	mu                sync.Mutex
	chunkChan         chan []byte
	doneChan          chan struct{}
	closeResult       chan struct{}
	closeOnce         sync.Once
	closed            bool
	closeErr          error
	responseTruncated atomic.Bool
	queuedBytes       int

	responseStatus   int
	statusWritten    bool
	responseHeaders  map[string][]string
	responseDigest   hash.Hash
	responseObserved atomic.Int64
	responseCaptured int64
	responseChunks   int64
	apiRequest       []byte
	apiResponse      []byte
	apiWebsocketTime []byte
	requestID        string
	apiResponseTS    time.Time
	firstChunkTS     time.Time
}

func newHomeStreamingLogWriter(url, method string, headers map[string][]string, body []byte, requestID string) *homeStreamingLogWriter {
	writer := &homeStreamingLogWriter{
		url:            url,
		method:         method,
		timestamp:      time.Now(),
		requestHeaders: cloneHeaders(headers),
		requestBody:    normalizeBodyLogMetadata(body, headerValue(headers, "Content-Type")),
		requestID:      strings.TrimSpace(requestID),
		chunkChan:      make(chan []byte, homeStreamingChunkQueueCapacity),
		doneChan:       make(chan struct{}),
		closeResult:    make(chan struct{}),
		responseDigest: sha256.New(),
	}

	go writer.asyncWriter()
	return writer
}

func (w *homeStreamingLogWriter) asyncWriter() {
	defer close(w.doneChan)
	for chunk := range w.chunkChan {
		w.mu.Lock()
		w.queuedBytes -= len(chunk)
		if w.queuedBytes < 0 {
			w.queuedBytes = 0
		}
		w.mu.Unlock()
		if len(chunk) == 0 {
			continue
		}
		_, _ = w.responseDigest.Write(chunk)
		w.responseCaptured += int64(len(chunk))
		w.responseChunks++
	}
}

func (w *homeStreamingLogWriter) WriteChunkAsync(chunk []byte) {
	if w == nil || len(chunk) == 0 {
		return
	}
	w.responseObserved.Add(int64(len(chunk)))

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.chunkChan == nil || w.responseTruncated.Load() {
		return
	}
	chunkLen := min(len(chunk), homeStreamingChunkMaxBytes)
	if len(w.chunkChan) >= cap(w.chunkChan) || w.queuedBytes+chunkLen > streamingLogQueueMaxBytes {
		w.responseTruncated.Store(true)
		return
	}
	if len(chunk) > homeStreamingChunkMaxBytes {
		w.responseTruncated.Store(true)
	}
	queued := cloneBoundedStreamingChunk(chunk)
	w.queuedBytes += len(queued)
	w.chunkChan <- queued
}

func (w *homeStreamingLogWriter) WriteStatus(status int, headers map[string][]string) error {
	if w == nil || status == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.responseStatus = status
	w.statusWritten = true
	w.responseHeaders = cloneHeaders(headers)
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIRequest(apiRequest []byte) error {
	if w == nil || len(apiRequest) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.apiRequest = normalizeBodyLogMetadata(apiRequest, "text/plain")
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIResponse(apiResponse []byte) error {
	if w == nil || len(apiResponse) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.apiResponse = normalizeBodyLogMetadata(apiResponse, "text/plain")
	return nil
}

func (w *homeStreamingLogWriter) WriteAPIWebsocketTimeline(apiWebsocketTimeline []byte) error {
	if w == nil || len(apiWebsocketTimeline) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.apiWebsocketTime = normalizeBodyLogMetadata(apiWebsocketTimeline, "text/plain")
	return nil
}

func boundedSourceMetadata(source *FileBodySource, limit int, _ string) ([]byte, error) {
	if source == nil {
		return nil, nil
	}
	defer cleanupFileBodySources(source)
	metadata, errMetadata := source.Metadata("text/plain")
	if errMetadata != nil {
		return nil, errMetadata
	}
	if limit <= 0 || !bytes.HasPrefix(metadata, []byte(bodyMetadataPrefix)) {
		return metadata, nil
	}
	var decoded BodyLogMetadata
	if errDecode := json.Unmarshal(metadata[len(bodyMetadataPrefix):], &decoded); errDecode != nil {
		return metadata, nil
	}
	if decoded.Bytes > int64(limit) {
		decoded.Truncated = true
	}
	return EncodeBodyLogMetadata(decoded), nil
}

func (w *homeStreamingLogWriter) WriteAPIRequestSource(source *FileBodySource) error {
	metadata, errMetadata := boundedSourceMetadata(source, homeAPISectionMaxBytes, homeAPIRequestTruncationMarker)
	if errMetadata != nil || len(metadata) == 0 {
		return errMetadata
	}
	return w.WriteAPIRequest(metadata)
}

func (w *homeStreamingLogWriter) WriteAPIResponseSource(source *FileBodySource) error {
	metadata, errMetadata := boundedSourceMetadata(source, homeAPISectionMaxBytes, homeAPIResponseTruncationMarker)
	if errMetadata != nil || len(metadata) == 0 {
		return errMetadata
	}
	return w.WriteAPIResponse(metadata)
}

func (w *homeStreamingLogWriter) WriteAPIWebsocketTimelineSource(source *FileBodySource) error {
	metadata, errMetadata := boundedSourceMetadata(source, homeAPISectionMaxBytes, homeAPIWebsocketTruncationMarker)
	if errMetadata != nil || len(metadata) == 0 {
		return errMetadata
	}
	return w.WriteAPIWebsocketTimeline(metadata)
}

func (w *homeStreamingLogWriter) SetFirstChunkTimestamp(timestamp time.Time) {
	if w == nil {
		return
	}
	if !timestamp.IsZero() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.closed {
			return
		}
		w.firstChunkTS = timestamp
		w.apiResponseTS = timestamp
	}
}

func (w *homeStreamingLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(func() {
		w.closeErr = w.close()
		close(w.closeResult)
	})
	<-w.closeResult
	return w.closeErr
}

func (w *homeStreamingLogWriter) close() error {
	w.mu.Lock()
	w.closed = true
	if w.chunkChan != nil {
		close(w.chunkChan)
	}
	w.mu.Unlock()
	<-w.doneChan
	defer func() {
		w.mu.Lock()
		w.chunkChan = nil
		w.requestBody = nil
		w.apiRequest = nil
		w.apiResponse = nil
		w.apiWebsocketTime = nil
		w.mu.Unlock()
	}()

	client := currentHomeRequestLogClient()
	if client == nil || !client.HeartbeatOK() {
		return nil
	}

	buf := newBoundedLogBuffer(homeRequestLogMaxBytes, homeRequestLogTruncationMarker)
	upstreamTransport := inferUpstreamTransport(w.apiRequest, nil, w.apiResponse, nil, w.apiWebsocketTime, nil, nil)
	if errWrite := writeRequestInfoWithBody(buf, w.url, w.method, w.requestHeaders, w.requestBody, w.timestamp, "http", upstreamTransport, true); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISection(buf, "=== API WEBSOCKET TIMELINE ===\n", "=== API WEBSOCKET TIMELINE", w.apiWebsocketTime, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISection(buf, "=== API REQUEST ===\n", "=== API REQUEST", w.apiRequest, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISection(buf, "=== API RESPONSE ===\n", "=== API RESPONSE", w.apiResponse, w.apiResponseTS); errWrite != nil {
		return errWrite
	}
	metadata := EncodeBodyLogMetadata(BodyLogMetadata{
		Bytes:       w.responseObserved.Load(),
		SHA256:      hex.EncodeToString(w.responseDigest.Sum(nil)),
		Chunks:      w.responseChunks,
		ContentType: headerValue(w.responseHeaders, "Content-Type"),
		Truncated:   w.responseTruncated.Load() || w.responseCaptured != w.responseObserved.Load(),
	})
	if errWrite := writeResponseSection(buf, w.responseStatus, w.statusWritten, w.responseHeaders, metadata, nil, false); errWrite != nil {
		return errWrite
	}

	payload := homeRequestLogPayload{
		Headers:    cloneHeaders(w.requestHeaders),
		RequestID:  w.requestID,
		RequestLog: buf.String(),
	}
	raw, errMarshal := json.Marshal(&payload)
	if errMarshal != nil {
		return errMarshal
	}
	return client.RPushRequestLog(context.Background(), raw)
}
