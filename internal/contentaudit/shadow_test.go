package contentaudit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func waitForShadowResult(t *testing.T, service *Service, id string) Event {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		detail, err := service.Get(t.Context(), id)
		if err == nil && detail.ModelReviewFallback != "shadow_pending" {
			return detail.Event
		}
		select {
		case <-deadline.C:
			t.Fatalf("shadow result was not persisted: %v", err)
		case <-tick.C:
		}
	}
}

func newShadowTestService(t *testing.T, reviewer Reviewer, reviewCfg config.ContentAuditModelReviewConfig) (*Service, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("version: shadow-test\nrules:\n  - id: shadow-rule\n    category: jailbreak\n    severity: high\n    action: observe\n    model-review: true\n    keywords: [review fixture]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reviewCfg.Mode = ModelReviewModeShadow
	reviewCfg.Model = "synthetic-reviewer"
	if reviewCfg.ShadowSampleRate == nil {
		fullSample := 1.0
		reviewCfg.ShadowSampleRate = &fullSample
	}
	service := NewServiceWithReviewer(config.ContentAuditConfig{Enabled: true, PolicyFile: policyPath, DatabasePath: filepath.Join(dir, "audit.db"), ModelReview: reviewCfg}, filepath.Join(dir, "config.yaml"), reviewer)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := service.state.Load().store.Close(); err != nil {
			t.Error(err)
		}
	})
	router := gin.New()
	router.Use(service.Middleware())
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	return service, router
}

func shadowRequest(router *gin.Engine, text string, ctx context.Context) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"input":%q,"model":"synthetic"}`, text))).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestShadowReturnsBeforeModelAndSurvivesRequestCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	service, router := newShadowTestService(t, modelReviewerFunc(func(ctx context.Context, _ ModelReviewRequest) (ModelReviewResult, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ModelReviewResult{}, ctx.Err()
		}
		return ModelReviewResult{Decision: ModelReviewBlock, Confidence: .99, Category: "jailbreak", ResolvedModel: "synthetic-resolved", StageLatenciesMS: map[string]int64{"parse": 2}}, nil
	}), config.ContentAuditModelReviewConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan *httptest.ResponseRecorder, 1)
	go func() { returned <- shadowRequest(router, "review fixture", ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("review did not start")
	}
	select {
	case response := <-returned:
		if response.Code != http.StatusNoContent {
			t.Fatalf("status=%d", response.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("shadow held the customer request waiting for the model")
	}
	cancel()
	list, err := service.List(t.Context(), ListFilter{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	if list.Items[0].ModelReviewFallback != "shadow_pending" {
		t.Fatal("expected pending result while model is blocked")
	}
	close(release)
	event := waitForShadowResult(t, service, list.Items[0].ID)
	if event.ModelReviewDecision != ModelReviewBlock || event.FinalAction != ModelReviewAllow || !event.UpstreamSent {
		t.Fatalf("shadow changed business decision or did not persist: %#v", event)
	}
	if event.ModelReviewResolvedModel != "synthetic-resolved" || event.ModelReviewDiagnostics["parse"] != 2 {
		t.Fatalf("missing safe diagnostics: %#v", event)
	}
}

func TestShadowQueueLimitsAndShutdown(t *testing.T) {
	entered := make(chan struct{}, shadowWorkerCount)
	service, router := newShadowTestService(t, modelReviewerFunc(func(ctx context.Context, _ ModelReviewRequest) (ModelReviewResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return ModelReviewResult{}, ctx.Err()
	}), config.ContentAuditModelReviewConfig{MaxConcurrent: shadowWorkerCount, ShadowQueueSize: 1, ShadowQueueBytes: 256})
	for i := 0; i < shadowWorkerCount; i++ {
		if response := shadowRequest(router, fmt.Sprintf("review fixture %d", i), context.Background()); response.Code != http.StatusNoContent {
			t.Fatal(response.Code)
		}
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("worker did not start")
		}
	}
	shadowRequest(router, "review fixture queued", context.Background())
	shadowRequest(router, "review fixture overflow", context.Background())
	shadowRequest(router, "review fixture "+strings.Repeat("x", 300), context.Background())
	stats := service.Status().Shadow
	if stats.Queued != 1 || stats.Active != shadowWorkerCount || stats.Skipped != 2 || stats.QueuedBytes > 256 {
		t.Fatalf("unbounded queue or incorrect skip accounting: %#v", stats)
	}
	list, err := service.List(t.Context(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fallbacks := map[string]int{}
	for _, e := range list.Items {
		fallbacks[e.ModelReviewFallback]++
	}
	if fallbacks["shadow_queue_full"] != 1 || fallbacks["shadow_oversize"] != 1 {
		t.Fatalf("fallbacks=%v", fallbacks)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	list, err = service.List(t.Context(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Items {
		if e.ModelReviewFallback == "shadow_pending" {
			t.Fatal("shutdown left pending job")
		}
	}
}

func TestShadowStoreUpdateDoesNotOverwriteFinalActionOrPolicy(t *testing.T) {
	service, _ := newShadowTestService(t, nil, config.ContentAuditModelReviewConfig{})
	store := service.state.Load().store
	event := Event{ID: "shadow-cas", PolicyVersion: "policy-v1", Category: "jailbreak", Severity: "high", RuleID: "rule", Action: RuleActionBlock, FinalAction: ModelReviewBlock, ModelReviewMode: ModelReviewModeShadow, ModelReviewFallback: "shadow_pending", ReviewLabel: "false_positive"}
	if err := store.Record(t.Context(), event, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	out := modelReviewOutcome{ModelReviewResult: ModelReviewResult{Decision: ModelReviewAllow, Confidence: .99}}
	if err := store.UpdateShadowReview(t.Context(), event.ID, "wrong-policy", out); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(t.Context(), event.ID)
	if err != nil || first.ModelReviewFallback != "shadow_pending" {
		t.Fatal("stale policy overwrote event")
	}
	if err := store.UpdateShadowReview(t.Context(), event.ID, "policy-v1", out); err != nil {
		t.Fatal(err)
	}
	out.Decision = ModelReviewBlock
	if err := store.UpdateShadowReview(t.Context(), event.ID, "policy-v1", out); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get(t.Context(), event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ModelReviewDecision != ModelReviewAllow || updated.FinalAction != ModelReviewBlock || updated.UpstreamSent || updated.ReviewLabel != "false_positive" {
		t.Fatalf("result overwrite crossed metadata boundary: %#v", updated.Event)
	}
}
