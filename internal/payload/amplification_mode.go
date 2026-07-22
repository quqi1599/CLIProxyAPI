package payload

import (
	"context"
	"strings"
)

// AmplificationMode controls whether exceeded request transforms are observed
// or rejected. A context without a mode retains the explicit Enforce API
// behavior and rejects exceeded output.
type AmplificationMode string

const (
	AmplificationModeObserve AmplificationMode = "observe"
	AmplificationModeEnforce AmplificationMode = "enforce"
)

type amplificationModeContextKey struct{}

// WithAmplificationMode attaches a valid request-scoped amplification mode.
// Invalid values leave the context unchanged.
func WithAmplificationMode(ctx context.Context, mode AmplificationMode) context.Context {
	mode = normalizeAmplificationMode(mode)
	if mode == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, amplificationModeContextKey{}, mode)
}

// AmplificationModeFromContext returns an explicitly attached request mode.
func AmplificationModeFromContext(ctx context.Context) (AmplificationMode, bool) {
	if ctx == nil {
		return "", false
	}
	mode, _ := ctx.Value(amplificationModeContextKey{}).(AmplificationMode)
	mode = normalizeAmplificationMode(mode)
	return mode, mode != ""
}

func normalizeAmplificationMode(mode AmplificationMode) AmplificationMode {
	switch AmplificationMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case AmplificationModeObserve:
		return AmplificationModeObserve
	case AmplificationModeEnforce:
		return AmplificationModeEnforce
	default:
		return ""
	}
}

func amplificationModeEnforces(ctx context.Context) bool {
	mode, configured := AmplificationModeFromContext(ctx)
	return !configured || mode == AmplificationModeEnforce
}
