package logging

import (
	"context"
	"strings"
	"sync"
)

const (
	ToolStreamRepairForceSerial             = "force_serial"
	ToolStreamRepairDropOrphanDelta         = "drop_orphan_delta"
	ToolStreamRepairDropInvalidAnnouncement = "drop_invalid_announcement"
	ToolStreamRepairFallbackDone            = "fallback_done"
)

type toolStreamRepairKey struct{}

type toolStreamRepairState struct {
	mu    sync.Mutex
	stats ToolStreamRepairStats
}

type ToolStreamRepairStats struct {
	ParallelToolCallsForced             bool
	OrphanToolDeltaDroppedCount         int
	InvalidToolAnnouncementDroppedCount int
	ToolDoneFallbackEmittedCount        int
	ToolStreamRepairKind                string
}

func (s ToolStreamRepairStats) HasData() bool {
	return s.ParallelToolCallsForced ||
		s.OrphanToolDeltaDroppedCount > 0 ||
		s.InvalidToolAnnouncementDroppedCount > 0 ||
		s.ToolDoneFallbackEmittedCount > 0
}

func WithToolStreamRepairTracking(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if state, ok := ctx.Value(toolStreamRepairKey{}).(*toolStreamRepairState); ok && state != nil {
		return ctx
	}
	return context.WithValue(ctx, toolStreamRepairKey{}, &toolStreamRepairState{})
}

func ObserveToolStreamRepair(ctx context.Context, kind string) {
	if ctx == nil {
		return
	}
	state, ok := ctx.Value(toolStreamRepairKey{}).(*toolStreamRepairState)
	if !ok || state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	switch strings.TrimSpace(kind) {
	case ToolStreamRepairForceSerial:
		state.stats.ParallelToolCallsForced = true
	case ToolStreamRepairDropOrphanDelta:
		state.stats.OrphanToolDeltaDroppedCount++
	case ToolStreamRepairDropInvalidAnnouncement:
		state.stats.InvalidToolAnnouncementDroppedCount++
	case ToolStreamRepairFallbackDone:
		state.stats.ToolDoneFallbackEmittedCount++
	}
}

func GetToolStreamRepairStats(ctx context.Context) ToolStreamRepairStats {
	if ctx == nil {
		return ToolStreamRepairStats{}
	}
	state, ok := ctx.Value(toolStreamRepairKey{}).(*toolStreamRepairState)
	if !ok || state == nil {
		return ToolStreamRepairStats{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	stats := state.stats
	stats.ToolStreamRepairKind = toolStreamRepairKind(stats)
	return stats
}

func toolStreamRepairKind(stats ToolStreamRepairStats) string {
	var kinds []string
	if stats.ParallelToolCallsForced {
		kinds = append(kinds, ToolStreamRepairForceSerial)
	}
	if stats.OrphanToolDeltaDroppedCount > 0 {
		kinds = append(kinds, ToolStreamRepairDropOrphanDelta)
	}
	if stats.InvalidToolAnnouncementDroppedCount > 0 {
		kinds = append(kinds, ToolStreamRepairDropInvalidAnnouncement)
	}
	if stats.ToolDoneFallbackEmittedCount > 0 {
		kinds = append(kinds, ToolStreamRepairFallbackDone)
	}
	return strings.Join(kinds, ",")
}
