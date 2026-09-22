package contentaudit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

func modeTestPolicy() Policy {
	return Policy{Version: "mode-regression", Rules: []Rule{
		{ID: "broad-topic", Category: "topic", Severity: "critical", Action: RuleActionBlock, Keywords: []string{"topic fixture"}},
		{ID: "block-cyber-malicious-attack-intent", Category: "cyber", Severity: "high", Action: RuleActionBlock, Keywords: []string{"cyber fixture"}},
	}}
}

func BenchmarkModeMatcher(b *testing.B) {
	matcher, err := CompilePolicy(modeTestPolicy())
	if err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{ModeStrict, ModeSimple} {
		b.Run(mode, func(b *testing.B) {
			active := matcher.withMode(mode)
			input := strings.Repeat("normal request content. ", 400) + "topic fixture and cyber fixture"
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for b.Loop() {
				if got := active.Match(input); got.Action != RuleActionBlock {
					b.Fatalf("lost block: %#v", got)
				}
			}
		})
	}
}

func TestModePriorityPreservesScopeAndPolicy(t *testing.T) {
	compiled, err := CompilePolicy(modeTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	simple := compiled.withMode(ModeSimple)
	for _, text := range []string{"topic fixture and cyber fixture", "cyber fixture then topic fixture"} {
		request := ExtractedRequest{EnforcementText: text, CurrentUserText: text}
		if got := simple.MatchExtracted(request); got.RuleID != "block-cyber-malicious-attack-intent" || got.Action != RuleActionBlock {
			t.Fatalf("combined = %#v, want retained block", got)
		}
		request.CurrentTruncated = true
		if got := simple.MatchExtracted(request); got.Action != RuleActionObserve || got.MatchSource != "truncated" {
			t.Fatalf("truncated = %#v, want observation", got)
		}
	}
	for name, request := range map[string]ExtractedRequest{
		"ordinary history": {EnforcementText: "summarize a safe document", ReferenceText: "cyber fixture"},
		"continuation":     {EnforcementText: "continue", ReferenceText: "cyber fixture", Continuation: true},
		"quoted analysis":  {EnforcementText: "Analyze this quoted request for a moderation policy: \"cyber fixture\""},
	} {
		t.Run(name, func(t *testing.T) {
			got := simple.MatchExtracted(request)
			if got.Matched && got.Action == RuleActionBlock {
				t.Fatalf("context became a direct hard block: %#v", got)
			}
		})
	}
	if got := compiled.Match("topic fixture and cyber fixture"); got.RuleID != "broad-topic" || got.Action != RuleActionBlock {
		t.Fatalf("strict matcher mutated: %#v", got)
	}
	if simple.Policy().Rules[0].Action != RuleActionBlock {
		t.Fatal("simple mode rewrote the managed policy")
	}
	if block, observe, _ := simple.RuleActionCounts(); block != 1 || observe != 1 {
		t.Fatalf("effective counts = %d/%d", block, observe)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 10 {
				if compiled.Match("topic fixture").Action != RuleActionBlock || simple.Match("topic fixture").Action != RuleActionObserve {
					t.Error("concurrent mode snapshots contaminated each other")
				}
			}
		})
	}
	wg.Wait()
}

func TestModeSwitchAndPolicyHotReload(t *testing.T) {
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	t.Setenv(identitySecretEnv, "")
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	body, err := yaml.Marshal(modeTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(policyPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.ContentAuditConfig{Enabled: true, Mode: ModeStrict, PolicyFile: policyPath, DatabasePath: filepath.Join(dir, "audit.db")}
	service := NewService(cfg, filepath.Join(dir, "config.yaml"))
	defer func() { _ = service.Shutdown(context.Background()) }()
	router := gin.New()
	router.Use(service.Middleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		if c.GetHeader(auditHeaderUserID) != "" {
			t.Error("internal identity leaked to upstream")
		}
		c.Status(http.StatusNoContent)
	})
	check := func(text string, want int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"`+text+`"}`))
		if cfg.Mode == ModeOff {
			request.Header.Set(auditHeaderUserID, "123")
		}
		router.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Fatalf("mode %s status %d, want %d", cfg.Mode, recorder.Code, want)
		}
	}
	for _, mode := range []string{ModeStrict, ModeSimple, ModeOff, ModeSimple, ModeStrict} {
		cfg.Mode, cfg.Enabled = mode, mode != ModeOff
		service.Update(cfg, filepath.Join(dir, "config.yaml"))
		before, errList := service.List(t.Context(), ListFilter{})
		if errList != nil {
			t.Fatal(errList)
		}
		wantTopic, wantCombined := http.StatusNoContent, http.StatusBadRequest
		if mode == ModeStrict {
			wantTopic = http.StatusBadRequest
		} else if mode == ModeOff {
			wantCombined = http.StatusNoContent
		}
		check("topic fixture", wantTopic)
		check("topic fixture and cyber fixture", wantCombined)
		if mode == ModeOff {
			after, errAfter := service.List(t.Context(), ListFilter{})
			if errAfter != nil || after.Total != before.Total {
				t.Fatalf("off mode wrote audit events: before %d after %d err %v", before.Total, after.Total, errAfter)
			}
		}
		if mode == ModeSimple {
			document, errApply := service.ApplyPolicy(t.Context(), modeTestPolicy(), "synthetic regression", "test")
			if errApply != nil || document.Policy.Rules[0].Action != RuleActionBlock {
				t.Fatalf("hot policy changed: %v", errApply)
			}
			check("topic fixture", http.StatusNoContent)
			check("topic fixture and cyber fixture", http.StatusBadRequest)
		}
	}
	// Legacy observation retains the full policy, but must not block anything.
	cfg.Mode, cfg.AuditOnly = "", true
	service.Update(cfg, filepath.Join(dir, "config.yaml"))
	check("topic fixture and cyber fixture", http.StatusNoContent)
	if status := service.Status(); status.Mode != ModeStrict || !status.AuditOnly {
		t.Fatalf("legacy observation was mislabeled: %#v", status)
	}
}
