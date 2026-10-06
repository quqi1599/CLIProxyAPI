package thinking

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
)

// IsMiniMaxM31Model recognizes the published preview and the configured short aliases.
// Recognition never rewrites the upstream model ID or creates a model mapping.
func IsMiniMaxM31Model(model string) bool {
	switch strings.ToLower(strings.TrimSpace(ParseSuffix(strings.TrimSpace(model)).ModelName)) {
	case "minimax-m3.1", "minimax-m3.1-flash", "minimax-m3.1-flash-preview":
		return true
	default:
		return false
	}
}

// IsMiniMaxM3Model includes M3.1 without matching unrelated versions such as M30.
func IsMiniMaxM3Model(model string) bool {
	base := strings.ToLower(strings.TrimSpace(ParseSuffix(strings.TrimSpace(model)).ModelName))
	return base == "minimax-m3" || strings.HasPrefix(base, "minimax-m3-") || IsMiniMaxM31Model(base)
}

// MiniMaxThinkingDisabled resolves explicit source controls with the same
// model-suffix precedence as M3.1 validation. It never infers an off intent
// from a translated payload or a provider default.
func MiniMaxThinkingDisabled(model, format string, body []byte) bool {
	config := extractMiniMaxThinkingConfig(body, format)
	if suffix := ParseSuffix(model); suffix.HasSuffix {
		config = parseSuffixToConfig(suffix.RawSuffix, format, suffix.ModelName)
	}
	return config.Mode == ModeNone || (config.Mode == ModeLevel && strings.EqualFold(strings.TrimSpace(string(config.Level)), "none"))
}

// applyMiniMaxM31Thinking keeps the canonical config/validation/provider pipeline,
// while enforcing MiniMax's mandatory adaptive thinking and five effort levels.
// Source controls avoid a lossy level -> Claude budget -> level round trip.
func applyMiniMaxM31Thinking(body []byte, suffix SuffixResult, fromFormat, toFormat string, sourceBody []byte, applier ProviderApplier) ([]byte, error) {
	var config ThinkingConfig
	if suffix.HasSuffix {
		config = parseSuffixToConfig(suffix.RawSuffix, toFormat, suffix.ModelName)
	} else {
		config = extractMiniMaxThinkingConfig(sourceBody, fromFormat)
		if !hasThinkingConfig(config) {
			config = extractMiniMaxThinkingConfig(body, toFormat)
		}
	}
	if !hasThinkingConfig(config) {
		return body, nil
	}
	if config.Mode == ModeLevel {
		config.Level = ThinkingLevel(strings.ToLower(strings.TrimSpace(string(config.Level))))
	}
	if config.Mode == ModeNone || (config.Mode == ModeLevel && config.Level == LevelNone) {
		return body, NewThinkingErrorWithModel(ErrLevelNotSupported,
			"MiniMax M3.1 requires adaptive thinking; use effort low instead of disabling thinking", suffix.ModelName)
	}
	if config.Mode == ModeAuto || (config.Mode == ModeLevel && config.Level == LevelAuto) {
		config = ThinkingConfig{Mode: ModeLevel, Level: LevelMax}
	}
	info := &registry.ModelInfo{ID: suffix.ModelName, Thinking: &registry.ThinkingSupport{
		Levels: []string{"low", "medium", "high", "xhigh", "max"}, DynamicAllowed: true,
	}}
	validated, err := ValidateConfig(config, info, fromFormat, toFormat, suffix.HasSuffix)
	if err != nil {
		return body, err
	}
	// Remove only controls, preserving message thinking blocks and output format.
	cleaned, changed, _ := stripThinkingObject(gjson.ParseBytes(body), []thinkingStripPath{
		{segments: []string{"thinking"}},
		{segments: []string{"enable_thinking"}},
		{segments: []string{"reasoning_effort"}},
		{segments: []string{"reasoning", "effort"}, pruneEmpty: true},
		{segments: []string{"output_config", "effort"}, pruneEmpty: true},
	})
	if changed {
		body = []byte(cleaned)
	}
	return applier.Apply(body, *validated, info)
}

func extractMiniMaxThinkingConfig(body []byte, format string) ThinkingConfig {
	// These compatibility toggles occur on all three client protocols. Resolve
	// an explicit disable before a stale effort label or a translated default.
	if gjson.GetBytes(body, "enable_thinking").Type == gjson.False {
		return ThinkingConfig{Mode: ModeNone}
	}
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "disabled", "off", "none":
		return ThinkingConfig{Mode: ModeNone}
	}
	if format == "openai-response" {
		format = "codex"
	}
	config := extractThinkingConfig(body, format)
	if config.Mode == ModeBudget && config.Budget == 0 && gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
		config = ThinkingConfig{Mode: ModeNone}
	}
	if config.Mode == ModeNone {
		return config
	}
	// SDKs can carry another protocol's effort spelling without a thinking object.
	// Keep native controls authoritative and reject an explicit none as usual.
	if !hasThinkingConfig(config) {
		for _, path := range []string{"reasoning_effort", "reasoning.effort", "output_config.effort"} {
			if effort := gjson.GetBytes(body, path); effort.Type == gjson.String && strings.TrimSpace(effort.String()) != "" {
				return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(strings.ToLower(strings.TrimSpace(effort.String())))}
			}
		}
	}
	// MiniMax accepts output_config.effort without a separate thinking object.
	if format == "claude" {
		if effort := gjson.GetBytes(body, "output_config.effort"); effort.Type == gjson.String && strings.TrimSpace(effort.String()) != "" {
			return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(strings.ToLower(strings.TrimSpace(effort.String())))}
		}
		if !hasThinkingConfig(config) && gjson.GetBytes(body, "thinking.type").String() == "adaptive" {
			return ThinkingConfig{Mode: ModeAuto, Budget: -1}
		}
	}
	return config
}
