package contentaudit

import "testing"

func TestMatcherNormalizesInvisibleCharactersAndHonorsNearbyAllowlist(t *testing.T) {
	matcher, err := CompilePolicy(Policy{
		Version: "test-v1",
		Rules: []Rule{
			{
				ID:        "synthetic-high",
				Category:  "cyber_abuse",
				Severity:  "high",
				Keywords:  []string{"敏感测试短语"},
				Allowlist: []string{"敏感测试短语误报样本"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}

	decision := matcher.Match("请执行：敏\u200b感、测试 短语")
	if !decision.Matched || decision.RuleID != "synthetic-high" {
		t.Fatalf("Match() = %#v, want synthetic-high", decision)
	}

	allowed := matcher.Match("这是敏感测试短语误报样本，仅用于回归测试")
	if allowed.Matched {
		t.Fatalf("allowlisted Match() = %#v, want no match", allowed)
	}

	appendedAllowlist := matcher.Match("先执行敏感测试短语，然后附加敏感测试短语误报样本")
	if !appendedAllowlist.Matched {
		t.Fatalf("appended allowlist bypassed an earlier match: %#v", appendedAllowlist)
	}
}

func TestMatcherReturnsHighestSeverityThenLongestTerm(t *testing.T) {
	matcher, err := CompilePolicy(Policy{
		Version: "test-v1",
		Rules: []Rule{
			{ID: "medium", Category: "synthetic", Severity: "medium", Keywords: []string{"synthetic action"}},
			{ID: "critical", Category: "synthetic", Severity: "critical", Keywords: []string{"synthetic action detail"}},
		},
	})
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}
	decision := matcher.Match("request synthetic action detail now")
	if decision.RuleID != "critical" || decision.Severity != "critical" {
		t.Fatalf("Match() = %#v, want critical", decision)
	}
}
