package payload

import (
	"context"
	"errors"
	"testing"
	"time"
)

type scopeTestGate struct {
	acquired, released int
	err                error
	elapsed            time.Duration
}

func (g *scopeTestGate) AcquireTransform(context.Context, int64) (func(time.Duration), error) {
	g.acquired++
	if g.err != nil {
		return nil, g.err
	}
	return func(d time.Duration) { g.released++; g.elapsed = d }, nil
}
func TestTransformScopeNestedReleaseAndFailure(t *testing.T) {
	gate := &scopeTestGate{}
	base := WithTransformAdmission(context.Background(), gate)
	before := CurrentTransformMetrics().Preparation.Scopes
	ctx, release, err := BeginTransformScope(base, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, child, err := BeginTransformScope(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	child()
	release()
	release()
	if gate.acquired != 1 || gate.released != 1 || gate.elapsed <= 0 {
		t.Fatalf("nested accounting: %+v", gate)
	}
	if CurrentTransformMetrics().Preparation.Scopes-before != 1 {
		t.Fatal("double-counted nested scope")
	}
	// Retained contexts after a completed scope must acquire again.
	_, again, err := BeginTransformScope(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	again()
	if gate.acquired != 2 || gate.released != 2 {
		t.Fatal("stale context bypassed admission")
	}
	before++

	gate.err = errors.New("busy")
	if _, release, err := BeginTransformScope(base, 100); err != gate.err || release != nil {
		t.Fatal("rejected scope received lease")
	}
	if CurrentTransformMetrics().Preparation.Scopes-before != 1 {
		t.Fatal("queue rejection counted as conversion")
	}
}
func TestPreparationOneSecondBoundaryIsIndependentOfExecution(t *testing.T) {
	before := CurrentTransformMetrics().Preparation
	observePreparationDuration(16<<20, time.Second)
	observePreparationDuration(16<<20, time.Second+1)
	observePreparationDuration(16<<20+1, 2*time.Second)
	after := CurrentTransformMetrics().Preparation
	if after.OverOneSecond-before.OverOneSecond != 2 || after.LargeOverOneSecond-before.LargeOverOneSecond != 1 || after.Scopes-before.Scopes != 3 {
		t.Fatalf("wrong independent metrics before=%+v after=%+v", before, after)
	}
}

func TestSlowReportCountsRequestOnceAcrossRetainedScopes(t *testing.T) {
	before := CurrentTransformMetrics().SlowReports
	ctx := WithTransformReport(context.Background(), 1024)
	releaseOuter, releaseChild := RetainTransformReport(ctx), RetainTransformReport(ctx)
	RecordTransformStage(ctx, TransformStageReport{Stage: "normalize.first", InputBytes: 1024, OutputBytes: 1024, Duration: 600 * time.Millisecond}, AmplificationOverride{})
	RecordTransformStage(ctx, TransformStageReport{Stage: "normalize.second", InputBytes: 1024, OutputBytes: 1024, Duration: 600 * time.Millisecond}, AmplificationOverride{})
	releaseOuter()
	if CurrentTransformMetrics().SlowReports != before {
		t.Fatal("observed unfinished nested report")
	}
	releaseChild()
	releaseChild()
	if CurrentTransformMetrics().SlowReports != before+1 {
		t.Fatal("slow request was not counted exactly once")
	}
}
