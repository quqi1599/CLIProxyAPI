package httpfetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetBytesReturnsBodyAndSendsHeaders(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "agent" || r.Header.Get("Accept") != "application/json" {
			http.Error(w, "missing headers", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(server.Close)

	data, errGet := GetBytes(context.Background(), server.Client(), server.URL, map[string]string{
		"User-Agent": "agent",
		"Accept":     "application/json",
	}, 1024)
	if errGet != nil {
		t.Fatalf("GetBytes() error = %v", errGet)
	}
	if string(data) != "payload" {
		t.Fatalf("GetBytes() = %q, want payload", data)
	}
}

func TestGetBytesRejectsErrorStatus(t *testing.T) {
	t.Parallel()

	const secret = "upstream-secret-marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"` + secret + `"}`))
	}))
	t.Cleanup(server.Close)

	_, errGet := GetBytes(context.Background(), server.Client(), server.URL, nil, 1024)
	if errGet == nil {
		t.Fatal("GetBytes() error = nil")
	}
	if !strings.Contains(errGet.Error(), "unexpected status 404") {
		t.Fatalf("GetBytes() error = %v, want status 404", errGet)
	}
	if strings.Contains(errGet.Error(), secret) || !strings.Contains(errGet.Error(), `"sha256":"`) {
		t.Fatalf("GetBytes() exposed raw error body: %v", errGet)
	}
}

func TestReadBytesRejectsMissingLimitWithoutReading(t *testing.T) {
	t.Parallel()

	reader := &countingReader{Reader: strings.NewReader("payload")}
	_, errRead := ReadBytes(reader, 0)
	if errRead == nil || !strings.Contains(errRead.Error(), "must be positive") {
		t.Fatalf("ReadBytes() error = %v, want invalid limit", errRead)
	}
	if reader.read != 0 {
		t.Fatalf("bytes read = %d, want 0", reader.read)
	}
}

func TestGetBytesEnforcesMaxSize(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	t.Cleanup(server.Close)

	_, errGet := GetBytes(context.Background(), server.Client(), server.URL, nil, 4)
	if errGet == nil {
		t.Fatal("GetBytes() error = nil")
	}
	if !strings.Contains(errGet.Error(), "maximum allowed size") {
		t.Fatalf("GetBytes() error = %v, want size limit error", errGet)
	}
	var tooLarge *ResponseTooLargeError
	if !errors.As(errGet, &tooLarge) || tooLarge.Limit != 4 {
		t.Fatalf("GetBytes() error = %#v, want ResponseTooLargeError(limit=4)", errGet)
	}
}

func TestReadBytesStopsAtLimitPlusOne(t *testing.T) {
	t.Parallel()

	reader := &countingReader{Reader: strings.NewReader("123456")}
	_, errRead := ReadBytes(reader, 4)
	var tooLarge *ResponseTooLargeError
	if !errors.As(errRead, &tooLarge) {
		t.Fatalf("ReadBytes() error = %#v, want ResponseTooLargeError", errRead)
	}
	if reader.read != 5 {
		t.Fatalf("bytes read = %d, want limit+1 (5)", reader.read)
	}
}

func TestReadResponseBytesAcceptsLegalResponseAndClosesOnce(t *testing.T) {
	t.Parallel()

	body := &trackingReadCloser{Reader: strings.NewReader("1234")}
	response := &http.Response{Body: body, ContentLength: 4}
	got, errRead := ReadResponseBytes(response, 4)
	if errRead != nil {
		t.Fatalf("ReadResponseBytes() error = %v", errRead)
	}
	if string(got) != "1234" {
		t.Fatalf("ReadResponseBytes() = %q, want 1234", got)
	}
	if body.closes != 1 {
		t.Fatalf("close count = %d, want 1", body.closes)
	}
}

func TestReadResponseBytesRejectsChunkedOverflowAndClosesOnce(t *testing.T) {
	t.Parallel()

	body := &trackingReadCloser{Reader: strings.NewReader("123456")}
	response := &http.Response{Body: body, ContentLength: -1}
	_, errRead := ReadResponseBytes(response, 4)
	var tooLarge *ResponseTooLargeError
	if !errors.As(errRead, &tooLarge) {
		t.Fatalf("ReadResponseBytes() error = %#v, want ResponseTooLargeError", errRead)
	}
	if body.read != 5 {
		t.Fatalf("bytes read = %d, want limit+1 (5)", body.read)
	}
	if body.closes != 1 {
		t.Fatalf("close count = %d, want 1", body.closes)
	}
}

func TestReadResponseBytesRejectsContentLengthBeforeReading(t *testing.T) {
	t.Parallel()

	body := &trackingReadCloser{Reader: strings.NewReader("123456")}
	response := &http.Response{Body: body, ContentLength: 6}
	_, errRead := ReadResponseBytes(response, 4)
	var tooLarge *ResponseTooLargeError
	if !errors.As(errRead, &tooLarge) {
		t.Fatalf("ReadResponseBytes() error = %#v, want ResponseTooLargeError", errRead)
	}
	if body.read != 0 {
		t.Fatalf("bytes read = %d, want 0", body.read)
	}
	if body.closes != 1 {
		t.Fatalf("close count = %d, want 1", body.closes)
	}
}

type countingReader struct {
	io.Reader
	read int
}

type trackingReadCloser struct {
	io.Reader
	read   int
	closes int
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func (r *trackingReadCloser) Close() error {
	r.closes++
	return nil
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}
