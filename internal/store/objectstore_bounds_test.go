package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpfetch"
)

func TestReadBoundedBytesAcceptsBoundedObjectAndClosesReader(t *testing.T) {
	reader := &objectReadCloser{Reader: bytes.NewReader([]byte("token"))}

	got, errRead := readBoundedBytes(reader, 5, 5)
	if errRead != nil {
		t.Fatalf("readBoundedBytes() error = %v", errRead)
	}
	if string(got) != "token" {
		t.Fatalf("readBoundedBytes() = %q, want token", got)
	}
	if reader.closed != 1 {
		t.Fatalf("reader close count = %d, want 1", reader.closed)
	}
}

func TestReadBoundedBytesRejectsUnknownSizeOverflowAndClosesReader(t *testing.T) {
	const limit = int64(4)
	reader := &objectReadCloser{Reader: bytes.NewReader([]byte("chunked"))}

	got, errRead := readBoundedBytes(reader, -1, limit)
	var tooLarge *httpfetch.ResponseTooLargeError
	if !errors.As(errRead, &tooLarge) {
		t.Fatalf("readBoundedBytes() error = %#v, want ResponseTooLargeError", errRead)
	}
	if got != nil {
		t.Fatalf("readBoundedBytes() data = %q, want nil", got)
	}
	if reader.bytesRead != limit+1 {
		t.Fatalf("reader bytes read = %d, want %d", reader.bytesRead, limit+1)
	}
	if reader.closed != 1 {
		t.Fatalf("reader close count = %d, want 1", reader.closed)
	}
}

func TestReadBoundedBytesRejectsDeclaredSizeBeforeReadingAndClosesReader(t *testing.T) {
	const limit = int64(4)
	reader := &objectReadCloser{Reader: bytes.NewReader([]byte("ignored"))}

	got, errRead := readBoundedBytes(reader, limit+1, limit)
	var tooLarge *httpfetch.ResponseTooLargeError
	if !errors.As(errRead, &tooLarge) {
		t.Fatalf("readBoundedBytes() error = %#v, want ResponseTooLargeError", errRead)
	}
	if got != nil {
		t.Fatalf("readBoundedBytes() data = %q, want nil", got)
	}
	if reader.bytesRead != 0 {
		t.Fatalf("reader bytes read = %d, want 0", reader.bytesRead)
	}
	if reader.closed != 1 {
		t.Fatalf("reader close count = %d, want 1", reader.closed)
	}
}

func TestReadFileBytesAcceptsExactLimit(t *testing.T) {
	const limit = int64(4)
	path := filepath.Join(t.TempDir(), "exact.json")
	if errWrite := os.WriteFile(path, []byte("1234"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	got, errRead := readFileBytes(path, limit)
	if errRead != nil {
		t.Fatalf("readFileBytes() error = %v", errRead)
	}
	if string(got) != "1234" {
		t.Fatalf("readFileBytes() = %q, want 1234", got)
	}
}

func TestReadFileBytesRejectsLimitPlusOne(t *testing.T) {
	const limit = int64(4)
	path := filepath.Join(t.TempDir(), "oversized.json")
	if errWrite := os.WriteFile(path, []byte("12345"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	got, errRead := readFileBytes(path, limit)
	var tooLarge *httpfetch.ResponseTooLargeError
	if !errors.As(errRead, &tooLarge) {
		t.Fatalf("readFileBytes() error = %#v, want ResponseTooLargeError", errRead)
	}
	if got != nil {
		t.Fatalf("readFileBytes() data = %q, want nil", got)
	}
}

type objectReadCloser struct {
	io.Reader
	bytesRead int64
	closed    int
}

func (r *objectReadCloser) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func (r *objectReadCloser) Close() error {
	r.closed++
	return nil
}
