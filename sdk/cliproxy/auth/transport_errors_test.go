package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

func TestWrapTransportErrorClassifiesMalformedHTTPResponse(t *testing.T) {
	t.Parallel()

	rawErr := errors.New(`net/http: HTTP/1.x transport connection broken: malformed HTTP response "0"`)
	err := WrapTransportError(rawErr)
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	if !IsRetryableTransportError(err) {
		t.Fatalf("expected retryable transport classification, got %v", err)
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		t.Fatalf("expected auth error view, got %T", err)
	}
	if authErr.Code != "transport_error" {
		t.Fatalf("code = %q, want %q", authErr.Code, "transport_error")
	}
	if authErr.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", authErr.HTTPStatus, http.StatusBadGateway)
	}
	if !authErr.Retryable {
		t.Fatal("expected retryable transport error")
	}
	if !errors.Is(err, rawErr) {
		t.Fatal("expected wrapped error to preserve original cause")
	}
}

func TestWrapTransportErrorClassifiesProxyDialFailures(t *testing.T) {
	t.Parallel()

	rawErr := &proxyutil.ProxyDialError{Scheme: "socks5", Host: "127.0.0.1:1080", Err: io.ErrUnexpectedEOF}
	err := WrapTransportError(rawErr)
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		t.Fatalf("expected auth error view, got %T", err)
	}
	if authErr.Code != "proxy_dial_failed" {
		t.Fatalf("code = %q, want %q", authErr.Code, "proxy_dial_failed")
	}
	var proxyErr *proxyutil.ProxyDialError
	if !errors.As(err, &proxyErr) || proxyErr == nil {
		t.Fatal("expected wrapped error to preserve proxy dial cause")
	}
}

func TestManagerShouldRetryAfterErrorRetriesTransportFailures(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	manager.requestRetry.Store(1)
	if _, err := manager.Register(context.Background(), &Auth{ID: "transport-auth", Provider: "claude"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	wait, shouldRetry := manager.shouldRetryAfterError(WrapTransportError(io.ErrUnexpectedEOF), 0, []string{"claude"}, "claude-sonnet-4-6", time.Minute)
	if !shouldRetry {
		t.Fatal("expected transport failure to be retried")
	}
	if wait != 0 {
		t.Fatalf("wait = %v, want zero immediate retry", wait)
	}
}

func TestResultErrorFromExecutionErrorPreservesTransportMetadata(t *testing.T) {
	t.Parallel()

	got := resultErrorFromExecutionError(WrapTransportError(io.ErrUnexpectedEOF))
	if got == nil {
		t.Fatal("expected result error")
	}
	if got.Code != "transport_error" {
		t.Fatalf("code = %q, want %q", got.Code, "transport_error")
	}
	if got.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", got.HTTPStatus, http.StatusBadGateway)
	}
	if !got.Retryable {
		t.Fatal("expected retryable result error")
	}
}

func TestWrapTransportErrorDoesNotWrapContextCancellation(t *testing.T) {
	t.Parallel()

	err := WrapTransportError(context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation passthrough, got %v", err)
	}
	if IsRetryableTransportError(err) {
		t.Fatal("context cancellation should not be treated as retryable transport failure")
	}
}
