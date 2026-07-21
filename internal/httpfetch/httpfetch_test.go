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
	}, 0)
	if errGet != nil {
		t.Fatalf("GetBytes() error = %v", errGet)
	}
	if string(data) != "payload" {
		t.Fatalf("GetBytes() = %q, want payload", data)
	}
}

func TestGetBytesRejectsErrorStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	_, errGet := GetBytes(context.Background(), server.Client(), server.URL, nil, 0)
	if errGet == nil {
		t.Fatal("GetBytes() error = nil")
	}
	if !strings.Contains(errGet.Error(), "unexpected status 404") {
		t.Fatalf("GetBytes() error = %v, want status 404", errGet)
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

type countingReader struct {
	io.Reader
	read int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}
