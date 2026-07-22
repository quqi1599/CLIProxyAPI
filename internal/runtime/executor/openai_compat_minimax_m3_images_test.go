package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type miniMaxM3RoundTripFunc func(*http.Request) (*http.Response, error)

func (f miniMaxM3RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAICompatExecutorMiniMaxM3InlinesRemoteImageURL(t *testing.T) {
	oldFetch := fetchMiniMaxM3ImageURL
	var fetched []string
	fetchMiniMaxM3ImageURL = func(_ context.Context, rawURL string) (string, []byte, bool) {
		fetched = append(fetched, rawURL)
		return "image/png", []byte("png-bytes"), true
	}
	t.Cleanup(func() {
		fetchMiniMaxM3ImageURL = oldFetch
	})

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("minimax", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test-key",
		"compat_kind": "minimax",
	}}

	payload := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"https://cdn.example.com/cat.png"}},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAAA"}}]}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "MiniMax-M3",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if len(fetched) != 1 || fetched[0] != "https://cdn.example.com/cat.png" {
		t.Fatalf("fetched URLs = %v, want only remote image", fetched)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.1.image_url.url").String(); got != "data:image/png;base64,cG5nLWJ5dGVz" {
		t.Fatalf("image_url.url = %q, want inlined data URL; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.2.image_url.url").String(); got != "data:image/jpeg;base64,AAAA" {
		t.Fatalf("existing data URL changed to %q; body=%s", got, string(gotBody))
	}
}

func TestSanitizeOpenAICompatHTTPRequestBodyMiniMaxM3InlinesRemoteImageURL(t *testing.T) {
	oldFetch := fetchMiniMaxM3ImageURL
	fetchMiniMaxM3ImageURL = func(_ context.Context, rawURL string) (string, []byte, bool) {
		if rawURL != "https://cdn.example.com/cat.png" {
			t.Fatalf("unexpected fetch URL %q", rawURL)
		}
		return "image/webp", []byte("webp-bytes"), true
	}
	t.Cleanup(func() {
		fetchMiniMaxM3ImageURL = oldFetch
	})

	payload := `{"model":"MiniMax-M3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://cdn.example.com/cat.png"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "https://api.minimax.io/v1/chat/completions", strings.NewReader(payload))

	if err := sanitizeOpenAICompatHTTPRequestBody(req, openAICompatProfileForKind("minimax"), "https://api.minimax.io/v1"); err != nil {
		t.Fatalf("sanitizeOpenAICompatHTTPRequestBody() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := gjson.GetBytes(body, "messages.0.content.0.image_url.url").String(); got != "data:image/webp;base64,d2VicC1ieXRlcw==" {
		t.Fatalf("image_url.url = %q, want inlined data URL; body=%s", got, string(body))
	}
}

func TestSanitizeOpenAICompatHTTPRequestBodyMiniMaxM3AllowsDeclaredImageExpansion(t *testing.T) {
	oldFetch := fetchMiniMaxM3ImageURL
	fetchMiniMaxM3ImageURL = func(context.Context, string) (string, []byte, bool) {
		return "image/png", bytes.Repeat([]byte{0xab}, 512<<10), true
	}
	t.Cleanup(func() {
		fetchMiniMaxM3ImageURL = oldFetch
	})

	payload := `{"model":"MiniMax-M3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://cdn.example.com/cat.png"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "https://api.minimax.io/v1/chat/completions", strings.NewReader(payload))
	if err := sanitizeOpenAICompatHTTPRequestBody(req, openAICompatProfileForKind("minimax"), "https://api.minimax.io/v1"); err != nil {
		t.Fatalf("sanitizeOpenAICompatHTTPRequestBody() error = %v", err)
	}
	body, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatalf("read body: %v", errRead)
	}
	if observation := internalpayload.ObserveAmplification(int64(len(payload)), int64(len(body)), internalpayload.AmplificationOverride{}); !observation.Exceeded {
		t.Fatalf("fixture output = %d bytes, want it above default allowance %d", len(body), observation.AllowedOutputBytes)
	}
	if observation := internalpayload.ObserveAmplification(int64(len(payload)), int64(len(body)), miniMaxM3InlineAmplificationOverride(true)); observation.Exceeded {
		t.Fatalf("fixture output = %d bytes, declared allowance = %d", len(body), observation.AllowedOutputBytes)
	}
}

func TestOpenAICompatExecutorMiniMaxM3ImageInliningSkipsOtherModels(t *testing.T) {
	oldFetch := fetchMiniMaxM3ImageURL
	fetchCalls := 0
	fetchMiniMaxM3ImageURL = func(_ context.Context, rawURL string) (string, []byte, bool) {
		fetchCalls++
		return "image/png", []byte(rawURL), true
	}
	t.Cleanup(func() {
		fetchMiniMaxM3ImageURL = oldFetch
	})

	var gotBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("minimax", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test-key",
		"compat_kind": "minimax",
	}}

	for _, model := range []string{"MiniMax-M3-highspeed", "MiniMax-M2.7"} {
		payload := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://cdn.example.com/cat.png"}}]}]}`)
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   model,
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}); err != nil {
			t.Fatalf("Execute(%s) error = %v", model, err)
		}
	}

	if fetchCalls != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetchCalls)
	}
	if len(gotBodies) != 2 {
		t.Fatalf("got %d bodies, want 2", len(gotBodies))
	}
	for i, body := range gotBodies {
		if got := gjson.GetBytes(body, "messages.0.content.0.image_url.url").String(); got != "https://cdn.example.com/cat.png" {
			t.Fatalf("body %d image_url.url = %q, want original URL; body=%s", i, got, string(body))
		}
	}
}

func TestInlineMiniMaxM3RemoteImageURLsLimitsImageAttempts(t *testing.T) {
	oldFetch := fetchMiniMaxM3ImageURL
	fetchCalls := 0
	fetchMiniMaxM3ImageURL = func(_ context.Context, rawURL string) (string, []byte, bool) {
		fetchCalls++
		if fetchCalls <= 2 {
			return "", nil, false
		}
		return "image/jpeg", []byte(rawURL), true
	}
	t.Cleanup(func() {
		fetchMiniMaxM3ImageURL = oldFetch
	})

	payload := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/1.jpg"}},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/2.jpg"}},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/3.jpg"}},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/4.jpg"}},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/5.jpg"}}
	]}]}`)
	out, changed := inlineMiniMaxM3RemoteImageURLs(context.Background(), payload, openAICompatProfileForKind("minimax"), "MiniMax-M3")
	if !changed {
		t.Fatal("expected payload to change")
	}
	if fetchCalls != miniMaxM3ImageInlineMaxImages {
		t.Fatalf("fetch calls = %d, want %d", fetchCalls, miniMaxM3ImageInlineMaxImages)
	}
	for i := 0; i < 2; i++ {
		path := "messages.0.content." + strconv.Itoa(i) + ".image_url.url"
		if got := gjson.GetBytes(out, path).String(); got != "https://cdn.example.com/"+strconv.Itoa(i+1)+".jpg" {
			t.Fatalf("content %d changed after failed fetch: %q; body=%s", i, got, string(out))
		}
	}
	for i := 2; i < miniMaxM3ImageInlineMaxImages; i++ {
		path := "messages.0.content." + strconv.Itoa(i) + ".image_url.url"
		if got := gjson.GetBytes(out, path).String(); !strings.HasPrefix(got, "data:image/jpeg;base64,") {
			t.Fatalf("content %d was not inlined: %q; body=%s", i, got, string(out))
		}
	}
	if got := gjson.GetBytes(out, "messages.0.content.4.image_url.url").String(); got != "https://cdn.example.com/5.jpg" {
		t.Fatalf("fifth image = %q, want original URL; body=%s", got, string(out))
	}
}

func TestMiniMaxM3ImageURLAllowedBlocksPrivateTargets(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: "https://example.com/cat.png", want: true},
		{raw: "http://127.0.0.1/cat.png", want: false},
		{raw: "http://10.0.0.1/cat.png", want: false},
		{raw: "http://localhost/cat.png", want: false},
		{raw: "file:///tmp/cat.png", want: false},
		{raw: "https://user:pass@example.com/cat.png", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			parsed, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}
			if got := miniMaxM3ImageURLAllowed(parsed); got != tt.want {
				t.Fatalf("miniMaxM3ImageURLAllowed(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestFetchMiniMaxM3ImageURLDefaultBlocksLoopbackBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer server.Close()

	if mediaType, data, ok := fetchMiniMaxM3ImageURLDefault(context.Background(), server.URL+"/cat.png"); ok {
		t.Fatalf("fetch succeeded unexpectedly: mediaType=%q data=%q", mediaType, string(data))
	}
	if called {
		t.Fatal("loopback image server should not have been requested")
	}
}

func TestMiniMaxM3ImageHTTPClientHasBoundedConnectionPoolWithoutRequestTimeout(t *testing.T) {
	transport, ok := miniMaxM3ImageHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", miniMaxM3ImageHTTPClient.Transport)
	}
	if !transport.DisableCompression {
		t.Fatal("DisableCompression = false, want wire bytes visible to bounded response reader")
	}
	if transport.MaxIdleConns <= 0 || transport.MaxIdleConnsPerHost <= 0 || transport.MaxConnsPerHost <= 0 {
		t.Fatalf("connection limits = idle:%d idle/host:%d total/host:%d, want positive bounds", transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost)
	}
	if transport.IdleConnTimeout <= 0 {
		t.Fatalf("IdleConnTimeout = %s, want positive idle eviction", transport.IdleConnTimeout)
	}
	if miniMaxM3ImageHTTPClient.Timeout != 0 || transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("active request timeouts = client:%s response-header:%s, want none", miniMaxM3ImageHTTPClient.Timeout, transport.ResponseHeaderTimeout)
	}
}

func TestFetchMiniMaxM3ImageURLDefaultReadsCompressedBodyWithinBounds(t *testing.T) {
	oldClient := miniMaxM3ImageHTTPClient
	t.Cleanup(func() {
		miniMaxM3ImageHTTPClient = oldClient
	})

	tests := []struct {
		name    string
		payload []byte
		wantOK  bool
	}{
		{name: "within decoded limit", payload: []byte("png-bytes"), wantOK: true},
		{name: "over decoded limit", payload: bytes.Repeat([]byte{'x'}, miniMaxM3ImageInlineMaxBytes+1), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(tt.payload); err != nil {
				t.Fatalf("write gzip body: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close gzip body: %v", err)
			}

			miniMaxM3ImageHTTPClient = &http.Client{Transport: miniMaxM3RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": {"image/png"}, "Content-Encoding": {"gzip"}},
					Body:          io.NopCloser(bytes.NewReader(compressed.Bytes())),
					ContentLength: int64(compressed.Len()),
					Request:       req,
				}, nil
			})}

			mediaType, data, okFetch := fetchMiniMaxM3ImageURLDefault(context.Background(), "https://example.com/cat.png")
			if okFetch != tt.wantOK {
				t.Fatalf("fetch ok = %v, want %v", okFetch, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if mediaType != "image/png" || !bytes.Equal(data, tt.payload) {
				t.Fatalf("fetch = (%q, %q), want image/png and original payload", mediaType, data)
			}
		})
	}
}
