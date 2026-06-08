package usage

import (
	"math"
	"strings"
	"time"
)

// StreamSummaryRecord stores the final stream timing and completion fields for one upstream attempt.
type StreamSummaryRecord struct {
	RequestID                  string
	AttemptNo                  int
	TimeToFirstChunkMs         int64
	UpstreamChunkWaitMs        int64
	UpstreamChunkWaitCount     int
	StreamDurationMs           int64
	TotalDurationMs            int64
	ChunksCount                int
	BytesOut                   int64
	DownstreamWriteMs          int64
	DownstreamWriteCalls       int
	DownstreamFlushMs          int64
	DownstreamFlushCalls       int
	StreamOutputTokens         int64
	StreamOutputTokensObserved bool
	ClientGone                 bool
	FinishReason               string
	RecordedAt                 time.Time
}

func normalizeStreamSummaryRecord(record StreamSummaryRecord) (StreamSummaryRecord, bool) {
	record.RequestID = strings.TrimSpace(record.RequestID)
	record.FinishReason = strings.TrimSpace(record.FinishReason)
	if record.RequestID == "" {
		return StreamSummaryRecord{}, false
	}
	if record.AttemptNo < 0 {
		record.AttemptNo = 0
	}
	if record.TimeToFirstChunkMs < 0 {
		record.TimeToFirstChunkMs = 0
	}
	if record.UpstreamChunkWaitMs < 0 {
		record.UpstreamChunkWaitMs = 0
	}
	if record.UpstreamChunkWaitCount < 0 {
		record.UpstreamChunkWaitCount = 0
	}
	if record.StreamDurationMs < 0 {
		record.StreamDurationMs = 0
	}
	if record.TotalDurationMs < 0 {
		record.TotalDurationMs = 0
	}
	if record.ChunksCount < 0 {
		record.ChunksCount = 0
	}
	if record.BytesOut < 0 {
		record.BytesOut = 0
	}
	if record.DownstreamWriteMs < 0 {
		record.DownstreamWriteMs = 0
	}
	if record.DownstreamWriteCalls < 0 {
		record.DownstreamWriteCalls = 0
	}
	if record.DownstreamFlushMs < 0 {
		record.DownstreamFlushMs = 0
	}
	if record.DownstreamFlushCalls < 0 {
		record.DownstreamFlushCalls = 0
	}
	if record.StreamOutputTokens < 0 {
		record.StreamOutputTokens = 0
	}
	if record.RecordedAt.IsZero() {
		record.RecordedAt = time.Now()
	}
	return record, true
}

func NormalizeStreamSummaryRecordForPersistence(record StreamSummaryRecord) (StreamSummaryRecord, bool) {
	return normalizeStreamSummaryRecord(record)
}

func computeTokensPerSecond(outputTokens, streamDurationMs int64) float64 {
	if outputTokens <= 0 || streamDurationMs <= 0 {
		return 0
	}
	perSecond := float64(outputTokens) / (float64(streamDurationMs) / 1000.0)
	return math.Round(perSecond*100) / 100
}
