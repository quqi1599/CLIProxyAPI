package httpfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

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

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("response exceeds maximum allowed size of %d bytes", e.Limit)
}

// ReadBytes reads at most maxSize+1 bytes so oversized responses are rejected
// without buffering the complete body. It does not close reader.
func ReadBytes(reader io.Reader, maxSize int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	if maxSize <= 0 {
		return io.ReadAll(reader)
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

// GetBytes performs a GET request with the supplied headers, requires a
// success status, and returns the response body. When maxSize is positive
// the body is rejected once it exceeds maxSize bytes.
func GetBytes(ctx context.Context, client Doer, requestURL string, headers map[string]string, maxSize int64) ([]byte, error) {
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
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close response body")
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	data, errRead := ReadBytes(resp.Body, maxSize)
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	return data, nil
}
