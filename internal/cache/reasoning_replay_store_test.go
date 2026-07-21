package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestReasoningReplayStore(now *time.Time, maxEntries, evictBatch int, maxBytes, maxSessionBytes int64) *reasoningReplayStore {
	return newReasoningReplayStore(reasoningReplayStoreConfig{
		ttl:             time.Hour,
		maxEntries:      maxEntries,
		evictBatchSize:  evictBatch,
		maxBytes:        maxBytes,
		maxSessionBytes: maxSessionBytes,
		now: func() time.Time {
			return *now
		},
		normalizeItems: func(items [][]byte) ([][]byte, bool) {
			if len(items) == 0 {
				return nil, false
			}
			return cloneReasoningReplayItems(items), true
		},
	})
}

func testReasoningReplayScope(key string) reasoningReplayScope {
	return reasoningReplayScope{localKey: key, kvKey: key}
}

func TestReasoningReplayStoreLifecycleAndByteAccounting(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 10, 1, 100, 10)
	scope := testReasoningReplayScope("a")
	input := []byte("abc")
	if !store.SaveBestEffort(context.Background(), scope, [][]byte{input}) {
		t.Fatal("save failed")
	}
	input[0] = 'x'

	items, found, errLoad := store.LoadRequired(context.Background(), scope)
	if errLoad != nil || !found || string(items[0]) != "abc" {
		t.Fatalf("load = %q, %v, %v; want abc, true, nil", items, found, errLoad)
	}
	items[0][0] = 'y'
	items, found, errLoad = store.LoadRequired(context.Background(), scope)
	if errLoad != nil || !found || string(items[0]) != "abc" {
		t.Fatalf("second load = %q, %v, %v; want independent abc copy", items, found, errLoad)
	}

	if !store.SaveBestEffort(context.Background(), scope, [][]byte{[]byte("abcd")}) {
		t.Fatal("overwrite failed")
	}
	if entries, bytes := store.stats(); entries != 1 || bytes != 5 {
		t.Fatalf("stats after overwrite = %d/%d, want 1/5", entries, bytes)
	}
	if errDelete := store.DeleteRequired(context.Background(), scope); errDelete != nil {
		t.Fatalf("delete: %v", errDelete)
	}
	if entries, bytes := store.stats(); entries != 0 || bytes != 0 {
		t.Fatalf("stats after delete = %d/%d, want 0/0", entries, bytes)
	}
}

func TestReasoningReplayStoreOwnsInputWhenNormalizerDoesNotClone(t *testing.T) {
	now := time.Unix(100, 0)
	store := newReasoningReplayStore(reasoningReplayStoreConfig{
		ttl:             time.Hour,
		maxEntries:      10,
		evictBatchSize:  1,
		maxBytes:        100,
		maxSessionBytes: 10,
		now: func() time.Time {
			return now
		},
		normalizeItems: func(items [][]byte) ([][]byte, bool) {
			return items, len(items) > 0
		},
	})
	input := []byte("abc")
	if !store.SaveBestEffort(context.Background(), testReasoningReplayScope("a"), [][]byte{input}) {
		t.Fatal("save failed")
	}
	input[0] = 'x'

	items, found, errLoad := store.LoadRequired(context.Background(), testReasoningReplayScope("a"))
	if errLoad != nil || !found || string(items[0]) != "abc" {
		t.Fatalf("load = %q, %v, %v; want owned abc copy", items, found, errLoad)
	}
}

func TestReasoningReplayStoreSlidingTTLAndPurge(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 10, 1, 100, 10)
	scope := testReasoningReplayScope("a")
	if !store.SaveBestEffort(context.Background(), scope, [][]byte{[]byte("x")}) {
		t.Fatal("save failed")
	}
	now = now.Add(30 * time.Minute)
	if _, found, errLoad := store.LoadRequired(context.Background(), scope); errLoad != nil || !found {
		t.Fatalf("load before ttl = found %v err %v, want hit", found, errLoad)
	}
	now = now.Add(59 * time.Minute)
	if _, found, errLoad := store.LoadRequired(context.Background(), scope); errLoad != nil || !found {
		t.Fatalf("load after sliding refresh = found %v err %v, want hit", found, errLoad)
	}
	now = now.Add(time.Hour + time.Nanosecond)
	store.PurgeExpired(now)
	if entries, bytes := store.stats(); entries != 0 || bytes != 0 {
		t.Fatalf("stats after purge = %d/%d, want 0/0", entries, bytes)
	}
}

func TestReasoningReplayStoreEvictsLeastRecentlyUsedByByteBudget(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 10, 1, 6, 10)
	for _, key := range []string{"a", "b"} {
		if !store.SaveBestEffort(context.Background(), testReasoningReplayScope(key), [][]byte{[]byte("xx")}) {
			t.Fatalf("save %s failed", key)
		}
		now = now.Add(time.Second)
	}
	if _, found, errLoad := store.LoadRequired(context.Background(), testReasoningReplayScope("a")); errLoad != nil || !found {
		t.Fatalf("touch a = found %v err %v, want hit", found, errLoad)
	}
	now = now.Add(time.Second)
	if !store.SaveBestEffort(context.Background(), testReasoningReplayScope("c"), [][]byte{[]byte("xx")}) {
		t.Fatal("save c failed")
	}
	if _, found, _ := store.LoadRequired(context.Background(), testReasoningReplayScope("b")); found {
		t.Fatal("least recently used entry b was not evicted")
	}
	for _, key := range []string{"a", "c"} {
		if _, found, errLoad := store.LoadRequired(context.Background(), testReasoningReplayScope(key)); errLoad != nil || !found {
			t.Fatalf("load %s = found %v err %v, want hit", key, found, errLoad)
		}
	}
	if entries, bytes := store.stats(); entries != 2 || bytes != 6 {
		t.Fatalf("stats after byte eviction = %d/%d, want 2/6", entries, bytes)
	}
}

func TestReasoningReplayStoreRejectsOversizedSessionWithoutReplacing(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 10, 1, 100, 3)
	scope := testReasoningReplayScope("a")
	if !store.SaveBestEffort(context.Background(), scope, [][]byte{[]byte("abc")}) {
		t.Fatal("exact-limit save failed")
	}
	if store.SaveBestEffort(context.Background(), scope, [][]byte{[]byte("abcd")}) {
		t.Fatal("over-limit save unexpectedly succeeded")
	}
	items, found, errLoad := store.LoadRequired(context.Background(), scope)
	if errLoad != nil || !found || string(items[0]) != "abc" {
		t.Fatalf("load after rejected overwrite = %q, %v, %v; want abc, true, nil", items, found, errLoad)
	}
}

func TestReasoningReplayStoreAccepts1024Items(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 10, 1, 4096, 2048)
	items := make([][]byte, 1024)
	for index := range items {
		items[index] = []byte{'x'}
	}
	if !store.SaveBestEffort(context.Background(), testReasoningReplayScope("many"), items) {
		t.Fatal("1024-item save failed")
	}
	got, found, errLoad := store.LoadRequired(context.Background(), testReasoningReplayScope("many"))
	if errLoad != nil || !found || len(got) != len(items) {
		t.Fatalf("1024-item load = %d, %v, %v; want %d, true, nil", len(got), found, errLoad, len(items))
	}
}

func TestReasoningReplayStoreRejectsRawLimitsBeforeNormalization(t *testing.T) {
	for _, tc := range []struct {
		name            string
		items           [][]byte
		maxSessionBytes int64
	}{
		{name: "bytes", items: [][]byte{[]byte("abcd")}, maxSessionBytes: 3},
		{name: "items", items: make([][]byte, reasoningReplayCacheMaxItems+1), maxSessionBytes: 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for index := range tc.items {
				if tc.items[index] == nil {
					tc.items[index] = []byte{'x'}
				}
			}
			now := time.Unix(100, 0)
			normalizeCalls := 0
			store := newReasoningReplayStore(reasoningReplayStoreConfig{
				ttl:             time.Hour,
				maxEntries:      10,
				evictBatchSize:  1,
				maxBytes:        1 << 20,
				maxSessionBytes: tc.maxSessionBytes,
				now: func() time.Time {
					return now
				},
				normalizeItems: func(items [][]byte) ([][]byte, bool) {
					normalizeCalls++
					return [][]byte{[]byte("x")}, true
				},
			})
			if store.SaveBestEffort(context.Background(), testReasoningReplayScope("save"), tc.items) {
				t.Fatal("over-limit save unexpectedly succeeded")
			}
			if normalizeCalls != 0 {
				t.Fatalf("save normalize calls = %d, want 0", normalizeCalls)
			}

			client := newFakeCodexReasoningReplayKVClient()
			raw, errMarshal := json.Marshal(tc.items)
			if errMarshal != nil {
				t.Fatalf("marshal items: %v", errMarshal)
			}
			client.values["load"] = raw
			store.config.maxEncodedBytes = reasoningReplayCacheMaxEncodedBytes
			store.config.currentKVClient = func() (reasoningReplayKVClient, bool, error) {
				return client, true, nil
			}
			loaded, found, errLoad := store.LoadRequired(context.Background(), testReasoningReplayScope("load"))
			if errLoad != nil || found || loaded != nil {
				t.Fatalf("over-limit load = %q, %v, %v; want nil, false, nil", loaded, found, errLoad)
			}
			if normalizeCalls != 0 {
				t.Fatalf("load normalize calls = %d, want 0", normalizeCalls)
			}
			if client.delCount != 1 || client.expireCount != 0 {
				t.Fatalf("invalid Home entry del/expire = %d/%d, want 1/0", client.delCount, client.expireCount)
			}
		})
	}
}

func TestReasoningReplayStoreLocalMissReturnsNilItems(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 10, 1, 100, 10)

	items, found, errLoad := store.LoadRequired(context.Background(), testReasoningReplayScope("missing"))
	if errLoad != nil || found || items != nil {
		t.Fatalf("local miss = %q, %v, %v; want nil, false, nil", items, found, errLoad)
	}
}

func TestReasoningReplayStorePreservesBatchHeadroom(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 2, 2, 100, 10)
	for _, key := range []string{"a", "b", "c"} {
		if !store.SaveBestEffort(context.Background(), testReasoningReplayScope(key), [][]byte{[]byte("x")}) {
			t.Fatalf("save %s failed", key)
		}
		now = now.Add(time.Second)
	}
	if entries, _ := store.stats(); entries != 1 {
		t.Fatalf("entries after batch eviction = %d, want 1", entries)
	}
	if _, found, _ := store.LoadRequired(context.Background(), testReasoningReplayScope("c")); !found {
		t.Fatal("newest entry was evicted")
	}
}

func TestReasoningReplayStoreConcurrentAccess(t *testing.T) {
	now := time.Unix(100, 0)
	store := newTestReasoningReplayStore(&now, 64, 4, 4096, 64)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				scope := testReasoningReplayScope(fmt.Sprintf("%d-%d", worker, iteration%16))
				store.SaveBestEffort(context.Background(), scope, [][]byte{[]byte("payload")})
				_, _, _ = store.LoadRequired(context.Background(), scope)
				if iteration%3 == 0 {
					_ = store.DeleteRequired(context.Background(), scope)
				}
			}
		}(worker)
	}
	workers.Wait()
	if entries, bytes := store.stats(); entries > 64 || bytes > 4096 {
		t.Fatalf("stats exceed limits after concurrent access = %d/%d", entries, bytes)
	}
}
