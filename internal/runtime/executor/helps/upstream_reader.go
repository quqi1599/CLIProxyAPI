package helps

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	log "github.com/sirupsen/logrus"
)

const (
	DefaultUpstreamErrorBodyBytes    int64 = 2 << 20
	DefaultUpstreamSuccessBodyBytes  int64 = 128 << 20
	DefaultUpstreamErrorWireBytes    int64 = 4 << 20
	DefaultUpstreamSuccessWireBytes  int64 = 192 << 20
	UpstreamErrorEmergencyBytes      int64 = 8 << 20
	UpstreamSuccessEmergencyBytes    int64 = 256 << 20
	DefaultUpstreamSSEEventBytes     int64 = 16 << 20
	UpstreamSSEEmergencyCeilingBytes int64 = 50 << 20
	minimumUpstreamZstdDecoderBytes  int64 = 1 << 20
	maxUpstreamContentEncodingLayers       = 1
	upstreamResponseReadFailedCode         = "upstream_response_read_failed"
	upstreamResponseDecodeFailedCode       = "upstream_response_decode_failed"
	transportDecodedContentEncoding        = "cliproxy-transport-decoded"
)

var errUpstreamGzipTrailingData = errors.New("upstream gzip response contains multiple members or trailing data")

type singleMemberGzipReader struct {
	decoder  *gzip.Reader
	source   *bufio.Reader
	terminal error
}

func newSingleMemberGzipReader(source io.Reader) (*singleMemberGzipReader, error) {
	buffered := bufio.NewReader(source)
	decoder, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, err
	}
	decoder.Multistream(false)
	return &singleMemberGzipReader{decoder: decoder, source: buffered}, nil
}

func (r *singleMemberGzipReader) Read(p []byte) (int, error) {
	if r == nil || r.decoder == nil {
		return 0, io.ErrClosedPipe
	}
	if r.terminal != nil {
		return 0, r.terminal
	}
	n, err := r.decoder.Read(p)
	if !errors.Is(err, io.EOF) {
		return n, err
	}
	_, errPeek := r.source.Peek(1)
	switch {
	case errPeek == nil:
		r.terminal = errUpstreamGzipTrailingData
	case errors.Is(errPeek, io.EOF):
		r.terminal = io.EOF
	default:
		r.terminal = errPeek
	}
	if n > 0 {
		return n, nil
	}
	return 0, r.terminal
}

func (r *singleMemberGzipReader) Close() error {
	if r == nil || r.decoder == nil {
		return nil
	}
	return r.decoder.Close()
}

// UpstreamBodyLimits keeps error and endpoint-specific success budgets separate.
// Zero values use the safe defaults above; negative values are rejected.
type UpstreamBodyLimits struct {
	ErrorBytes       int64
	SuccessBytes     int64
	ErrorWireBytes   int64
	SuccessWireBytes int64
}

// ReadBoundedUpstreamHTTPResponse consumes and closes response.Body while also
// preserving whether net/http already decoded an automatically requested gzip
// response. Provider call sites should prefer this response-level entrypoint.
func ReadBoundedUpstreamHTTPResponse(response *http.Response, limits UpstreamBodyLimits) ([]byte, error) {
	if response == nil {
		return nil, newUpstreamProtocolFailure(
			"upstream_response_missing",
			"upstream response is unavailable",
			errors.New("upstream response is nil"),
		)
	}
	return ReadBoundedUpstreamBody(
		response.Body,
		upstreamResponseContentEncoding(response),
		response.StatusCode,
		limits,
	)
}

func upstreamResponseContentEncoding(response *http.Response) string {
	if response == nil {
		return ""
	}
	if response.Uncompressed {
		return transportDecodedContentEncoding
	}
	return response.Header.Get("Content-Encoding")
}

// ReadBoundedUpstreamBody consumes and closes body exactly once. Limits apply
// after reversing the Content-Encoding chain.
func ReadBoundedUpstreamBody(body io.ReadCloser, contentEncoding string, statusCode int, limits UpstreamBodyLimits) (data []byte, err error) {
	if body == nil {
		return nil, newUpstreamProtocolFailure(
			"upstream_response_body_missing",
			"upstream response body is unavailable",
			errors.New("upstream response body is nil"),
		)
	}
	defer func() {
		if errClose := body.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close upstream response body")
		}
	}()

	maxBytes, maxWireBytes, bodyClass, errLimit := limits.limit(statusCode)
	if errLimit != nil {
		return nil, errLimit
	}
	wireReader := &io.LimitedReader{R: body, N: maxWireBytes + 1}
	reader, decoderClosers, decoded, errDecode := decodeUpstreamBody(wireReader, contentEncoding, maxBytes)
	if errDecode != nil {
		if errors.Is(errDecode, context.Canceled) || errors.Is(errDecode, context.DeadlineExceeded) {
			return nil, classifyUpstreamReadFailure(errDecode)
		}
		if wireReader.N <= 0 {
			return nil, upstreamBodyTooLargeFailure(bodyClass, "wire", maxWireBytes)
		}
		if isUpstreamTransportReadFailure(errDecode) {
			return preserveKnownUpstreamErrorStatus(bodyClass, nil, classifyUpstreamReadFailure(errDecode))
		}
		if !decoded {
			return preserveKnownUpstreamErrorStatus(bodyClass, nil, classifyUpstreamReadFailure(errDecode))
		}
		return nil, newUpstreamProtocolFailure(upstreamResponseDecodeFailedCode, "upstream response could not be decoded", errDecode)
	}
	defer func() {
		if errClose := closeReaderLayers(decoderClosers); errClose != nil {
			log.WithError(errClose).Warn("failed to close upstream response decoder")
		}
	}()

	data, err = io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, classifyUpstreamReadFailure(err)
		}
		if wireReader.N <= 0 {
			return nil, upstreamBodyTooLargeFailure(bodyClass, "wire", maxWireBytes)
		}
		if isUpstreamTransportReadFailure(err) {
			return preserveKnownUpstreamErrorStatus(bodyClass, data, classifyUpstreamReadFailure(err))
		}
		if decoded {
			return data, newUpstreamProtocolFailure(upstreamResponseDecodeFailedCode, "upstream response could not be decoded", err)
		}
		return preserveKnownUpstreamErrorStatus(bodyClass, data, classifyUpstreamReadFailure(err))
	}
	if wireReader.N <= 0 {
		return nil, upstreamBodyTooLargeFailure(bodyClass, "wire", maxWireBytes)
	}
	if int64(len(data)) > maxBytes {
		return nil, upstreamBodyTooLargeFailure(bodyClass, "decoded", maxBytes)
	}
	return data, nil
}

func hasUpstreamContentEncoding(contentEncoding string) bool {
	for _, rawEncoding := range strings.Split(contentEncoding, ",") {
		encoding := strings.ToLower(strings.TrimSpace(rawEncoding))
		if encoding != "" && encoding != "identity" {
			return true
		}
	}
	return false
}

func (limits UpstreamBodyLimits) limit(statusCode int) (maxBytes, maxWireBytes int64, bodyClass string, err error) {
	maxBytes = limits.ErrorBytes
	maxWireBytes = limits.ErrorWireBytes
	bodyClass = "error"
	emergencyBytes := UpstreamErrorEmergencyBytes
	defaultWireBytes := DefaultUpstreamErrorWireBytes
	if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
		maxBytes = limits.SuccessBytes
		maxWireBytes = limits.SuccessWireBytes
		bodyClass = "success"
		emergencyBytes = UpstreamSuccessEmergencyBytes
		defaultWireBytes = DefaultUpstreamSuccessWireBytes
	}
	if maxBytes == 0 {
		if bodyClass == "success" {
			maxBytes = DefaultUpstreamSuccessBodyBytes
		} else {
			maxBytes = DefaultUpstreamErrorBodyBytes
		}
	}
	if maxWireBytes == 0 {
		maxWireBytes = defaultWireBytes
	}
	if maxBytes < 1 || maxBytes > emergencyBytes || maxWireBytes < 1 || maxWireBytes > emergencyBytes {
		return 0, 0, "", &failurecontract.Failure{
			Kind:          failurecontract.InternalTransformError,
			Scope:         failurecontract.ScopeRequest,
			HTTPStatus:    http.StatusInternalServerError,
			ProviderCode:  "upstream_response_limit_invalid",
			Cause:         fmt.Errorf("invalid upstream %s limits: decoded=%d wire=%d emergency=%d", bodyClass, maxBytes, maxWireBytes, emergencyBytes),
			PublicMessage: "upstream response limit is invalid",
		}
	}
	return maxBytes, maxWireBytes, bodyClass, nil
}

func decodeUpstreamBody(body io.Reader, contentEncoding string, maxBytes int64) (io.Reader, []func() error, bool, error) {
	if contentEncoding == transportDecodedContentEncoding {
		return body, nil, true, nil
	}
	reader := body
	var closers []func() error
	if !hasUpstreamContentEncoding(contentEncoding) {
		buffered := bufio.NewReader(reader)
		var errSniff error
		contentEncoding, errSniff = sniffUpstreamContentEncoding(buffered)
		if errSniff != nil {
			return nil, nil, false, errSniff
		}
		reader = buffered
	}
	layers, errEncoding := validateUpstreamContentEncoding(contentEncoding)
	if errEncoding != nil {
		return nil, nil, true, errEncoding
	}
	encodings := strings.Split(contentEncoding, ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		encoding := strings.ToLower(strings.TrimSpace(encodings[i]))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			decoder, err := newSingleMemberGzipReader(reader)
			if err != nil {
				_ = closeReaderLayers(closers)
				return nil, nil, true, fmt.Errorf("create gzip decoder: %w", err)
			}
			reader = decoder
			closers = append(closers, decoder.Close)
		case "deflate":
			decoder := flate.NewReader(reader)
			reader = decoder
			closers = append(closers, decoder.Close)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoderBytes := max(maxBytes, minimumUpstreamZstdDecoderBytes)
			decoder, err := zstd.NewReader(
				reader,
				zstd.WithDecoderConcurrency(1),
				zstd.WithDecoderMaxMemory(uint64(decoderBytes)),
				zstd.WithDecoderMaxWindow(uint64(decoderBytes)),
			)
			if err != nil {
				_ = closeReaderLayers(closers)
				return nil, nil, true, fmt.Errorf("create zstd decoder: %w", err)
			}
			reader = decoder
			closers = append(closers, func() error {
				decoder.Close()
				return nil
			})
		default:
			_ = closeReaderLayers(closers)
			return nil, nil, true, fmt.Errorf("unsupported upstream content encoding %q", encoding)
		}
	}
	return reader, closers, layers > 0, nil
}

func validateUpstreamContentEncoding(contentEncoding string) (int, error) {
	if contentEncoding == transportDecodedContentEncoding {
		return 1, nil
	}
	layers := 0
	for _, rawEncoding := range strings.Split(contentEncoding, ",") {
		encoding := strings.ToLower(strings.TrimSpace(rawEncoding))
		switch encoding {
		case "", "identity":
			continue
		case "gzip", "deflate", "br", "zstd":
			layers++
			if layers > maxUpstreamContentEncodingLayers {
				return 0, fmt.Errorf("upstream content encoding exceeds %d layers", maxUpstreamContentEncodingLayers)
			}
		default:
			return 0, fmt.Errorf("unsupported upstream content encoding %q", encoding)
		}
	}
	return layers, nil
}

func sniffUpstreamContentEncoding(buffered *bufio.Reader) (string, error) {
	first, errPeek := buffered.Peek(1)
	if errPeek != nil && !errors.Is(errPeek, io.EOF) && len(first) == 0 {
		return "", fmt.Errorf("inspect upstream response encoding: %w", errPeek)
	}
	if len(first) == 0 {
		return "", nil
	}

	magicBytes := 0
	switch first[0] {
	case 0x1f:
		magicBytes = 2
	case 0x28:
		magicBytes = 4
	default:
		return "", nil
	}
	magic, errPeek := buffered.Peek(magicBytes)
	if errPeek != nil && !errors.Is(errPeek, io.EOF) && len(magic) == 0 {
		return "", fmt.Errorf("inspect upstream response encoding: %w", errPeek)
	}
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		return "gzip", nil
	}
	if len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd {
		return "zstd", nil
	}
	return "", nil
}

func upstreamBodyTooLargeFailure(bodyClass, sizeClass string, limit int64) *failurecontract.Failure {
	code := "upstream_" + bodyClass + "_body_too_large"
	detail := "after decompression"
	if sizeClass == "wire" {
		code = "upstream_" + bodyClass + "_wire_body_too_large"
		detail = "before decompression"
	}
	return newUpstreamProtocolFailure(
		code,
		"upstream response exceeded the configured limit",
		fmt.Errorf("upstream %s body exceeds %d bytes %s", bodyClass, limit, detail),
	)
}

func closeReaderLayers(closers []func() error) error {
	var firstErr error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func isUpstreamTransportReadFailure(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError)
}

// BoundedSSEReader reads complete SSE events without adding a timeout or a
// goroutine. The caller keeps ownership of the underlying stream.
type BoundedSSEReader struct {
	scanner       *bufio.Scanner
	maxEventBytes int64
}

// BoundedUpstreamSSEStream owns an upstream body, reverses supported content
// encodings, and enforces the per-event decoded budget without a total stream
// timeout or lifetime byte cap.
type BoundedUpstreamSSEStream struct {
	reader          *BoundedSSEReader
	body            io.ReadCloser
	contentEncoding string
	maxEventBytes   int64
	decoderClosers  []func() error
	decoded         bool
	readMu          sync.Mutex
	closed          atomic.Bool
	closeOnce       sync.Once
	closeErr        error
}

// NewBoundedUpstreamSSEStream transfers body ownership to the returned stream.
// Decoder initialization is lazy so response headers and the cancellation hook
// can reach the caller before the upstream emits its first body byte.
func NewBoundedUpstreamSSEStream(body io.ReadCloser, contentEncoding string, maxEventBytes int64) (*BoundedUpstreamSSEStream, error) {
	if body == nil {
		return nil, newUpstreamProtocolFailure(
			"upstream_response_body_missing",
			"upstream response body is unavailable",
			errors.New("upstream response body is nil"),
		)
	}
	if maxEventBytes == 0 {
		maxEventBytes = DefaultUpstreamSSEEventBytes
	}
	if maxEventBytes < 1 || maxEventBytes > UpstreamSSEEmergencyCeilingBytes {
		if errClose := body.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close upstream SSE body")
		}
		return nil, fmt.Errorf("SSE event limit must be between 1 and %d bytes", UpstreamSSEEmergencyCeilingBytes)
	}
	if hasUpstreamContentEncoding(contentEncoding) {
		if _, errEncoding := validateUpstreamContentEncoding(contentEncoding); errEncoding != nil {
			if errClose := body.Close(); errClose != nil {
				log.WithError(errClose).Warn("failed to close upstream SSE body")
			}
			return nil, newUpstreamProtocolFailure(upstreamResponseDecodeFailedCode, "upstream response could not be decoded", errEncoding)
		}
	}
	return &BoundedUpstreamSSEStream{
		body:            body,
		contentEncoding: contentEncoding,
		maxEventBytes:   maxEventBytes,
	}, nil
}

// NewBoundedUpstreamHTTPResponseSSEStream transfers response.Body ownership to
// a lazy bounded stream and preserves net/http automatic-gzip metadata.
func NewBoundedUpstreamHTTPResponseSSEStream(response *http.Response, maxEventBytes int64) (*BoundedUpstreamSSEStream, error) {
	if response == nil {
		return nil, newUpstreamProtocolFailure(
			"upstream_response_missing",
			"upstream response is unavailable",
			errors.New("upstream response is nil"),
		)
	}
	return NewBoundedUpstreamSSEStream(response.Body, upstreamResponseContentEncoding(response), maxEventBytes)
}

// ReadEvent returns the next decoded SSE event.
func (stream *BoundedUpstreamSSEStream) ReadEvent() ([]byte, error) {
	if stream == nil || stream.body == nil {
		return nil, errors.New("upstream SSE stream is unavailable")
	}
	if stream.closed.Load() {
		return nil, io.ErrClosedPipe
	}
	stream.readMu.Lock()
	defer stream.readMu.Unlock()
	if stream.closed.Load() {
		return nil, io.ErrClosedPipe
	}
	if stream.reader == nil {
		if errInitialize := stream.initializeReaderLocked(); errInitialize != nil {
			return nil, errInitialize
		}
	}
	event, err := stream.reader.ReadEvent()
	if !stream.decoded || err == nil || errors.Is(err, io.EOF) {
		return event, err
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.TransportError {
		return event, err
	}
	if isUpstreamTransportReadFailure(typed.Cause) {
		return event, err
	}
	return nil, newUpstreamProtocolFailure(upstreamResponseDecodeFailedCode, "upstream response could not be decoded", typed.Cause)
}

func (stream *BoundedUpstreamSSEStream) initializeReaderLocked() error {
	decodedReader, decoderClosers, decoded, errDecode := decodeUpstreamBody(stream.body, stream.contentEncoding, stream.maxEventBytes)
	if errDecode != nil {
		_ = closeReaderLayers(decoderClosers)
		if errors.Is(errDecode, context.Canceled) || errors.Is(errDecode, context.DeadlineExceeded) {
			return classifyUpstreamReadFailure(errDecode)
		}
		if isUpstreamTransportReadFailure(errDecode) {
			return classifyUpstreamReadFailure(errDecode)
		}
		if !decoded {
			return classifyUpstreamReadFailure(errDecode)
		}
		return newUpstreamProtocolFailure(upstreamResponseDecodeFailedCode, "upstream response could not be decoded", errDecode)
	}
	reader, errReader := NewBoundedSSEReader(decodedReader, stream.maxEventBytes)
	if errReader != nil {
		_ = closeReaderLayers(decoderClosers)
		return errReader
	}
	stream.reader = reader
	stream.decoderClosers = decoderClosers
	stream.decoded = decoded
	return nil
}

// Close closes decoder layers and the upstream body exactly once.
func (stream *BoundedUpstreamSSEStream) Close() error {
	if stream == nil {
		return nil
	}
	stream.closeOnce.Do(func() {
		stream.closed.Store(true)
		if stream.body != nil {
			if errClose := stream.body.Close(); errClose != nil {
				stream.closeErr = errClose
			}
		}
		stream.readMu.Lock()
		if errClose := closeReaderLayers(stream.decoderClosers); errClose != nil {
			log.WithError(errClose).Debug("upstream SSE decoder closed after body cancellation")
		}
		stream.readMu.Unlock()
	})
	return stream.closeErr
}

// NewBoundedSSEReader builds an event reader. A zero limit uses the 16 MiB
// default; the 50 MiB emergency ceiling remains available during migration.
func NewBoundedSSEReader(reader io.Reader, maxEventBytes int64) (*BoundedSSEReader, error) {
	if reader == nil {
		return nil, errors.New("SSE reader is nil")
	}
	if maxEventBytes == 0 {
		maxEventBytes = DefaultUpstreamSSEEventBytes
	}
	if maxEventBytes < 1 || maxEventBytes > UpstreamSSEEmergencyCeilingBytes {
		return nil, fmt.Errorf("SSE event limit must be between 1 and %d bytes", UpstreamSSEEmergencyCeilingBytes)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), int(maxEventBytes)+2)
	scanner.Split(splitSSELine)
	return &BoundedSSEReader{scanner: scanner, maxEventBytes: maxEventBytes}, nil
}

// ReadEvent returns one event without its terminating blank line. Line endings
// inside the event are preserved and count toward the event budget.
func (reader *BoundedSSEReader) ReadEvent() ([]byte, error) {
	if reader == nil || reader.scanner == nil {
		return nil, errors.New("SSE reader is unavailable")
	}
	var event []byte
	for reader.scanner.Scan() {
		line := reader.scanner.Bytes()
		blankLine := bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\r"))
		if !blankLine && len(line) > 0 {
			if int64(len(event))+int64(len(line)) > reader.maxEventBytes {
				return nil, upstreamSSEEventTooLargeFailure(reader.maxEventBytes)
			}
			event = append(event, line...)
		} else if blankLine {
			if len(event) == 0 {
				return []byte{}, nil
			}
			return event, nil
		}
	}
	if err := reader.scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return nil, upstreamSSEEventTooLargeFailure(reader.maxEventBytes)
		}
		return nil, classifyUpstreamReadFailure(err)
	}
	if len(event) > 0 {
		return event, nil
	}
	return nil, io.EOF
}

func splitSSELine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for index, value := range data {
		switch value {
		case '\n':
			return index + 1, data[:index+1], nil
		case '\r':
			if index+1 < len(data) {
				if data[index+1] == '\n' {
					return index + 2, data[:index+2], nil
				}
				return index + 1, data[:index+1], nil
			}
			if atEOF {
				return index + 1, data[:index+1], nil
			}
			return 0, nil, nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func upstreamSSEEventTooLargeFailure(limit int64) *failurecontract.Failure {
	return newUpstreamProtocolFailure(
		"upstream_sse_event_too_large",
		"upstream SSE event exceeded the configured limit",
		fmt.Errorf("upstream SSE event exceeds %d bytes", limit),
	)
}

func classifyUpstreamReadFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &failurecontract.Failure{
			Kind:          failurecontract.Cancelled,
			Scope:         failurecontract.ScopeRequest,
			HTTPStatus:    http.StatusRequestTimeout,
			ProviderCode:  "request_cancelled",
			Cause:         err,
			PublicMessage: "request cancelled",
		}
	}
	return newUpstreamTransportFailure(upstreamResponseReadFailedCode, "upstream response could not be read", err)
}

// NormalizeUpstreamReadError maps a raw response read failure to the shared
// provider/request scoped failure contract.
func NormalizeUpstreamReadError(err error) error {
	if err == nil {
		return nil
	}
	return classifyUpstreamReadFailure(err)
}

func preserveKnownUpstreamErrorStatus(bodyClass string, data []byte, err error) ([]byte, error) {
	if bodyClass != "error" || err == nil {
		return data, err
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.TransportError {
		return data, err
	}
	log.WithError(err).Warn("upstream error response ended before its body completed")
	return data, nil
}

func newUpstreamProtocolFailure(code, message string, cause error) *failurecontract.Failure {
	return &failurecontract.Failure{
		Kind:          failurecontract.UpstreamProtocolError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		ProviderCode:  code,
		Cause:         cause,
		PublicMessage: message,
	}
}

func newUpstreamTransportFailure(code, message string, cause error) *failurecontract.Failure {
	return &failurecontract.Failure{
		Kind:          failurecontract.TransportError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		ProviderCode:  code,
		Retryable:     true,
		Cause:         cause,
		PublicMessage: message,
	}
}
