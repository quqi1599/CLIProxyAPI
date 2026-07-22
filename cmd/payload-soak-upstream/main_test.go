package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFakeUpstreamDeterministicResponses(t *testing.T) {
	server := newTestUpstreamServer(t)
	client := server.Client()
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "normal", status: http.StatusOK, body: upstreamMarker},
		{name: "rate_limit_429", status: http.StatusTooManyRequests, body: "soak_rate_limit"},
		{name: "upstream_5xx", status: http.StatusServiceUnavailable, body: "soak_upstream_5xx"},
		{name: "malformed_sse", status: http.StatusOK, body: "malformed SSE sentinel"},
		{name: "downstream_stream", status: http.StatusOK, body: "data: [DONE]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := client.Do(upstreamRequest(t, server.URL, test.name, test.name == "malformed_sse" || test.name == "downstream_stream"))
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			body, errRead := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if errRead != nil {
				t.Fatalf("read error = %v", errRead)
			}
			if response.StatusCode != test.status || !strings.Contains(string(body), test.body) {
				t.Fatalf("response = status %d body %q, want status %d containing %q", response.StatusCode, body, test.status, test.body)
			}
		})
	}
}

func TestFakeUpstreamSlowAndBadGzip(t *testing.T) {
	server := newTestUpstreamServer(t)
	started := time.Now()
	response, err := server.Client().Do(upstreamRequest(t, server.URL, "slow_upstream", false))
	if err != nil {
		t.Fatalf("slow request error = %v", err)
	}
	_ = response.Body.Close()
	if elapsed := time.Since(started); elapsed < 8*time.Millisecond {
		t.Fatalf("slow response arrived after %s, want at least 8ms", elapsed)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()
	response, err = client.Do(upstreamRequest(t, server.URL, "bad_gzip", false))
	if err != nil {
		t.Fatalf("bad gzip request error = %v", err)
	}
	body, errRead := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if errRead != nil || response.Header.Get("Content-Encoding") != "gzip" || string(body) != "not-a-gzip-stream" {
		t.Fatalf("bad gzip fixture = encoding %q body %q error %v", response.Header.Get("Content-Encoding"), body, errRead)
	}
}

func TestFakeUpstreamHalfCloseAndReset(t *testing.T) {
	server := newTestUpstreamServer(t)
	response, err := server.Client().Do(upstreamRequest(t, server.URL, "http_half_close", false))
	if err != nil {
		t.Fatalf("half-close request error = %v", err)
	}
	_, errRead := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if errRead == nil {
		t.Fatal("half-close response unexpectedly had a complete body")
	}

	response, err = server.Client().Do(upstreamRequest(t, server.URL, "http_reset", false))
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("reset request unexpectedly succeeded")
	}
}

func TestFakeUpstreamCancelStreamObservesClientContext(t *testing.T) {
	server := newTestUpstreamServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	request := upstreamRequest(t, server.URL, "client_mid_stream_cancel", true).WithContext(ctx)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("cancel stream request error = %v", err)
	}
	buffer := make([]byte, 1024)
	read, errRead := response.Body.Read(buffer)
	if read == 0 || (errRead != nil && errRead != io.EOF) {
		t.Fatalf("first stream read = %d, %v", read, errRead)
	}
	cancel()
	_ = response.Body.Close()
}

func TestFakeUpstreamRejectsWrongCredential(t *testing.T) {
	server := newTestUpstreamServer(t)
	request := upstreamRequest(t, server.URL, "normal", false)
	request.Header.Set("Authorization", "Bearer wrong")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

func newTestUpstreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newFakeUpstreamHandler(upstreamConfig{
		apiKey:         defaultAPIKey,
		slowDelay:      10 * time.Millisecond,
		streamInterval: time.Millisecond,
	}))
	t.Cleanup(server.Close)
	return server
}

func upstreamRequest(t *testing.T, baseURL, scenario string, stream bool) *http.Request {
	t.Helper()
	body := []byte(`{"model":"payload-soak-model","messages":[{"role":"user","content":"soak-scenario:` + scenario + `"}],"stream":`)
	if stream {
		body = append(body, "true}"...)
	} else {
		body = append(body, "false}"...)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+defaultAPIKey)
	request.Header.Set("Content-Type", "application/json")
	return request
}
