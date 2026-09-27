package contentaudit

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func assertShadowRecoveryIndex(t *testing.T, store *Store) {
	t.Helper()
	rows, err := store.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+recoverInterruptedShadowReviewsSQL, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		found = found || strings.Contains(detail, "idx_audit_events_shadow_pending")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("recovery query does not use the pending-only index")
	}
}

func TestShadowRecoveryIndexPreservesTerminalAndFutureRows(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cases := []struct {
		id, mode, fallback string
		created            int64
		recover            bool
	}{
		{"old", "shadow", "shadow_pending", 10, true},
		{"boundary", "shadow", "shadow_pending", 20, true},
		{"future", "shadow", "shadow_pending", 21, false},
		{"terminal", "shadow", "", 10, false},
		{"enforce", "enforce", "shadow_pending", 10, false},
		{"off", "", "shadow_pending", 10, false},
	}
	for _, tc := range cases {
		_, err := store.db.ExecContext(t.Context(), `INSERT INTO audit_events
			(id,created_at,category,severity,rule_id,model_review_mode,model_review_fallback,model_review_decision,final_action,evidence_nonce,evidence_ciphertext)
			VALUES(?,?,'none','low','fixture',?,?,'allow','allow',?,?)`, tc.id, tc.created, tc.mode, tc.fallback, []byte("nonce"), []byte("opaque-evidence"))
		if err != nil {
			t.Fatal(err)
		}
	}
	assertShadowRecoveryIndex(t, store)
	count, err := store.RecoverInterruptedShadowReviews(t.Context(), 20)
	if err != nil || count != 2 {
		t.Fatalf("recovered=%d err=%v", count, err)
	}
	for _, tc := range cases {
		var decision, fallback, action string
		var nonce, ciphertext []byte
		err := store.db.QueryRowContext(t.Context(), `SELECT model_review_decision,model_review_fallback,final_action,evidence_nonce,evidence_ciphertext FROM audit_events WHERE id=?`, tc.id).Scan(&decision, &fallback, &action, &nonce, &ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		wantDecision, wantFallback := "allow", tc.fallback
		if tc.recover {
			wantDecision, wantFallback = "uncertain", "shadow_interrupted"
		}
		if decision != wantDecision || fallback != wantFallback || action != "allow" || !bytes.Equal(nonce, []byte("nonce")) || !bytes.Equal(ciphertext, []byte("opaque-evidence")) {
			t.Fatalf("unexpected recovery mutation for %s", tc.id)
		}
	}
	if count, err := store.RecoverInterruptedShadowReviews(t.Context(), 20); err != nil || count != 0 {
		t.Fatalf("second recovery=%d err=%v", count, err)
	}
}

func TestShadowPendingIndexMigratesAfterReviewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := NewStore(path, "0123456789abcdef0123456789abcdef", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"DROP INDEX idx_audit_events_shadow_pending",
		"ALTER TABLE audit_events DROP COLUMN model_review_mode",
		"ALTER TABLE audit_events DROP COLUMN model_review_fallback",
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(path, "0123456789abcdef0123456789abcdef", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	assertShadowRecoveryIndex(t, store)
	if err := store.ensureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertShadowRecoveryIndex(t, store)
}
