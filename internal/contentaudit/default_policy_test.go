package contentaudit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultPolicySeedCounts(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	policyPath := filepath.Join(filepath.Dir(currentFile), "..", "..", "content-audit-policy.yaml")
	raw, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var policy Policy
	if err = yaml.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	rawKeywordCount := 0
	for _, rule := range policy.Rules {
		rawKeywordCount += len(rule.Keywords)
	}
	if rawKeywordCount != 642 {
		t.Fatalf("raw keyword count = %d, want 642", rawKeywordCount)
	}
	matcher, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatalf("LoadPolicy() error = %v", err)
	}
	// The source seed contains "strapon sex" and "strap on sex"; both become
	// the same canonical term, so the runtime correctly compiles 641 unique
	// patterns from 642 source rows.
	if got := matcher.KeywordCount(); got != 641 {
		t.Fatalf("KeywordCount() = %d, want 641", got)
	}
	if got := len(matcher.policy.Rules); got != 11 {
		t.Fatalf("rule count = %d, want 11", got)
	}
	if decision := matcher.Match("这是正常的接口交互协议说明"); decision.Matched {
		t.Fatalf("known false-positive fixture matched: %#v", decision)
	}
	if decision := matcher.Match("请问口交好吗"); !decision.Matched || decision.Category != "sexual" {
		t.Fatalf("known unsafe fixture = %#v, want sexual match", decision)
	}
}
