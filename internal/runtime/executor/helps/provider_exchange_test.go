package helps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestProviderExchangeDoAndReadBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginContext)
	requestBody := []byte(`{"prompt":"request-secret"}`)
	responseBody := &providerExchangeCountingReadCloser{Reader: strings.NewReader(`{"ok":true}`)}
	transport := providerExchangeRoundTripper(func(request *http.Request) (*http.Response, error) {
		if _, hasDeadline := request.Context().Deadline(); hasDeadline {
			t.Fatal("provider exchange added a request deadline")
		}
		if request.Method != http.MethodPost || request.URL.String() != "https://upstream.example/v1/messages" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer request-secret" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		gotBody, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		if !bytes.Equal(gotBody, requestBody) {
			t.Fatalf("request body = %q, want %q", gotBody, requestBody)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Content-Type": {"application/json"}, "X-Upstream-Request-Id": {"upstream-1"}},
			Body:       responseBody,
			Request:    request,
		}, nil
	})
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	auth := &cliproxyauth.Auth{ID: "auth-1", Label: "primary", Attributes: map[string]string{"api_key": "auth-secret"}}
	reporter := NewUsageReporter(ctx, "claude", "claude-test", auth)

	exchange := ProviderExchange{Config: cfg, Auth: auth, Provider: "claude", Reporter: reporter}
	result, errDo := exchange.Do(ctx, ProviderExchangeRequest{
		Method: http.MethodPost,
		URL:    "https://upstream.example/v1/messages",
		Body:   requestBody,
		ApplyHeaders: func(request *http.Request) error {
			request.Header.Set("Authorization", "Bearer request-secret")
			request.Header.Set("Content-Type", "application/json")
			return nil
		},
	})
	if errDo != nil {
		t.Fatalf("ProviderExchange.Do() error = %v", errDo)
	}
	if result.ResponseLog == nil {
		t.Fatal("response log runtime is nil")
	}
	got, errRead := result.ReadBounded(UpstreamBodyLimits{SuccessBytes: 64})
	if errRead != nil {
		t.Fatalf("ReadBounded() error = %v", errRead)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("response body = %q", got)
	}
	if responseBody.closeCalls != 1 {
		t.Fatalf("response body close calls = %d, want 1", responseBody.closeCalls)
	}
	if !reporter.ttftSet {
		t.Fatal("usage reporter did not observe the first response byte")
	}

	requestLog, ok := ginContext.Get(apiRequestKey)
	if !ok {
		t.Fatal("request log missing")
	}
	requestLogBytes, ok := requestLog.([]byte)
	if !ok {
		t.Fatalf("request log type = %T, want []byte", requestLog)
	}
	responseLog := MaterializeAPIResponse(ginContext)
	for name, logBody := range map[string][]byte{"request": requestLogBytes, "response": responseLog} {
		if bytes.Contains(logBody, []byte("request-secret")) || bytes.Contains(logBody, []byte("auth-secret")) {
			t.Fatalf("%s log leaked a secret: %s", name, logBody)
		}
	}
	if !bytes.Contains(responseLog, []byte("Status: 201")) || !bytes.Contains(responseLog, []byte(`"bytes":11`)) {
		t.Fatalf("response log missing bounded metadata: %s", responseLog)
	}
}

func TestProviderExchangeReadBoundedRecordsErrorOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginContext)
	responseBody := &providerExchangeCountingReadCloser{Reader: strings.NewReader("12345")}
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(providerExchangeRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       responseBody,
			Request:    request,
		}, nil
	})))
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	result, errDo := (ProviderExchange{Config: cfg, Provider: "claude"}).Do(ctx, ProviderExchangeRequest{
		Method: http.MethodPost,
		URL:    "https://upstream.example/v1/messages",
		Body:   []byte(`{}`),
	})
	if errDo != nil {
		t.Fatalf("ProviderExchange.Do() error = %v", errDo)
	}
	_, errRead := result.ReadBounded(UpstreamBodyLimits{SuccessBytes: 4})
	if errRead == nil {
		t.Fatal("ReadBounded() error = nil, want limit failure")
	}
	if responseBody.closeCalls != 1 {
		t.Fatalf("response body close calls = %d, want 1", responseBody.closeCalls)
	}
	responseLog := string(MaterializeAPIResponse(ginContext))
	if got := strings.Count(responseLog, "Error: upstream request failed"); got != 1 {
		t.Fatalf("response error entries = %d, want 1; log=%s", got, responseLog)
	}
}

func TestProviderExchangeDoTransportErrorRecordsOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginContext)
	errTransport := errors.New("transport failed")
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(providerExchangeRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errTransport
	})))
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	_, errDo := (ProviderExchange{Config: cfg, Provider: "claude"}).Do(ctx, ProviderExchangeRequest{
		Method: http.MethodPost,
		URL:    "https://upstream.example/v1/messages",
		Body:   []byte(`{}`),
	})
	if !errors.Is(errDo, errTransport) {
		t.Fatalf("ProviderExchange.Do() error = %v, want %v", errDo, errTransport)
	}
	responseLog := string(MaterializeAPIResponse(ginContext))
	if got := strings.Count(responseLog, "Error: upstream request failed"); got != 1 {
		t.Fatalf("response error entries = %d, want 1; log=%s", got, responseLog)
	}
}

type providerExchangeRoundTripper func(*http.Request) (*http.Response, error)

func (roundTripper providerExchangeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripper(request)
}

type providerExchangeCountingReadCloser struct {
	io.Reader
	closeCalls int
}

func (reader *providerExchangeCountingReadCloser) Close() error {
	reader.closeCalls++
	return nil
}
