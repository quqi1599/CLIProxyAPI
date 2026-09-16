package handlers

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	log "github.com/sirupsen/logrus"
)

func addTransformReportLogObserver(ctx context.Context) bool {
	requestID := logging.GetRequestID(ctx)
	return internalpayload.AddTransformReportObserver(ctx, func(report internalpayload.TransformReport) {
		fields := log.Fields{
			"event":                     "payload_transform_summary",
			"wire_input_bytes":          report.WireInputBytes,
			"decoded_input_bytes":       report.InputBytes,
			"transform_output_bytes":    report.OutputBytes,
			"transform_added_bytes":     report.AddedBytes,
			"transform_removed_bytes":   report.RemovedBytes,
			"transform_synthetic_bytes": report.SyntheticBytes,
			"transform_patched_count":   report.PatchedCount,
			"transform_duration_ms":     report.Duration.Milliseconds(),
			"transform_stage_count":     len(report.Stages),
			"amplification_ratio":       report.FinalAmplification.Ratio,
			"amplification_exceeded":    report.FinalAmplification.Exceeded,
			"instrumented":              report.Instrumented,
			"finalized":                 report.Finalized,
		}
		if includeTransformStageDetails(ctx, report) {
			if stages, errMarshal := json.Marshal(report.Stages); errMarshal == nil {
				fields["transform_stages"] = string(stages)
			}
		}
		entry := log.WithFields(fields)
		if requestID != "" {
			entry = entry.WithField("request_id", requestID)
		}
		entry.Info("payload transform summary")
	})
}

func includeTransformStageDetails(ctx context.Context, report internalpayload.TransformReport) bool {
	if report.Failed || log.IsLevelEnabled(log.DebugLevel) || logging.GetResponseStatus(ctx) >= 400 ||
		!report.Finalized || report.FinalAmplification.Exceeded || report.Duration >= 100*time.Millisecond {
		return true
	}
	for _, stage := range report.Stages {
		if stage.Amplification.Exceeded {
			return true
		}
	}
	// A stable one-percent sample avoids a shared counter on the request path.
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(logging.GetRequestID(ctx)))
	return hash.Sum32()%100 == 0
}
