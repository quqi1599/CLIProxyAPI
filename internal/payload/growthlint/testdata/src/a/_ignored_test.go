package a

import "testing"

func BenchmarkPayloadGrowthIgnored(b *testing.B) {
	_ = ignoredBenchmark(nil, nil)
}
