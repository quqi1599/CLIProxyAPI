package payload

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const largeCloneThresholdBytes = 1 << 20

const (
	maxLargeCloneHotspots = 64
	largeCloneOverflowKey = "overflow"
)

// LargeCloneMetrics tracks only copies at or above one MiB so normal stream
// chunks do not pay an atomic-accounting cost.
type LargeCloneMetrics struct {
	Count             uint64              `json:"count"`
	Bytes             uint64              `json:"bytes"`
	LargestCloneBytes uint64              `json:"largest_clone_bytes"`
	ActiveScopedCount uint64              `json:"active_scoped_count"`
	ActiveScopedBytes uint64              `json:"active_scoped_bytes"`
	PeakScopedCount   uint64              `json:"peak_scoped_count"`
	PeakScopedBytes   uint64              `json:"peak_scoped_bytes"`
	Hotspots          []LargeCloneHotspot `json:"hotspots,omitempty"`
}

// LargeCloneHotspot is a bounded aggregate keyed by a compiled call site.
type LargeCloneHotspot struct {
	Name  string `json:"name"`
	Count uint64 `json:"count"`
	Bytes uint64 `json:"bytes"`
}

type largeCloneHotspotCounters struct {
	count atomic.Uint64
	bytes atomic.Uint64
}

var largeCloneMetrics struct {
	count             atomic.Uint64
	bytes             atomic.Uint64
	largest           atomic.Uint64
	activeScopedCount atomic.Uint64
	activeScopedBytes atomic.Uint64
	peakScopedCount   atomic.Uint64
	peakScopedBytes   atomic.Uint64
	hotspotsMu        sync.Mutex
	hotspots          map[string]*largeCloneHotspotCounters
}

// CloneBytes makes an owned copy and records copies large enough to affect the
// request-level payload copy budget. Callers should still avoid cloning when
// immutable ownership can be shared safely.
func CloneBytes(source []byte) []byte {
	if len(source) == 0 {
		return nil
	}
	cloned := make([]byte, len(source))
	copy(cloned, source)
	if len(source) >= largeCloneThresholdBytes {
		size := uint64(len(source))
		recordLargeClone(size, largeCloneCaller(2))
	}
	return cloned
}

// CloneStringBytes converts an immutable string to owned bytes and applies the
// same large-copy accounting as CloneBytes without making a second byte copy.
func CloneStringBytes(source string) []byte {
	if source == "" {
		return nil
	}
	cloned := []byte(source)
	if len(cloned) >= largeCloneThresholdBytes {
		recordLargeClone(uint64(len(cloned)), largeCloneCaller(2))
	}
	return cloned
}

// CloneBytesScoped records the live lifetime of a large owned copy. Callers
// must invoke the returned release function exactly when the copy is no longer retained.
func CloneBytesScoped(source []byte, hotspot string) ([]byte, func()) {
	if len(source) == 0 {
		return nil, func() {}
	}
	cloned := make([]byte, len(source))
	copy(cloned, source)
	if len(source) < largeCloneThresholdBytes {
		return cloned, func() {}
	}
	size := uint64(len(source))
	if strings.TrimSpace(hotspot) == "" {
		hotspot = largeCloneCaller(2)
	}
	recordLargeClone(size, hotspot)
	return cloned, retainLargeClone(size)
}

// RetainBytesScoped records how long an already-owned large byte slice remains
// live without making another copy. Callers must release the returned scope
// when the retained reference is replaced or its owner closes.
func RetainBytesScoped(source []byte) func() {
	if len(source) < largeCloneThresholdBytes {
		return func() {}
	}
	return retainLargeClone(uint64(len(source)))
}

func retainLargeClone(size uint64) func() {
	activeCount := largeCloneMetrics.activeScopedCount.Add(1)
	activeBytes := largeCloneMetrics.activeScopedBytes.Add(size)
	updateAtomicPeak(&largeCloneMetrics.peakScopedCount, activeCount)
	updateAtomicPeak(&largeCloneMetrics.peakScopedBytes, activeBytes)
	var once sync.Once
	return func() {
		once.Do(func() {
			largeCloneMetrics.activeScopedCount.Add(^uint64(0))
			largeCloneMetrics.activeScopedBytes.Add(^uint64(size - 1))
		})
	}
}

func recordLargeClone(size uint64, hotspot string) {
	largeCloneMetrics.count.Add(1)
	largeCloneMetrics.bytes.Add(size)
	updateAtomicPeak(&largeCloneMetrics.largest, size)
	hotspot = normalizeTransformMetadataID(hotspot)
	if hotspot == "" {
		hotspot = "unknown"
	}
	largeCloneMetrics.hotspotsMu.Lock()
	if largeCloneMetrics.hotspots == nil {
		largeCloneMetrics.hotspots = make(map[string]*largeCloneHotspotCounters, maxLargeCloneHotspots)
	}
	counters := largeCloneMetrics.hotspots[hotspot]
	if counters == nil {
		// Reserve one slot for overflow so the backing map itself never exceeds
		// the advertised cardinality cap.
		if hotspot != largeCloneOverflowKey && len(largeCloneMetrics.hotspots) >= maxLargeCloneHotspots-1 {
			hotspot = largeCloneOverflowKey
			counters = largeCloneMetrics.hotspots[hotspot]
		}
		if counters == nil {
			counters = &largeCloneHotspotCounters{}
			largeCloneMetrics.hotspots[hotspot] = counters
		}
	}
	largeCloneMetrics.hotspotsMu.Unlock()
	counters.count.Add(1)
	counters.bytes.Add(size)
}

func updateAtomicPeak(target *atomic.Uint64, value uint64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func largeCloneCaller(skip int) string {
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	function := runtime.FuncForPC(pc)
	if function == nil {
		return "unknown"
	}
	name := function.Name()
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	return name
}

// CurrentLargeCloneMetrics returns a lock-free process snapshot.
func CurrentLargeCloneMetrics() LargeCloneMetrics {
	hotspots := make([]LargeCloneHotspot, 0, 16)
	largeCloneMetrics.hotspotsMu.Lock()
	for name, counters := range largeCloneMetrics.hotspots {
		if counters == nil {
			continue
		}
		hotspots = append(hotspots, LargeCloneHotspot{Name: name, Count: counters.count.Load(), Bytes: counters.bytes.Load()})
	}
	largeCloneMetrics.hotspotsMu.Unlock()
	sort.Slice(hotspots, func(i, j int) bool {
		if hotspots[i].Bytes == hotspots[j].Bytes {
			return hotspots[i].Name < hotspots[j].Name
		}
		return hotspots[i].Bytes > hotspots[j].Bytes
	})
	if len(hotspots) > 16 {
		hotspots = hotspots[:16]
	}
	return LargeCloneMetrics{
		Count:             largeCloneMetrics.count.Load(),
		Bytes:             largeCloneMetrics.bytes.Load(),
		LargestCloneBytes: largeCloneMetrics.largest.Load(),
		ActiveScopedCount: largeCloneMetrics.activeScopedCount.Load(),
		ActiveScopedBytes: largeCloneMetrics.activeScopedBytes.Load(),
		PeakScopedCount:   largeCloneMetrics.peakScopedCount.Load(),
		PeakScopedBytes:   largeCloneMetrics.peakScopedBytes.Load(),
		Hotspots:          hotspots,
	}
}
