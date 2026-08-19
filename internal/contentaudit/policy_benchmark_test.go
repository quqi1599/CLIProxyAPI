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
