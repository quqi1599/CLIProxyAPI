package handlers

import (
	"context"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	log "github.com/sirupsen/logrus"
)

func addTransformReportLogObserver(ctx context.Context) bool {
	requestID := logging.GetRequestID(ctx)
	return internalpayload.AddTransformReportObserver(ctx, func(report internalpayload.TransformReport) {
		stages, errMarshal := json.Marshal(report.Stages)
		if errMarshal != nil {
			stages = []byte("[]")
		}
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
			"transform_stages":          string(stages),
			"amplification_ratio":       report.FinalAmplification.Ratio,
			"amplification_exceeded":    report.FinalAmplification.Exceeded,
			"instrumented":              report.Instrumented,
			"finalized":                 report.Finalized,
		}
		entry := log.WithFields(fields)
		if requestID != "" {
			entry = entry.WithField("request_id", requestID)
		}
		entry.Info("payload transform summary")
	})
}
