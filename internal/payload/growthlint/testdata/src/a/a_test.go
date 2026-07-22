package a

import "testing"

func BenchmarkPayloadGrowthJustified(b *testing.B) {
	values := [][]byte{[]byte(`{"type":"message"}`)}
	for b.Loop() {
		_ = justifiedAppend([]byte(`{"output":[]}`), values)
	}
}

func BenchmarkPayloadGrowthClassic(b *testing.B) {
	values := [][]byte{[]byte(`{"type":"message"}`)}
	for i := 0; i < b.N; i++ {
		_ = justifiedClassicAppend([]byte(`{"output":[]}`), values)
	}
}

func BenchmarkPayloadGrowthSkipped(b *testing.B) {
	b.Skip("not evidence")
	_ = skippedBenchmark(nil, nil)
}

func BenchmarkPayloadGrowthOutsideLoop(b *testing.B) {
	_ = outsideLoopBenchmark(nil, nil)
	for b.Loop() {
	}
}

func BenchmarkPayloadGrowthShadowed(b *testing.B) {
	shadowedBenchmark := func([]byte, [][]byte) []byte { return nil }
	for b.Loop() {
		_ = shadowedBenchmark(nil, nil)
	}
}

func BenchmarkPayloadGrowthBrokenLoop(b *testing.B) {
	for b.N > 0 {
		_ = brokenLoopBenchmark(nil, nil)
		break
	}
}

func BenchmarkPayloadGrowthUnreachable(b *testing.B) {
	for b.Loop() {
		continue
		_ = unreachableBenchmark(nil, nil)
	}
}

func BenchmarkPayloadGrowthEarlyExit(b *testing.B) {
	for b.Loop() {
		_ = earlyExitBenchmark(nil, nil)
		break
	}
}
