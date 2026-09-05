package contentaudit

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// All controllers sharing this audit database share one durable quota. UTC+08
// fixes the operational day independently of host locale or container timezone.
func modelReviewBudgetWindows(now time.Time) (string, string) {
	day := now.In(time.FixedZone("audit-UTC+08", 8*60*60)).Format("2006-01-02")
	minute := now.UTC().Format("2006-01-02T15:04")
	return "day:" + day, "minute:" + minute
}

func (s *Store) ReserveModelReviewCall(ctx context.Context, dayLimit, minuteLimit int) (string, error) {
	return s.reserveModelReviewCallAt(ctx, dayLimit, minuteLimit, time.Now())
}

// A failed or canceled provider attempt still consumes its reservation. A denied
// minute quota rolls back the day increment, so no external call is unaccounted.
func (s *Store) reserveModelReviewCallAt(ctx context.Context, dayLimit, minuteLimit int, now time.Time) (string, error) {
	if dayLimit <= 0 || minuteLimit <= 0 {
		return "daily_budget_exhausted", nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	day, minute := modelReviewBudgetWindows(now)
	if _, err = tx.ExecContext(ctx, legacyBudgetBootstrapSQL, legacyBudgetBootstrapArgs(dayLimit, now)...); err != nil {
		return "", err
	}
	for i, item := range []struct {
		key   string
		limit int
	}{{day, dayLimit}, {minute, minuteLimit}} {
		var count int
		err = tx.QueryRowContext(ctx, `INSERT INTO audit_model_review_budget(period_key,calls,created_at) VALUES(?,1,?)
			ON CONFLICT(period_key) DO UPDATE SET calls=calls+1 WHERE calls<? AND legacy_closed=0 RETURNING calls`, item.key, now.Unix(), item.limit).Scan(&count)
		if errors.Is(err, sql.ErrNoRows) {
			if i == 0 {
				return "daily_budget_exhausted", nil
			}
			return "minute_rate_limited", nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", tx.Commit()
}

type ModelReviewBudgetStatus struct {
	Available       bool  `json:"available"`
	DayLimit        int   `json:"day_limit"`
	DayUsed         int64 `json:"day_used"`
	MinuteLimit     int   `json:"minute_limit"`
	MinuteUsed      int64 `json:"minute_used"`
	LegacyDayClosed bool  `json:"legacy_day_closed"`
}

func (s *Store) ModelReviewBudgetStatus(ctx context.Context, dayLimit, minuteLimit int) ModelReviewBudgetStatus {
	status := ModelReviewBudgetStatus{DayLimit: dayLimit, MinuteLimit: minuteLimit}
	day, minute := modelReviewBudgetWindows(time.Now())
	rows, err := s.db.QueryContext(ctx, `SELECT period_key,calls,legacy_closed FROM audit_model_review_budget WHERE period_key IN (?,?)`, day, minute)
	if err != nil {
		return status
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var calls int64
		var legacyClosed bool
		if err = rows.Scan(&key, &calls, &legacyClosed); err != nil {
			return status
		}
		if key == day {
			status.DayUsed = calls
			status.LegacyDayClosed = legacyClosed
		} else {
			status.MinuteUsed = calls
		}
	}
	status.Available = rows.Err() == nil
	return status
}

// Legacy event logs do not count actual outbound attempts reliably. During the
// first upgrade day, conservatively close the remaining budget if unmetered
// reviews already happened; the next UTC+08 day starts a normal durable budget.
func (s *Store) InitializeLegacyModelReviewBudget(ctx context.Context, dayLimit int, now time.Time) error {
	_, err := s.db.ExecContext(ctx, legacyBudgetBootstrapSQL, legacyBudgetBootstrapArgs(dayLimit, now)...)
	return err
}

const legacyBudgetBootstrapSQL = `INSERT INTO audit_model_review_budget(period_key,calls,created_at,legacy_closed)
	SELECT ?,?,?,1 WHERE NOT EXISTS (SELECT 1 FROM audit_model_review_budget WHERE period_key=?)
	AND EXISTS (SELECT 1 FROM audit_events WHERE created_at>=? AND created_at<?
	AND model_review_mode IN ('shadow','enforce') AND model_review_model<>'' AND model_review_prompt_version='')
	ON CONFLICT(period_key) DO NOTHING`

func legacyBudgetBootstrapArgs(dayLimit int, now time.Time) []any {
	day, _ := modelReviewBudgetWindows(now)
	local := now.In(time.FixedZone("audit-UTC+08", 8*60*60))
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).Unix()
	return []any{day, dayLimit, now.Unix(), day, start, start + 86400}
}
