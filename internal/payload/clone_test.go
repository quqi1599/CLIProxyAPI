package payload

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCloneBytesOwnsStorageAndCountsOnlyLargeCopies(t *testing.T) {
	before := CurrentLargeCloneMetrics()
	small := []byte("small")
	smallClone := CloneBytes(small)
	smallClone[0] = 'S'
	if bytes.Equal(small, smallClone) {
		t.Fatal("small clone shares caller storage")
	}
	if afterSmall := CurrentLargeCloneMetrics(); afterSmall.Count != before.Count || afterSmall.Bytes != before.Bytes {
		t.Fatalf("small clone changed large-copy metrics: before=%+v after=%+v", before, afterSmall)
	}

	large := make([]byte, largeCloneThresholdBytes)
	largeClone := CloneBytes(large)
	largeClone[0] = 1
	if large[0] != 0 {
		t.Fatal("large clone shares caller storage")
	}
	after := CurrentLargeCloneMetrics()
	if after.Count-before.Count != 1 || after.Bytes-before.Bytes != largeCloneThresholdBytes || after.LargestCloneBytes < largeCloneThresholdBytes || len(after.Hotspots) == 0 {
		t.Fatalf("large clone metric delta: before=%+v after=%+v", before, after)
	}
}

func TestCloneStringBytesOwnsStorageAndAttributesLargeCopiesToCaller(t *testing.T) {
	before := CurrentLargeCloneMetrics()
	source := strings.Repeat("x", largeCloneThresholdBytes)
	cloned := cloneStringBytesHotspotProbe(source)
	cloned[0] = 'y'
	if source[0] != 'x' {
		t.Fatal("string clone changed immutable source")
	}
	after := CurrentLargeCloneMetrics()
	if after.Count != before.Count+1 || after.Bytes != before.Bytes+largeCloneThresholdBytes {
		t.Fatalf("large string clone metric delta: before=%+v after=%+v", before, after)
	}
	for _, hotspot := range after.Hotspots {
		if strings.HasSuffix(hotspot.Name, "payload.cloneStringBytesHotspotProbe") {
			return
		}
	}
	t.Fatalf("large string clone caller hotspot missing: %+v", after.Hotspots)
}

//go:noinline
func cloneStringBytesHotspotProbe(source string) []byte {
	return CloneStringBytes(source)
}

func TestCloneBytesScopedTracksAndReleasesLiveCopies(t *testing.T) {
	before := CurrentLargeCloneMetrics()
	payload := make([]byte, largeCloneThresholdBytes)
	cloned, release := CloneBytesScoped(payload, "test.scoped")
	if len(cloned) != len(payload) {
		t.Fatalf("clone length = %d, want %d", len(cloned), len(payload))
	}
	during := CurrentLargeCloneMetrics()
	if during.ActiveScopedCount != before.ActiveScopedCount+1 || during.ActiveScopedBytes != before.ActiveScopedBytes+largeCloneThresholdBytes {
		t.Fatalf("active scoped metrics: before=%+v during=%+v", before, during)
	}
	if during.PeakScopedCount < during.ActiveScopedCount || during.PeakScopedBytes < during.ActiveScopedBytes {
		t.Fatalf("peak scoped metrics = %+v", during)
	}
	release()
	release()
	after := CurrentLargeCloneMetrics()
	if after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("scoped release metrics: before=%+v after=%+v", before, after)
	}
}

func TestRetainBytesScopedTracksOverlappingActiveAndPeak(t *testing.T) {
	before := CurrentLargeCloneMetrics()
	first := make([]byte, largeCloneThresholdBytes)
	second := make([]byte, largeCloneThresholdBytes+1)

	releaseFirst := RetainBytesScoped(first)
	releaseSecond := RetainBytesScoped(second)
	during := CurrentLargeCloneMetrics()
	wantBytes := before.ActiveScopedBytes + uint64(len(first)+len(second))
	if during.ActiveScopedCount != before.ActiveScopedCount+2 || during.ActiveScopedBytes != wantBytes {
		t.Fatalf("overlapping active scopes: before=%+v during=%+v", before, during)
	}
	if during.PeakScopedCount < during.ActiveScopedCount || during.PeakScopedBytes < during.ActiveScopedBytes {
		t.Fatalf("overlapping peak scopes: %+v", during)
	}
	if during.Count != before.Count || during.Bytes != before.Bytes {
		t.Fatalf("retaining owned bytes changed clone totals: before=%+v during=%+v", before, during)
	}

	releaseFirst()
	releaseSecond()
	releaseSecond()
	after := CurrentLargeCloneMetrics()
	if after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("retained scopes did not release: before=%+v after=%+v", before, after)
	}
}

func TestCloneBytesLargeMetricsAreConcurrent(t *testing.T) {
	before := CurrentLargeCloneMetrics()
	payload := make([]byte, largeCloneThresholdBytes)
	const workers = 8
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			if cloned := CloneBytes(payload); len(cloned) != len(payload) {
				t.Errorf("clone length = %d, want %d", len(cloned), len(payload))
			}
		}()
	}
	wait.Wait()
	after := CurrentLargeCloneMetrics()
	if after.Count-before.Count != workers || after.Bytes-before.Bytes != workers*largeCloneThresholdBytes {
		t.Fatalf("concurrent metric delta: before=%+v after=%+v", before, after)
	}
}

func TestLargeCloneHotspotsAreControlledAndBounded(t *testing.T) {
	recordLargeClone(1, "secret prompt must not escape")
	for index := 0; index < maxLargeCloneHotspots+16; index++ {
		recordLargeClone(1, fmt.Sprintf("test.hotspot.%d", index))
	}

	largeCloneMetrics.hotspotsMu.Lock()
	defer largeCloneMetrics.hotspotsMu.Unlock()
	if len(largeCloneMetrics.hotspots) > maxLargeCloneHotspots {
		t.Fatalf("hotspot cardinality = %d, want <= %d", len(largeCloneMetrics.hotspots), maxLargeCloneHotspots)
	}
	if _, exists := largeCloneMetrics.hotspots["secret prompt must not escape"]; exists {
		t.Fatal("uncontrolled hotspot label was retained")
	}
	if _, exists := largeCloneMetrics.hotspots[largeCloneOverflowKey]; !exists {
		t.Fatal("overflow hotspot was not aggregated")
	}
}
