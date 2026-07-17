package auth

import (
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const mimoV25ProImageInputSignal = "mimo_v2_5_pro_image_input"

func rejectMiMoV25ProImageInput(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	requestedModel := requestedModelAliasFromOptions(opts, req.Model)
	if (!isMiMoV25ProModel(req.Model) && !isMiMoV25ProModel(requestedModel)) || !requestHasImageInput(req, opts) {
		return nil
	}
	return &Error{
		Code:       "request_feature_unsupported",
		Message:    mimoV25ProImageInputSignal + ". mimo-v2.5-pro does not support image input; use mimo-v2.5 instead. The requested model was not changed.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func isMiMoV25ProModel(model string) bool {
	base := strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	if slash := strings.LastIndex(base, "/"); slash >= 0 {
		base = base[slash+1:]
	}
	return base == "mimo-v2.5-pro"
}

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
	payload := miniMaxRoutingPayload(req, opts)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	root := gjson.ParseBytes(payload)
	for _, path := range []string{"messages", "input"} {
		if content := root.Get(path); content.Exists() && parsedRequestHasMiniMaxM3PartType(content, isMiniMaxM3ImagePartType) {
			return true
		}
	}
	return false
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
