package helps

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestReadBoundedUpstreamBodyUsesIndependentLimitPlusOneBudgets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		limits     UpstreamBodyLimits
		wantCode   string
	}{
		{
			name:       "success",
			statusCode: http.StatusOK,
			limits:     UpstreamBodyLimits{ErrorBytes: 8, SuccessBytes: 4},
			wantCode:   "upstream_success_body_too_large",
		},
		{
			name:       "error",
			statusCode: http.StatusInternalServerError,
			limits:     UpstreamBodyLimits{ErrorBytes: 4, SuccessBytes: 8},
			wantCode:   "upstream_error_body_too_large",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &countingReadCloser{Reader: strings.NewReader("12345")}
			_, err := ReadBoundedUpstreamBody(source, "identity", test.statusCode, test.limits)
			assertUpstreamProtocolFailure(t, err, test.wantCode)
			if source.bytesRead != 5 {
				t.Fatalf("bytes read = %d, want limit+1 (5)", source.bytesRead)
			}
			if source.closeCalls != 1 {
				t.Fatalf("Close calls = %d, want 1", source.closeCalls)
			}
		})
	}
}

func TestReadBoundedUpstreamBodyAcceptsExactLimitAndClosesOnce(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: strings.NewReader("1234")}
	got, err := ReadBoundedUpstreamBody(source, "identity", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 4})
	if err != nil {
		t.Fatalf("ReadBoundedUpstreamBody() error = %v", err)
	}
	if string(got) != "1234" {
		t.Fatalf("body = %q, want 1234", got)
	}
	if source.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", source.closeCalls)
	}
}

func TestReadBoundedUpstreamBodyKeepsSuccessfulDataWhenCloseFails(t *testing.T) {
	t.Parallel()

	source := &closeErrorReadCloser{Reader: strings.NewReader("ok"), err: errors.New("close failed")}
	got, err := ReadBoundedUpstreamBody(source, "identity", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 2})
	if err != nil {
		t.Fatalf("ReadBoundedUpstreamBody() error = %v", err)
	}
	if string(got) != "ok" || source.closeCalls != 1 {
		t.Fatalf("result = %q, close calls = %d", got, source.closeCalls)
	}
}

func TestReadBoundedUpstreamBodyEnforcesWireBudget(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: strings.NewReader("12345")}
	_, err := ReadBoundedUpstreamBody(source, "identity", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 64, SuccessWireBytes: 4})
	assertUpstreamProtocolFailure(t, err, "upstream_success_wire_body_too_large")
	if source.bytesRead != 5 {
		t.Fatalf("bytes read = %d, want wire limit+1", source.bytesRead)
	}
}

func TestReadBoundedUpstreamBodyKeepsSuccessAndErrorWireBudgetsIndependent(t *testing.T) {
	t.Parallel()

	limits := UpstreamBodyLimits{
		ErrorBytes:       4,
		SuccessBytes:     64,
		ErrorWireBytes:   4,
		SuccessWireBytes: DefaultUpstreamSuccessWireBytes,
	}
	_, err := ReadBoundedUpstreamBody(
		&countingReadCloser{Reader: strings.NewReader("12345")},
		"identity",
		http.StatusInternalServerError,
		limits,
	)
	assertUpstreamProtocolFailure(t, err, "upstream_error_wire_body_too_large")
}

func TestReadBoundedUpstreamBodyLimitsDecodedGzipBrotliAndZstd(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("a"), 65)
	tests := []struct {
		name     string
		encoding string
		encode   func(*testing.T, []byte) []byte
	}{
		{name: "gzip", encoding: "gzip", encode: encodeUpstreamGzip},
		{name: "brotli", encoding: "br", encode: encodeUpstreamBrotli},
		{name: "zstd", encoding: "zstd", encode: encodeUpstreamZstd},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &countingReadCloser{Reader: bytes.NewReader(test.encode(t, payload))}
			_, err := ReadBoundedUpstreamBody(source, test.encoding, http.StatusOK, UpstreamBodyLimits{SuccessBytes: 64})
			assertUpstreamProtocolFailure(t, err, "upstream_success_body_too_large")
			if source.closeCalls != 1 {
				t.Fatalf("Close calls = %d, want 1", source.closeCalls)
			}
		})
	}
}

func TestReadBoundedUpstreamBodyRejectsConcatenatedGzipMembers(t *testing.T) {
	t.Parallel()

	encoded := append(encodeUpstreamGzip(t, nil), encodeUpstreamGzip(t, []byte("small"))...)
	_, err := ReadBoundedUpstreamBody(
		&countingReadCloser{Reader: bytes.NewReader(encoded)},
		"gzip",
		http.StatusOK,
		UpstreamBodyLimits{SuccessBytes: 64},
	)
	assertUpstreamProtocolFailure(t, err, upstreamResponseDecodeFailedCode)
}

func TestReadBoundedUpstreamBodyDetectsHeaderlessGzipAndZstd(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"test","output":"ok"}`)
	tests := []struct {
		name   string
		encode func(*testing.T, []byte) []byte
	}{
		{name: "gzip", encode: encodeUpstreamGzip},
		{name: "zstd", encode: encodeUpstreamZstd},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &countingReadCloser{Reader: bytes.NewReader(test.encode(t, payload))}
			got, err := ReadBoundedUpstreamBody(source, "", http.StatusOK, UpstreamBodyLimits{SuccessBytes: int64(len(payload))})
			if err != nil {
				t.Fatalf("ReadBoundedUpstreamBody() error = %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("body = %q, want %q", got, payload)
			}
			if source.closeCalls != 1 {
				t.Fatalf("Close calls = %d, want 1", source.closeCalls)
			}
		})
	}
}

func TestReadBoundedUpstreamBodyClassifiesTruncatedHeaderlessCompressionAsProtocolFailure(t *testing.T) {
	t.Parallel()

	encoded := truncateTail(encodeUpstreamGzip(t, []byte("decoded body")), 4)
	_, err := ReadBoundedUpstreamBody(
		&countingReadCloser{Reader: bytes.NewReader(encoded)},
		"",
		http.StatusOK,
		UpstreamBodyLimits{SuccessBytes: 64},
	)
	assertUpstreamProtocolFailure(t, err, upstreamResponseDecodeFailedCode)
}

func TestReadBoundedUpstreamBodySupportsDeflateCompatibility(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"output":"ok"}`)
	got, err := ReadBoundedUpstreamBody(
		&countingReadCloser{Reader: bytes.NewReader(encodeUpstreamDeflate(t, payload))},
		"deflate",
		http.StatusOK,
		UpstreamBodyLimits{SuccessBytes: int64(len(payload))},
	)
	if err != nil {
		t.Fatalf("ReadBoundedUpstreamBody() error = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("body = %q, want %q", got, payload)
	}
}

func TestReadBoundedUpstreamBodyDecodeFailureClosesOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{name: "header", body: []byte("not gzip")},
		{name: "truncated stream", body: truncateTail(encodeUpstreamGzip(t, []byte("decoded body")), 4)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &countingReadCloser{Reader: bytes.NewReader(test.body)}
			_, err := ReadBoundedUpstreamBody(source, "gzip", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 64})
			assertUpstreamProtocolFailure(t, err, upstreamResponseDecodeFailedCode)
			if source.closeCalls != 1 {
				t.Fatalf("Close calls = %d, want 1", source.closeCalls)
			}
		})
	}
}

func TestReadBoundedUpstreamBodyClassifiesCancellationAsRequestScoped(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: errorReader{err: context.Canceled}}
	_, err := ReadBoundedUpstreamBody(source, "identity", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 64})
	typed, ok := failurecontract.As(err)
	if !ok {
		t.Fatalf("error = %T, want typed failure", err)
	}
	if typed.Kind != failurecontract.Cancelled || typed.Scope != failurecontract.ScopeRequest {
		t.Fatalf("failure = %q/%q, want cancelled/request", typed.Kind, typed.Scope)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("typed failure does not preserve context cancellation")
	}
	if source.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", source.closeCalls)
	}
}

func TestReadBoundedUpstreamBodyClassifiesHeaderlessPeekFailureAsTransport(t *testing.T) {
	t.Parallel()

	errReset := errors.New("connection reset")
	_, err := ReadBoundedUpstreamBody(
		&countingReadCloser{Reader: errorReader{err: errReset}},
		"",
		http.StatusOK,
		UpstreamBodyLimits{SuccessBytes: 64},
	)
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.TransportError || typed.Scope != failurecontract.ScopeProvider || !typed.Retryable {
		t.Fatalf("failure = %#v, want retryable transport/provider", typed)
	}
	if typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != upstreamResponseReadFailedCode {
		t.Fatalf("failure metadata = status:%d code:%q", typed.HTTPStatus, typed.ProviderCode)
	}
	if !errors.Is(err, errReset) {
		t.Fatal("typed failure does not preserve transport cause")
	}
}

func TestReadBoundedUpstreamBodyPreservesKnownErrorStatusOnTransportTruncation(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: &dataThenErrorReader{data: []byte(`{"error":"rate limited"}`), err: io.ErrUnexpectedEOF}}
	got, err := ReadBoundedUpstreamBody(source, "", http.StatusTooManyRequests, UpstreamBodyLimits{})
	if err != nil {
		t.Fatalf("ReadBoundedUpstreamBody() error = %v, want upstream status to remain authoritative", err)
	}
	if string(got) != `{"error":"rate limited"}` {
		t.Fatalf("body = %q", got)
	}
	if source.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", source.closeCalls)
	}
}

func TestReadBoundedUpstreamHTTPResponseTreatsTransportDecodedFailureAsProtocolError(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: &dataThenErrorReader{data: []byte(`{"error":"rate limited"}`), err: io.ErrUnexpectedEOF}}
	response := &http.Response{
		StatusCode:   http.StatusTooManyRequests,
		Body:         source,
		Header:       make(http.Header),
		Uncompressed: true,
	}
	got, err := ReadBoundedUpstreamHTTPResponse(response, UpstreamBodyLimits{})
	if string(got) != `{"error":"rate limited"}` {
		t.Fatalf("partial body = %q", got)
	}
	assertUpstreamProtocolFailure(t, err, upstreamResponseDecodeFailedCode)
	if source.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", source.closeCalls)
	}
}

func TestReadBoundedUpstreamBodyClassifiesCompressedCancellationAsRequestScoped(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: errorReader{err: context.Canceled}}
	_, err := ReadBoundedUpstreamBody(source, "gzip", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 64})
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.Cancelled || typed.Scope != failurecontract.ScopeRequest {
		t.Fatalf("failure = %#v, want cancelled/request", typed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("typed failure does not preserve context cancellation")
	}
}

func TestReadBoundedUpstreamBodyPreservesCompressedTransportReset(t *testing.T) {
	t.Parallel()

	encoded := truncateTail(encodeUpstreamGzip(t, []byte("decoded body")), 4)
	source := &countingReadCloser{Reader: &dataThenErrorReader{data: encoded, err: syscall.ECONNRESET}}
	_, err := ReadBoundedUpstreamBody(source, "gzip", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 64})
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.TransportError || typed.Scope != failurecontract.ScopeProvider || !typed.Retryable {
		t.Fatalf("failure = %#v, want retryable transport/provider", typed)
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal("typed failure does not preserve connection reset")
	}
}

func TestReadBoundedUpstreamBodyRejectsEncodingLayersAndUnsafeLimits(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: strings.NewReader("body")}
	_, err := ReadBoundedUpstreamBody(source, "gzip, gzip", http.StatusOK, UpstreamBodyLimits{SuccessBytes: 64})
	assertUpstreamProtocolFailure(t, err, upstreamResponseDecodeFailedCode)
	typedEncoding, _ := failurecontract.As(err)
	if typedEncoding == nil || typedEncoding.Cause == nil || !strings.Contains(typedEncoding.Cause.Error(), "exceeds 1 layer") {
		t.Fatalf("encoding-layer error = %v", err)
	}

	_, err = ReadBoundedUpstreamBody(
		&countingReadCloser{Reader: strings.NewReader("body")},
		"identity",
		http.StatusOK,
		UpstreamBodyLimits{SuccessBytes: UpstreamSuccessEmergencyBytes + 1},
	)
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InternalTransformError || typed.Scope != failurecontract.ScopeRequest {
		t.Fatalf("unsafe limit failure = %#v", typed)
	}
}

func TestBoundedSSEReaderReadsEventsAndRejectsOversize(t *testing.T) {
	t.Parallel()

	reader, err := NewBoundedSSEReader(strings.NewReader("data: one\r\ndata: two\r\n\r\ndata: end"), 64)
	if err != nil {
		t.Fatalf("NewBoundedSSEReader() error = %v", err)
	}
	first, err := reader.ReadEvent()
	if err != nil || string(first) != "data: one\r\ndata: two\r\n" {
		t.Fatalf("first event = %q, error = %v", first, err)
	}
	second, err := reader.ReadEvent()
	if err != nil || string(second) != "data: end" {
		t.Fatalf("second event = %q, error = %v", second, err)
	}
	if _, err = reader.ReadEvent(); !errors.Is(err, io.EOF) {
		t.Fatalf("final error = %v, want EOF", err)
	}

	oversized, err := NewBoundedSSEReader(strings.NewReader("data: 12345\n\n"), 8)
	if err != nil {
		t.Fatalf("NewBoundedSSEReader() error = %v", err)
	}
	_, err = oversized.ReadEvent()
	assertUpstreamProtocolFailure(t, err, "upstream_sse_event_too_large")
}

func TestBoundedSSEReaderSupportsBareCarriageReturnEvents(t *testing.T) {
	t.Parallel()

	reader, err := NewBoundedSSEReader(strings.NewReader("data: one\rdata: two\r\rdata: end\r\r"), 64)
	if err != nil {
		t.Fatalf("NewBoundedSSEReader() error = %v", err)
	}
	first, err := reader.ReadEvent()
	if err != nil || string(first) != "data: one\rdata: two\r" {
		t.Fatalf("first event = %q, error = %v", first, err)
	}
	second, err := reader.ReadEvent()
	if err != nil || string(second) != "data: end\r" {
		t.Fatalf("second event = %q, error = %v", second, err)
	}
}

func TestBoundedSSEReaderPreservesEmptyHeartbeatLines(t *testing.T) {
	t.Parallel()

	reader, err := NewBoundedSSEReader(strings.NewReader("\n\ndata: end\n\n"), 64)
	if err != nil {
		t.Fatalf("NewBoundedSSEReader() error = %v", err)
	}
	for index := 0; index < 2; index++ {
		event, errRead := reader.ReadEvent()
		if errRead != nil || event == nil || len(event) != 0 {
			t.Fatalf("heartbeat %d = %#v, error = %v", index, event, errRead)
		}
	}
	event, err := reader.ReadEvent()
	if err != nil || string(event) != "data: end\n" {
		t.Fatalf("data event = %q, error = %v", event, err)
	}
}

func TestBoundedUpstreamSSEStreamDecodesAndOwnsBody(t *testing.T) {
	t.Parallel()

	encoded := encodeUpstreamGzip(t, []byte("data: one\n\ndata: two\n\n"))
	source := &countingReadCloser{Reader: bytes.NewReader(encoded)}
	stream, err := NewBoundedUpstreamSSEStream(source, "gzip", 64)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	first, err := stream.ReadEvent()
	if err != nil || string(first) != "data: one\n" {
		t.Fatalf("first event = %q, error = %v", first, err)
	}
	second, err := stream.ReadEvent()
	if err != nil || string(second) != "data: two\n" {
		t.Fatalf("second event = %q, error = %v", second, err)
	}
	if err = stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err = stream.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if source.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", source.closeCalls)
	}
}

func TestBoundedUpstreamSSEStreamRejectsConcatenatedGzipMembers(t *testing.T) {
	t.Parallel()

	encoded := append(encodeUpstreamGzip(t, nil), encodeUpstreamGzip(t, []byte("data: hidden\n\n"))...)
	stream, err := NewBoundedUpstreamSSEStream(&countingReadCloser{Reader: bytes.NewReader(encoded)}, "gzip", 64)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, errRead := stream.ReadEvent(); errRead == nil {
		t.Fatal("ReadEvent() succeeded for concatenated gzip members")
	} else {
		assertUpstreamProtocolFailure(t, errRead, upstreamResponseDecodeFailedCode)
	}
}

func TestBoundedUpstreamSSEStreamDetectsHeaderlessCompression(t *testing.T) {
	t.Parallel()

	encoded := encodeUpstreamGzip(t, []byte("data: one\n\n"))
	source := &countingReadCloser{Reader: bytes.NewReader(encoded)}
	stream, err := NewBoundedUpstreamSSEStream(source, "", 64)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	defer func() {
		if errClose := stream.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	}()
	event, err := stream.ReadEvent()
	if err != nil || string(event) != "data: one\n" {
		t.Fatalf("event = %q, error = %v", event, err)
	}
}

func TestBoundedUpstreamSSEStreamDoesNotWaitForFourBytesBeforeHeartbeat(t *testing.T) {
	t.Parallel()

	source := newBlockingReadCloser([]byte("\n"), 1)
	defer func() { _ = source.Close() }()
	result := make(chan struct {
		stream *BoundedUpstreamSSEStream
		err    error
	}, 1)
	go func() {
		stream, err := NewBoundedUpstreamSSEStream(source, "", 64)
		result <- struct {
			stream *BoundedUpstreamSSEStream
			err    error
		}{stream: stream, err: err}
	}()

	var created struct {
		stream *BoundedUpstreamSSEStream
		err    error
	}
	select {
	case created = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE construction waited for four bytes after receiving a complete heartbeat line")
	}
	if created.err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", created.err)
	}
	event, err := created.stream.ReadEvent()
	if err != nil || event == nil || len(event) != 0 {
		t.Fatalf("heartbeat = %#v, error = %v", event, err)
	}
	if err = created.stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestBoundedUpstreamSSEStreamClassifiesTruncatedCompressionAsProtocolFailure(t *testing.T) {
	t.Parallel()

	encoded := truncateTail(encodeUpstreamGzip(t, []byte("data: one\n\n")), 4)
	stream, err := NewBoundedUpstreamSSEStream(
		&countingReadCloser{Reader: bytes.NewReader(encoded)},
		"",
		64,
	)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err = stream.ReadEvent(); err != nil {
		t.Fatalf("first ReadEvent() error = %v", err)
	}
	_, err = stream.ReadEvent()
	assertUpstreamProtocolFailure(t, err, upstreamResponseDecodeFailedCode)
}

func TestBoundedUpstreamSSEStreamPreservesCompressedTransportReset(t *testing.T) {
	t.Parallel()

	encoded := truncateTail(encodeUpstreamGzip(t, []byte("data: one\n\n")), 4)
	stream, err := NewBoundedUpstreamSSEStream(
		&countingReadCloser{Reader: &dataThenErrorReader{data: encoded, err: syscall.ECONNRESET}},
		"gzip",
		64,
	)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err = stream.ReadEvent(); err != nil {
		t.Fatalf("first ReadEvent() error = %v", err)
	}
	_, err = stream.ReadEvent()
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.TransportError || typed.Scope != failurecontract.ScopeProvider || !typed.Retryable {
		t.Fatalf("failure = %#v, want retryable transport/provider", typed)
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal("typed failure does not preserve connection reset")
	}
}

func TestBoundedUpstreamSSEStreamClosesBodyOnDecodeFailure(t *testing.T) {
	t.Parallel()

	source := &countingReadCloser{Reader: strings.NewReader("not gzip")}
	stream, err := NewBoundedUpstreamSSEStream(source, "gzip", 64)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	_, err = stream.ReadEvent()
	assertUpstreamProtocolFailure(t, err, upstreamResponseDecodeFailedCode)
	if err = stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if source.closeCalls != 1 {
		t.Fatalf("body close calls = %d, want 1", source.closeCalls)
	}
}

func TestBoundedUpstreamSSEStreamClassifiesHeaderlessPeekFailureAsTransport(t *testing.T) {
	t.Parallel()

	errReset := errors.New("connection reset")
	stream, err := NewBoundedUpstreamSSEStream(
		&countingReadCloser{Reader: errorReader{err: errReset}},
		"",
		64,
	)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()
	_, err = stream.ReadEvent()
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.TransportError || typed.Scope != failurecontract.ScopeProvider || !typed.Retryable {
		t.Fatalf("failure = %#v, want retryable transport/provider", typed)
	}
	if !errors.Is(err, errReset) {
		t.Fatal("typed failure does not preserve transport cause")
	}
}

func TestBoundedUpstreamSSEStreamReturnsBeforeFirstBodyByteAndCanCancel(t *testing.T) {
	t.Parallel()

	source := newBlockingReadCloser(nil, 1)
	stream, err := NewBoundedUpstreamSSEStream(source, "", 64)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	select {
	case <-source.blocked:
		t.Fatal("SSE constructor read the upstream body before returning")
	default:
	}

	readDone := make(chan error, 1)
	go func() {
		_, errRead := stream.ReadEvent()
		readDone <- errRead
	}()
	select {
	case <-source.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadEvent did not begin reading the upstream body")
	}
	if err = stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case errRead := <-readDone:
		if errRead == nil {
			t.Fatal("ReadEvent returned nil after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not interrupt lazy SSE initialization")
	}
}

func TestBoundedUpstreamSSEStreamCloseInterruptsReadBeforeClosingDecoder(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write([]byte("data: one\n\n")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("gzip flush: %v", err)
	}
	source := newBlockingReadCloser(bytes.Clone(encoded.Bytes()), 1)
	stream, err := NewBoundedUpstreamSSEStream(source, "gzip", 64)
	if err != nil {
		t.Fatalf("NewBoundedUpstreamSSEStream() error = %v", err)
	}
	first, err := stream.ReadEvent()
	if err != nil || string(first) != "data: one\n" {
		t.Fatalf("first event = %q, error = %v", first, err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, errRead := stream.ReadEvent()
		readDone <- errRead
	}()
	select {
	case <-source.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("second read did not block on the upstream body")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- stream.Close() }()
	select {
	case errClose := <-closeDone:
		if errClose != nil {
			t.Fatalf("Close() error = %v", errClose)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not interrupt the active read")
	}
	select {
	case errRead := <-readDone:
		if errRead == nil {
			t.Fatal("active read returned nil after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active read did not return after Close")
	}
}

func TestBoundedSSEReaderPreservesEmergencyCeiling(t *testing.T) {
	t.Parallel()

	if UpstreamSSEEmergencyCeilingBytes != 50<<20 {
		t.Fatalf("emergency ceiling = %d, want 50 MiB", UpstreamSSEEmergencyCeilingBytes)
	}
	if _, err := NewBoundedSSEReader(strings.NewReader(""), UpstreamSSEEmergencyCeilingBytes); err != nil {
		t.Fatalf("emergency ceiling rejected: %v", err)
	}
	if _, err := NewBoundedSSEReader(strings.NewReader(""), UpstreamSSEEmergencyCeilingBytes+1); err == nil {
		t.Fatal("limit above emergency ceiling was accepted")
	}
}

func assertUpstreamProtocolFailure(t *testing.T, err error, wantCode string) {
	t.Helper()
	typed, ok := failurecontract.As(err)
	if !ok {
		t.Fatalf("error = %T %v, want typed failure", err, err)
	}
	if typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider {
		t.Fatalf("failure = %q/%q, want upstream_protocol_error/provider", typed.Kind, typed.Scope)
	}
	if typed.Kind == failurecontract.RequestTooLarge || typed.HTTPStatus == http.StatusRequestEntityTooLarge {
		t.Fatalf("upstream overflow was classified as client request overflow: %#v", typed)
	}
	if typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != wantCode || typed.Retryable {
		t.Fatalf("failure metadata = status:%d code:%q retryable:%t", typed.HTTPStatus, typed.ProviderCode, typed.Retryable)
	}
}

func encodeUpstreamGzip(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return encoded.Bytes()
}

func encodeUpstreamDeflate(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer, err := flate.NewWriter(&encoded, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate.NewWriter: %v", err)
	}
	if _, err = writer.Write(payload); err != nil {
		t.Fatalf("deflate write: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("deflate close: %v", err)
	}
	return encoded.Bytes()
}

func encodeUpstreamBrotli(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := brotli.NewWriter(&encoded)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return encoded.Bytes()
}

func encodeUpstreamZstd(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer, err := zstd.NewWriter(&encoded)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err = writer.Write(payload); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return encoded.Bytes()
}

func truncateTail(data []byte, bytesToRemove int) []byte {
	return data[:len(data)-bytesToRemove]
}

type countingReadCloser struct {
	io.Reader
	bytesRead  int
	closeCalls int
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	read, err := reader.Reader.Read(buffer)
	reader.bytesRead += read
	return read, err
}

func (reader *countingReadCloser) Close() error {
	reader.closeCalls++
	return nil
}

type errorReader struct {
	err error
}

type closeErrorReadCloser struct {
	io.Reader
	err        error
	closeCalls int
}

func (reader *closeErrorReadCloser) Close() error {
	reader.closeCalls++
	return reader.err
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type dataThenErrorReader struct {
	data []byte
	err  error
}

func (reader *dataThenErrorReader) Read(buffer []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, reader.err
	}
	read := copy(buffer, reader.data)
	reader.data = reader.data[read:]
	if len(reader.data) == 0 {
		return read, reader.err
	}
	return read, nil
}

type blockingReadCloser struct {
	reader      *bytes.Reader
	maxChunk    int
	closed      chan struct{}
	blocked     chan struct{}
	closeOnce   sync.Once
	blockedOnce sync.Once
}

func newBlockingReadCloser(data []byte, maxChunk int) *blockingReadCloser {
	return &blockingReadCloser{
		reader:   bytes.NewReader(data),
		maxChunk: maxChunk,
		closed:   make(chan struct{}),
		blocked:  make(chan struct{}),
	}
}

func (reader *blockingReadCloser) Read(buffer []byte) (int, error) {
	if reader.reader.Len() > 0 {
		if reader.maxChunk > 0 && len(buffer) > reader.maxChunk {
			buffer = buffer[:reader.maxChunk]
		}
		return reader.reader.Read(buffer)
	}
	reader.blockedOnce.Do(func() { close(reader.blocked) })
	<-reader.closed
	return 0, io.ErrClosedPipe
}

func (reader *blockingReadCloser) Close() error {
	reader.closeOnce.Do(func() { close(reader.closed) })
	return nil
}
