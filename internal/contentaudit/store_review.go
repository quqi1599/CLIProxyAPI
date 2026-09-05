package contentaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func safeReviewDiagnostics(values map[string]int64) map[string]int64 {
	out := make(map[string]int64)
	for _, key := range []string{"shadow_queue", "queue", "admission", "provider", "total", "auth_select", "connect", "request_write", "ttfb", "transport", "read", "parse"} {
		if value, ok := values[key]; ok && value >= 0 {
			out[key] = value
		}
	}
	return out
}

// UpdateShadowReview fills one pending result and cannot change the customer's
// action, evidence, annotation, policy version, or forwarding metadata.
func (s *Store) UpdateShadowReview(ctx context.Context, eventID, policyVersion string, outcome modelReviewOutcome) error {
	diagnostics, err := json.Marshal(safeReviewDiagnostics(outcome.StageLatenciesMS))
	if err != nil {
		return fmt.Errorf("marshal content audit review diagnostics: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE audit_events SET
		model_review_model=?, model_review_resolved_model=?, model_review_decision=?, model_review_category=?,
		model_review_confidence=?, model_review_latency_ms=?, model_review_cache_hit=?,
		model_review_fallback=?, model_review_diagnostics=?
		WHERE id=? AND policy_version=? AND model_review_mode='shadow'
		AND model_review_fallback='shadow_pending'`,
		outcome.Model, outcome.ResolvedModel, outcome.Decision, outcome.Category, outcome.Confidence,
		outcome.Latency.Milliseconds(), boolInt(outcome.CacheHit), outcome.Fallback, string(diagnostics), eventID, policyVersion)
	return err
}

// InterruptShadowReviews accounts for admitted work that was canceled at shutdown.
func (s *Store) InterruptShadowReviews(ctx context.Context, ids []string, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, reason)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE audit_events SET model_review_decision='uncertain', model_review_fallback=?
		WHERE model_review_mode='shadow' AND model_review_fallback='shadow_pending'
		AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	return err
}

// RecoverInterruptedShadowReviews is startup-only, before this service admits
// new observations. Terminal results and decisions remain untouched.
func (s *Store) RecoverInterruptedShadowReviews(ctx context.Context, before int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE audit_events
		SET model_review_decision='uncertain', model_review_fallback='shadow_interrupted'
		WHERE model_review_mode='shadow' AND model_review_fallback='shadow_pending' AND created_at<=?`, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
