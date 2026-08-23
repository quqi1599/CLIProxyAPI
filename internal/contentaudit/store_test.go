package contentaudit

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreEncryptsEvidenceAndSeparatesMetadataFromReveal(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit", "events.db")
	store, err := NewStore(dbPath, "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	event := Event{
		ID:               "aud_test",
		CreatedAt:        time.Now().Unix(),
		RequestID:        "req-1",
		UserID:           42,
		TokenID:          73,
		TokenName:        "production-token",
		Method:           "POST",
		Path:             "/v1/responses",
		Protocol:         "openai_responses",
		Model:            "gpt-test",
		Category:         "synthetic",
		Severity:         "high",
		RuleID:           "synthetic-rule",
		MatchedTerm:      "syntheticprohibitedaction",
		PolicyVersion:    "test-v1",
		RequestBytes:     100,
		IdentityVerified: true,
		EvidenceKeyID:    "test-key",
	}
	evidence := []byte(`{"extracted_text":"plaintext evidence sentinel"}`)
	if err = store.Record(context.Background(), event, evidence); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	list, err := store.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].UpstreamSent {
		t.Fatalf("List() = %#v", list)
	}

	revealed, err := store.Reveal(context.Background(), event.ID, "investigate false positive", "127.0.0.1")
	if err != nil {
		t.Fatalf("Reveal() error = %v", err)
	}
	if !bytes.Equal(revealed, evidence) {
		t.Fatalf("Reveal() = %s, want %s", revealed, evidence)
	}
	detail, err := store.Get(context.Background(), event.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(detail.AccessHistory) != 1 || detail.AccessHistory[0].Action != "reveal" {
		t.Fatalf("access history = %#v", detail.AccessHistory)
	}

	for _, path := range []string{dbPath, dbPath + "-wal"} {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		if bytes.Contains(raw, []byte("plaintext evidence sentinel")) {
			t.Fatalf("encrypted evidence appeared in plaintext in %s", path)
		}
	}
}

func TestStoreRejectsWrongEvidenceKeyForExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit", "events.db")
	store, err := NewStore(dbPath, "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err = NewStore(dbPath, "abcdef0123456789abcdef0123456789", "rotated-key"); err == nil {
		t.Fatal("NewStore() wrong key error = nil")
	}
}

func TestStoreDeduplicatesObserveEvidenceAndRevealsThroughReference(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit", "events.db")
	store, err := NewStore(dbPath, "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().Unix()
	base := Event{
		ID:                  "aud_canonical",
		CreatedAt:           now,
		Method:              "POST",
		Path:                "/v1/responses",
		Protocol:            "openai_responses",
		Category:            "fraud",
		Severity:            "high",
		RuleID:              "seed-fraud",
		Action:              RuleActionObserve,
		MatchedTerm:         "synthetic phrase",
		MatchedRoles:        []string{"user"},
		PolicyVersion:       "test-v1",
		UpstreamSent:        true,
		fingerprintMaterial: "user:stableprompt",
		dedupeWindow:        10 * time.Minute,
	}
	firstEvidence := []byte(`{"request_body":{"input":"first evidence"}}`)
	if err = store.Record(t.Context(), base, firstEvidence); err != nil {
		t.Fatalf("Record(canonical) error = %v", err)
	}
	duplicate := base
	duplicate.ID = "aud_duplicate"
	duplicate.CreatedAt++
	secondEvidence := []byte(`{"request_body":{"input":"second evidence"}}`)
	if err = store.Record(t.Context(), duplicate, secondEvidence); err != nil {
		t.Fatalf("Record(duplicate) error = %v", err)
	}

	canonicalDetail, err := store.Get(t.Context(), base.ID)
	if err != nil {
		t.Fatalf("Get(canonical) error = %v", err)
	}
	if canonicalDetail.EvidenceStatus != "encrypted" || canonicalDetail.DuplicateCount != 2 {
		t.Fatalf("canonical detail = %#v", canonicalDetail.Event)
	}
	duplicateDetail, err := store.Get(t.Context(), duplicate.ID)
	if err != nil {
		t.Fatalf("Get(duplicate) error = %v", err)
	}
	if duplicateDetail.EvidenceStatus != "encrypted" || duplicateDetail.EvidenceRefID != base.ID {
		t.Fatalf("duplicate detail = %#v", duplicateDetail.Event)
	}
	if duplicateDetail.ContentFingerprint == "" || duplicateDetail.ContentFingerprint != canonicalDetail.ContentFingerprint {
		t.Fatalf("fingerprints canonical=%q duplicate=%q", canonicalDetail.ContentFingerprint, duplicateDetail.ContentFingerprint)
	}
	revealed, err := store.Reveal(t.Context(), duplicate.ID, "review duplicate evidence", "127.0.0.1")
	if err != nil {
		t.Fatalf("Reveal(duplicate) error = %v", err)
	}
	if !bytes.Equal(revealed, firstEvidence) {
		t.Fatalf("Reveal(duplicate) = %s, want canonical %s", revealed, firstEvidence)
	}

	filtered, err := store.List(t.Context(), ListFilter{MatchedRole: "user", DuplicatesOnly: true})
	if err != nil || filtered.Total != 2 {
		t.Fatalf("List(duplicates) = %#v err=%v", filtered, err)
	}
	var encryptedRows int
	if err = store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events WHERE evidence_ciphertext IS NOT NULL`).Scan(&encryptedRows); err != nil {
		t.Fatalf("count ciphertext rows: %v", err)
	}
	if encryptedRows != 1 {
		t.Fatalf("encrypted rows = %d, want 1", encryptedRows)
	}
}

func TestStoreDeduplicatesConcurrentObserveEvidence(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	const requests = 20
	start := make(chan struct{})
	errors := make(chan error, requests)
	var wait sync.WaitGroup
	for index := 0; index < requests; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			event := Event{
				ID:                  fmt.Sprintf("aud_concurrent_%02d", index),
				CreatedAt:           time.Now().Unix(),
				Category:            "fraud",
				Severity:            "high",
				RuleID:              "seed-fraud",
				Action:              RuleActionObserve,
				MatchedTerm:         "same phrase",
				PolicyVersion:       "test-v1",
				UpstreamSent:        true,
				fingerprintMaterial: "user:samephrase",
				dedupeWindow:        time.Minute,
			}
			errors <- store.Record(t.Context(), event, []byte(fmt.Sprintf(`{"request":%d}`, index)))
		}(index)
	}
	close(start)
	wait.Wait()
	close(errors)
	for err = range errors {
		if err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}
	var total, encrypted, referenced int
	if err = store.db.QueryRowContext(t.Context(), `SELECT COUNT(*),
		SUM(CASE WHEN evidence_ciphertext IS NOT NULL THEN 1 ELSE 0 END),
		SUM(CASE WHEN evidence_ref_id <> '' THEN 1 ELSE 0 END) FROM audit_events`).Scan(&total, &encrypted, &referenced); err != nil {
		t.Fatalf("count concurrent rows: %v", err)
	}
	if total != requests || encrypted != 1 || referenced != requests-1 {
		t.Fatalf("rows total=%d encrypted=%d referenced=%d", total, encrypted, referenced)
	}
}

func TestStoreDoesNotDeduplicateBlockEvidence(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	base := Event{
		ID:                  "aud_block_1",
		CreatedAt:           time.Now().Unix(),
		Category:            "sexual",
		Severity:            "high",
		RuleID:              "block-rule",
		Action:              RuleActionBlock,
		MatchedTerm:         "blocked phrase",
		PolicyVersion:       "test-v1",
		fingerprintMaterial: "user:samecontent",
		dedupeWindow:        time.Hour,
	}
	if err = store.Record(t.Context(), base, []byte(`{"case":1}`)); err != nil {
		t.Fatalf("Record(first) error = %v", err)
	}
	base.ID = "aud_block_2"
	base.CreatedAt++
	if err = store.Record(t.Context(), base, []byte(`{"case":2}`)); err != nil {
		t.Fatalf("Record(second) error = %v", err)
	}
	var encryptedRows int
	if err = store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events WHERE evidence_status = 'encrypted'`).Scan(&encryptedRows); err != nil {
		t.Fatalf("count encrypted rows: %v", err)
	}
	if encryptedRows != 2 {
		t.Fatalf("encrypted rows = %d, want 2", encryptedRows)
	}
}

func TestStoreDoesNotDeduplicateModelEscalatedBlockEvidence(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	base := Event{
		ID:                  "aud_escalated_1",
		CreatedAt:           time.Now().Unix(),
		Category:            "cyber",
		Severity:            "high",
		RuleID:              "observe-rule",
		Action:              RuleActionObserve,
		FinalAction:         ModelReviewBlock,
		MatchedTerm:         "reviewed phrase",
		PolicyVersion:       "test-v1",
		fingerprintMaterial: "user:samecontent",
		dedupeWindow:        time.Hour,
	}
	if err = store.Record(t.Context(), base, []byte(`{"case":1}`)); err != nil {
		t.Fatalf("Record(first) error = %v", err)
	}
	base.ID = "aud_escalated_2"
	base.CreatedAt++
	if err = store.Record(t.Context(), base, []byte(`{"case":2}`)); err != nil {
		t.Fatalf("Record(second) error = %v", err)
	}
	var encryptedRows int
	if err = store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events WHERE evidence_ciphertext IS NOT NULL`).Scan(&encryptedRows); err != nil {
		t.Fatalf("count encrypted rows: %v", err)
	}
	if encryptedRows != 2 {
		t.Fatalf("encrypted rows = %d, want 2", encryptedRows)
	}
}

func TestStorePruneKeepsCanonicalEvidenceForNewerDuplicate(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now()
	canonical := Event{
		ID:                  "aud_old_canonical",
		CreatedAt:           now.Add(-30*24*time.Hour - 30*time.Second).Unix(),
		Category:            "fraud",
		Severity:            "high",
		RuleID:              "seed-fraud",
		Action:              RuleActionObserve,
		MatchedTerm:         "duplicate phrase",
		PolicyVersion:       "test-v1",
		UpstreamSent:        true,
		fingerprintMaterial: "user:duplicatephrase",
		dedupeWindow:        2 * time.Minute,
	}
	evidence := []byte(`{"prompt":"canonical evidence"}`)
	if err = store.Record(t.Context(), canonical, evidence); err != nil {
		t.Fatalf("Record(canonical) error = %v", err)
	}
	duplicate := canonical
	duplicate.ID = "aud_newer_duplicate"
	duplicate.CreatedAt = now.Add(-30*24*time.Hour + 30*time.Second).Unix()
	if err = store.Record(t.Context(), duplicate, []byte(`{"prompt":"duplicate evidence"}`)); err != nil {
		t.Fatalf("Record(duplicate) error = %v", err)
	}
	if err = store.Prune(t.Context(), 30, 180); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	revealed, err := store.Reveal(t.Context(), duplicate.ID, "verify retained duplicate", "127.0.0.1")
	if err != nil || !bytes.Equal(revealed, evidence) {
		t.Fatalf("Reveal() = %s err=%v", revealed, err)
	}
}

func TestStoreSchemaIncludesAuditGroupingColumns(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	rows, err := store.db.QueryContext(t.Context(), `PRAGMA table_info(audit_events)`)
	if err != nil {
		t.Fatalf("table info error = %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan table info: %v", err)
		}
		found[name] = true
	}
	for _, name := range []string{"action", "matched_roles", "content_fingerprint", "evidence_ref_id", "duplicate_count"} {
		if !found[name] {
			t.Fatalf("missing audit_events column %q", name)
		}
	}
}

func TestNewStoreMigratesLegacyAuditEventSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	store, err := NewStore(dbPath, "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy fixture: %v", err)
	}
	if _, err = db.Exec(`DROP INDEX IF EXISTS idx_audit_events_fingerprint`); err != nil {
		t.Fatalf("drop fingerprint index: %v", err)
	}
	for _, column := range []string{"duplicate_count", "evidence_ref_id", "content_fingerprint", "matched_roles", "action"} {
		if _, err = db.Exec(`ALTER TABLE audit_events DROP COLUMN ` + column); err != nil {
			t.Fatalf("drop legacy column %s: %v", column, err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatalf("close legacy fixture: %v", err)
	}
	migrated, err := NewStore(dbPath, "0123456789abcdef0123456789abcdef", "test-key")
	if err != nil {
		t.Fatalf("NewStore(legacy) error = %v", err)
	}
	defer func() { _ = migrated.Close() }()
	var count int
	if err = migrated.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('audit_events')
		WHERE name IN ('action','matched_roles','content_fingerprint','evidence_ref_id','duplicate_count')`).Scan(&count); err != nil {
		t.Fatalf("inspect migrated schema: %v", err)
	}
	if count != 5 {
		t.Fatalf("migrated columns = %d, want 5", count)
	}
}
