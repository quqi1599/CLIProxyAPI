package executor

import (
	"context"
	"testing"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

func retainExecutorTransformReport(ctx context.Context, inputBytes int) (context.Context, func()) {
	ctx = internalpayload.WithTransformReport(ctx, int64(inputBytes))
	return ctx, internalpayload.RetainTransformReport(ctx)
}

func assertExecutorRequestTransformReport(t *testing.T, ctx context.Context, release func(), stage string, upstreamRequestBytes int) {
	t.Helper()
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || report.Finalized || len(report.Stages) == 0 {
		t.Fatalf("open transform report = %#v, want unfinalized stage %q", report, stage)
	}
	wantStage := report.Stages[len(report.Stages)-1]
	if wantStage.Stage != stage {
		t.Fatalf("last transform stage = %q, want %q; report=%#v", wantStage.Stage, stage, report)
	}
	if upstreamRequestBytes < 0 {
		upstreamRequestBytes = int(wantStage.OutputBytes)
	}
	if upstreamRequestBytes <= 0 || report.OutputBytes != int64(upstreamRequestBytes) || wantStage.OutputBytes != int64(upstreamRequestBytes) {
		t.Fatalf("transform report = %#v, want upstream request bytes %d", report, upstreamRequestBytes)
	}
	if !containsTransformMetadataID(wantStage.AppliedPolicies, internalpayload.DefaultAmplificationPolicyID) || len(wantStage.Downgrades) != 0 {
		t.Fatalf("transform stage policy metadata = %#v, want default amplification policy", wantStage)
	}

	release()
	sealed, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || !sealed.Finalized || sealed.OutputBytes != int64(upstreamRequestBytes) {
		t.Fatalf("sealed transform report = %#v, want upstream request bytes %d", sealed, upstreamRequestBytes)
	}
}

func containsTransformMetadataID(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestExecutorTransformReportOwnerSealsLastAttempt(t *testing.T) {
	ctx, release := retainExecutorTransformReport(context.Background(), 10)
	internalpayload.RecordTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       openAICompatRequestPlanTransformStage,
		InputBytes:  10,
		OutputBytes: 12,
	}, internalpayload.AmplificationOverride{})
	internalpayload.RecordTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       openAICompatRequestPlanTransformStage,
		InputBytes:  10,
		OutputBytes: 14,
	}, internalpayload.AmplificationOverride{})

	open, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || open.Finalized || len(open.Stages) != 2 || open.OutputBytes != 14 {
		t.Fatalf("open transform report = %#v, want both attempts and last request body", open)
	}
	release()
	sealed, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || !sealed.Finalized || len(sealed.Stages) != 2 || sealed.OutputBytes != 14 {
		t.Fatalf("sealed transform report = %#v, want last attempt request body", sealed)
	}
}
