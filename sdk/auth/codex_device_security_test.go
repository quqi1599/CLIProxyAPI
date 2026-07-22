package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type codexDeviceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexDeviceRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexDeviceErrorsDoNotExposeOAuthBody(t *testing.T) {
	const secret = "codex-device-oauth-sentinel"
	client := &http.Client{Transport: codexDeviceRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"` + secret + `"}`)),
			Request:    req,
		}, nil
	})}

	_, userCodeErr := requestCodexDeviceUserCode(context.Background(), client)
	_, pollErr := pollCodexDeviceToken(context.Background(), client, "device", "code", time.Millisecond)
	for _, err := range []error{userCodeErr, pollErr} {
		if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), `"sha256":`) || !strings.Contains(err.Error(), `"content_type":"application/json"`) {
			t.Fatalf("unsafe device OAuth error: %v", err)
		}
	}
}
