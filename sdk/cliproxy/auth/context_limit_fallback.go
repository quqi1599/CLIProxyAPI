package auth

import (
	"context"
	"sort"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const contextLimitFallbackMaxAttempts = 4

// Context capacity belongs to an upstream/model combination, not an API key.
// Keep different model pools separate so a larger-context fallback can run.
func (m *Manager) contextLimitRouteKey(auth *Auth, model string) string {
	base := compatibilityFallbackRouteKey(auth)
	if base == "" {
		return ""
	}
	requested := m.selectionModelForAuth(auth, model)
	models := m.resolveAPIKeyUpstreamModelPool(auth, requested)
	if len(models) == 0 {
		models = m.resolveOpenAICompatUpstreamModelPool(auth, requested)
	}
	if len(models) == 0 {
		models = []string{m.applyAPIKeyModelAlias(auth, requested)}
	}
	models = append([]string(nil), models...)
	sort.Strings(models)
	return base + "\x00" + strings.Join(models, "\x00")
}

func (m *Manager) markContextLimitRouteTried(ctx context.Context, tried map[string]struct{}, selected *Auth, model string) {
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		trace.configureBudget(contextLimitFallbackMaxAttempts, contextLimitFallbackMaxAttempts-1)
	}
	key := m.contextLimitRouteKey(selected, model)
	if key == "" {
		return
	}
	for _, candidate := range m.List() {
		if m.contextLimitRouteKey(candidate, model) == key {
			tried[candidate.ID] = struct{}{}
		}
	}
}

func credentialRetryLimitReached(ctx context.Context, attempted, configured int, model string, opts cliproxyexecutor.Options, lastErr error) bool {
	if shouldBypassCredentialRetryLimitForRequest(model, opts, lastErr) {
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			return trace.attemptCount() >= contextLimitFallbackMaxAttempts
		}
		return attempted >= contextLimitFallbackMaxAttempts
	}
	if trace := requestAttemptTraceFromContext(ctx); trace != nil && trace.totalAttemptBudgetExhausted() {
		return true
	}
	return configured > 0 && attempted > configured
}

func (t *requestAttemptTrace) configureTotalAttemptBudget(retries, credentials int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Preserve the explicitly unbounded credential mode. With a configured
	// credential limit, outer rounds and inner failover share one total budget.
	t.enforceTotalAttempts = credentials > 0
	// Retain room for short transport recovery even with one fallback credential.
	t.maxAttempts = max(4, retries+1, credentials+1)
	t.maxFallbacks = t.maxAttempts - 1
}

func (t *requestAttemptTrace) totalAttemptBudgetExhausted() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enforceTotalAttempts && t.maxAttempts > 0 && t.attempts >= t.maxAttempts
}
