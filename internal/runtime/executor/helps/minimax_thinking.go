package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Preserve the original off intent for the measured M3 endpoint. Generic
// OpenAI reasoning fields can otherwise be removed and re-enable its default.
func PreserveMiniMaxM3ThinkingOff(base, model, format string, source, body []byte) ([]byte, error) {
	if !strings.EqualFold(thinking.ParseSuffix(model).ModelName, "MiniMax-M3") || OfficialMiniMaxModel("MiniMax-M3.1", base) != MiniMaxM31OfficialModel || !thinking.MiniMaxThinkingDisabled(model, format, source) {
		return body, nil
	}
	var err error
	for _, path := range []string{"thinking", "thinking_budget", "enable_thinking", "reasoning_effort", "reasoning.effort", "output_config.effort"} {
		body, err = sjson.DeleteBytes(body, path)
		if err != nil {
			return nil, err
		}
	}
	for _, path := range []string{"reasoning", "output_config"} {
		if value := gjson.GetBytes(body, path); value.IsObject() && len(value.Map()) == 0 {
			body, err = sjson.DeleteBytes(body, path)
			if err != nil {
				return nil, err
			}
		}
	}
	return sjson.SetBytes(body, "thinking.type", "disabled")
}
