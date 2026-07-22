package helps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

type groundingURLRoundTripper func(*http.Request) (*http.Response, error)

func (f groundingURLRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestResolveAntigravityGroundingURLsLargePayloadResolvesDuplicateOnce(t *testing.T) {
	t.Parallel()

	const (
		items       = 256
		redirectURL = "https://vertexaisearch.cloud.google.com/grounding-api-redirect/shared-token"
		resolvedURL = "https://example.com/shared"
	)
	requests := 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", groundingURLRoundTripper(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{resolvedURL}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}))

	var builder strings.Builder
	builder.WriteString(`{"before":{"keep":true},"response":{"candidates":[{"groundingMetadata":{"groundingChunks":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&builder, `{"web":{"uri":"%s","title":"title-%d","unknown":%d},"chunk_unknown":%d}`, redirectURL, i, i, i)
	}
	builder.WriteString(`]}}]},"after":{"keep":true}}`)
	input := []byte(builder.String())
	original := bytes.Clone(input)

	output := ResolveAntigravityGroundingURLs(ctx, nil, nil, input)
	if requests != 1 {
		t.Fatalf("redirect requests = %d, want 1", requests)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("ResolveAntigravityGroundingURLs mutated its input")
	}
	chunks := gjson.GetBytes(output, "response.candidates.0.groundingMetadata.groundingChunks")
	if got := len(chunks.Array()); got != items {
		t.Fatalf("chunks = %d, want %d", got, items)
	}
	if got := chunks.Get("255.web.uri").String(); got != resolvedURL {
		t.Fatalf("last uri = %q, want %q", got, resolvedURL)
	}
	if got := chunks.Get("255.web.unknown").Int(); got != 255 {
		t.Fatalf("last unknown field = %d, want 255", got)
	}
	assertTopLevelOrder(t, output, `"before"`, `"response"`, `"after"`)
}

func TestResolveAntigravityGroundingURLsResolvesVertexRedirects(t *testing.T) {
	t.Parallel()

	const redirectURL = "https://vertexaisearch.cloud.google.com/grounding-api-redirect/example-token"
	const resolvedURL = "https://example.com/weather"

	var sawRedirectRequest bool
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", groundingURLRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodHead {
			t.Fatalf("method = %s, want HEAD", req.Method)
		}
		if req.URL.String() != redirectURL {
			t.Fatalf("url = %s, want %s", req.URL.String(), redirectURL)
		}
		sawRedirectRequest = true
		return &http.Response{
			StatusCode: http.StatusFound,
			Header: http.Header{
				"Location": []string{resolvedURL},
			},
			Body: io.NopCloser(strings.NewReader("")),
		}, nil
	}))

	input := []byte(`{
		"response": {
			"candidates": [{
				"groundingMetadata": {
					"groundingChunks": [
						{"web": {"uri": "` + redirectURL + `", "title": "Weather"}},
						{"web": {"uri": "https://already.example/source", "title": "Existing"}}
					]
				}
			}]
		}
	}`)

	output := ResolveAntigravityGroundingURLs(ctx, nil, nil, input)
	if !sawRedirectRequest {
		t.Fatal("expected resolver to request the vertex redirect")
	}
	if got := gjson.GetBytes(output, "response.candidates.0.groundingMetadata.groundingChunks.0.web.uri").String(); got != resolvedURL {
		t.Fatalf("resolved uri = %q, want %q; output=%s", got, resolvedURL, output)
	}
	if got := gjson.GetBytes(output, "response.candidates.0.groundingMetadata.groundingChunks.1.web.uri").String(); got != "https://already.example/source" {
		t.Fatalf("non-vertex uri = %q", got)
	}
}
