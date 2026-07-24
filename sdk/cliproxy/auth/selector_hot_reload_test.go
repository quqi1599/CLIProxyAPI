package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerSetSelectorStopsReplacedSessionAffinity(t *testing.T) {
	previous := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Minute,
	})
	manager := NewManager(nil, previous, nil)

	manager.SetSelector(&RoundRobinSelector{})

	select {
	case <-previous.cache.stopCh:
	default:
		t.Fatal("replaced session affinity selector was not stopped")
	}
}

func TestSessionCacheStopConcurrent(t *testing.T) {
	cache := NewSessionCache(time.Minute)
	const callers = 32
	var waitGroup sync.WaitGroup
	waitGroup.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer waitGroup.Done()
			cache.Stop()
		}()
	}
	waitGroup.Wait()
}

func TestManagerSelectorHotReloadConcurrentResultAccounting(t *testing.T) {
	manager := NewManager(nil, &SpreadSelector{}, nil)
	defer manager.StopAutoRefresh()
	const iterations = 500

	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		for i := 0; i < iterations; i++ {
			switch i % 3 {
			case 0:
				manager.SetSelector(&SpreadSelector{})
			case 1:
				manager.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
			default:
				manager.SetSelector(&RoundRobinSelector{})
			}
		}
	}()
	go func() {
		defer waitGroup.Done()
		for i := 0; i < iterations; i++ {
			_ = manager.useSchedulerFastPath()
			manager.invalidateSessionAffinity("hot-reload-auth")
			_, _ = manager.authMetricRouting(&Auth{ID: "hot-reload-auth", Provider: "codex"})
			manager.markSelectorLoadDone(context.Background(), "hot-reload-auth", "gpt-5.5")
			manager.markSelectorResult(context.Background(), Result{
				AuthID:  "hot-reload-auth",
				Model:   "gpt-5.5",
				Success: i%2 == 0,
				TTFT:    100 * time.Millisecond,
			})
		}
	}()
	waitGroup.Wait()

	manager.mu.RLock()
	selector := manager.selector
	manager.mu.RUnlock()
	if selector == nil {
		t.Fatal("selector is nil after hot reload")
	}
	manager.scheduler.mu.Lock()
	strategy := manager.scheduler.strategy
	manager.scheduler.mu.Unlock()
	if want := selectorStrategy(selector); strategy != want {
		t.Fatalf("scheduler strategy = %v, want %v for final selector", strategy, want)
	}
}

func TestSpreadSelectorConcurrentFirstPickAndResult(t *testing.T) {
	selector := &SpreadSelector{}
	auth := &Auth{
		ID:       "concurrent-first-pick",
		Provider: "codex",
		Attributes: map[string]string{
			"type":     "api_key",
			"base_url": "https://concurrent-first-pick.example.com/v1",
		},
	}
	start := make(chan struct{})
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		<-start
		if _, err := selector.Pick(context.Background(), "codex", "gpt-5.5", cliproxyexecutor.Options{}, []*Auth{auth}); err != nil {
			t.Errorf("Pick() error = %v", err)
		}
	}()
	go func() {
		defer waitGroup.Done()
		<-start
		selector.MarkResult(auth.ID, "gpt-5.5", true, 100*time.Millisecond)
	}()
	close(start)
	waitGroup.Wait()
}
