package httpfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	log "github.com/sirupsen/logrus"
)

// Doer abstracts the HTTP client used to execute requests.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// ResponseTooLargeError reports a bounded response read overflow.
type ResponseTooLargeError struct {
	Limit int64
}

// ErrorBodyMetadata returns deterministic body metadata without exposing raw
// upstream response content, OAuth tokens, prompts, or provider diagnostics.
func ErrorBodyMetadata(contentType string, body []byte) string {
	return internalpayload.SummarizeBodyMetadata(body, contentType)
}

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("response exceeds maximum allowed size of %d bytes", e.Limit)
}

// ReadBytes reads at most maxSize+1 bytes so oversized responses are rejected
// without buffering the complete body. A positive limit is mandatory. It does
// not close reader.
func ReadBytes(reader io.Reader, maxSize int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	if maxSize <= 0 {
		return nil, fmt.Errorf("maximum response size must be positive")
	}
	data, errRead := io.ReadAll(io.LimitReader(reader, maxSize+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(data)) > maxSize {
		return nil, &ResponseTooLargeError{Limit: maxSize}
	}
	return data, nil
}

// ReadResponseBytes consumes and closes response.Body exactly once. It rejects
// an oversized declared Content-Length before reading and still enforces the
// limit for chunked or otherwise unknown-length responses.
func ReadResponseBytes(response *http.Response, maxSize int64) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close response body")
		}
	}()

	if maxSize > 0 && response.ContentLength > maxSize {
		return nil, &ResponseTooLargeError{Limit: maxSize}
	}
	return ReadBytes(response.Body, maxSize)
}

// GetBytes performs a GET request with the supplied headers, requires a
// success status, and returns the response body. maxSize must be positive and
// the body is rejected once it exceeds that many bytes.
func GetBytes(ctx context.Context, client Doer, requestURL string, headers map[string]string, maxSize int64) ([]byte, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("maximum response size must be positive")
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create request: %w", errRequest)
	}
	for key, value := range headers {
		if value != "" {
			req.Header.Set(key, value)
		}
	}

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request failed: %w", errDo)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("failed to close response body")
			}
		}()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, ErrorBodyMetadata(resp.Header.Get("Content-Type"), body))
	}

	data, errRead := ReadResponseBytes(resp, maxSize)
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	return data, nil
}
