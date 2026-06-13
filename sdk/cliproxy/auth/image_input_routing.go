package auth

import (
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func filterImageInputUnsupportedExecutionModels(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, candidates []string) []string {
	if len(candidates) == 0 || !candidateSetHasImageInputUnsupportedModel(candidates) {
		return candidates
	}
	if !requestHasImageInput(req, opts) {
		return candidates
	}

	filtered := make([]string, 0, len(candidates))
	removed := false
	for _, candidate := range candidates {
		if isImageInputUnsupportedExecutionModel(candidate) {
			removed = true
			continue
		}
		filtered = append(filtered, candidate)
	}
	if !removed {
		return candidates
	}
	return filtered
}

func candidateSetHasImageInputUnsupportedModel(candidates []string) bool {
	for _, candidate := range candidates {
		if isImageInputUnsupportedExecutionModel(candidate) {
			return true
		}
	}
	return false
}

func requestHasImageInput(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	return requestHasMiniMaxM3ImageInput(miniMaxRoutingPayload(req, opts))
}

func isImageInputUnsupportedExecutionModel(model string) bool {
	base := strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	switch base {
	case "gpt-5.3-codex-spark":
		return true
	default:
		return false
	}
}
