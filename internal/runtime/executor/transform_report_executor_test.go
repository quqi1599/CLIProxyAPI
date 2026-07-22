package executor

import (
	"context"
	"testing"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

func assertTransformStageContract(t *testing.T, ctx context.Context, releaseReport func(), wantStage string, wantOutputBytes int64) {
	t.Helper()
	releaseReport()
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("transform report missing")
	}
	if !report.Finalized {
		t.Fatalf("transform report not finalized: %#v", report)
	}
	if report.OutputBytes != wantOutputBytes || report.FinalAmplification.OutputBytes != wantOutputBytes {
		t.Fatalf("final request bytes = %d/%d, want upstream body bytes %d", report.OutputBytes, report.FinalAmplification.OutputBytes, wantOutputBytes)
	}
	for i := len(report.Stages) - 1; i >= 0; i-- {
		stage := report.Stages[i]
		if stage.Stage != wantStage {
			continue
		}
		if stage.InputBytes <= 0 || stage.OutputBytes != wantOutputBytes {
			t.Fatalf("stage bytes = %d -> %d, want positive sizes", stage.InputBytes, stage.OutputBytes)
		}
		if stage.Duration < 0 {
			t.Fatalf("stage duration = %s, want non-negative", stage.Duration)
		}
		if len(stage.AppliedPolicies) != 1 || stage.AppliedPolicies[0] != internalpayload.DefaultAmplificationPolicyID || len(stage.Downgrades) != 0 {
			t.Fatalf("stage policy metadata = %#v, want default amplification policy", stage)
		}
		return
	}
	t.Fatalf("stage %q missing from report: %#v", wantStage, report)
}

func TestTransformReportRetainKeepsRetryStagesOpen(t *testing.T) {
	ctx := internalpayload.WithTransformReport(context.Background(), 10)
	releaseReport := internalpayload.RetainTransformReport(ctx)
	if err := internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       "request_plan.retry",
		InputBytes:  10,
		OutputBytes: 20,
	}, internalpayload.AmplificationOverride{}); err != nil {
		t.Fatalf("first request transform: %v", err)
	}
	first, _ := internalpayload.TransformReportFromContext(ctx)
	if first.Finalized {
		t.Fatalf("first attempt prematurely finalized report: %#v", first)
	}
	if err := internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       "request_plan.retry",
		InputBytes:  10,
		OutputBytes: 30,
	}, internalpayload.AmplificationOverride{}); err != nil {
		t.Fatalf("second request transform: %v", err)
	}

	assertTransformStageContract(t, ctx, releaseReport, "request_plan.retry", 30)
	report, _ := internalpayload.TransformReportFromContext(ctx)
	if len(report.Stages) != 2 {
		t.Fatalf("stages = %d, want both attempts", len(report.Stages))
	}
}
