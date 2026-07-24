package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	softAffinityTestProvider = "openai-compatibility"
	softAffinityTestModel    = "gpt-5.5"
)

func TestSessionAffinitySoft_FailedCandidateDoesNotCreateBindingWithRequestTrace(t *testing.T) {
	selector := newSoftAffinityTestSelector(&FillFirstSelector{})
	defer selector.Stop()

	manager := NewManager(nil, selector, nil)
	candidate := softAffinityTestAuth("soft-failed-candidate", "https://failed.example.com/v1")
	registerSoftAffinityTestAuths(t, manager, candidate)

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	opts := softAffinityTestOptions("failed-candidate")
	cacheKey := softAffinityTestCacheKey(opts)

	picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{candidate})
	if errPick != nil {
		t.Fatalf("pick candidate: %v", errPick)
	}
	if picked.ID != candidate.ID {
		t.Fatalf("picked auth = %q, want %q", picked.ID, candidate.ID)
	}
	if got, ok := selector.cache.Get(cacheKey); ok {
		t.Errorf("binding before result = %q, want no committed binding", got)
	}

	manager.MarkResult(ctx, Result{
		AuthID:   candidate.ID,
		Provider: softAffinityTestProvider,
		Model:    softAffinityTestModel,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusServiceUnavailable,
			Message:    "candidate unavailable",
		},
	})

	if got, ok := selector.cache.Get(cacheKey); ok {
		t.Fatalf("binding after failed result = %q, want no committed binding", got)
	}
}

func TestSessionAffinitySoft_BackupSuccessCommitsRebinding(t *testing.T) {
	selector := newSoftAffinityTestSelector(&FillFirstSelector{})
	defer selector.Stop()

	manager := NewManager(nil, selector, nil)
	original := softAffinityTestAuth("soft-original-success", "https://original-success.example.com/v1")
	backup := softAffinityTestAuth("soft-backup-success", "https://backup-success.example.com/v1")
	registerSoftAffinityTestAuths(t, manager, original, backup)

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	opts := softAffinityTestOptions("backup-success")
	cacheKey := softAffinityTestCacheKey(opts)
	selector.cache.Set(cacheKey, original.ID)

	picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{backup})
	if errPick != nil {
		t.Fatalf("pick backup: %v", errPick)
	}
	if picked.ID != backup.ID {
		t.Fatalf("picked auth = %q, want backup %q", picked.ID, backup.ID)
	}
	if got, ok := selector.cache.Get(cacheKey); !ok || got != original.ID {
		t.Errorf("binding before backup success = %q, %v, want original %q", got, ok, original.ID)
	}

	manager.MarkResult(ctx, Result{
		AuthID:   backup.ID,
		Provider: softAffinityTestProvider,
		Model:    softAffinityTestModel,
		Success:  true,
	})

	if got, ok := selector.cache.Get(cacheKey); !ok || got != backup.ID {
		t.Fatalf("binding after backup success = %q, %v, want backup %q", got, ok, backup.ID)
	}
}

func TestSessionAffinitySoft_AllFallbacksFailPreservesOriginalBinding(t *testing.T) {
	selector := newSoftAffinityTestSelector(&FillFirstSelector{})
	defer selector.Stop()

	manager := NewManager(nil, selector, nil)
	original := softAffinityTestAuth("soft-original-failures", "https://original-failures.example.com/v1")
	backupA := softAffinityTestAuth("soft-backup-a-failure", "https://backup-a-failure.example.com/v1")
	backupB := softAffinityTestAuth("soft-backup-b-failure", "https://backup-b-failure.example.com/v1")
	registerSoftAffinityTestAuths(t, manager, original, backupA, backupB)

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	opts := softAffinityTestOptions("all-fallbacks-fail")
	cacheKey := softAffinityTestCacheKey(opts)
	selector.cache.Set(cacheKey, original.ID)

	for _, backup := range []*Auth{backupA, backupB} {
		picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{backup})
		if errPick != nil {
			t.Fatalf("pick backup %q: %v", backup.ID, errPick)
		}
		if picked.ID != backup.ID {
			t.Fatalf("picked auth = %q, want %q", picked.ID, backup.ID)
		}

		manager.MarkResult(ctx, Result{
			AuthID:   backup.ID,
			Provider: softAffinityTestProvider,
			Model:    softAffinityTestModel,
			Success:  false,
			Error: &Error{
				HTTPStatus: http.StatusServiceUnavailable,
				Message:    "backup unavailable",
			},
		})

		if got, ok := selector.cache.Get(cacheKey); !ok || got != original.ID {
			t.Errorf("binding after failed backup %q = %q, %v, want original %q", backup.ID, got, ok, original.ID)
		}
	}
}

func TestSessionAffinitySoft_SpreadCacheHitTracksInflight(t *testing.T) {
	spread := &SpreadSelector{load: newSpreadLoadTracker()}
	selector := newSoftAffinityTestSelector(spread)
	defer selector.Stop()

	bound := softAffinityTestAuth("soft-inflight-bound", "https://inflight-bound.example.com/v1")
	peer := softAffinityTestAuth("soft-inflight-peer", "https://inflight-peer.example.com/v1")
	opts := softAffinityTestOptions("cache-hit-inflight")
	cacheKey := softAffinityTestCacheKey(opts)
	selector.cache.Set(cacheKey, bound.ID)

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{bound, peer})
	if errPick != nil {
		t.Fatalf("pick cached auth: %v", errPick)
	}
	if picked.ID != bound.ID {
		t.Fatalf("picked auth = %q, want bound auth %q", picked.ID, bound.ID)
	}
	if got := softAffinityTestInflight(spread, bound.ID); got != 1 {
		t.Errorf("bound auth inflight after cache hit = %d, want 1", got)
	}

	selector.MarkDone(bound.ID, softAffinityTestModel)
	if got := softAffinityTestInflight(spread, bound.ID); got != 0 {
		t.Fatalf("bound auth inflight after MarkDone = %d, want 0", got)
	}
}

func TestSessionAffinitySoft_OverloadedBindingMovesToPeer(t *testing.T) {
	spread := &SpreadSelector{load: newSpreadLoadTracker()}
	selector := newSoftAffinityTestSelector(spread)
	defer selector.Stop()

	bound := softAffinityTestAuth("soft-overload-bound", "https://overload-bound.example.com/v1")
	peer := softAffinityTestAuth("soft-overload-peer", "https://overload-peer.example.com/v1")
	opts := softAffinityTestOptions("overloaded-binding")
	selector.cache.Set(softAffinityTestCacheKey(opts), bound.ID)

	key := softAffinityTestProvider + ":" + canonicalModelKey(softAffinityTestModel)
	now := time.Now()
	for i := 0; i < 4; i++ {
		spread.load.markPicked(key, bound.ID, now, spreadLoadDefaultKeyLimit)
	}

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{bound, peer})
	if errPick != nil {
		t.Fatalf("pick with overloaded binding: %v", errPick)
	}
	if picked.ID != peer.ID {
		t.Fatalf("picked auth = %q, want less-loaded peer %q", picked.ID, peer.ID)
	}
}

func TestSessionAffinitySoft_NoticeablySlowerBindingMovesToPeer(t *testing.T) {
	spread := &SpreadSelector{load: newSpreadLoadTracker()}
	selector := newSoftAffinityTestSelector(spread)
	defer selector.Stop()

	bound := softAffinityTestAuth("soft-slow-bound", "https://slow-bound.example.com/v1")
	peer := softAffinityTestAuth("soft-fast-peer", "https://fast-peer.example.com/v1")
	opts := softAffinityTestOptions("slow-binding")
	selector.cache.Set(softAffinityTestCacheKey(opts), bound.ID)

	spread.MarkPicked(softAffinityTestProvider, softAffinityTestModel, bound.ID)
	spread.MarkResult(bound.ID, softAffinityTestModel, true, 3*time.Second)
	spread.MarkPicked(softAffinityTestProvider, softAffinityTestModel, peer.ID)
	spread.MarkResult(peer.ID, softAffinityTestModel, true, 100*time.Millisecond)

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{bound, peer})
	if errPick != nil {
		t.Fatalf("pick with slow binding: %v", errPick)
	}
	if picked.ID != peer.ID {
		t.Fatalf("picked auth = %q, want faster peer %q", picked.ID, peer.ID)
	}
}

func TestSessionAffinitySoft_SpreadCanMigrateAcrossPriority(t *testing.T) {
	spread := &SpreadSelector{load: newSpreadLoadTracker()}
	selector := newSoftAffinityTestSelector(spread)
	defer selector.Stop()

	bound := softAffinityTestAuth("priority-bound", "https://priority-bound.example.com/v1")
	backup := softAffinityTestAuth("priority-backup", "https://priority-backup.example.com/v1")
	backup.Attributes["priority"] = "1"
	opts := softAffinityTestOptions("cross-priority")
	selector.cache.Set(softAffinityTestCacheKey(opts), bound.ID)

	for range 4 {
		spread.MarkPicked(softAffinityTestProvider, softAffinityTestModel, bound.ID)
	}

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{bound, backup})
	if errPick != nil {
		t.Fatalf("pick across priority: %v", errPick)
	}
	if picked.ID != backup.ID {
		t.Fatalf("picked auth = %q, want lower-priority backup %q", picked.ID, backup.ID)
	}
}

func TestSessionAffinitySoft_BadBoundChannelDoesNotSelectPeerCredential(t *testing.T) {
	spread := &SpreadSelector{load: newSpreadLoadTracker()}
	selector := newSoftAffinityTestSelector(spread)
	defer selector.Stop()

	bound := softAffinityTestAuth("shared-channel-bound", "https://shared-channel.example.com/v1")
	sameChannelPeer := softAffinityTestAuth("shared-channel-peer", "https://shared-channel.example.com/v1")
	bound.Attributes["provider_key"] = "shared-channel"
	sameChannelPeer.Attributes["provider_key"] = "shared-channel"
	backup := softAffinityTestAuth("different-channel-backup", "https://different-channel.example.com/v1")
	opts := softAffinityTestOptions("exclude-bound-channel")
	selector.cache.Set(softAffinityTestCacheKey(opts), bound.ID)

	for range 4 {
		spread.MarkPicked(softAffinityTestProvider, softAffinityTestModel, bound.ID)
	}

	ctx, _ := ensureRequestAttemptTrace(context.Background())
	picked, errPick := selector.Pick(ctx, softAffinityTestProvider, softAffinityTestModel, opts, []*Auth{bound, sameChannelPeer, backup})
	if errPick != nil {
		t.Fatalf("pick outside bad bound channel: %v", errPick)
	}
	if picked.ID != backup.ID {
		t.Fatalf("picked auth = %q, want different-channel backup %q", picked.ID, backup.ID)
	}
}

func TestSessionAffinitySoft_NonGPTBindsImmediatelyAndRefreshesTTL(t *testing.T) {
	const (
		provider = "claude"
		model    = "claude-3-7-sonnet"
	)
	selector := newSoftAffinityTestSelector(&FillFirstSelector{})
	defer selector.Stop()
	auth := &Auth{ID: "claude-affinity", Provider: provider, Status: StatusActive}
	opts := softAffinityTestOptions("non-gpt-immediate")
	primaryID, _ := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	cacheKey := provider + "::" + primaryID + "::" + model
	ctx, _ := ensureRequestAttemptTrace(context.Background())

	picked, errPick := selector.Pick(ctx, provider, model, opts, []*Auth{auth})
	if errPick != nil {
		t.Fatalf("pick non-GPT auth: %v", errPick)
	}
	if picked.ID != auth.ID {
		t.Fatalf("picked auth = %q, want %q", picked.ID, auth.ID)
	}
	if got, ok := selector.cache.Get(cacheKey); !ok || got != auth.ID {
		t.Fatalf("immediate non-GPT binding = %q, %v, want %q", got, ok, auth.ID)
	}

	nearExpiry := time.Now().Add(time.Second)
	selector.cache.mu.Lock()
	entry := selector.cache.entries[cacheKey]
	entry.expiresAt = nearExpiry
	selector.cache.entries[cacheKey] = entry
	selector.cache.mu.Unlock()

	if _, errPick = selector.Pick(ctx, provider, model, opts, []*Auth{auth}); errPick != nil {
		t.Fatalf("refresh non-GPT binding: %v", errPick)
	}
	selector.cache.mu.RLock()
	refreshedExpiry := selector.cache.entries[cacheKey].expiresAt
	selector.cache.mu.RUnlock()
	if !refreshedExpiry.After(nearExpiry.Add(30 * time.Second)) {
		t.Fatalf("refreshed expiry = %v, want TTL extension beyond %v", refreshedExpiry, nearExpiry)
	}
}

func newSoftAffinityTestSelector(fallback Selector) *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: fallback,
		TTL:      time.Minute,
	})
}

func softAffinityTestAuth(id, baseURL string) *Auth {
	return &Auth{
		ID:       id,
		Provider: softAffinityTestProvider,
		Status:   StatusActive,
		Attributes: map[string]string{
			"provider_key": id,
			"base_url":     baseURL,
			"priority":     "10",
		},
	}
}

func softAffinityTestOptions(sessionID string) cliproxyexecutor.Options {
	headers := make(http.Header)
	headers.Set("X-Session-ID", sessionID)
	return cliproxyexecutor.Options{Headers: headers}
}

func softAffinityTestCacheKey(opts cliproxyexecutor.Options) string {
	sessionID := ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	return softAffinityTestProvider + "::" + sessionID + "::" + softAffinityTestModel
}

func registerSoftAffinityTestAuths(t *testing.T, manager *Manager, auths ...*Auth) {
	t.Helper()
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %q: %v", auth.ID, errRegister)
		}
	}
}

func softAffinityTestInflight(spread *SpreadSelector, authID string) int {
	key := softAffinityTestProvider + ":" + canonicalModelKey(softAffinityTestModel)
	spread.mu.Lock()
	defer spread.mu.Unlock()
	recordKey := spread.load.recordKey(key, authID)
	snapshot := spread.load.snapshot(key, []string{recordKey}, time.Now(), spreadLoadDefaultKeyLimit)
	return snapshot[recordKey].inFlight
}
