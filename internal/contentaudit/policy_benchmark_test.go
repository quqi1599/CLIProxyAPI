package contentaudit

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkMatcherKeywordSets(b *testing.B) {
	for _, keywordCount := range []int{653, 5_000, 10_000} {
		b.Run(fmt.Sprintf("keywords_%d", keywordCount), func(b *testing.B) {
			keywords := make([]string, 0, keywordCount)
			for index := 0; index < keywordCount; index++ {
				keywords = append(keywords, fmt.Sprintf("synthetic-risk-term-%05d", index))
			}
			matcher, err := CompilePolicy(Policy{
				Version: "benchmark",
				Rules: []Rule{{
					ID:       "benchmark-rule",
					Category: "synthetic",
					Severity: "high",
					Keywords: keywords,
				}},
			})
			if err != nil {
				b.Fatalf("CompilePolicy() error = %v", err)
			}
			text := strings.Repeat("normal request content for matcher throughput. ", 360)
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if decision := matcher.Match(text); decision.Matched {
					b.Fatalf("unexpected benchmark match: %#v", decision)
				}
			}
		})
	}
}

func BenchmarkMatcherCandidateSegmentation(b *testing.B) {
	matcher, err := CompilePolicy(Policy{
		Version:         "benchmark",
		GlobalAllowlist: []string{"接口交互"},
		Rules: []Rule{{
			ID:       "benchmark-rule",
			Category: "synthetic",
			Severity: "high",
			Keywords: []string{"口交"},
		}},
	})
	if err != nil {
		b.Fatalf("CompilePolicy() error = %v", err)
	}
	benchmarks := []struct {
		name      string
		text      string
		wantMatch bool
	}{
		{name: "allowlisted_candidate", text: strings.Repeat("正常接口交互协议说明。", 1_200)},
		{name: "hit_at_end", text: strings.Repeat("normal request content. ", 700) + "口交", wantMatch: true},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(benchmark.text)))
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if decision := matcher.Match(benchmark.text); decision.Matched != benchmark.wantMatch {
					b.Fatalf("Match() = %#v, want matched=%t", decision, benchmark.wantMatch)
				}
			}
		})
	}
}

func BenchmarkMatcherScoped(b *testing.B) {
	matcher, err := CompilePolicy(Policy{
		Version: "benchmark",
		Rules: []Rule{
			{ID: "block", Category: "synthetic", Severity: "high", Action: RuleActionBlock, Keywords: []string{"synthetic blocked phrase"}},
			{ID: "observe", Category: "synthetic", Severity: "medium", Action: RuleActionObserve, Keywords: []string{"synthetic observed phrase"}},
		},
	})
	if err != nil {
		b.Fatalf("CompilePolicy() error = %v", err)
	}
	current := strings.Repeat("normal current user content. ", 80)
	history := strings.Repeat("normal historical user and tool content. ", 400) + current
	for _, benchmark := range []struct {
		name        string
		enforcement string
		observation string
	}{
		{name: "single_turn_fast_path", enforcement: current, observation: current},
		{name: "multi_turn_scoped", enforcement: current, observation: history},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(benchmark.observation)))
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if decision := matcher.MatchScoped(benchmark.enforcement, benchmark.observation, false); decision.Matched {
					b.Fatalf("unexpected benchmark match: %#v", decision)
				}
			}
		})
	}
}

func BenchmarkMatcherDenseKeywordThreshold(b *testing.B) {
	matcher, err := CompilePolicy(Policy{
		Version: "benchmark",
		Rules: []Rule{{
			ID:                "dense",
			Category:          "synthetic",
			Severity:          "high",
			Action:            RuleActionBlock,
			Keywords:          []string{"alpha risk", "beta risk", "gamma risk", "delta risk"},
			MinKeywordMatches: 3,
		}},
	})
	if err != nil {
		b.Fatalf("CompilePolicy() error = %v", err)
	}
	text := strings.Repeat("ordinary request content. ", 200) + "alpha risk beta risk gamma risk"
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if decision := matcher.Match(text); !decision.Matched {
			b.Fatalf("unexpected dense threshold miss: %#v", decision)
		}
	}
}
