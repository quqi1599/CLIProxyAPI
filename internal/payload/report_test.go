package payload

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransformReportRecordsMetadataOnly(t *testing.T) {
	const secretMarker = "sk-secret-prompt-marker"
	body := []byte(`{"model":"test","prompt":"` + secretMarker + `"}`)
	ctx := WithTransformReportBytes(context.Background(), int64(len(body)-1), int64(len(body)))
	policies := []string{"compat.history.repair"}
	downgrades := []string{"placeholder"}
	RecordTransformStage(ctx, TransformStageReport{
		Stage:           "repair_history",
		InputBytes:      int64(len(body)),
		OutputBytes:     int64(len(body) + 64),
		SyntheticBytes:  64,
		PatchedCount:    2,
		Duration:        3 * time.Millisecond,
		AppliedPolicies: policies,
		Downgrades:      downgrades,
		ReusedInput:     false,
	}, AmplificationOverride{})
	FinalizeTransformReport(ctx, int64(len(body)+64), AmplificationOverride{})

	report, ok := TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("transform report missing")
	}
	if report.InputBytes != int64(len(body)) || report.OutputBytes != int64(len(body)+64) {
		t.Fatalf("report sizes = %d -> %d", report.InputBytes, report.OutputBytes)
	}
	if report.WireInputBytes != int64(len(body)-1) {
		t.Fatalf("wire input bytes = %d", report.WireInputBytes)
	}
	if !report.Instrumented || !report.Finalized {
		t.Fatalf("report completeness = instrumented:%t finalized:%t", report.Instrumented, report.Finalized)
	}
	if report.AddedBytes != 64 || report.RemovedBytes != 0 || report.SyntheticBytes != 64 || report.PatchedCount != 2 {
		t.Fatalf("report accounting = added %d removed %d synthetic %d patched %d", report.AddedBytes, report.RemovedBytes, report.SyntheticBytes, report.PatchedCount)
	}
	if len(report.Stages) != 1 || report.Stages[0].Duration != 3*time.Millisecond || report.Stages[0].PatchedCount != 2 {
		t.Fatalf("unexpected stages: %#v", report.Stages)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secretMarker) {
		t.Fatalf("transform report leaked request content: %s", encoded)
	}

	policies[0] = secretMarker
	downgrades[0] = secretMarker
	second, _ := TransformReportFromContext(ctx)
	if second.Stages[0].AppliedPolicies[0] != "compat.history.repair" || second.Stages[0].Downgrades[0] != "placeholder" {
		t.Fatal("report retained caller-owned metadata slices")
	}
	encoded, err = json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secretMarker) {
		t.Fatalf("transform report leaked caller-owned metadata: %s", encoded)
	}
}

func TestTransformReportSnapshotIsIndependent(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 100)
	RecordTransformStage(ctx, TransformStageReport{
		Stage:           "translate",
		InputBytes:      100,
		OutputBytes:     80,
		AppliedPolicies: []string{"translate.v1"},
		Downgrades:      []string{"strip"},
		ReusedInput:     true,
	}, AmplificationOverride{})

	first, _ := TransformReportFromContext(ctx)
	first.Stages[0].Stage = "mutated"
	first.Stages[0].AppliedPolicies[0] = "mutated"
	first.Stages[0].Downgrades[0] = "mutated"
	second, _ := TransformReportFromContext(ctx)
	if second.Stages[0].Stage != "translate" || second.Stages[0].AppliedPolicies[0] != "translate.v1" || second.Stages[0].Downgrades[0] != "strip" {
		t.Fatalf("snapshot mutation reached accumulator: %#v", second.Stages[0])
	}
	if second.AddedBytes != 0 || second.RemovedBytes != 20 || second.Stages[0].AddedBytes != 0 || second.Stages[0].RemovedBytes != 20 {
		t.Fatalf("unexpected removal accounting: %#v", second)
	}
	if !second.Stages[0].ReusedInput {
		t.Fatal("reused-input observation was lost")
	}
}

func TestTransformReportAggregatesPatchedCount(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 10)
	RecordTransformStage(ctx, TransformStageReport{Stage: "first", InputBytes: 10, OutputBytes: 10, PatchedCount: 2}, AmplificationOverride{})
	RecordTransformStage(ctx, TransformStageReport{Stage: "second", InputBytes: 10, OutputBytes: 10, PatchedCount: 3}, AmplificationOverride{})
	report, _ := TransformReportFromContext(ctx)
	if report.PatchedCount != 5 || len(report.Stages) != 2 || report.Stages[0].PatchedCount != 2 || report.Stages[1].PatchedCount != 3 {
		t.Fatalf("patched count report = %+v", report)
	}
}

func TestTransformReportKeepsOnlyControlledMetadataIDs(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 10)
	RecordTransformStage(ctx, TransformStageReport{
		Stage:           "provider_quirk_patch",
		InputBytes:      10,
		OutputBytes:     10,
		AppliedPolicies: []string{strings.Repeat("a", maxTransformMetadataIDBytes+1), "compat.safe-v1", "request derived value"},
		Downgrades:      []string{"strip", "unsafe value"},
	}, AmplificationOverride{})
	report, _ := TransformReportFromContext(ctx)
	if len(report.Stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(report.Stages))
	}
	stage := report.Stages[0]
	if len(stage.AppliedPolicies) != 1 || stage.AppliedPolicies[0] != "compat.safe-v1" {
		t.Fatalf("applied policies = %#v", stage.AppliedPolicies)
	}
	if len(stage.Downgrades) != 1 || stage.Downgrades[0] != "strip" {
		t.Fatalf("downgrades = %#v", stage.Downgrades)
	}
}

func TestTransformReportRecordsStageAndFinalAmplification(t *testing.T) {
	const inputBytes int64 = 1024 * 1024
	const outputBytes int64 = inputBytes + inputBytes/2 + 1
	ctx := WithTransformReport(context.Background(), inputBytes)
	RecordTransformStage(ctx, TransformStageReport{
		Stage:       "default_guard",
		InputBytes:  inputBytes,
		OutputBytes: outputBytes,
	}, AmplificationOverride{})
	RecordTransformStage(ctx, TransformStageReport{
		Stage:       "policy_guard",
		InputBytes:  inputBytes,
		OutputBytes: outputBytes,
	}, AmplificationOverride{
		PolicyID:          "compat.expand-v1",
		MaxExpansionRatio: 2,
	})
	FinalizeTransformReport(ctx, outputBytes, AmplificationOverride{
		PolicyID:          "compat.final-v1",
		MaxExpansionRatio: 2,
	})

	report, _ := TransformReportFromContext(ctx)
	if len(report.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(report.Stages))
	}
	if !report.Stages[0].Amplification.Exceeded || report.Stages[0].Amplification.OverrideApplied {
		t.Fatalf("default stage guard = %#v", report.Stages[0].Amplification)
	}
	policyGuard := report.Stages[1].Amplification
	if policyGuard.Exceeded || !policyGuard.OverrideApplied || policyGuard.PolicyID != "compat.expand-v1" {
		t.Fatalf("policy stage guard = %#v", policyGuard)
	}
	if report.FinalAmplification.Exceeded || report.FinalAmplification.PolicyID != "compat.final-v1" {
		t.Fatalf("final guard = %#v", report.FinalAmplification)
	}
}

func TestTransformReportContextIsIdempotentAndConcurrent(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 10)
	if nested := WithTransformReport(ctx, 999); nested != ctx {
		t.Fatal("nested WithTransformReport replaced the existing context")
	}

	const workers = 32
	var wait sync.WaitGroup
	wait.Add(workers)
	for idx := 0; idx < workers; idx++ {
		go func() {
			defer wait.Done()
			RecordTransformStage(ctx, TransformStageReport{
				Stage:       "parallel_stage",
				InputBytes:  10,
				OutputBytes: 11,
			}, AmplificationOverride{})
		}()
	}
	wait.Wait()

	report, ok := TransformReportFromContext(ctx)
	if !ok || len(report.Stages) != workers {
		t.Fatalf("recorded stages = %d, want %d", len(report.Stages), workers)
	}
	if report.AddedBytes != workers {
		t.Fatalf("added bytes = %d, want %d", report.AddedBytes, workers)
	}
}

func TestHasTransformReport(t *testing.T) {
	if HasTransformReport(nil) || HasTransformReport(context.Background()) {
		t.Fatal("context without a transform report was reported as instrumented")
	}
	ctx := WithTransformReport(context.Background(), 1)
	if !HasTransformReport(ctx) {
		t.Fatal("transform report was not detected")
	}
}

func TestTransformReportLegacyInputKeepsWireSizeUnknown(t *testing.T) {
	ctx := WithTransformReport(nil, 10)
	report, ok := TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("legacy transform report missing")
	}
	if report.WireInputBytes != 0 || report.InputBytes != 10 {
		t.Fatalf("legacy input sizes = wire:%d decoded:%d", report.WireInputBytes, report.InputBytes)
	}

	nested := WithTransformReportBytes(ctx, 20, 30)
	if nested != ctx {
		t.Fatal("explicit input sizes replaced the existing context")
	}
	report, _ = TransformReportFromContext(ctx)
	if report.WireInputBytes != 0 || report.InputBytes != 10 {
		t.Fatalf("nested input sizes replaced the report: %+v", report)
	}
}

func TestTransformReportWithoutContextIsNoop(t *testing.T) {
	RecordTransformStage(nil, TransformStageReport{Stage: "ignored"}, AmplificationOverride{})
	FinalizeTransformReport(nil, 10, AmplificationOverride{})
	if _, ok := TransformReportFromContext(nil); ok {
		t.Fatal("nil context unexpectedly contained a report")
	}
}

func TestTransformReportFinalizationAndStageCountAreBounded(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 1)
	for range maxTransformReportStages + 10 {
		RecordTransformStage(ctx, TransformStageReport{Stage: "bounded", InputBytes: 1, OutputBytes: 2}, AmplificationOverride{})
	}
	FinalizeTransformReport(ctx, 2, AmplificationOverride{})
	RecordTransformStage(ctx, TransformStageReport{Stage: "ignored", InputBytes: 2, OutputBytes: 3}, AmplificationOverride{})
	FinalizeTransformReport(ctx, 3, AmplificationOverride{})

	report, _ := TransformReportFromContext(ctx)
	if len(report.Stages) != maxTransformReportStages {
		t.Fatalf("stages = %d, want %d", len(report.Stages), maxTransformReportStages)
	}
	if report.OutputBytes != 2 {
		t.Fatalf("final output bytes = %d, want 2", report.OutputBytes)
	}
}

func TestTransformReportPublishesOnceAfterFinalNestedRelease(t *testing.T) {
	const wireInputBytes int64 = 512 << 10
	const inputBytes int64 = 1 << 20
	const outputBytes int64 = 2 << 20
	before := CurrentTransformMetrics()
	ctx := WithTransformReportBytes(context.Background(), wireInputBytes, inputBytes)
	releaseOuter := RetainTransformReport(ctx)
	releaseInner := RetainTransformReport(ctx)
	RecordTransformStage(ctx, TransformStageReport{
		Stage:          "expand",
		InputBytes:     inputBytes,
		OutputBytes:    outputBytes,
		SyntheticBytes: 32,
		Duration:       time.Millisecond,
	}, AmplificationOverride{})

	releaseInner()
	if current := CurrentTransformMetrics(); current.Reports != before.Reports {
		t.Fatalf("nested release published early: before=%+v current=%+v", before, current)
	}
	releaseOuter()
	releaseOuter()
	after := CurrentTransformMetrics()
	if after.Reports-before.Reports != 1 || after.InstrumentedReports-before.InstrumentedReports != 1 || after.FinalizedReports-before.FinalizedReports != 1 ||
		after.Stages-before.Stages != 1 || after.ExceededReports-before.ExceededReports != 1 || after.ExceededStages-before.ExceededStages != 1 {
		t.Fatalf("metric delta = reports:%d stages:%d exceeded reports:%d stages:%d", after.Reports-before.Reports, after.Stages-before.Stages, after.ExceededReports-before.ExceededReports, after.ExceededStages-before.ExceededStages)
	}
	if after.WireInputBytes-before.WireInputBytes != uint64(wireInputBytes) || after.InputBytes-before.InputBytes != uint64(inputBytes) || after.OutputBytes-before.OutputBytes != uint64(outputBytes) || after.SyntheticBytes-before.SyntheticBytes != 32 {
		t.Fatalf("byte metric delta = wire:%d input:%d output:%d synthetic:%d", after.WireInputBytes-before.WireInputBytes, after.InputBytes-before.InputBytes, after.OutputBytes-before.OutputBytes, after.SyntheticBytes-before.SyntheticBytes)
	}
	if after.TransformNanoseconds-before.TransformNanoseconds < uint64(time.Millisecond) {
		t.Fatalf("duration metric delta = %d", after.TransformNanoseconds-before.TransformNanoseconds)
	}
	report, _ := TransformReportFromContext(ctx)
	if !report.FinalAmplification.Exceeded {
		t.Fatalf("final report = %+v", report)
	}
}

func TestTransformMetricsPublishFixedCatalogDistributions(t *testing.T) {
	before := CurrentTransformMetrics()
	ctx := WithTransformReport(context.Background(), 64<<10)
	release := RetainTransformReport(ctx)
	RecordTransformStage(ctx, TransformStageReport{
		Stage:           "normalize.thinking_history.openai",
		InputBytes:      64 << 10,
		OutputBytes:     256 << 10,
		Duration:        time.Millisecond,
		AppliedPolicies: []string{transformPolicyThinkingPlaceholder},
	}, AmplificationOverride{})
	RecordTransformStage(ctx, TransformStageReport{
		Stage:           "request_plan.codex",
		InputBytes:      1 << 20,
		OutputBytes:     4 << 20,
		Duration:        100 * time.Millisecond,
		AppliedPolicies: []string{DefaultAmplificationPolicyID},
	}, AmplificationOverride{})
	RecordTransformStage(ctx, TransformStageReport{
		Stage:           "request-derived-stage",
		InputBytes:      1,
		OutputBytes:     (64 << 20) + 1,
		Duration:        time.Second + time.Millisecond,
		AppliedPolicies: []string{"request-derived-policy"},
	}, AmplificationOverride{})
	FinalizeTransformReport(ctx, (64<<20)+1, AmplificationOverride{})
	release()

	after := CurrentTransformMetrics()
	if len(after.StageCatalog) != len(transformStageCatalog) || len(after.PolicyCatalog) != len(transformPolicyCatalog) {
		t.Fatalf("catalog sizes = stages:%d policies:%d", len(after.StageCatalog), len(after.PolicyCatalog))
	}
	if _, exists := after.StageCatalog["request-derived-stage"]; exists {
		t.Fatal("unknown stage escaped the fixed catalog")
	}
	if _, exists := after.PolicyCatalog["request-derived-policy"]; exists {
		t.Fatal("unknown policy escaped the fixed catalog")
	}

	normalizeBefore := before.StageCatalog[transformStageNormalize]
	normalizeAfter := after.StageCatalog[transformStageNormalize]
	if normalizeAfter.Results.WithinLimit-normalizeBefore.Results.WithinLimit != 1 ||
		normalizeAfter.InputSizeBuckets.LessThanOrEqual64KiB-normalizeBefore.InputSizeBuckets.LessThanOrEqual64KiB != 1 ||
		normalizeAfter.OutputSizeBuckets.LessThanOrEqual256KiB-normalizeBefore.OutputSizeBuckets.LessThanOrEqual256KiB != 1 ||
		normalizeAfter.DurationBuckets.LessThanOrEqual1Millis-normalizeBefore.DurationBuckets.LessThanOrEqual1Millis != 1 ||
		normalizeAfter.AmplificationRatios.LessThanOrEqualFour-normalizeBefore.AmplificationRatios.LessThanOrEqualFour != 1 {
		t.Fatalf("normalize distribution delta = before:%+v after:%+v", normalizeBefore, normalizeAfter)
	}

	planBefore := before.StageCatalog[transformStageRequestPlan]
	planAfter := after.StageCatalog[transformStageRequestPlan]
	if planAfter.Results.Exceeded-planBefore.Results.Exceeded != 1 ||
		planAfter.InputSizeBuckets.LessThanOrEqual1MiB-planBefore.InputSizeBuckets.LessThanOrEqual1MiB != 1 ||
		planAfter.OutputSizeBuckets.LessThanOrEqual4MiB-planBefore.OutputSizeBuckets.LessThanOrEqual4MiB != 1 ||
		planAfter.DurationBuckets.LessThanOrEqual100Millis-planBefore.DurationBuckets.LessThanOrEqual100Millis != 1 {
		t.Fatalf("request-plan distribution delta = before:%+v after:%+v", planBefore, planAfter)
	}

	otherStageBefore := before.StageCatalog[transformMetricOther]
	otherStageAfter := after.StageCatalog[transformMetricOther]
	if otherStageAfter.Results.Exceeded-otherStageBefore.Results.Exceeded != 1 ||
		otherStageAfter.OutputSizeBuckets.Overflow-otherStageBefore.OutputSizeBuckets.Overflow != 1 ||
		otherStageAfter.DurationBuckets.GreaterThanOneSecond-otherStageBefore.DurationBuckets.GreaterThanOneSecond != 1 ||
		otherStageAfter.AmplificationRatios.Overflow-otherStageBefore.AmplificationRatios.Overflow != 1 {
		t.Fatalf("other-stage distribution delta = before:%+v after:%+v", otherStageBefore, otherStageAfter)
	}

	for _, policyID := range []string{transformPolicyThinkingPlaceholder, DefaultAmplificationPolicyID, transformMetricOther} {
		policyBefore := before.PolicyCatalog[policyID]
		policyAfter := after.PolicyCatalog[policyID]
		if policyAfter.Results.WithinLimit+policyAfter.Results.Exceeded-
			(policyBefore.Results.WithinLimit+policyBefore.Results.Exceeded) != 1 {
			t.Fatalf("policy %q apply delta = before:%+v after:%+v", policyID, policyBefore, policyAfter)
		}
	}
	if after.ReportDistribution.Results.Exceeded-before.ReportDistribution.Results.Exceeded != 1 ||
		after.ReportDistribution.OutputSizeBuckets.Overflow-before.ReportDistribution.OutputSizeBuckets.Overflow != 1 ||
		after.ReportDistribution.AmplificationRatios.Overflow-before.ReportDistribution.AmplificationRatios.Overflow != 1 {
		t.Fatalf("report distribution delta = before:%+v after:%+v", before.ReportDistribution, after.ReportDistribution)
	}
}

func TestTransformPolicyCatalogIncludesOpenAICompatPolicies(t *testing.T) {
	for _, policyID := range []string{
		transformPolicyOpenAICompatDeepSeek,
		transformPolicyOpenAICompatDoubao,
		transformPolicyOpenAICompatKimi,
		transformPolicyOpenAICompatMiniMax,
		transformPolicyOpenAICompatQwen38,
		transformPolicyOpenAICompatXiaomi,
		transformPolicyOpenAICompatPostConfig,
	} {
		if got := transformPolicyCatalogID(policyID); got != policyID {
			t.Fatalf("transformPolicyCatalogID(%q) = %q", policyID, got)
		}
	}
}

func TestTransformReportObserversReceiveIndependentSnapshotOnce(t *testing.T) {
	ctx := WithTransformReportBytes(context.Background(), 8, 10)
	releaseOuter := RetainTransformReport(ctx)
	releaseInner := RetainTransformReport(ctx)
	RecordTransformStage(ctx, TransformStageReport{
		Stage:           "translate",
		InputBytes:      10,
		OutputBytes:     12,
		AppliedPolicies: []string{"compat.safe"},
	}, AmplificationOverride{})

	var calls int
	if !AddTransformReportObserver(ctx, func(TransformReport) {
		calls++
		panic("observer failure")
	}) {
		t.Fatal("panic observer was not registered")
	}
	if !AddTransformReportObserver(ctx, func(report TransformReport) {
		calls++
		report.Stages[0].Stage = "mutated"
		report.Stages[0].AppliedPolicies[0] = "mutated"
	}) {
		t.Fatal("mutating observer was not registered")
	}
	var observed TransformReport
	if !AddTransformReportObserver(ctx, func(report TransformReport) {
		calls++
		observed = report
	}) {
		t.Fatal("recording observer was not registered")
	}

	releaseInner()
	if calls != 0 {
		t.Fatalf("observers called before final release: %d", calls)
	}
	releaseOuter()
	releaseOuter()
	if calls != 3 {
		t.Fatalf("observer calls = %d, want 3", calls)
	}
	if len(observed.Stages) != 1 || observed.Stages[0].Stage != "translate" || observed.Stages[0].AppliedPolicies[0] != "compat.safe" {
		t.Fatalf("observer snapshot was shared: %+v", observed)
	}
	if !observed.Finalized || observed.WireInputBytes != 8 || observed.InputBytes != 10 {
		t.Fatalf("observer report = %+v", observed)
	}
	if AddTransformReportObserver(ctx, func(TransformReport) {}) {
		t.Fatal("observer registered after publication")
	}
	if AddTransformReportObserver(ctx, nil) || AddTransformReportObserver(nil, func(TransformReport) {}) {
		t.Fatal("nil observer or context was accepted")
	}
}

func TestTransformReportObserverRegistrationIsBounded(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 1)
	release := RetainTransformReport(ctx)
	var calls atomic.Int64
	for idx := 0; idx < maxTransformReportObservers; idx++ {
		if !AddTransformReportObserver(ctx, func(TransformReport) { calls.Add(1) }) {
			t.Fatalf("observer %d was rejected", idx)
		}
	}
	if AddTransformReportObserver(ctx, func(TransformReport) {}) {
		t.Fatal("observer limit was not enforced")
	}
	release()
	if got := calls.Load(); got != maxTransformReportObservers {
		t.Fatalf("observer calls = %d, want %d", got, maxTransformReportObservers)
	}
}

func TestTransformReportObserverRegistrationRacesFinalRelease(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 1)
	release := RetainTransformReport(ctx)
	start := make(chan struct{})
	var accepted atomic.Int64
	var called atomic.Int64
	var wait sync.WaitGroup
	for range maxTransformReportObservers * 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if AddTransformReportObserver(ctx, func(TransformReport) { called.Add(1) }) {
				accepted.Add(1)
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		release()
	}()
	close(start)
	wait.Wait()
	if got, want := called.Load(), accepted.Load(); got != want {
		t.Fatalf("observer calls = %d, accepted = %d", got, want)
	}
}

func TestTransformReportPublishesUninstrumentedAsUnknown(t *testing.T) {
	before := CurrentTransformMetrics()
	ctx := WithTransformReport(context.Background(), 1024)
	release := RetainTransformReport(ctx)
	release()

	after := CurrentTransformMetrics()
	if after.Reports-before.Reports != 1 || after.UninstrumentedReports-before.UninstrumentedReports != 1 || after.UnfinalizedReports-before.UnfinalizedReports != 1 {
		t.Fatalf("unknown metric delta = reports:%d uninstrumented:%d unfinalized:%d", after.Reports-before.Reports, after.UninstrumentedReports-before.UninstrumentedReports, after.UnfinalizedReports-before.UnfinalizedReports)
	}
	if after.InstrumentedReports != before.InstrumentedReports || after.FinalizedReports != before.FinalizedReports || after.OutputBytes != before.OutputBytes || after.ExceededReports != before.ExceededReports {
		t.Fatalf("uninstrumented report was counted as complete: before=%+v after=%+v", before, after)
	}
	report, ok := TransformReportFromContext(ctx)
	if !ok || report.Instrumented || report.Finalized {
		t.Fatalf("unknown report completeness = %+v, ok=%t", report, ok)
	}
}

func TestRecordTransformStageSinceMeasuresDuration(t *testing.T) {
	ctx := WithTransformReport(context.Background(), 10)
	RecordTransformStageSince(ctx, TransformStageReport{
		Stage:       "timed",
		InputBytes:  10,
		OutputBytes: 10,
	}, time.Now().Add(-time.Millisecond), AmplificationOverride{})
	report, _ := TransformReportFromContext(ctx)
	if len(report.Stages) != 1 || report.Stages[0].Duration < time.Millisecond {
		t.Fatalf("measured duration = %v", report.Stages[0].Duration)
	}
}

func BenchmarkTransformReportRecord(b *testing.B) {
	stage := TransformStageReport{
		Stage:           "provider_quirk_patch",
		InputBytes:      1024,
		OutputBytes:     1056,
		SyntheticBytes:  32,
		AppliedPolicies: []string{"compat.example.v1"},
		ReusedInput:     false,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for idx := 0; idx < b.N; idx++ {
		ctx := WithTransformReport(context.Background(), 1024)
		RecordTransformStage(ctx, stage, AmplificationOverride{})
	}
}
