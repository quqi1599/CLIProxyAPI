package helps

import (
	"context"
	"time"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

// EnforceSemanticTransformStage records one non-overlapping request preparation
// phase and applies its amplification contract before the next phase runs.
func EnforceSemanticTransformStage(
	ctx context.Context,
	stage string,
	input, output []byte,
	started time.Time,
	appliedPolicies, downgrades []string,
	override internalpayload.AmplificationOverride,
) error {
	duration := time.Duration(0)
	if !started.IsZero() {
		duration = time.Since(started)
	}
	return internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:           stage,
		InputBytes:      int64(len(input)),
		OutputBytes:     int64(len(output)),
		Duration:        duration,
		AppliedPolicies: appliedPolicies,
		Downgrades:      downgrades,
		ReusedInput:     samePayloadStorage(input, output),
	}, override)
}

func samePayloadStorage(input, output []byte) bool {
	if len(input) == 0 || len(output) == 0 {
		return len(input) == 0 && len(output) == 0
	}
	return &input[0] == &output[0]
}
