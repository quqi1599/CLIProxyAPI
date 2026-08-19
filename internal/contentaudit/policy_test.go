package contentaudit

import (
	"fmt"
	"sync"
	"testing"
)

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

func TestMatcherPrefersBlockActionAndHonorsContext(t *testing.T) {
	matcher, err := CompilePolicy(Policy{
		Version: "test-v1",
		Rules: []Rule{
			{ID: "observe", Category: "sexual", Severity: "critical", Action: RuleActionObserve, Keywords: []string{"explicit phrase"}},
			{
				ID:         "block",
				Category:   "sexual",
				Severity:   "high",
				Action:     RuleActionBlock,
				Keywords:   []string{"explicit phrase"},
				RequireAny: []string{"generate story", "roleplay"},
				ExcludeAny: []string{"academic abstract"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}

	blocked := matcher.Match("generate story with explicit phrase")
	if !blocked.Matched || blocked.RuleID != "block" || blocked.Action != RuleActionBlock {
		t.Fatalf("blocked Match() = %#v", blocked)
	}
	observed := matcher.Match("academic abstract discusses explicit phrase")
	if !observed.Matched || observed.RuleID != "observe" || observed.Action != RuleActionObserve {
		t.Fatalf("observed Match() = %#v", observed)
	}
	withoutIntent := matcher.Match("quoted explicit phrase")
	if !withoutIntent.Matched || withoutIntent.RuleID != "observe" {
		t.Fatalf("without-intent Match() = %#v", withoutIntent)
	}
	block, observe, disabled := matcher.RuleActionCounts()
	if block != 1 || observe != 1 || disabled != 0 {
		t.Fatalf("RuleActionCounts() = %d, %d, %d", block, observe, disabled)
	}
}

func TestCompilePolicyRejectsInvalidAction(t *testing.T) {
	_, err := CompilePolicy(Policy{
		Version: "test-v1",
		Rules: []Rule{{
			ID:       "invalid",
			Category: "synthetic",
			Severity: "high",
			Action:   "drop",
			Keywords: []string{"synthetic phrase"},
		}},
	})
	if err == nil {
		t.Fatal("CompilePolicy() error = nil")
	}
}

func TestMatcherUsesEnglishTermBoundaries(t *testing.T) {
	matcher, err := CompilePolicy(Policy{
		Version: "test-v1",
		Rules: []Rule{{
			ID:       "boundary",
			Category: "synthetic",
			Severity: "high",
			Keywords: []string{"bomb"},
		}},
	})
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}
	if decision := matcher.Match("a bomber flew overhead"); decision.Matched {
		t.Fatalf("substring Match() = %#v, want no match", decision)
	}
	if decision := matcher.Match("the token is bomb"); !decision.Matched {
		t.Fatalf("whole-term Match() = %#v, want match", decision)
	}
}

func TestMatcherUsesKeywordSeededChineseSegmentation(t *testing.T) {
	matcher, err := CompilePolicy(Policy{
		Version:         "test-v1",
		GlobalAllowlist: []string{"接口交互", "光口交换机"},
		Rules: []Rule{{
			ID:       "boundary",
			Category: "synthetic",
			Severity: "high",
			Keywords: []string{"口交"},
		}},
	})
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}
	for _, safe := range []string{"接口交互协议", "连接光口交换机"} {
		if decision := matcher.Match(safe); decision.Matched {
			t.Fatalf("safe Match(%q) = %#v, want no match", safe, decision)
		}
	}
	for _, unsafe := range []string{"请问口交好吗", "口，交"} {
		if decision := matcher.Match(unsafe); !decision.Matched {
			t.Fatalf("evasion Match(%q) = %#v, want match", unsafe, decision)
		}
	}
}

func TestMatcherIsSafeForConcurrentRequests(t *testing.T) {
	matcher, err := CompilePolicy(Policy{
		Version:         "test-v1",
		GlobalAllowlist: []string{"接口交互"},
		Rules: []Rule{{
			ID:       "concurrent",
			Category: "synthetic",
			Severity: "high",
			Keywords: []string{"口交", "synthetic blocked phrase"},
		}},
	})
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}

	const workers = 32
	const iterations = 40
	errCh := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				if decision := matcher.Match("接口交互协议"); decision.Matched {
					errCh <- fmt.Errorf("safe decision = %#v", decision)
					return
				}
				if decision := matcher.Match("synthetic blocked phrase"); !decision.Matched {
					errCh <- fmt.Errorf("blocked decision = %#v", decision)
					return
				}
			}
		}()
	}
	waitGroup.Wait()
	close(errCh)
	for matchErr := range errCh {
		t.Error(matchErr)
	}
}
