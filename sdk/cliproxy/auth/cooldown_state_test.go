package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

type recordingCooldownStateStore struct {
	saveCount atomic.Int32
	mu        sync.Mutex
	records   []CooldownStateRecord
	load      []CooldownStateRecord
}

type recordingPolicyCooldownStateStore struct {
	recordingCooldownStateStore
	policySaveCount atomic.Int32
	policyMu        sync.Mutex
	policyRecords   []GPTFirstEventPolicyStateRecord
	policyLoad      []GPTFirstEventPolicyStateRecord
	policySaved     chan struct{}
}

type blockingPolicyCooldownStateStore struct {
	recordingCooldownStateStore
	policySaveCount atomic.Int32
	firstStarted    chan struct{}
	releaseFirst    chan struct{}
	policySaved     chan struct{}
	policyMu        sync.Mutex
	policySaves     [][]GPTFirstEventPolicyStateRecord
}

func (s *recordingCooldownStateStore) Load(context.Context) ([]CooldownStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneCooldownStateRecords(s.load), nil
}

func (s *recordingCooldownStateStore) Save(_ context.Context, records []CooldownStateRecord) error {
	s.saveCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = cloneCooldownStateRecords(records)
	return nil
}

func (s *recordingPolicyCooldownStateStore) LoadGPTFirstEventPolicyStates(context.Context) ([]GPTFirstEventPolicyStateRecord, error) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	return append([]GPTFirstEventPolicyStateRecord(nil), s.policyLoad...), nil
}

func (s *recordingPolicyCooldownStateStore) SaveGPTFirstEventPolicyStates(_ context.Context, records []GPTFirstEventPolicyStateRecord) error {
	s.policySaveCount.Add(1)
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	s.policyRecords = append([]GPTFirstEventPolicyStateRecord(nil), records...)
	if s.policySaved != nil {
		select {
		case s.policySaved <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *blockingPolicyCooldownStateStore) LoadGPTFirstEventPolicyStates(context.Context) ([]GPTFirstEventPolicyStateRecord, error) {
	return nil, nil
}

func (s *blockingPolicyCooldownStateStore) SaveGPTFirstEventPolicyStates(ctx context.Context, records []GPTFirstEventPolicyStateRecord) error {
	call := s.policySaveCount.Add(1)
	if call == 1 {
		close(s.firstStarted)
		select {
		case <-s.releaseFirst:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.policyMu.Lock()
	s.policySaves = append(s.policySaves, append([]GPTFirstEventPolicyStateRecord(nil), records...))
	s.policyMu.Unlock()
	s.policySaved <- struct{}{}
	return nil
}

func cloneCooldownStateRecords(records []CooldownStateRecord) []CooldownStateRecord {
	if len(records) == 0 {
		return nil
	}
	cloned := make([]CooldownStateRecord, len(records))
	for i := range records {
		cloned[i] = records[i]
		cloned[i].LastError = cloneError(records[i].LastError)
	}
	return cloned
}

func TestFileCooldownStateStore_StateRelativePath(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auths")
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)

	cases := []struct {
		name   string
		record CooldownStateRecord
		want   string
	}{
		{
			name: "absolute auth file under auth dir",
			record: CooldownStateRecord{
				AuthID:   "auth-1",
				AuthFile: filepath.Join(authDir, "nested", "xai.json"),
			},
			want: filepath.Join("nested", "xai.cds"),
		},
		{
			name: "relative auth file",
			record: CooldownStateRecord{
				AuthID:   "auth-2",
				AuthFile: filepath.Join("team", "xai.json"),
			},
			want: filepath.Join("team", "xai.cds"),
		},
		{
			name: "absolute auth file outside auth dir",
			record: CooldownStateRecord{
				AuthID:   "auth-3",
				AuthFile: filepath.Join(t.TempDir(), "outside.json"),
			},
			want: "outside.cds",
		},
		{
			name: "relative parent escape is rejected",
			record: CooldownStateRecord{
				AuthID:   "auth-4",
				AuthFile: filepath.Join("..", "escape.json"),
			},
			want: "",
		},
		{
			name: "auth id fallback",
			record: CooldownStateRecord{
				AuthID: "auth/id 5",
			},
			want: "auth_id_5.cds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.stateRelativePath(tc.record); got != tc.want {
				t.Fatalf("stateRelativePath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFileCooldownStateStore_SaveLoadAndCleanStale(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	ctx := context.Background()

	stalePath := filepath.Join(authDir, "stale.cds")
	if errWrite := os.WriteFile(stalePath, []byte("{}\n"), 0o600); errWrite != nil {
		t.Fatalf("write stale file: %v", errWrite)
	}

	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	updatedAt := time.Now().UTC().Truncate(time.Second)
	record := CooldownStateRecord{
		Provider:       "xai",
		AuthID:         "auth-1",
		AuthFile:       filepath.Join(authDir, "xai.json"),
		Model:          "grok-4",
		Status:         "cooling",
		NextRetryAfter: nextRetry,
		Reason:         "quota",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: nextRetry,
			BackoffLevel:  1,
		},
		LastError: &Error{Message: "rate limited", HTTPStatus: 429},
		UpdatedAt: updatedAt,
	}

	if errSave := store.Save(ctx, []CooldownStateRecord{record}); errSave != nil {
		t.Fatalf("Save() returned error: %v", errSave)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "xai.cds")); errStat != nil {
		t.Fatalf("expected xai.cds to exist: %v", errStat)
	}
	if _, errStat := os.Stat(stalePath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("expected stale.cds to be removed, stat error = %v", errStat)
	}

	loaded, errLoad := store.Load(ctx)
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded records = %d, want 1", len(loaded))
	}
	if loaded[0].AuthID != record.AuthID || loaded[0].Model != record.Model || !loaded[0].NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("loaded record = %+v, want auth/model/retry from %+v", loaded[0], record)
	}
	if loaded[0].LastError == nil || loaded[0].LastError.HTTPStatus != 429 {
		t.Fatalf("loaded last error = %+v, want HTTP 429", loaded[0].LastError)
	}

	if errSave := store.Save(ctx, nil); errSave != nil {
		t.Fatalf("Save(nil) returned error: %v", errSave)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "xai.cds")); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("expected xai.cds to be removed, stat error = %v", errStat)
	}
}

func TestFileCooldownStateStore_ConcurrentSave(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	ctx := context.Background()
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Save(ctx, []CooldownStateRecord{
				{
					Provider:       "xai",
					AuthID:         "auth-1",
					AuthFile:       filepath.Join(authDir, "xai.json"),
					Model:          "grok-4",
					Status:         "cooling",
					NextRetryAfter: nextRetry.Add(time.Duration(i) * time.Second),
					UpdatedAt:      nextRetry,
				},
			})
		}()
	}
	wg.Wait()
	close(errs)
	for errSave := range errs {
		if errSave != nil {
			t.Fatalf("Save() returned error: %v", errSave)
		}
	}

	loaded, errLoad := store.Load(ctx)
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded records = %d, want 1", len(loaded))
	}

	tmpMatches, errGlob := filepath.Glob(filepath.Join(authDir, "*.tmp"))
	if errGlob != nil {
		t.Fatalf("glob temp files: %v", errGlob)
	}
	if len(tmpMatches) != 0 {
		t.Fatalf("leftover temp files = %v, want none", tmpMatches)
	}
}

func TestFileCooldownStateStore_SaveLoadGPTFirstEventPolicyStatesIndependently(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	ctx := context.Background()
	now := time.Now().UTC()
	cooldown := CooldownStateRecord{
		Provider:       "codex",
		AuthID:         "auth-1",
		AuthFile:       filepath.Join(authDir, "codex.json"),
		Model:          "gpt-5.6-sol",
		NextRetryAfter: now.Add(time.Hour),
		UpdatedAt:      now,
	}
	policy := GPTFirstEventPolicyStateRecord{
		Model:            "gpt-5.6-sol",
		PolicyState:      gptFirstEventPolicyStateSlow40,
		PreviousState:    gptFirstEventPolicyStateSlow30,
		DecisionReason:   "local_timeout_pressure",
		LastTransitionAt: now.Add(-time.Minute),
		UpdatedAt:        now,
	}

	if errSave := store.Save(ctx, []CooldownStateRecord{cooldown}); errSave != nil {
		t.Fatalf("Save() returned error: %v", errSave)
	}
	if errSave := store.SaveGPTFirstEventPolicyStates(ctx, []GPTFirstEventPolicyStateRecord{policy}); errSave != nil {
		t.Fatalf("SaveGPTFirstEventPolicyStates() returned error: %v", errSave)
	}
	policyPath := store.gptFirstEventPolicyStatePath()
	if _, errStat := os.Stat(policyPath); errStat != nil {
		t.Fatalf("expected policy state document to exist: %v", errStat)
	}
	loaded, errLoad := store.LoadGPTFirstEventPolicyStates(ctx)
	if errLoad != nil {
		t.Fatalf("LoadGPTFirstEventPolicyStates() returned error: %v", errLoad)
	}
	if len(loaded) != 1 || loaded[0].Model != policy.Model || loaded[0].PolicyState != policy.PolicyState || !loaded[0].UpdatedAt.Equal(now) {
		t.Fatalf("loaded policy records = %+v, want %+v", loaded, policy)
	}

	if errSave := store.Save(ctx, nil); errSave != nil {
		t.Fatalf("Save(nil) returned error: %v", errSave)
	}
	if _, errStat := os.Stat(policyPath); errStat != nil {
		t.Fatalf("cooldown cleanup removed independent policy state: %v", errStat)
	}

	if errSave := store.Save(ctx, []CooldownStateRecord{cooldown}); errSave != nil {
		t.Fatalf("Save() returned error: %v", errSave)
	}
	if errSave := store.SaveGPTFirstEventPolicyStates(ctx, nil); errSave != nil {
		t.Fatalf("SaveGPTFirstEventPolicyStates(nil) returned error: %v", errSave)
	}
	if _, errStat := os.Stat(policyPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("expected policy state document to be removed, stat error = %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "codex.cds")); errStat != nil {
		t.Fatalf("policy cleanup removed .cds state: %v", errStat)
	}
}

func TestManager_MarkResult_PersistsCooldownOnlyWhenStateChanges(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "xai", Model: "grok-4", Success: true})
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("healthy success saved cooldown state %d times, want 0", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4",
		Success:  false,
		Error:    &Error{Message: "upstream unavailable", HTTPStatus: 500},
	})
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("cooldown failure saved cooldown state %d times, want 1", got)
	}

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "xai", Model: "grok-4", Success: true})
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("cooldown clear saved cooldown state %d times, want 2", got)
	}

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "xai", Model: "grok-4", Success: true})
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("clean success saved cooldown state %d times, want 2", got)
	}
}

func TestManager_RestoreCooldownStates(t *testing.T) {
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	store := &recordingCooldownStateStore{
		load: []CooldownStateRecord{
			{
				Provider:       "xai",
				AuthID:         "auth-1",
				Model:          "grok-4",
				Status:         "cooling",
				NextRetryAfter: nextRetry,
				Reason:         "quota",
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: nextRetry,
				},
				LastError: &Error{Message: "rate limited", HTTPStatus: 429},
				UpdatedAt: nextRetry.Add(-time.Minute),
			},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "auth-1", Provider: "xai"}); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() returned error: %v", errRestore)
	}

	auth, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("restored auth was not found")
	}
	state := auth.ModelStates["grok-4"]
	if state == nil {
		t.Fatal("model state was not restored")
	}
	if !state.Unavailable || state.Status != StatusError || !state.NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("restored state = %+v, want unavailable status error until %v", state, nextRetry)
	}
	if state.LastError == nil || state.LastError.HTTPStatus != 429 {
		t.Fatalf("restored last error = %+v, want HTTP 429", state.LastError)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("restore cleanup saved cooldown state %d times, want 1", got)
	}
}

func TestManager_RestoreModelCooldownDoesNotBlockSiblingModel(t *testing.T) {
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	store := &recordingCooldownStateStore{load: []CooldownStateRecord{{
		Provider: "codex", AuthID: "codex-model-only", Model: "gpt-5.4",
		Status: "cooling", NextRetryAfter: nextRetry, Reason: "rate_limit",
		Quota:     QuotaState{Exceeded: true, Reason: "rate_limit", NextRecoverAt: nextRetry},
		LastError: &Error{Kind: string(failurecontract.RateLimited), Scope: string(failurecontract.ScopeModel), HTTPStatus: 429},
		UpdatedAt: nextRetry.Add(-time.Minute),
	}}}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "codex-model-only", Provider: "codex"}); err != nil {
		t.Fatalf("Register() returned error: %v", err)
	}
	if err := manager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("RestoreCooldownStates() returned error: %v", err)
	}
	auth, _ := manager.GetByID("codex-model-only")
	if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded {
		t.Fatalf("model record leaked to credential state: %+v", auth)
	}
	if blocked, _, _ := isAuthBlockedForModelRoute(auth, "gpt-5.4", time.Now(), true); !blocked {
		t.Fatal("restored gpt-5.4 cooldown was not enforced")
	}
	if blocked, reason, _ := isAuthBlockedForModelRoute(auth, "gpt-5.6-terra", time.Now(), true); blocked {
		t.Fatalf("sibling model was blocked: %v", reason)
	}
}

func TestManager_AccountQuotaSurvivesLaterModelRateLimitAndRestore(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	auth := &Auth{ID: "account-quota-preserve", Provider: "codex", Status: StatusActive}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register() returned error: %v", err)
	}
	retryAfter := time.Hour
	quotaFailure := &failurecontract.Failure{
		Kind: failurecontract.QuotaExceeded, Scope: failurecontract.ScopeCredential,
		HTTPStatus: 429, OuterStatus: 429, SemanticCode: "usage_limit_reached",
		RetryAfter: &retryAfter, PublicMessage: "account quota exhausted",
	}
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5.4", Cause: quotaFailure})
	modelRetry := 30 * time.Second
	manager.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "codex", Model: "gpt-5.6-terra", RetryAfter: &modelRetry,
		Error: &Error{Kind: string(failurecontract.RateLimited), Scope: string(failurecontract.ScopeCredential), HTTPStatus: 429, Message: "rate limited"},
	})
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5.6-sol", Success: true})
	updated, _ := manager.GetByID(auth.ID)
	if !updated.Unavailable || !updated.Quota.Exceeded || updated.LastError == nil || updated.LastError.Code != "usage_limit_reached" {
		t.Fatalf("account quota was overwritten by sibling results: %+v", updated)
	}
	store.mu.Lock()
	loaded := cloneCooldownStateRecords(store.records)
	store.mu.Unlock()
	restarted := NewManager(nil, nil, nil)
	restarted.SetCooldownStateStore(&recordingCooldownStateStore{load: loaded})
	if _, err := restarted.Register(WithSkipPersist(context.Background()), &Auth{ID: auth.ID, Provider: "codex"}); err != nil {
		t.Fatalf("restart Register() returned error: %v", err)
	}
	if err := restarted.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("restart RestoreCooldownStates() returned error: %v", err)
	}
	restored, _ := restarted.GetByID(auth.ID)
	if !restored.Unavailable || !restored.Quota.Exceeded || restored.LastError == nil || restored.LastError.Code != "usage_limit_reached" {
		t.Fatalf("account quota did not survive restore: %+v", restored)
	}
}

func TestManager_RestoreCooldownStatesRestoresPolicyWithoutCooldownRecords(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	now := time.Now().UTC()

	first := NewManager(nil, nil, nil)
	first.SetCooldownStateStore(store)
	first.gptFirstEventObserver.restorePolicyStates([]GPTFirstEventPolicyStateRecord{
		{
			Model:            "gpt-5.6-sol",
			PolicyState:      gptFirstEventPolicyStateSlow40,
			PreviousState:    gptFirstEventPolicyStateSlow30,
			DecisionReason:   "local_timeout_pressure",
			LastTransitionAt: now.Add(-time.Minute),
			UpdatedAt:        now,
		},
	})
	first.persistGPTFirstEventPolicyStates(context.Background())

	cooldowns, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(cooldowns) != 0 {
		t.Fatalf("cooldown records = %d, want none", len(cooldowns))
	}

	second := NewManager(nil, nil, nil)
	second.SetCooldownStateStore(store)
	if errRestore := second.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() returned error: %v", errRestore)
	}
	snapshot := second.GPTFirstEventPolicySnapshot("gpt-5.6-sol")
	if snapshot.PolicyState != gptFirstEventPolicyStateSlow40 || snapshot.EnforcedTimeoutMs != 40000 {
		t.Fatalf("restored policy = %q %dms, want slow_40s/40000ms", snapshot.PolicyState, snapshot.EnforcedTimeoutMs)
	}
	if snapshot.EligibleFirstAttempts != 0 {
		t.Fatalf("restored sample count = %d, want no persisted samples", snapshot.EligibleFirstAttempts)
	}
}

func TestManager_PersistsGPTFirstEventPolicyOnlyOnTransitionOrCheckpoint(t *testing.T) {
	store := &recordingPolicyCooldownStateStore{policySaved: make(chan struct{}, 1)}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	manager.gptFirstEventObserver.restorePolicyStates([]GPTFirstEventPolicyStateRecord{
		{Model: "gpt-5.6-sol", PolicyState: gptFirstEventPolicyStateSlow30, UpdatedAt: time.Now()},
	})

	manager.persistGPTFirstEventPolicyUpdate(context.Background(), GPTFirstEventPolicySnapshot{})
	if got := store.policySaveCount.Load(); got != 0 {
		t.Fatalf("non-transition saved policy state %d times, want 0", got)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	manager.persistGPTFirstEventPolicyUpdate(canceledCtx, GPTFirstEventPolicySnapshot{Transitioned: true})
	select {
	case <-store.policySaved:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for asynchronous policy persistence")
	}
	if got := store.policySaveCount.Load(); got != 1 {
		t.Fatalf("transition saved policy state %d times, want 1", got)
	}
	manager.persistGPTFirstEventPolicyUpdate(context.Background(), GPTFirstEventPolicySnapshot{stateCheckpointed: true})
	select {
	case <-store.policySaved:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for asynchronous checkpoint persistence")
	}
	if got := store.policySaveCount.Load(); got != 2 {
		t.Fatalf("checkpoint saved policy state %d total times, want 2", got)
	}
	store.policyMu.Lock()
	records := append([]GPTFirstEventPolicyStateRecord(nil), store.policyRecords...)
	store.policyMu.Unlock()
	if len(records) != 1 || records[0].Model != "gpt-5.6-sol" || records[0].PolicyState != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("persisted policy records = %+v", records)
	}
}

func TestManager_SerializesGPTFirstEventPolicyExportAndSave(t *testing.T) {
	store := &blockingPolicyCooldownStateStore{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		policySaved:  make(chan struct{}, 2),
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	now := time.Now().UTC()
	manager.gptFirstEventObserver.restorePolicyStates([]GPTFirstEventPolicyStateRecord{
		{Model: "gpt-5.6-sol", PolicyState: gptFirstEventPolicyStateSlow30, UpdatedAt: now},
	})

	go manager.persistGPTFirstEventPolicyStates(context.Background())
	select {
	case <-store.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first blocked save")
	}

	manager.gptFirstEventObserver.mu.Lock()
	manager.gptFirstEventObserver.policies["gpt-5.6-sol"] = &gptFirstEventPolicyState{
		name:      gptFirstEventPolicyStateSlow40,
		updatedAt: now.Add(time.Minute),
	}
	manager.gptFirstEventObserver.policies["gpt-5.6-terra"] = &gptFirstEventPolicyState{
		name:      gptFirstEventPolicyStateSlow30,
		updatedAt: now.Add(time.Minute),
	}
	manager.gptFirstEventObserver.mu.Unlock()

	go manager.persistGPTFirstEventPolicyStates(context.Background())
	close(store.releaseFirst)
	for i := 0; i < 2; i++ {
		select {
		case <-store.policySaved:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for serialized policy saves")
		}
	}

	store.policyMu.Lock()
	saves := append([][]GPTFirstEventPolicyStateRecord(nil), store.policySaves...)
	store.policyMu.Unlock()
	if len(saves) != 2 {
		t.Fatalf("policy saves = %d, want 2", len(saves))
	}
	latest := make(map[string]string, len(saves[1]))
	for _, record := range saves[1] {
		latest[record.Model] = record.PolicyState
	}
	if latest["gpt-5.6-sol"] != gptFirstEventPolicyStateSlow40 || latest["gpt-5.6-terra"] != gptFirstEventPolicyStateSlow30 {
		t.Fatalf("latest persisted policy states = %+v, want latest Sol plus Terra", latest)
	}
}
