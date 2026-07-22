package executor

import (
	"errors"
	"io"
	"net/http"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

type trackedExecutorRequestBody struct {
	remaining int64
	readBytes int64
	closes    int
}

type failingExecutorRequestBody struct {
	closes int
}

func (b *failingExecutorRequestBody) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func (b *failingExecutorRequestBody) Close() error {
	b.closes++
	return nil
}

func (b *trackedExecutorRequestBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	clear(p[:n])
	b.remaining -= n
	b.readBytes += n
	return int(n), nil
}

func (b *trackedExecutorRequestBody) Close() error {
	b.closes++
	return nil
}

func TestExecutorHTTPRequestSanitizersRejectKnownOversizeAndCloseBody(t *testing.T) {
	tests := []struct {
		name     string
		sanitize func(*http.Request) error
	}{
		{
			name: "openai compat",
			sanitize: func(req *http.Request) error {
				return sanitizeOpenAICompatHTTPRequestBody(req, openAICompatProfileForKind("newapi"), "https://example.test/v1")
			},
		},
		{name: "kimi", sanitize: sanitizeKimiHTTPRequestBody},
		{
			name: "claude",
			sanitize: func(req *http.Request) error {
				_, err := sanitizeClaudeHTTPRequestToolNames(req)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &trackedExecutorRequestBody{remaining: 1}
			req, errNew := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", source)
			if errNew != nil {
				t.Fatalf("new request: %v", errNew)
			}
			req.ContentLength = executorHTTPRequestBodyBytes + 1

			err := test.sanitize(req)
			assertExecutorRequestTooLarge(t, err)
			if source.readBytes != 0 {
				t.Fatalf("read bytes = %d, want preflight rejection", source.readBytes)
			}
			if source.closes != 1 {
				t.Fatalf("close count = %d, want 1", source.closes)
			}
		})
	}
}

func TestReadAndCloseExecutorHTTPRequestBodyStopsAtLimitPlusOne(t *testing.T) {
	source := &trackedExecutorRequestBody{remaining: executorHTTPRequestBodyBytes + 2}
	req, errNew := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", source)
	if errNew != nil {
		t.Fatalf("new request: %v", errNew)
	}
	req.ContentLength = -1

	body, err := readAndCloseExecutorHTTPRequestBody(req, "test executor")
	assertExecutorRequestTooLarge(t, err)
	if body != nil {
		t.Fatalf("body length = %d, want nil", len(body))
	}
	if source.readBytes != executorHTTPRequestBodyBytes+1 {
		t.Fatalf("read bytes = %d, want %d", source.readBytes, executorHTTPRequestBodyBytes+1)
	}
	if source.closes != 1 {
		t.Fatalf("close count = %d, want 1", source.closes)
	}
}

func TestReadAndCloseExecutorHTTPRequestBodyClosesOnReadError(t *testing.T) {
	source := &failingExecutorRequestBody{}
	req, errNew := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", source)
	if errNew != nil {
		t.Fatalf("new request: %v", errNew)
	}

	body, err := readAndCloseExecutorHTTPRequestBody(req, "test executor")
	if body != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body=%q error=%v, want read error", body, err)
	}
	if source.closes != 1 {
		t.Fatalf("close count = %d, want 1", source.closes)
	}
}

func assertExecutorRequestTooLarge(t *testing.T, err error) {
	t.Helper()
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.RequestTooLarge || typed.Scope != failurecontract.ScopeRequest || typed.HTTPStatus != http.StatusRequestEntityTooLarge || typed.ProviderCode != "request_too_large" {
		t.Fatalf("error = %#v, want typed request-too-large", err)
	}
}
