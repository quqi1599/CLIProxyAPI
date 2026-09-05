package contentaudit

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestModelReviewBudgetConcurrentAndRestartSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	key := "0123456789abcdef0123456789abcdef"
	store, err := NewStore(path, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 3, 0, 0, 0, time.UTC)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reason, err := store.reserveModelReviewCallAt(t.Context(), 7, 60, now)
			if err != nil {
				t.Error(err)
				return
			}
			if reason == "" {
				admitted.Add(1)
			} else if reason != "daily_budget_exhausted" {
				t.Errorf("reason=%s", reason)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 7 {
		t.Fatalf("admitted=%d", admitted.Load())
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(path, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if reason, err := store.reserveModelReviewCallAt(t.Context(), 7, 60, now.Add(time.Minute)); err != nil || reason != "daily_budget_exhausted" {
		t.Fatalf("restart reset day budget: reason=%s err=%v", reason, err)
	}
}

func TestModelReviewBudgetMinuteRollbackAndUTCEightDay(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "budget.db"), "0123456789abcdef0123456789abcdef", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Date(2026, 9, 5, 15, 58, 0, 0, time.UTC)
	for _, test := range []struct {
		at   time.Time
		want string
	}{
		{now, ""}, {now, "minute_rate_limited"}, {now.Add(time.Minute), ""},
		{now.Add(time.Minute), "daily_budget_exhausted"},
		{now.Add(2 * time.Minute), ""},
	} {
		reason, err := store.reserveModelReviewCallAt(t.Context(), 2, 1, test.at)
		if err != nil || reason != test.want {
			t.Fatalf("at=%s reason=%s want=%s err=%v", test.at, reason, test.want, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.reserveModelReviewCallAt(ctx, 2, 60, now.Add(3*time.Minute)); err == nil {
		t.Fatal("canceled reservation succeeded")
	}
	day, _ := modelReviewBudgetWindows(now.Add(2 * time.Minute))
	var used int
	if err := store.db.QueryRowContext(t.Context(), `SELECT calls FROM audit_model_review_budget WHERE period_key=?`, day).Scan(&used); err != nil || used != 1 {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

func TestShadowSampleAndControllerReuse(t *testing.T) {
	rate := 0.0
	var calls atomic.Int32
	service, router := newShadowTestService(t, modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		return ModelReviewResult{Decision: ModelReviewAllow, Confidence: .99}, nil
	}), config.ContentAuditModelReviewConfig{ShadowSampleRate: &rate})
	response := shadowRequest(router, "review fixture", t.Context())
	if response.Code != 204 {
		t.Fatal(response.Code)
	}
	list, err := service.List(t.Context(), ListFilter{})
	if err != nil || len(list.Items) != 1 || list.Items[0].ModelReviewFallback != "shadow_sampled_out" || calls.Load() != 0 {
		t.Fatalf("list=%#v calls=%d err=%v", list, calls.Load(), err)
	}
	state := service.state.Load()
	service.Update(state.cfg, filepath.Join(filepath.Dir(state.policyPath), "config.yaml"))
	if service.state.Load().modelReview != state.modelReview {
		t.Fatal("equivalent hot reload replaced controller/cache")
	}
	cfg := config.ContentAuditModelReviewConfig{Mode: ModelReviewModeEnforce, MaxCallsPerDay: 5000, TimeoutMilliseconds: 10000}
	normalizeModelReviewConfig(&cfg)
	if cfg.MaxCallsPerDay != 1000 || cfg.MaxCallsPerMinute != 5 || cfg.TimeoutMilliseconds != 4000 || *cfg.ShadowSampleRate != .2 {
		t.Fatalf("unsafe defaults: %#v", cfg)
	}
	state.cfg.ModelReview.ShadowSampleRate = cfg.ShadowSampleRate
	request := ModelReviewRequest{Text: "review fixture", TenantScope: "synthetic", PolicyVersion: "v1"}
	first := sampleShadowReview(state, request)
	for range 10 {
		if sampleShadowReview(state, request) != first {
			t.Fatal("unstable sampling")
		}
	}
	request.Severity = "critical"
	if !sampleShadowReview(state, request) {
		t.Fatal("critical candidate should bypass sampling, not quotas")
	}
}

func TestModelReviewLegacyUpgradeDoesNotGrantSecondDayBudget(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "legacy-budget.db"), "0123456789abcdef0123456789abcdef", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Date(2026, 9, 5, 3, 0, 0, 0, time.UTC)
	event := Event{ID: "legacy-event", CreatedAt: now.Unix(), Category: "jailbreak", Severity: "high", RuleID: "legacy-rule", ModelReviewMode: ModelReviewModeShadow, ModelReviewModel: "synthetic-reviewer"}
	if err := store.Record(t.Context(), event, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeLegacyModelReviewBudget(t.Context(), 100, now); err != nil {
		t.Fatal(err)
	}
	// A configuration increase must not reopen an unmetered upgrade day.
	if reason, err := store.reserveModelReviewCallAt(t.Context(), 1000, 5, now); err != nil || reason != "daily_budget_exhausted" {
		t.Fatalf("configuration increase reopened legacy day: reason=%s err=%v", reason, err)
	}
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM audit_model_review_budget`); err != nil {
		t.Fatal(err)
	}
	// Even if startup reconciliation was interrupted, reservation reconciles
	// legacy traffic atomically before it can grant any outbound permission.
	if reason, err := store.reserveModelReviewCallAt(t.Context(), 1000, 5, now); err != nil || reason != "daily_budget_exhausted" {
		t.Fatalf("reason=%s err=%v", reason, err)
	}
	if reason, err := store.reserveModelReviewCallAt(t.Context(), 1000, 5, now.Add(24*time.Hour)); err != nil || reason != "" {
		t.Fatalf("next day reason=%s err=%v", reason, err)
	}
}

func TestShadowDailyQuotaStopsActualProviderCalls(t *testing.T) {
	var calls atomic.Int32
	service, router := newShadowTestService(t, modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		return ModelReviewResult{Decision: ModelReviewAllow, Confidence: .99}, nil
	}), config.ContentAuditModelReviewConfig{MaxCallsPerDay: 1, MaxCallsPerMinute: 5})
	for _, text := range []string{"review fixture first", "review fixture second"} {
		if r := shadowRequest(router, text, t.Context()); r.Code != 204 {
			t.Fatal(r.Code)
		}
		list, err := service.List(t.Context(), ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list.Items {
			if e.ModelReviewFallback == "shadow_pending" {
				waitForShadowResult(t, service, e.ID)
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("external calls=%d", calls.Load())
	}
	status := service.Status().ModelReviewBudget
	if !status.Available || status.DayUsed != 1 {
		t.Fatalf("budget=%#v", status)
	}
}
