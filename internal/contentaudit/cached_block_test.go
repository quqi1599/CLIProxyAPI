package contentaudit

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCachedBlockRequiresExactContextAndConfidentSelectedResult(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*modelReviewController, *ModelReviewRequest, *ModelReviewResult)
		want   bool
	}{
		{name: "exact selected block", want: true},
		{name: "below floor", change: func(_ *modelReviewController, _ *ModelReviewRequest, r *ModelReviewResult) { r.Confidence = .94 }},
		{name: "higher configured threshold", change: func(c *modelReviewController, _ *ModelReviewRequest, _ *ModelReviewResult) {
			c.cfg.BlockMinConfidence = .99
		}},
		{name: "allow cannot enforce", change: func(_ *modelReviewController, _ *ModelReviewRequest, r *ModelReviewResult) {
			r.Decision = ModelReviewAllow
		}},
		{name: "uncertain cannot enforce", change: func(_ *modelReviewController, _ *ModelReviewRequest, r *ModelReviewResult) {
			r.Decision = ModelReviewUncertain
		}},
		{name: "missing category", change: func(_ *modelReviewController, _ *ModelReviewRequest, r *ModelReviewResult) { r.Category = "" }},
		{name: "invalid confidence", change: func(_ *modelReviewController, _ *ModelReviewRequest, r *ModelReviewResult) { r.Confidence = math.NaN() }},
		{name: "confidence above one", change: func(_ *modelReviewController, _ *ModelReviewRequest, r *ModelReviewResult) { r.Confidence = 2 }},
		{name: "another tenant", change: func(_ *modelReviewController, r *ModelReviewRequest, _ *ModelReviewResult) {
			r.TenantScope = "verified:43:73"
		}},
		{name: "another token", change: func(_ *modelReviewController, r *ModelReviewRequest, _ *ModelReviewResult) {
			r.TenantScope = "verified:42:74"
		}},
		{name: "another task", change: func(_ *modelReviewController, r *ModelReviewRequest, _ *ModelReviewResult) { r.Text += " safe context" }},
		{name: "another reference", change: func(_ *modelReviewController, r *ModelReviewRequest, _ *ModelReviewResult) {
			r.ReferenceText += " changed"
		}},
		{name: "another policy", change: func(_ *modelReviewController, r *ModelReviewRequest, _ *ModelReviewResult) { r.PolicyVersion += "-new" }},
		{name: "another prompt version", change: func(c *modelReviewController, _ *ModelReviewRequest, _ *ModelReviewResult) {
			c.cfg.PromptVersion += "-new"
		}},
		{name: "incomplete context", change: func(_ *modelReviewController, r *ModelReviewRequest, _ *ModelReviewResult) {
			r.ContextIncomplete = true
		}},
		{name: "not opted in", change: func(c *modelReviewController, _ *ModelReviewRequest, _ *ModelReviewResult) { c.cachedBlockRules = nil }},
		{name: "not selected for review", change: func(c *modelReviewController, _ *ModelReviewRequest, _ *ModelReviewResult) {
			c.ruleGate = true
			c.rules = nil
		}},
		{name: "cache disabled", change: func(c *modelReviewController, _ *ModelReviewRequest, _ *ModelReviewResult) { c.cfg.CacheSeconds = 0 }},
		{name: "mode off", change: func(c *modelReviewController, _ *ModelReviewRequest, _ *ModelReviewResult) {
			c.cfg.Mode = ModelReviewModeOff
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller := newModelReviewController(config.ContentAuditModelReviewConfig{
				Mode: ModelReviewModeShadow, Model: "test-review", PromptVersion: "v1", Rules: []string{"rule"},
				CachedBlockRules: []string{"rule"}, CacheSeconds: 600, BlockMinConfidence: .9,
			}, modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
				t.Fatal("cache lookup started external review")
				return ModelReviewResult{}, nil
			}))
			request := ModelReviewRequest{Model: "test-review", PromptVersion: "v1", RuleID: "rule", Category: "cyber", Severity: "high",
				Text: "synthetic task", ReferenceText: "synthetic reference", TenantScope: "verified:42:73", PolicyVersion: "policy-v1"}
			key := controller.fingerprint(request)
			result := ModelReviewResult{Decision: ModelReviewBlock, Category: "cyber", Confidence: .98}
			if test.change != nil {
				test.change(controller, &request, &result)
			}
			controller.storeCache(key, result)
			outcome, got := controller.cachedBlock(request)
			if got != test.want {
				t.Fatalf("cachedBlock() = %t, want %t", got, test.want)
			}
			if got && (!outcome.Reviewed || !outcome.CacheHit || outcome.Fallback != "") {
				t.Fatalf("bad cache attribution: %#v", outcome)
			}
			if got {
				controller.cache[key] = modelReviewCacheEntry{result: result, expiresAt: time.Now().Add(-time.Second)}
				if _, found := controller.cachedBlock(request); found {
					t.Fatal("expired block reused")
				}
			}
		})
	}
}

func TestCachedBlockMiddlewareBlocksRepeatWithoutProviderWait(t *testing.T) {
	t.Setenv(identitySecretEnv, "synthetic-identity-secret")
	var calls atomic.Int32
	zeroSample := 0.0
	service, router := newShadowTestService(t, modelReviewerFunc(func(context.Context, ModelReviewRequest) (ModelReviewResult, error) {
		calls.Add(1)
		return ModelReviewResult{Decision: ModelReviewBlock, Category: "jailbreak", Confidence: .99, ReasonCodes: []string{"SYNTHETIC_RISK"}}, nil
	}), config.ContentAuditModelReviewConfig{CachedBlockRules: []string{"shadow-rule"}, ShadowSampleRate: &zeroSample})
	request := func(userID, tokenID string, signed bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"review fixture","model":"synthetic"}`))
		req.Header.Set("Content-Type", "application/json")
		if signed {
			signAuditTestRequest(req, time.Now(), userID, tokenID, "test-token", fmt.Sprintf("test-%d", time.Now().UnixNano()), "synthetic", "synthetic-identity-secret")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	if first := request("42", "73", true); first.Code != http.StatusNoContent {
		t.Fatalf("cache miss blocked: %d", first.Code)
	}
	list, err := service.List(t.Context(), ListFilter{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	originalID := list.Items[0].ID
	first := waitForShadowResult(t, service, originalID)
	if first.FinalAction != ModelReviewAllow || !first.UpstreamSent || first.ModelReviewDecision != ModelReviewBlock {
		t.Fatalf("first event: %#v", first)
	}
	started := time.Now()
	if second := request("42", "73", true); second.Code != http.StatusBadRequest {
		t.Fatalf("repeat not blocked: %d", second.Code)
	}
	if time.Since(started) > time.Second || calls.Load() != 1 {
		t.Fatalf("repeat waited or called provider: calls=%d", calls.Load())
	}
	list, err = service.List(t.Context(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var blocked Event
	for _, event := range list.Items {
		if event.ID != originalID {
			blocked = event
		}
	}
	if blocked.FinalAction != ModelReviewBlock || blocked.UpstreamSent || !blocked.ModelReviewCacheHit || blocked.ModelReviewMode != ModelReviewModeEnforce || blocked.ModelReviewFallback != "" {
		t.Fatalf("repeat attribution: %#v", blocked)
	}
	unchanged, err := service.Get(t.Context(), originalID)
	if err != nil || unchanged.FinalAction != ModelReviewAllow || !unchanged.UpstreamSent {
		t.Fatal("original shadow event was rewritten")
	}
	var repeats sync.WaitGroup
	statuses := make(chan int, 8)
	for range 8 {
		repeats.Add(1)
		go func() {
			defer repeats.Done()
			statuses <- request("42", "73", true).Code
		}()
	}
	repeats.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusBadRequest {
			t.Fatalf("concurrent repeat status=%d", status)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent repeats called provider")
	}
	if other := request("43", "73", true); other.Code != http.StatusNoContent {
		t.Fatal("cache crossed users")
	}
	if other := request("42", "74", true); other.Code != http.StatusNoContent {
		t.Fatal("cache crossed tokens")
	}
	if anonymous := request("", "", false); anonymous.Code != http.StatusNoContent {
		t.Fatal("cache applied to unsigned request")
	}
	cfg := service.state.Load().cfg
	cfg.AuditOnly = true
	service.Update(cfg, filepath.Join(filepath.Dir(cfg.PolicyFile), "config.yaml"))
	if auditOnly := request("42", "73", true); auditOnly.Code != http.StatusNoContent {
		t.Fatal("cache ignored audit-only mode")
	}
}
