package auth

import (
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// hasCommittedOutput is a terminal replay boundary, not just a reason to skip
// immediate failover. Neither a second model, credential, round nor credential
// refresh may replay a request once any layer reports delivered output.
func hasCommittedOutput(err error) bool {
	failure, ok := failurecontract.As(err)
	return ok && (failure.OutputCommitted || failure.StreamPhase == failurecontract.StreamPhaseAfterOutput)
}

// isTerminalRoutingFailure preserves request-scoped failures across model pools,
// credentials and rounds. Other scopes may switch to an independent routing
// object even when the failed object is not retryable. Existing narrow request
// context/compatibility migrations remain allowed before output.
func isTerminalRoutingFailure(model string, opts cliproxyexecutor.Options, err error) bool {
	if hasCommittedOutput(err) {
		return true
	}
	failure, ok := failurecontract.As(err)
	if !ok || failure.Scope != failurecontract.ScopeRequest {
		return false
	}
	return !shouldFallbackRequestScopedRouteErrorForRequest(model, opts, err)
}
