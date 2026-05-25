package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const transportFailureStatusCode = http.StatusBadGateway

type transportWrappedError struct {
	authErr *Error
	cause   error
}

func (e *transportWrappedError) Error() string {
	if e == nil {
		return ""
	}
	if e.authErr != nil && strings.TrimSpace(e.authErr.Message) != "" {
		return e.authErr.Error()
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	return ""
}

func (e *transportWrappedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *transportWrappedError) As(target any) bool {
	if target == nil || e == nil || e.authErr == nil {
		return false
	}
	authErrTarget, ok := target.(**Error)
	if !ok {
		return false
	}
	*authErrTarget = e.authErr
	return true
}

func (e *transportWrappedError) StatusCode() int {
	if e == nil || e.authErr == nil {
		return 0
	}
	return e.authErr.StatusCode()
}

func (e *transportWrappedError) ProviderStatusCode() int { return 0 }

func (e *transportWrappedError) ErrorCode() string {
	if e == nil || e.authErr == nil {
		return ""
	}
	return e.authErr.Code
}

// WrapTransportError classifies retryable network and transport failures so
// scheduler retry and health cooling can treat them like transient upstream 502s.
func WrapTransportError(err error) error {
	code, ok := classifyRetryableTransportError(err)
	if !ok {
		return err
	}
	if wrapped := existingTransportWrappedError(err); wrapped != nil {
		return wrapped
	}
	return &transportWrappedError{
		authErr: &Error{
			Code:       code,
			Message:    err.Error(),
			Retryable:  true,
			HTTPStatus: transportFailureStatusCode,
		},
		cause: err,
	}
}

// IsRetryableTransportError reports whether the error represents a transient
// connection-level failure where no valid upstream HTTP response was produced.
func IsRetryableTransportError(err error) bool {
	_, ok := classifyRetryableTransportError(err)
	return ok
}

func existingTransportWrappedError(err error) *transportWrappedError {
	var wrapped *transportWrappedError
	if errors.As(err, &wrapped) && wrapped != nil {
		return wrapped
	}
	return nil
}

func classifyRetryableTransportError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if existingTransportWrappedError(err) != nil {
		return "transport_error", true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	if proxyutil.IsProxyDialError(err) {
		return "proxy_dial_failed", true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return "transport_error", true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr != nil {
		return "transport_error", true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr != nil {
		return "transport_error", true
	}
	var recordHeaderErr tls.RecordHeaderError
	if errors.As(err, &recordHeaderErr) {
		return "transport_error", true
	}
	switch {
	case errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ECONNABORTED):
		return "transport_error", true
	}
	lower := strings.ToLower(strings.TrimSpace(err.Error()))
	if lower == "" {
		return "", false
	}
	patterns := [...]string{
		"malformed http response",
		"transport connection broken",
		"server closed idle connection",
		"connection reset by peer",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"http2: server sent goaway",
		"http2: client connection lost",
		"http2: stream closed",
		"dial tcp",
		"lookup ",
		"tls handshake timeout",
		"first record does not look like a tls handshake",
		"server gave http response to https client",
		"remote error: tls:",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return "transport_error", true
		}
	}
	return "", false
}
