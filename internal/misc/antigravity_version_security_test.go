package misc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAntigravityInvalidVersionErrorsDoNotExposeBody(t *testing.T) {
	const secret = "antigravity-version-sentinel"
	tests := []struct {
		name string
		body string
		call func(context.Context, *http.Client) (string, error)
	}{
		{name: "manifest", body: `{"version":"` + secret + `"}`, call: fetchAntigravityCLIUpdaterManifestVersion},
		{name: "latest", body: secret, call: fetchAntigravityCLILatestVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}
			_, err := tc.call(context.Background(), client)
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), `"sha256":`) || !strings.Contains(err.Error(), `"content_type":"application/json"`) {
				t.Fatalf("unsafe version error: %v", err)
			}
		})
	}
}
