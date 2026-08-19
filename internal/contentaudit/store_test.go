package contentaudit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
