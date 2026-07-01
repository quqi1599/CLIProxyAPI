package auth

import (
	"context"
	"sync"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

type streamSummaryContextKey struct{}

type streamSummaryRuntimeState struct {
	mu sync.Mutex

	meta    streamExecutionLogMeta
	attempt coreusage.RequestAttempt
	record  internalusage.StreamSummaryRecord

	downstreamWriteDuration time.Duration
	downstreamWriteCalls    int
	downstreamFlushDuration time.Duration
	downstreamFlushCalls    int

	upstreamDone   bool
	downstreamDone bool
	finalized      bool
}

func WithStreamSummaryTracking(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if state := streamSummaryRuntimeStateFromContext(ctx); state != nil {
		return ctx
	}
	return context.WithValue(ctx, streamSummaryContextKey{}, &streamSummaryRuntimeState{})
}

func ObserveStreamDownstreamWrite(ctx context.Context, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	state := streamSummaryRuntimeStateFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finalized {
		return
	}
	state.downstreamWriteDuration += duration
	state.downstreamWriteCalls++
}

func ObserveStreamDownstreamFlush(ctx context.Context, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	state := streamSummaryRuntimeStateFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finalized {
		return
	}
	state.downstreamFlushDuration += duration
	state.downstreamFlushCalls++
}

func MarkStreamSummaryDownstreamDone(ctx context.Context) {
	state := streamSummaryRuntimeStateFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.downstreamDone {
		state.mu.Unlock()
		return
	}
	state.downstreamDone = true
	record, meta, attempt, ready := state.snapshotForFinalizeLocked()
	state.mu.Unlock()

	if ready {
		logAndPersistStreamSummary(ctx, meta, attempt, record)
	}
}

func completeStreamSummaryUpstream(ctx context.Context, meta streamExecutionLogMeta, attempt coreusage.RequestAttempt, record internalusage.StreamSummaryRecord) bool {
	state := streamSummaryRuntimeStateFromContext(ctx)
	if state == nil {
		return false
	}

	state.mu.Lock()
	if state.finalized {
		state.mu.Unlock()
		return true
	}
	state.meta = meta
	state.attempt = attempt
	state.record = record
	state.upstreamDone = true
	snapshot, snapshotMeta, snapshotAttempt, ready := state.snapshotForFinalizeLocked()
	state.mu.Unlock()

	if ready {
		logAndPersistStreamSummary(ctx, snapshotMeta, snapshotAttempt, snapshot)
	}
	return true
}

func (s *streamSummaryRuntimeState) snapshotForFinalizeLocked() (internalusage.StreamSummaryRecord, streamExecutionLogMeta, coreusage.RequestAttempt, bool) {
	if s == nil || s.finalized || !s.upstreamDone || !s.downstreamDone {
		return internalusage.StreamSummaryRecord{}, streamExecutionLogMeta{}, coreusage.RequestAttempt{}, false
	}

	record := s.record
	record.DownstreamWriteMs = s.downstreamWriteDuration.Milliseconds()
	record.DownstreamWriteCalls = s.downstreamWriteCalls
	record.DownstreamFlushMs = s.downstreamFlushDuration.Milliseconds()
	record.DownstreamFlushCalls = s.downstreamFlushCalls
	s.finalized = true
	return record, s.meta, s.attempt, true
}

func streamSummaryRuntimeStateFromContext(ctx context.Context) *streamSummaryRuntimeState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(streamSummaryContextKey{}).(*streamSummaryRuntimeState)
	return state
}

func logAndPersistStreamSummary(ctx context.Context, meta streamExecutionLogMeta, attempt coreusage.RequestAttempt, record internalusage.StreamSummaryRecord) {
	if record.RequestID == "" {
		record.RequestID = attempt.RequestID
	}
	if record.RequestID == "" {
		record.RequestID = internallogging.GetRequestID(ctx)
	}
	if record.AttemptNo <= 0 {
		record.AttemptNo = attempt.AttemptNo
	}
	record, ok := internalusage.NormalizeStreamSummaryRecordForPersistence(record)
	if !ok {
		return
	}

	fields := log.Fields{
		"event":                         "stream_execution_summary",
		"requested_model":               meta.requestedModel,
		"upstream_model":                meta.upstreamModel,
		"provider":                      meta.provider,
		"executor":                      meta.executor,
		"request_path":                  meta.requestPath,
		"compat_kind":                   meta.compatKind,
		"compat_kind_source":            meta.compatKindSource,
		"compat_mapping":                meta.compatMapping,
		"time_to_first_chunk_ms":        record.TimeToFirstChunkMs,
		"upstream_chunk_wait_ms":        record.UpstreamChunkWaitMs,
		"upstream_chunk_wait_count":     record.UpstreamChunkWaitCount,
		"stream_duration_ms":            record.StreamDurationMs,
		"total_duration_ms":             record.TotalDurationMs,
		"downstream_write_ms":           record.DownstreamWriteMs,
		"downstream_write_calls":        record.DownstreamWriteCalls,
		"downstream_flush_ms":           record.DownstreamFlushMs,
		"downstream_flush_calls":        record.DownstreamFlushCalls,
		"chunks_count":                  record.ChunksCount,
		"bytes_out":                     record.BytesOut,
		"stream_output_tokens":          record.StreamOutputTokens,
		"stream_output_tokens_observed": record.StreamOutputTokensObserved,
		"output_tokens":                 record.StreamOutputTokens,
		"tokens_per_second":             streamTokensPerSecond(record.StreamOutputTokens, time.Duration(record.StreamDurationMs)*time.Millisecond),
		"client_gone":                   record.ClientGone,
		"finish_reason":                 record.FinishReason,
	}
	addToolShapeLogFields(fields, meta.toolShape)
	addToolStreamRepairLogFields(fields, internallogging.GetToolStreamRepairStats(ctx))
	addStreamSummaryAttemptFields(fields, attempt)
	logEntryWithRequestID(ctx).WithFields(fields).Info("stream execution summary")

	if dbPlugin := internalusage.GetDatabasePlugin(); dbPlugin != nil {
		dbPlugin.HandleStreamSummary(ctx, record)
	}
}

func addToolShapeLogFields(fields log.Fields, shape coreusage.ToolShape) {
	if len(fields) == 0 || !shape.HasData() {
		return
	}
	if shape.DeclaredToolCount > 0 {
		fields["declared_tool_count"] = shape.DeclaredToolCount
	}
	if shape.InteractionCount > 0 {
		fields["tool_interaction_count"] = shape.InteractionCount
	}
	if shape.MCPToolCount > 0 {
		fields["mcp_tool_count"] = shape.MCPToolCount
	}
	if shape.BuiltinToolCount > 0 {
		fields["builtin_tool_count"] = shape.BuiltinToolCount
	}
	if shape.ToolTypes != "" {
		fields["tool_types"] = shape.ToolTypes
	}
	if shape.ToolNameHashes != "" {
		fields["tool_name_hashes"] = shape.ToolNameHashes
	}
}

func addToolStreamRepairLogFields(fields log.Fields, stats internallogging.ToolStreamRepairStats) {
	if len(fields) == 0 || !stats.HasData() {
		return
	}
	fields["parallel_tool_calls_forced"] = stats.ParallelToolCallsForced
	if stats.ToolStreamRepairKind != "" {
		fields["tool_stream_repair_kind"] = stats.ToolStreamRepairKind
	}
	if stats.OrphanToolDeltaDroppedCount > 0 {
		fields["orphan_tool_delta_dropped_count"] = stats.OrphanToolDeltaDroppedCount
	}
	if stats.InvalidToolAnnouncementDroppedCount > 0 {
		fields["invalid_tool_announcement_dropped_count"] = stats.InvalidToolAnnouncementDroppedCount
	}
	if stats.ToolDoneFallbackEmittedCount > 0 {
		fields["tool_done_fallback_emitted_count"] = stats.ToolDoneFallbackEmittedCount
	}
}

func addStreamSummaryAttemptFields(fields log.Fields, attempt coreusage.RequestAttempt) {
	if len(fields) == 0 {
		return
	}
	if attempt.RequestID != "" {
		fields["request_id"] = attempt.RequestID
	}
	if attempt.AttemptNo > 0 {
		fields["attempt_no"] = attempt.AttemptNo
	}
	if attempt.RetryReason != "" {
		fields["retry_reason"] = attempt.RetryReason
	}
}
