package cache

import (
	"testing"
	"time"
)

func TestBoundedLRUDoesNotRegressTimestampOnOutOfOrderAccess(t *testing.T) {
	base := time.Unix(100, 0)
	cache := newBoundedLRU[string, string](time.Hour, 10, 1, 100)
	if !cache.Set("key", "old", 3, base) {
		t.Fatal("initial set failed")
	}
	if value, ok := cache.Get("key", base.Add(-time.Hour)); !ok || value != "old" {
		t.Fatalf("out-of-order get = %q/%v, want old/true", value, ok)
	}
	if !cache.Set("key", "new", 3, base.Add(-time.Hour)) {
		t.Fatal("out-of-order overwrite failed")
	}
	if value, ok := cache.Get("key", base.Add(time.Hour)); !ok || value != "new" {
		t.Fatalf("value at exact TTL = %q/%v, want new/true", value, ok)
	}
}

func TestBoundedLRUPurgeScansPastNewerTail(t *testing.T) {
	base := time.Unix(100, 0)
	cache := newBoundedLRU[string, string](time.Hour, 10, 1, 100)
	if !cache.Set("new", "value", 5, base) {
		t.Fatal("new set failed")
	}
	if !cache.Set("old", "value", 5, base.Add(-2*time.Hour)) {
		t.Fatal("old set failed")
	}

	cache.PurgeExpired(base.Add(30 * time.Minute))
	if _, ok := cache.Get("old", base.Add(30*time.Minute)); ok {
		t.Fatal("expired entry survived purge")
	}
	if value, ok := cache.Get("new", base.Add(30*time.Minute)); !ok || value != "value" {
		t.Fatalf("new entry after purge = %q/%v, want value/true", value, ok)
	}
	if entries, bytes := cache.Stats(); entries != 1 || bytes != 5 {
		t.Fatalf("stats after purge = %d/%d, want 1/5", entries, bytes)
	}
}
