package executor

import (
	"context"
	"net/http/httptrace"
	"sync"
	"time"
)

type contentAuditReviewTraceKey struct{}

// ContentAuditReviewTrace collects only bounded, non-sensitive phase durations.
// TTFB is measured from HTTP dispatch, so it may overlap connect and request_write.
// Missing transport hooks remain absent rather than being reported as zero latency.
type ContentAuditReviewTrace struct {
	mu     sync.Mutex
	stages map[string]int64
}

func WithContentAuditReviewTrace(ctx context.Context) (context.Context, *ContentAuditReviewTrace) {
	trace := &ContentAuditReviewTrace{stages: make(map[string]int64)}
	return context.WithValue(ctx, contentAuditReviewTraceKey{}, trace), trace
}

func ContentAuditReviewTraceFromContext(ctx context.Context) *ContentAuditReviewTrace {
	trace, _ := ctx.Value(contentAuditReviewTraceKey{}).(*ContentAuditReviewTrace)
	return trace
}

func (t *ContentAuditReviewTrace) Record(stage string, elapsed time.Duration) {
	if t == nil {
		return
	}
	switch stage {
	case "auth_select", "connect", "request_write", "ttfb", "transport", "read", "parse":
	default:
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	t.mu.Lock()
	if t.stages == nil {
		t.stages = make(map[string]int64)
	}
	t.stages[stage] = elapsed.Milliseconds()
	t.mu.Unlock()
}

func (t *ContentAuditReviewTrace) Snapshot() map[string]int64 {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	stages := make(map[string]int64, len(t.stages))
	for stage, millis := range t.stages {
		stages[stage] = millis
	}
	return stages
}

// HTTPContext installs observation hooks only for the internal audit request.
// It does not add deadlines or change the business executor's transport behavior.
func (t *ContentAuditReviewTrace) HTTPContext(ctx context.Context) context.Context {
	if t == nil {
		return ctx
	}
	started := time.Now()
	var phaseMu sync.Mutex
	var connectStarted, writeStarted time.Time
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			phaseMu.Lock()
			connectStarted = time.Now()
			phaseMu.Unlock()
		},
		ConnectDone: func(_, _ string, _ error) {
			phaseMu.Lock()
			if !connectStarted.IsZero() {
				t.Record("connect", time.Since(connectStarted))
			}
			phaseMu.Unlock()
		},
		GotConn: func(_ httptrace.GotConnInfo) {
			phaseMu.Lock()
			writeStarted = time.Now()
			phaseMu.Unlock()
		},
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			phaseMu.Lock()
			if !writeStarted.IsZero() {
				t.Record("request_write", time.Since(writeStarted))
			}
			phaseMu.Unlock()
		},
		GotFirstResponseByte: func() { t.Record("ttfb", time.Since(started)) },
	})
}
