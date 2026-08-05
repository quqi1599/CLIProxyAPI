package logging

import (
	"context"
	"errors"
	"testing"
)

func TestCancellationOriginFromContext(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(NewCancellationCause(CancelOriginDownstreamDisconnected, context.Canceled))

	if got := CancellationOriginFromContext(ctx); got != CancelOriginDownstreamDisconnected {
		t.Fatalf("cancel origin = %q", got)
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("cause = %v, want context.Canceled", context.Cause(ctx))
	}
}

func TestCancellationOriginFromDeadline(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.DeadlineExceeded)

	if got := CancellationOriginFromContext(ctx); got != CancelOriginGatewayDeadline {
		t.Fatalf("cancel origin = %q", got)
	}
}
