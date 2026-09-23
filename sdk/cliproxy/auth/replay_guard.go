package auth

import failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"

// hasCommittedOutput is a terminal replay boundary, not just a reason to skip
// immediate failover. Neither a second model, credential, round nor credential
// refresh may replay a request once any layer reports delivered output.
func hasCommittedOutput(err error) bool {
	failure, ok := failurecontract.As(err)
	return ok && (failure.OutputCommitted || failure.StreamPhase == failurecontract.StreamPhaseAfterOutput)
}
