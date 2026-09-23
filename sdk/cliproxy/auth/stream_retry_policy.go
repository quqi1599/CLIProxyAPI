package auth

import cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"

// OwnsStreamRetryBudget reports whether the manager applies a bounded retry
// policy to the whole streaming request. Callers must not start a fresh manager
// operation after it returns a terminal failure, which would reset its attempts,
// rounds, admission and first-event wait budgets.
func (m *Manager) OwnsStreamRetryBudget(providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	return cliproxyexecutor.IsRemoteCompactionIntent(compactionIntentFromRequest(req, opts)) ||
		isGPTRetryRoute(m.normalizeProviders(providers), req.Model)
}
