package auth

import (
	"context"
	"fmt"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func BenchmarkSpreadRouteState(b *testing.B) {
	for _, n := range []int{8, 64, 256, 1024} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			auths := make([]*Auth, n)
			for i := range auths {
				auths[i] = &Auth{ID: fmt.Sprint(i), Provider: "claude"}
			}
			b.Run("weighted-deficit", func(b *testing.B) {
				s := &SpreadSelector{}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := s.Pick(context.Background(), "claude", "benchmark-route", cliproxyexecutor.Options{}, auths); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("snapshot-copy", func(b *testing.B) {
				s := &SpreadSelector{}
				s.SetSpreadStateStore(NewFileCooldownStateStore(b.TempDir()))
				for i := 0; i < 16; i++ {
					_, _ = s.Pick(context.Background(), "claude", fmt.Sprint("route-", i), cliproxyexecutor.Options{}, auths)
				}
				s.Stop()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _, _, _ = s.spreadStateSnapshot()
				}
			})
			b.Run("static-fenwick-query", func(b *testing.B) {
				tree := make([]int, n+1)
				for i := 1; i <= n; i++ {
					for j := i; j <= n; j += j & -j {
						tree[j]++
					}
				}
				sink := 0
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					target := i % n
					index := 0
					for bit := n; bit > 0; bit >>= 1 {
						next := index + bit
						if next <= n && tree[next] <= target {
							index = next
							target -= tree[next]
						}
					}
					sink += index
				}
				if sink < 0 {
					b.Fatal(sink)
				}
			})
		})
	}
}
