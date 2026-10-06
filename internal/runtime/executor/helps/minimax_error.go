package helps

import (
	"strings"

	"github.com/tidwall/gjson"
)

// MiniMaxRejectionDiagnostic emits fixed diagnostics, never provider prose.
// Code 2013 alone does not identify the rejected parameter.
func MiniMaxRejectionDiagnostic(body []byte) (string, string) {
	message := ""
	for _, path := range []string{"error.message", "base_resp.status_msg", "message"} {
		if value := gjson.GetBytes(body, path).String(); value != "" {
			message = strings.ToLower(value)
			break
		}
	}
	contains := func(patterns ...string) bool {
		for _, pattern := range patterns {
			if strings.Contains(message, pattern) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("unknown model", "model not found", "model_not_found", "unsupported model"):
		return "minimax_model_not_supported", "MiniMax upstream does not recognize the configured model; check the model mapping"
	case contains("requires adaptive thinking") || strings.Contains(message, "thinking") && contains("cannot be disabled", "can not be disabled", "disabled is not allowed"):
		return "minimax_adaptive_thinking_required", "MiniMax M3.1 requires adaptive thinking; use low effort instead of disabling thinking"
	case strings.Contains(message, "reasoning_split") && contains("false", "not supported", "unsupported"):
		return "minimax_reasoning_split_unsupported", "MiniMax M3.1 requires separated reasoning output"
	case contains("reasoning_effort", "output_config.effort") && contains("invalid", "unsupported", "not support", "not allowed", "must be", "one of"):
		return "minimax_effort_invalid", "MiniMax M3.1 effort must be low, medium, high, xhigh or max"
	default:
		return "", ""
	}
}
