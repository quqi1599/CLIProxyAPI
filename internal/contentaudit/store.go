package contentaudit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultReviewLabel = "unreviewed"
	maxAuditPageSize   = 200
)

var validReviewLabels = map[string]struct{}{
	"unreviewed":          {},
	"confirmed_block":     {},
	"false_positive":      {},
	"needs_policy_change": {},
	"out_of_scope":        {},
}

// Event is the plaintext metadata stored for one locally blocked request.
type Event struct {
	ID               string `json:"id"`
	CreatedAt        int64  `json:"created_at"`
	RequestID        string `json:"request_id"`
	UserID           int64  `json:"user_id,omitempty"`
	TokenID          int64  `json:"token_id,omitempty"`
	TokenName        string `json:"token_name,omitempty"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	Protocol         string `json:"protocol"`
	Model            string `json:"model,omitempty"`
	Stream           bool   `json:"stream"`
	Category         string `json:"category"`
	Severity         string `json:"severity"`
	RuleID           string `json:"rule_id"`
	MatchedTerm      string `json:"matched_term,omitempty"`
	PolicyVersion    string `json:"policy_version"`
	RequestBytes     int64  `json:"request_bytes"`
	IdentityVerified bool   `json:"identity_verified"`
	UpstreamSent     bool   `json:"upstream_sent"`
	EvidenceStatus   string `json:"evidence_status"`
	EvidenceKeyID    string `json:"evidence_key_id"`
	ReviewLabel      string `json:"review_label"`
	ReviewNote       string `json:"review_note,omitempty"`
	ReviewedAt       int64  `json:"reviewed_at,omitempty"`
	ReviewedBy       string `json:"reviewed_by,omitempty"`
}

// AccessLog records sensitive evidence reveals and operator annotations.
type AccessLog struct {
	ID        int64  `json:"id"`
	EventID   string `json:"event_id"`
	CreatedAt int64  `json:"created_at"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
}

// EventDetail is the metadata view returned without decrypted evidence.
type EventDetail struct {
	Event
	AccessHistory []AccessLog `json:"access_history"`
}

// ListFilter selects audit events for the Management API.
type ListFilter struct {
	Search      string
	Category    string
	Severity    string
	ReviewLabel string
	UserID      int64
	TokenID     int64
	Page        int
	PageSize    int
}

// ListResult is a stable paginated Management API envelope.
type ListResult struct {
	Items    []Event `json:"items"`
	Total    int64   `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
}

// Store owns the isolated SQLite metadata database and AES-GCM evidence key.
type Store struct {
	db    *sql.DB
	path  string
	aead  cipher.AEAD
	keyID string
}

// NewStore opens the isolated audit database and initializes its schema.
func NewStore(path, evidenceKey, keyID string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("content audit database path is required")
	}
	evidenceKey = strings.TrimSpace(evidenceKey)
	if len(evidenceKey) < 32 {
		return nil, fmt.Errorf("content audit evidence key must contain at least 32 characters")
	}
	keyDigest := sha256.Sum256([]byte(evidenceKey))
	block, err := aes.NewCipher(keyDigest[:])
	if err != nil {
		return nil, fmt.Errorf("initialize content audit cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize content audit AEAD: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create content audit database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open content audit database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	store := &Store{db: db, path: path, aead: aead, keyID: strings.TrimSpace(keyID)}
	if store.keyID == "" {
		store.keyID = "primary"
	}
	if err = store.ensureSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = store.verifyEvidenceKey(context.Background(), keyDigest[:]); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure content audit database permissions: %w", err)
	}
	return store, nil
}

func (s *Store) ensureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("content audit store is unavailable")
	}
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			request_id TEXT NOT NULL DEFAULT '',
			user_id INTEGER NOT NULL DEFAULT 0,
			token_id INTEGER NOT NULL DEFAULT 0,
			token_name TEXT NOT NULL DEFAULT '',
			method TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			protocol TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			stream INTEGER NOT NULL DEFAULT 0,
			category TEXT NOT NULL,
			severity TEXT NOT NULL,
			rule_id TEXT NOT NULL,
			matched_term TEXT NOT NULL DEFAULT '',
			policy_version TEXT NOT NULL DEFAULT '',
			request_bytes INTEGER NOT NULL DEFAULT 0,
			identity_verified INTEGER NOT NULL DEFAULT 0,
			upstream_sent INTEGER NOT NULL DEFAULT 0,
			evidence_status TEXT NOT NULL DEFAULT 'encrypted',
			evidence_key_id TEXT NOT NULL DEFAULT '',
			evidence_nonce BLOB,
			evidence_ciphertext BLOB,
			review_label TEXT NOT NULL DEFAULT 'unreviewed',
			review_note TEXT NOT NULL DEFAULT '',
			reviewed_at INTEGER NOT NULL DEFAULT 0,
			reviewed_by TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_events_created_at ON audit_events(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_events_customer ON audit_events(user_id, token_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_events_category ON audit_events(category, severity, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_events_review ON audit_events(review_label, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS audit_access_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			action TEXT NOT NULL,
			reason TEXT NOT NULL,
			actor TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(event_id) REFERENCES audit_events(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_access_event ON audit_access_log(event_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS audit_store_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize content audit schema: %w", err)
		}
	}
	return nil
}

func (s *Store) verifyEvidenceKey(ctx context.Context, derivedKey []byte) error {
	mac := hmac.New(sha256.New, derivedKey)
	_, _ = mac.Write([]byte("cpa-content-audit-evidence-key-verifier-v1"))
	verifier := fmt.Sprintf("v1:%x", mac.Sum(nil))
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM audit_store_meta WHERE key = 'evidence_key_verifier'`).Scan(&stored)
	switch {
	case err == sql.ErrNoRows:
		if _, err = s.db.ExecContext(ctx, `INSERT INTO audit_store_meta(key, value) VALUES ('evidence_key_verifier', ?)`, verifier); err != nil {
			return fmt.Errorf("store content audit evidence key verifier: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read content audit evidence key verifier: %w", err)
	case !hmac.Equal([]byte(stored), []byte(verifier)):
		return fmt.Errorf("content audit evidence key does not match the existing database")
	default:
		return nil
	}
}

func (s *Store) encryptEvidence(eventID string, evidence []byte) ([]byte, []byte, error) {
	if s == nil || s.aead == nil {
		return nil, nil, fmt.Errorf("content audit evidence cipher is unavailable")
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(evidence); err != nil {
		_ = writer.Close()
		return nil, nil, fmt.Errorf("compress content audit evidence: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, nil, fmt.Errorf("finish content audit evidence compression: %w", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate content audit evidence nonce: %w", err)
	}
	ciphertext := s.aead.Seal(nil, nonce, compressed.Bytes(), []byte(eventID))
	return nonce, ciphertext, nil
}

func (s *Store) decryptEvidence(eventID string, nonce, ciphertext []byte) ([]byte, error) {
	compressed, err := s.aead.Open(nil, nonce, ciphertext, []byte(eventID))
	if err != nil {
		return nil, fmt.Errorf("decrypt content audit evidence: %w", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open content audit evidence compression: %w", err)
	}
	defer func() { _ = reader.Close() }()
	plain, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decompress content audit evidence: %w", err)
	}
	return plain, nil
}

// Record inserts one hit and its encrypted evidence before the client response is returned.
func (s *Store) Record(ctx context.Context, event Event, evidence []byte) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("content audit store is unavailable")
	}
	nonce, ciphertext, err := s.encryptEvidence(event.ID, evidence)
	if err != nil {
		return err
	}
	if event.ReviewLabel == "" {
		event.ReviewLabel = defaultReviewLabel
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit_events (
		id, created_at, request_id, user_id, token_id, token_name, method, path, protocol,
		model, stream, category, severity, rule_id, matched_term, policy_version,
		request_bytes, identity_verified, upstream_sent, evidence_status, evidence_key_id,
		evidence_nonce, evidence_ciphertext, review_label
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'encrypted', ?, ?, ?, ?)`,
		event.ID, event.CreatedAt, event.RequestID, event.UserID, event.TokenID, event.TokenName,
		event.Method, event.Path, event.Protocol, event.Model, boolInt(event.Stream), event.Category,
		event.Severity, event.RuleID, event.MatchedTerm, event.PolicyVersion, event.RequestBytes,
		boolInt(event.IdentityVerified), boolInt(event.UpstreamSent), s.keyID, nonce, ciphertext,
		event.ReviewLabel,
	)
	if err != nil {
		return fmt.Errorf("insert content audit event: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

type eventScanner interface {
	Scan(dest ...any) error
}

func scanEvent(scanner eventScanner) (Event, error) {
	var event Event
	var stream, identityVerified, upstreamSent int
	err := scanner.Scan(
		&event.ID, &event.CreatedAt, &event.RequestID, &event.UserID, &event.TokenID,
		&event.TokenName, &event.Method, &event.Path, &event.Protocol, &event.Model, &stream,
		&event.Category, &event.Severity, &event.RuleID, &event.MatchedTerm, &event.PolicyVersion, &event.RequestBytes,
		&identityVerified, &upstreamSent, &event.EvidenceStatus, &event.EvidenceKeyID,
		&event.ReviewLabel, &event.ReviewNote, &event.ReviewedAt, &event.ReviewedBy,
	)
	event.Stream = stream != 0
	event.IdentityVerified = identityVerified != 0
	event.UpstreamSent = upstreamSent != 0
	return event, err
}

const eventSelectColumns = `id, created_at, request_id, user_id, token_id, token_name,
	method, path, protocol, model, stream, category, severity, rule_id, matched_term, policy_version,
	request_bytes, identity_verified, upstream_sent, evidence_status, evidence_key_id,
	review_label, review_note, reviewed_at, reviewed_by`

// List returns metadata only; evidence is never included in list responses.
func (s *Store) List(ctx context.Context, filter ListFilter) (ListResult, error) {
	if s == nil || s.db == nil {
		return ListResult{}, fmt.Errorf("content audit store is unavailable")
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 50
	}
	filter.PageSize = min(filter.PageSize, maxAuditPageSize)
	clauses := []string{"1=1"}
	args := make([]any, 0, 12)
	if value := strings.TrimSpace(filter.Search); value != "" {
		like := "%" + value + "%"
		clauses = append(clauses, `(id LIKE ? OR request_id LIKE ? OR token_name LIKE ? OR model LIKE ? OR rule_id LIKE ? OR matched_term LIKE ?)`)
		args = append(args, like, like, like, like, like, like)
	}
	if value := strings.TrimSpace(filter.Category); value != "" {
		clauses = append(clauses, "category = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.Severity); value != "" {
		clauses = append(clauses, "severity = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.ReviewLabel); value != "" {
		clauses = append(clauses, "review_label = ?")
		args = append(args, value)
	}
	if filter.UserID > 0 {
		clauses = append(clauses, "user_id = ?")
		args = append(args, filter.UserID)
	}
	if filter.TokenID > 0 {
		clauses = append(clauses, "token_id = ?")
		args = append(args, filter.TokenID)
	}
	where := strings.Join(clauses, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_events WHERE "+where, args...).Scan(&total); err != nil {
		return ListResult{}, fmt.Errorf("count content audit events: %w", err)
	}
	queryArgs := append(append([]any(nil), args...), filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventSelectColumns+" FROM audit_events WHERE "+where+" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?", queryArgs...)
	if err != nil {
		return ListResult{}, fmt.Errorf("list content audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]Event, 0, filter.PageSize)
	for rows.Next() {
		event, errScan := scanEvent(rows)
		if errScan != nil {
			return ListResult{}, fmt.Errorf("scan content audit event: %w", errScan)
		}
		items = append(items, event)
	}
	if err = rows.Err(); err != nil {
		return ListResult{}, fmt.Errorf("iterate content audit events: %w", err)
	}
	return ListResult{Items: items, Total: total, Page: filter.Page, PageSize: filter.PageSize}, nil
}

// Get returns event metadata and its evidence access history.
func (s *Store) Get(ctx context.Context, eventID string) (EventDetail, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+eventSelectColumns+" FROM audit_events WHERE id = ?", eventID)
	event, err := scanEvent(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return EventDetail{}, err
		}
		return EventDetail{}, fmt.Errorf("get content audit event: %w", err)
	}
	access, err := s.listAccess(ctx, eventID)
	if err != nil {
		return EventDetail{}, err
	}
	return EventDetail{Event: event, AccessHistory: access}, nil
}

func (s *Store) listAccess(ctx context.Context, eventID string) ([]AccessLog, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, event_id, created_at, action, reason, actor
		FROM audit_access_log WHERE event_id = ? ORDER BY created_at DESC, id DESC LIMIT 200`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list content audit access history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AccessLog, 0, 16)
	for rows.Next() {
		var item AccessLog
		if err = rows.Scan(&item.ID, &item.EventID, &item.CreatedAt, &item.Action, &item.Reason, &item.Actor); err != nil {
			return nil, fmt.Errorf("scan content audit access history: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Reveal decrypts evidence and records the reason and actor.
func (s *Store) Reveal(ctx context.Context, eventID, reason, actor string) (json.RawMessage, error) {
	var nonce, ciphertext []byte
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT evidence_nonce, evidence_ciphertext, evidence_status
		FROM audit_events WHERE id = ?`, eventID).Scan(&nonce, &ciphertext, &status); err != nil {
		return nil, err
	}
	if status != "encrypted" || len(nonce) == 0 || len(ciphertext) == 0 {
		return nil, fmt.Errorf("content audit evidence is not available")
	}
	plain, err := s.decryptEvidence(eventID, nonce, ciphertext)
	if err != nil {
		return nil, err
	}
	if !json.Valid(plain) {
		return nil, fmt.Errorf("content audit evidence is invalid")
	}
	if err = s.RecordAccess(ctx, eventID, "reveal", reason, actor); err != nil {
		return nil, err
	}
	return json.RawMessage(plain), nil
}

// RecordAccess stores evidence reveal/copy/download and review audit events.
func (s *Store) RecordAccess(ctx context.Context, eventID, action, reason, actor string) error {
	action = strings.TrimSpace(action)
	reason = strings.TrimSpace(reason)
	actor = strings.TrimSpace(actor)
	if action == "" || len([]rune(reason)) < 4 {
		return fmt.Errorf("content audit access action and a reason of at least four characters are required")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO audit_access_log(event_id, created_at, action, reason, actor)
		SELECT id, ?, ?, ?, ? FROM audit_events WHERE id = ?`, time.Now().Unix(), action, reason, actor, eventID)
	if err != nil {
		return fmt.Errorf("record content audit access: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateReview labels an event for policy tuning without changing request or account state.
func (s *Store) UpdateReview(ctx context.Context, eventID, label, note, reason, actor string) error {
	label = strings.TrimSpace(label)
	if _, valid := validReviewLabels[label]; !valid {
		return fmt.Errorf("invalid content audit review label")
	}
	if len([]rune(strings.TrimSpace(reason))) < 4 {
		return fmt.Errorf("a review reason of at least four characters is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start content audit review: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE audit_events SET review_label = ?, review_note = ?, reviewed_at = ?, reviewed_by = ? WHERE id = ?`,
		label, strings.TrimSpace(note), time.Now().Unix(), strings.TrimSpace(actor), eventID)
	if err != nil {
		return fmt.Errorf("update content audit review: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_access_log(event_id, created_at, action, reason, actor)
		VALUES (?, ?, 'review', ?, ?)`, eventID, time.Now().Unix(), strings.TrimSpace(reason), strings.TrimSpace(actor)); err != nil {
		return fmt.Errorf("record content audit review access: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit content audit review: %w", err)
	}
	return nil
}

// Prune removes expired ciphertext first and later removes expired metadata.
func (s *Store) Prune(ctx context.Context, rawRetentionDays, metadataRetentionDays int) error {
	if rawRetentionDays <= 0 {
		rawRetentionDays = 30
	}
	if metadataRetentionDays <= 0 {
		metadataRetentionDays = 180
	}
	now := time.Now()
	rawCutoff := now.Add(-time.Duration(rawRetentionDays) * 24 * time.Hour).Unix()
	metadataCutoff := now.Add(-time.Duration(metadataRetentionDays) * 24 * time.Hour).Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE audit_events SET evidence_nonce = NULL, evidence_ciphertext = NULL,
		evidence_status = 'expired' WHERE created_at < ? AND evidence_status = 'encrypted'`, rawCutoff); err != nil {
		return fmt.Errorf("expire content audit evidence: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_access_log WHERE event_id IN (
		SELECT id FROM audit_events WHERE created_at < ?
	)`, metadataCutoff); err != nil {
		return fmt.Errorf("delete expired content audit access history: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_events WHERE created_at < ?`, metadataCutoff); err != nil {
		return fmt.Errorf("delete expired content audit metadata: %w", err)
	}
	return nil
}

// Close closes the isolated database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
