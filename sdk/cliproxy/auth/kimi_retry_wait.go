package auth

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	log "github.com/sirupsen/logrus"
)

// This bounds waiting requests, not upstream concurrency or subscription quota.
// The existing retry count and max-retry-interval still decide whether to wait.
const kimiRetryQueueLimit = 64

func (m *Manager) waitForRetryCooldown(ctx context.Context, model string, wait time.Duration) (bool, error) {
	model = strings.ToLower(canonicalModelKey(model))
	if wait <= 0 || (!strings.HasPrefix(model, "kimi-") && !strings.HasPrefix(model, "kimi/") && !thinking.IsKimiCodeModel(model)) {
		return true, waitForCooldown(ctx, wait)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for {
		depth := m.kimiRetryWaiters.Load()
		if depth >= kimiRetryQueueLimit {
			logEntryWithRequestID(ctx).WithFields(log.Fields{
				"event": "kimi_retry_wait", "kimi_retry_queue_depth": depth,
				"kimi_retry_queue_full": true, "kimi_retry_wait_ms": wait.Milliseconds(),
			}).Info("Kimi retry wait queue full")
			return false, nil
		}
		if m.kimiRetryWaiters.CompareAndSwap(depth, depth+1) {
			defer m.kimiRetryWaiters.Add(-1)
			logEntryWithRequestID(ctx).WithFields(log.Fields{
				"event": "kimi_retry_wait", "kimi_retry_queue_depth": depth + 1,
				"kimi_retry_queue_full": false, "kimi_retry_wait_ms": wait.Milliseconds(),
			}).Info("waiting for Kimi cooldown")
			return true, waitForCooldown(ctx, wait)
		}
	}
}
