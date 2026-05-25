package helps

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

type stubRoundTripper func(*http.Request) (*http.Response, error)

func (s stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return s(req) }

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := UnwrapTransportErrorsRoundTripper(client.Transport).(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientReusesAuthProxyTransport(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{ProxyURL: "socks5://auth-proxy-cache.example.com:1080"}
	clientOne := NewProxyAwareHTTPClient(context.Background(), nil, auth, 0)
	clientTwo := NewProxyAwareHTTPClient(context.Background(), nil, auth, 0)

	if clientOne.Transport == nil {
		t.Fatal("expected first client transport to be set")
	}
	if clientTwo.Transport == nil {
		t.Fatal("expected second client transport to be set")
	}
	if UnwrapTransportErrorsRoundTripper(clientOne.Transport) != UnwrapTransportErrorsRoundTripper(clientTwo.Transport) {
		t.Fatal("expected auth proxy transport to be reused")
	}
}

func TestNewProxyAwareHTTPClientReusesGlobalProxyTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy-cache.example.com:8080"},
	}
	clientOne := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	clientTwo := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)

	if clientOne.Transport == nil {
		t.Fatal("expected first client transport to be set")
	}
	if clientTwo.Transport == nil {
		t.Fatal("expected second client transport to be set")
	}
	if UnwrapTransportErrorsRoundTripper(clientOne.Transport) != UnwrapTransportErrorsRoundTripper(clientTwo.Transport) {
		t.Fatal("expected global proxy transport to be reused")
	}
}

func TestNewProxyAwareHTTPClientWrapsMalformedTransportErrors(t *testing.T) {
	t.Parallel()

	rawErr := errors.New(`net/http: HTTP/1.x transport connection broken: malformed HTTP response "0"`)
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", stubRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, rawErr
	}))
	client := NewProxyAwareHTTPClient(ctx, nil, nil, 0)
	req, errReq := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}

	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected wrapped transport error")
	}
	if !cliproxyauth.IsRetryableTransportError(err) {
		t.Fatalf("expected retryable transport error, got %v", err)
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("status = %v, want %d", err, http.StatusBadGateway)
	}
	var code interface{ ErrorCode() string }
	if !errors.As(err, &code) || code.ErrorCode() != "transport_error" {
		t.Fatalf("error code = %q, want %q", code.ErrorCode(), "transport_error")
	}
	if !errors.Is(err, rawErr) {
		t.Fatal("expected wrapped error to preserve original cause")
	}
}

func TestNewProxyAwareHTTPClientWrapsProxyDialFailures(t *testing.T) {
	t.Parallel()

	rawErr := &proxyutil.ProxyDialError{Scheme: "socks5", Host: "127.0.0.1:1080", Err: errors.New("connect: connection refused")}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", stubRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, rawErr
	}))
	client := NewProxyAwareHTTPClient(ctx, nil, nil, 0)
	req, errReq := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}

	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected wrapped proxy dial error")
	}
	var code interface{ ErrorCode() string }
	if !errors.As(err, &code) || code.ErrorCode() != "proxy_dial_failed" {
		t.Fatalf("error code = %q, want %q", code.ErrorCode(), "proxy_dial_failed")
	}
	var proxyErr *proxyutil.ProxyDialError
	if !errors.As(err, &proxyErr) || proxyErr == nil {
		t.Fatal("expected wrapped error to preserve proxy dial cause")
	}
}
