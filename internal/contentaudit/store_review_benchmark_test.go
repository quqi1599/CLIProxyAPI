package contentaudit

import (
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkShadowRecovery(b *testing.B) {
	for _, history := range []int{1000, 100000} {
		for _, pending := range []int{0, 32} {
			for _, indexed := range []bool{false, true} {
				version := "legacy"
				if indexed {
					version = "partial"
				}
				b.Run(fmt.Sprintf("history%d/pending%d/%s", history, pending, version), func(b *testing.B) {
					store := newShadowRecoveryFixture(b, history, pending)
					var err error
					if !indexed {
						if _, err = store.db.ExecContext(b.Context(), "DROP INDEX idx_audit_events_shadow_pending"); err != nil {
							b.Fatal(err)
						}
					}
					if _, err = store.db.ExecContext(b.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						for id := 0; id < pending; id++ {
							_, err = store.db.ExecContext(b.Context(), "UPDATE audit_events SET model_review_fallback='shadow_pending' WHERE id=?", fmt.Sprintf("pending-%d", id))
							if err != nil {
								b.Fatal(err)
							}
						}
						b.StartTimer()
						count, err := store.RecoverInterruptedShadowReviews(b.Context(), int64(history))
						if err != nil || count != int64(pending) {
							b.Fatalf("recovered=%d err=%v", count, err)
						}
					}
				})
			}
		}
	}
}

func newShadowRecoveryFixture(b *testing.B, history, pending int) *Store {
	store, err := NewStore(filepath.Join(b.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef", "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	_, err = store.db.ExecContext(b.Context(), `WITH RECURSIVE seq(n) AS
		(SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
		INSERT INTO audit_events(id,created_at,category,severity,rule_id,model_review_mode,model_review_fallback,evidence_ciphertext)
		SELECT printf('history-%d',n),n,'none','low','fixture','shadow','',zeroblob(512) FROM seq`, history)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < pending; i++ {
		_, err = store.db.ExecContext(b.Context(), `INSERT INTO audit_events(id,created_at,category,severity,rule_id,model_review_mode,model_review_fallback)
			VALUES(?,1,'none','low','fixture','shadow','shadow_pending')`, fmt.Sprintf("pending-%d", i))
		if err != nil {
			b.Fatal(err)
		}
	}
	return store
}

// Building the migration is deliberately measured separately from recovery.
func BenchmarkShadowPendingIndexBuild(b *testing.B) {
	store := newShadowRecoveryFixture(b, 100000, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := store.db.ExecContext(b.Context(), "DROP INDEX idx_audit_events_shadow_pending"); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := store.db.ExecContext(b.Context(), shadowPendingIndexSQL); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	var bytes int64
	if err := store.db.QueryRowContext(b.Context(), "SELECT sum(pgsize) FROM dbstat WHERE name='idx_audit_events_shadow_pending'").Scan(&bytes); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(bytes), "index-B")
}
