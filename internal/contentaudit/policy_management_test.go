package contentaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestManagedPolicyApplyAndRollback(t *testing.T) {
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	tempDir := t.TempDir()
	policyPath := filepath.Join(tempDir, "policy.yaml")
	baseline := `version: baseline-v1
rules:
  - id: observe
    category: academic
    severity: high
    action: observe
    keywords: ["academic phrase"]
`
	if err := os.WriteFile(policyPath, []byte(baseline), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.ContentAuditConfig{
		Enabled:      true,
		PolicyFile:   policyPath,
		DatabasePath: filepath.Join(tempDir, "audit.db"),
	}, filepath.Join(tempDir, "config.yaml"))
	state := service.state.Load()
	defer func() { _ = state.store.Close() }()

	document, err := service.CurrentPolicy(t.Context())
	if err != nil || document.Policy.Version != "baseline-v1" || len(document.History) != 1 || !document.History[0].Active {
		t.Fatalf("CurrentPolicy() = %#v err=%v", document, err)
	}
	baselineID := document.History[0].ID

	updated, err := service.ApplyPolicy(t.Context(), Policy{
		Rules: []Rule{{
			ID:       "block",
			Category: "jailbreak",
			Severity: "critical",
			Action:   RuleActionBlock,
			Keywords: []string{"managed blocked phrase"},
		}},
	}, "enable managed block", "127.0.0.1")
	if err != nil || !strings.HasPrefix(updated.Policy.Version, "managed-") || len(updated.History) != 2 || !updated.History[0].Active {
		t.Fatalf("ApplyPolicy() = %#v err=%v", updated, err)
	}
	if decision := service.state.Load().matcher.Match("managed blocked phrase"); !decision.Matched || decision.Action != RuleActionBlock {
		t.Fatalf("managed Match() = %#v", decision)
	}
	raw, err := os.ReadFile(policyPath)
	if err != nil || !strings.Contains(string(raw), updated.Policy.Version) {
		t.Fatalf("policy file = %s err=%v", raw, err)
	}

	rolledBack, err := service.RollbackPolicy(t.Context(), baselineID, "restore baseline", "127.0.0.1")
	if err != nil || len(rolledBack.History) != 3 || !rolledBack.History[0].Active {
		t.Fatalf("RollbackPolicy() = %#v err=%v", rolledBack, err)
	}
	if decision := service.state.Load().matcher.Match("academic phrase"); !decision.Matched || decision.Action != RuleActionObserve {
		t.Fatalf("rollback Match() = %#v", decision)
	}
}

func TestManagedPolicyRejectsInvalidUpdateWithoutChangingFile(t *testing.T) {
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	tempDir := t.TempDir()
	policyPath := filepath.Join(tempDir, "policy.yaml")
	baseline := []byte("version: baseline-v1\nrules:\n  - id: observe\n    category: academic\n    severity: high\n    action: observe\n    keywords: [\"academic phrase\"]\n")
	if err := os.WriteFile(policyPath, baseline, 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.ContentAuditConfig{
		Enabled:      true,
		PolicyFile:   policyPath,
		DatabasePath: filepath.Join(tempDir, "audit.db"),
	}, filepath.Join(tempDir, "config.yaml"))
	state := service.state.Load()
	defer func() { _ = state.store.Close() }()

	_, err := service.ApplyPolicy(t.Context(), Policy{Rules: []Rule{{
		ID: "invalid", Category: "synthetic", Severity: "high", Action: "drop", Keywords: []string{"invalid phrase"},
	}}}, "invalid update", "127.0.0.1")
	if err == nil {
		t.Fatal("ApplyPolicy() error = nil")
	}
	raw, readErr := os.ReadFile(policyPath)
	if readErr != nil || string(raw) != string(baseline) {
		t.Fatalf("policy file changed = %q err=%v", raw, readErr)
	}
}
