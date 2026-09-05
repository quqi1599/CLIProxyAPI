package executor

import (
	"sync"
	"testing"
	"time"
)

func TestContentAuditReviewTraceBoundsAndCopiesDiagnostics(t *testing.T) {
	ctx, trace := WithContentAuditReviewTrace(t.Context())
	if ContentAuditReviewTraceFromContext(ctx) != trace {
		t.Fatal("trace not bound to context")
	}
	trace.Record("sensitive-diagnostic-key", time.Second)
	trace.Record("parse", -time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); trace.Record("transport", time.Millisecond); _ = trace.Snapshot() }()
	}
	wg.Wait()
	stages := trace.Snapshot()
	if len(stages) != 2 || stages["parse"] != 0 || stages["transport"] != 1 {
		t.Fatalf("unexpected stages: %v", stages)
	}
	stages["transport"] = 99
	if trace.Snapshot()["transport"] != 1 {
		t.Fatal("snapshot aliases trace")
	}
}
