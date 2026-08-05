package logging

import (
	"context"
	"errors"
)

const (
	CancelOriginDownstreamDisconnected = "downstream_disconnected"
	CancelOriginDownstreamDeadline     = "downstream_deadline"
	CancelOriginGatewayDeadline        = "gateway_deadline"
	CancelOriginUpstreamTimeout        = "upstream_timeout"
	CancelOriginInternalAbort          = "internal_abort"
)

type cancellationCause struct {
	origin string
	cause  error
}

func (e *cancellationCause) Error() string {
	if e == nil || e.cause == nil {
		return "request canceled"
	}
	return e.cause.Error()
}

func (e *cancellationCause) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *cancellationCause) CancellationOrigin() string {
	if e == nil {
		return ""
	}
	return e.origin
}

// NewCancellationCause preserves context cancellation semantics while recording its origin.
func NewCancellationCause(origin string, cause error) error {
	if cause == nil {
		cause = context.Canceled
	}
	return &cancellationCause{origin: origin, cause: cause}
}

// CancellationOriginFromContext classifies the first cause that canceled ctx.
func CancellationOriginFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	cause := context.Cause(ctx)
	if cause == nil {
		return ""
	}
	var origin interface{ CancellationOrigin() string }
	if errors.As(cause, &origin) {
		return origin.CancellationOrigin()
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return CancelOriginGatewayDeadline
	}
	if errors.Is(cause, context.Canceled) {
		return CancelOriginInternalAbort
	}
	return ""
}
