package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	spreadStateDirName         = ".runtime/spread"
	spreadStatePersistInterval = 5 * time.Second
)

// SpreadStateSnapshot stores the weighted-deficit indexes for one selector namespace.
// It intentionally excludes in-flight and health data, which are reconstructed from
// live requests and auth state after a restart.
type SpreadStateSnapshot struct {
	Version        int                       `json:"version"`
	Namespace      string                    `json:"namespace"`
	UpdatedAt      time.Time                 `json:"updated_at"`
	Cursors        map[string]int            `json:"cursors,omitempty"`
	CurrentWeights map[string]map[string]int `json:"current_weights,omitempty"`
}

// SpreadStateStore persists route-scoped Spread selection indexes.
type SpreadStateStore interface {
	LoadSpreadState(context.Context, string) (SpreadStateSnapshot, error)
	SaveSpreadState(context.Context, string, SpreadStateSnapshot) error
}

// bindSpreadStateStore connects a selector (including its session-affinity fallback)
// to the optional persistent weighted-deficit store.
func bindSpreadStateStore(selector Selector, store SpreadStateStore) {
	switch current := selector.(type) {
	case *SpreadSelector:
		current.SetSpreadStateStore(store)
	case *SessionAffinitySelector:
		bindSpreadStateStore(current.fallback, store)
	}
}

// SetSpreadStateStore attaches a store and restores the last route snapshot once.
func (s *SpreadSelector) SetSpreadStateStore(store SpreadStateStore) {
	if s == nil {
		return
	}
	s.mu.Lock()
	namespace := normalizeSpreadStateNamespace(s.stateNamespace)
	if s.stateStore == store && s.stateLoaded {
		s.mu.Unlock()
		return
	}
	s.stateStore = store
	s.stateNamespace = namespace
	s.stateLoaded = store == nil
	s.mu.Unlock()
	if store == nil {
		return
	}

	snapshot, errLoad := store.LoadSpreadState(context.Background(), namespace)
	if errLoad != nil {
		log.WithError(errLoad).WithField("namespace", namespace).Warn("failed to restore Spread state")
		s.mu.Lock()
		if s.stateStore == store && s.stateNamespace == namespace {
			s.stateLoaded = true
		}
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateStore != store || s.stateNamespace != namespace {
		return
	}
	if snapshot.Namespace != "" && normalizeSpreadStateNamespace(snapshot.Namespace) != namespace {
		log.WithField("namespace", namespace).Warn("ignored Spread state with mismatched namespace")
		s.stateLoaded = true
		return
	}
	if len(snapshot.Cursors) > 0 {
		if s.cursors == nil {
			s.cursors = make(map[string]int, len(snapshot.Cursors))
		}
		for key, cursor := range snapshot.Cursors {
			s.cursors[key] = cursor
		}
	}
	if len(snapshot.CurrentWeights) > 0 {
		if s.currentWeights == nil {
			s.currentWeights = make(map[string]map[string]int, len(snapshot.CurrentWeights))
		}
		for key, weights := range snapshot.CurrentWeights {
			cloned := make(map[string]int, len(weights))
			for member, deficit := range weights {
				cloned[member] = deficit
			}
			s.currentWeights[key] = cloned
		}
	}
	s.stateLoaded = true
}

// Stop flushes the latest Spread snapshot and prevents new asynchronous writes.
func (s *SpreadSelector) Stop() {
	if s == nil {
		return
	}
	s.stateStopOnce.Do(func() {
		s.statePersistMu.Lock()
		s.stateStopped = true
		s.statePersistMu.Unlock()
		// Wait for an older asynchronous snapshot before capturing the final state.
		// Otherwise shutdown during a write silently loses selections made since it.
		s.statePersistWG.Wait()
		store, namespace, snapshot, ok := s.spreadStateSnapshot()
		if ok {
			if errSave := store.SaveSpreadState(context.Background(), namespace, snapshot); errSave != nil {
				log.WithError(errSave).WithField("namespace", namespace).Warn("failed to persist final Spread state")
			}
		}
	})
}

func (s *SpreadSelector) persistSpreadStateIfDue() {
	s.persistSpreadState(false)
}

func (s *SpreadSelector) persistSpreadState(force bool) {
	if s == nil {
		return
	}
	s.statePersistMu.Lock()
	if s.stateStopped || s.statePersisting || (!force && !s.statePersistAt.IsZero() && time.Since(s.statePersistAt) < spreadStatePersistInterval) {
		s.statePersistMu.Unlock()
		return
	}
	s.statePersisting = true
	s.statePersistAt = time.Now()
	s.statePersistWG.Add(1)
	s.statePersistMu.Unlock()

	store, namespace, snapshot, ok := s.spreadStateSnapshot()
	if !ok {
		s.finishSpreadStatePersist()
		return
	}
	go func() {
		defer s.finishSpreadStatePersist()
		if errSave := store.SaveSpreadState(context.Background(), namespace, snapshot); errSave != nil {
			log.WithError(errSave).WithField("namespace", namespace).Warn("failed to persist Spread state")
		}
	}()
}

func (s *SpreadSelector) finishSpreadStatePersist() {
	s.statePersistMu.Lock()
	s.statePersisting = false
	s.statePersistMu.Unlock()
	s.statePersistWG.Done()
}

func (s *SpreadSelector) spreadStateSnapshot() (SpreadStateStore, string, SpreadStateSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateStore == nil || !s.stateLoaded {
		return nil, "", SpreadStateSnapshot{}, false
	}
	namespace := normalizeSpreadStateNamespace(s.stateNamespace)
	snapshot := SpreadStateSnapshot{
		Version:        1,
		Namespace:      namespace,
		Cursors:        make(map[string]int, len(s.cursors)),
		CurrentWeights: make(map[string]map[string]int, len(s.currentWeights)),
	}
	for key, cursor := range s.cursors {
		snapshot.Cursors[key] = cursor
	}
	for key, weights := range s.currentWeights {
		cloned := make(map[string]int, len(weights))
		for member, deficit := range weights {
			cloned[member] = deficit
		}
		snapshot.CurrentWeights[key] = cloned
	}
	return s.stateStore, namespace, snapshot, true
}

func normalizeSpreadStateNamespace(namespace string) string {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return "default"
	}
	return namespace
}

// FileCooldownStateStore also implements SpreadStateStore so both runtime state
// families share the same private .runtime directory and atomic file semantics.
func (s *FileCooldownStateStore) LoadSpreadState(ctx context.Context, namespace string) (SpreadStateSnapshot, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return SpreadStateSnapshot{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return SpreadStateSnapshot{}, errCtx
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.spreadStatePath(namespace)
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return SpreadStateSnapshot{}, nil
		}
		return SpreadStateSnapshot{}, fmt.Errorf("read Spread state %s: %w", path, errRead)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return SpreadStateSnapshot{}, nil
	}
	var snapshot SpreadStateSnapshot
	if errUnmarshal := json.Unmarshal(data, &snapshot); errUnmarshal != nil {
		return SpreadStateSnapshot{}, fmt.Errorf("parse Spread state %s: %w", path, errUnmarshal)
	}
	if snapshot.Namespace == "" {
		snapshot.Namespace = normalizeSpreadStateNamespace(namespace)
	}
	return snapshot, nil
}

func (s *FileCooldownStateStore) SaveSpreadState(ctx context.Context, namespace string, snapshot SpreadStateSnapshot) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if snapshot.Version <= 0 {
		snapshot.Version = 1
	}
	snapshot.Namespace = normalizeSpreadStateNamespace(namespace)
	snapshot.UpdatedAt = time.Now().UTC()
	data, errMarshal := json.MarshalIndent(snapshot, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal Spread state: %w", errMarshal)
	}
	data = append(data, '\n')
	path := s.spreadStatePath(snapshot.Namespace)
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create Spread state directory: %w", errMkdir)
	}
	return writeSpreadStateFile(ctx, path, data)
}

func (s *FileCooldownStateStore) spreadStatePath(namespace string) string {
	namespace = normalizeSpreadStateNamespace(namespace)
	hash := sha256.Sum256([]byte(namespace))
	filename := "spread-" + hex.EncodeToString(hash[:8]) + ".json"
	return filepath.Join(s.dir, filepath.FromSlash(spreadStateDirName), filename)
}

func writeSpreadStateFile(ctx context.Context, path string, data []byte) error {
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	dir := filepath.Dir(path)
	tmpFile, errCreate := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create Spread state temp file: %w", errCreate)
	}
	tmp := tmpFile.Name()
	if _, errWrite := tmpFile.Write(data); errWrite != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write Spread state temp file: %w", errWrite)
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close Spread state temp file: %w", errClose)
	}
	if errCtx := ctx.Err(); errCtx != nil {
		_ = os.Remove(tmp)
		return errCtx
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace Spread state file: %w", errRename)
	}
	return nil
}
