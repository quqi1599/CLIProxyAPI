package auth

import (
	"context"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestFileCooldownStateStoreSpreadStateRoundTrip(t *testing.T) {
	store := NewFileCooldownStateStore(t.TempDir())
	want := SpreadStateSnapshot{
		Version:   1,
		Namespace: "provider:openai-compatibility",
		Cursors:   map[string]int{"route::group": 7},
		CurrentWeights: map[string]map[string]int{
			"route::weighted": {"auth-a": 12, "auth-b": -12},
		},
	}

	if errSave := store.SaveSpreadState(context.Background(), want.Namespace, want); errSave != nil {
		t.Fatalf("SaveSpreadState() error = %v", errSave)
	}
	got, errLoad := store.LoadSpreadState(context.Background(), want.Namespace)
	if errLoad != nil {
		t.Fatalf("LoadSpreadState() error = %v", errLoad)
	}
	if got.Namespace != want.Namespace || got.Cursors["route::group"] != 7 || got.CurrentWeights["route::weighted"]["auth-a"] != 12 {
		t.Fatalf("loaded Spread state = %+v, want %+v", got, want)
	}
}

type blockedSpreadStore struct {
	mu      sync.Mutex
	started chan struct{}
	resume  chan struct{}
	saves   []SpreadStateSnapshot
}

func (s *blockedSpreadStore) LoadSpreadState(context.Context, string) (SpreadStateSnapshot, error) {
	return SpreadStateSnapshot{}, nil
}
func (s *blockedSpreadStore) SaveSpreadState(_ context.Context, _ string, v SpreadStateSnapshot) error {
	s.mu.Lock()
	first := len(s.saves) == 0
	s.saves = append(s.saves, v)
	s.mu.Unlock()
	if first {
		close(s.started)
		<-s.resume
	}
	return nil
}

func TestSpreadStopFlushesChangesAfterInflightSnapshot(t *testing.T) {
	store := &blockedSpreadStore{started: make(chan struct{}), resume: make(chan struct{})}
	s := &SpreadSelector{}
	s.SetSpreadStateStore(store)
	s.mu.Lock()
	s.cursors = map[string]int{"route": 1}
	s.mu.Unlock()
	s.persistSpreadStateIfDue()
	<-store.started
	s.mu.Lock()
	s.cursors["route"] = 2
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	close(store.resume)
	<-done
	s.Stop()
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saves) != 2 || store.saves[1].Cursors["route"] != 2 {
		t.Fatalf("final state lost: %+v", store.saves)
	}
}

func TestSpreadSelectorRestoresWeightedDeficitAcrossRestart(t *testing.T) {
	store := NewFileCooldownStateStore(t.TempDir())
	auths := []*Auth{
		{ID: "auth-a", Provider: "claude"},
		{ID: "auth-b", Provider: "claude"},
	}

	first := &SpreadSelector{stateNamespace: "provider:claude"}
	first.SetSpreadStateStore(store)
	if _, errPick := first.Pick(context.Background(), "claude", "claude-sonnet-4-6", cliproxyexecutor.Options{}, auths); errPick != nil {
		t.Fatalf("first Pick() error = %v", errPick)
	}
	first.Stop()

	second := &SpreadSelector{stateNamespace: "provider:claude"}
	second.SetSpreadStateStore(store)
	defer second.Stop()
	second.mu.Lock()
	defer second.mu.Unlock()
	if len(second.currentWeights) == 0 {
		t.Fatalf("restored currentWeights = empty, want persisted weighted deficit")
	}
	if len(second.currentWeights["claude:claude-sonnet-4-6::weighted"]) == 0 {
		t.Fatalf("restored route weighted state = empty, want route-scoped index")
	}
}
