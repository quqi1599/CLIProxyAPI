package executor

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type countTokensSummaryLogMeta struct {
	requestedModel string
	upstreamModel  string
	provider       string
	executor       string
	requestPath    string
	clientProfile  string
	payloadBytes   int
	messageCount   int
	toolCount      int
}

func newCountTokensSummaryLogMeta(opts cliproxyexecutor.Options, requestedModel, upstreamModel, provider, executor string, payload []byte) countTokensSummaryLogMeta {
	return countTokensSummaryLogMeta{
		requestedModel: strings.TrimSpace(requestedModel),
		upstreamModel:  strings.TrimSpace(upstreamModel),
		provider:       strings.TrimSpace(provider),
		executor:       strings.TrimSpace(executor),
		requestPath:    helps.PayloadRequestPath(opts),
		clientProfile:  executorMetadataStringValue(opts.Metadata, cliproxyexecutor.ClientProfileMetadataKey),
		payloadBytes:   len(payload),
		messageCount:   executorIntMetadataValue(opts.Metadata[cliproxyexecutor.MessageCountMetadataKey]),
		toolCount:      executorIntMetadataValue(opts.Metadata[cliproxyexecutor.ToolCountMetadataKey]),
	}
}

func logCountTokensSummary(ctx context.Context, meta countTokensSummaryLogMeta, inputTokens int64, duration time.Duration) {
	fields := log.Fields{
		"event":         "count_tokens_summary",
		"provider":      meta.provider,
		"executor":      meta.executor,
		"payload_bytes": meta.payloadBytes,
		"message_count": meta.messageCount,
		"tool_count":    meta.toolCount,
		"duration_ms":   duration.Milliseconds(),
		"input_tokens":  inputTokens,
	}
	if meta.requestedModel != "" {
		fields["requested_model"] = meta.requestedModel
	}
	if meta.upstreamModel != "" {
		fields["upstream_model"] = meta.upstreamModel
	}
	if meta.requestPath != "" {
		fields["request_path"] = meta.requestPath
	}
	if meta.clientProfile != "" {
		fields["client_profile"] = meta.clientProfile
	}
	helps.LogWithRequestID(ctx).WithFields(fields).Info("count tokens summary")
}

func executorMetadataStringValue(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}
