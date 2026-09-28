package auth

import (
	"runtime"
	"testing"
	"time"
)

func TestDynamicSelectorCreationWithPendingManagerWriter(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	store := NewFileCooldownStateStore(t.TempDir())
	manager.SetSpreadStateStore(store)
	defer manager.stopDynamicSelectors()

	// Legacy selection already holds this read lock. A pending writer makes
	// recursive RLock acquisition block, even on the same request.
	manager.mu.RLock()
	writerDone := make(chan struct{})
	go func() {
		manager.mu.Lock()
		manager.mu.Unlock()
		close(writerDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for manager.mu.TryRLock() {
		manager.mu.RUnlock()
		if time.Now().After(deadline) {
			manager.mu.RUnlock()
			<-writerDone
			t.Fatal("writer did not reach the manager lock")
		}
		runtime.Gosched()
	}

	selected := make(chan Selector, 1)
	go func() {
		selected <- manager.selectorForStrategyGroup("group:regression", RoutingStrategySpread)
	}()
	var result Selector
	blocked := false
	select {
	case result = <-selected:
	case <-time.After(2 * time.Second):
		blocked = true
	}
	manager.mu.RUnlock()
	<-writerDone
	if blocked {
		<-selected // Let the old implementation unwind before failing.
		t.Fatal("dynamic selector creation recursively acquired the manager lock")
	}
	if _, ok := result.(*SpreadSelector); !ok {
		t.Fatalf("selector = %T, want *SpreadSelector", result)
	}
	if manager.currentSpreadStateStore() != store {
		t.Fatal("dynamic selector creation lost the configured state store")
	}
}

func TestSpreadStoreFallbackAndExplicitReplacement(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	first := NewFileCooldownStateStore(t.TempDir())
	second := NewFileCooldownStateStore(t.TempDir())
	manager.SetCooldownStateStore(first)
	if manager.currentSpreadStateStore() != first {
		t.Fatal("cooldown store fallback was not installed")
	}
	manager.SetSpreadStateStore(second)
	manager.SetCooldownStateStore(first)
	if manager.currentSpreadStateStore() != second {
		t.Fatal("cooldown fallback replaced an explicit spread store")
	}
	manager.SetSpreadStateStore(nil)
	if manager.currentSpreadStateStore() != nil {
		t.Fatal("explicit nil did not clear the spread store")
	}
}
