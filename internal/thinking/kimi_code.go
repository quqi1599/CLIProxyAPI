package thinking

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
)

// IsKimiCodeModel matches published Coding Plan IDs without guessing aliases.
func IsKimiCodeModel(model string) bool {
	return registry.KimiCodeModelInfo(ParseSuffix(model).ModelName) != nil
}

// applyKimiCodeThinking normalizes client intent before protocol-specific output.
// Unlike the older Moonshot K3 profile, Coding Plan supports three effort levels.
func applyKimiCodeThinking(body []byte, suffix SuffixResult, fromFormat, toFormat string, source []byte, applier ProviderApplier) ([]byte, error) {
	info := registry.KimiCodeModelInfo(suffix.ModelName)
	var config ThinkingConfig
	if suffix.HasSuffix {
		config = parseSuffixToConfig(suffix.RawSuffix, toFormat, suffix.ModelName)
		if !hasThinkingConfig(config) {
			config = ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(suffix.RawSuffix)}
		}
	} else {
		config = extractKimiCodeThinking(source, fromFormat)
		if !hasThinkingConfig(config) {
			config = extractKimiCodeThinking(body, toFormat)
		}
	}
	if config.Mode == ModeLevel {
		switch strings.ToLower(strings.TrimSpace(string(config.Level))) {
		case "ultra", "max", "xhigh":
			config.Level = LevelMax
		case "high", "medium":
			config.Level = LevelHigh
		case "low", "minimum", "light", "minimal":
			config.Level = LevelLow
		case "off", "none", "disabled":
			config = ThinkingConfig{Mode: ModeNone}
		default:
			return body, NewThinkingErrorWithModel(ErrUnknownLevel, "Kimi Code effort must be low, high or max", suffix.ModelName)
		}
	}
	if config.Mode == ModeNone && !info.Thinking.ZeroAllowed {
		return body, NewThinkingErrorWithModel(ErrThinkingNotSupported, "K3.1 Pro requires thinking; use low, high or max instead of disabling it", suffix.ModelName)
	}
	if len(info.Thinking.Levels) == 0 {
		if config.Mode == ModeLevel || (config.Mode == ModeBudget && config.Budget > 0) {
			return body, NewThinkingErrorWithModel(ErrLevelNotSupported, "K2.7 Code Highspeed does not support adjustable thinking effort", suffix.ModelName)
		}
		if config.Mode != ModeNone {
			// No adjustable effort: leave the default enabled mode to the upstream.
			config = ThinkingConfig{Mode: ModeAuto, Budget: -1}
		}
	} else if !hasThinkingConfig(config) || config.Mode == ModeAuto {
		level := LevelMax
		if strings.TrimSuffix(info.ID, "[1m]") == "k3" || info.ID == "k3-256k" {
			level = LevelHigh
		}
		config = ThinkingConfig{Mode: ModeLevel, Level: level}
	}
	validated, err := ValidateConfig(config, info, fromFormat, toFormat, suffix.HasSuffix)
	if err != nil {
		return body, err
	}
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
	if len(info.Thinking.Levels) == 0 && config.Mode == ModeAuto {
		return body, nil
	}
	return applier.Apply(body, *validated, info)
}

func extractKimiCodeThinking(body []byte, format string) ThinkingConfig {
	if gjson.GetBytes(body, "enable_thinking").Type == gjson.False {
		return ThinkingConfig{Mode: ModeNone}
	}
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "disabled", "off", "none":
		return ThinkingConfig{Mode: ModeNone}
	}
	if budget := gjson.GetBytes(body, "thinking.budget_tokens"); budget.Type == gjson.Number && budget.Int() == 0 {
		return ThinkingConfig{Mode: ModeNone}
	}
	// Preserve effort from the original protocol before translators turn it into
	// a numeric budget or discard it. Null means the model's default.
	paths := []string{"reasoning_effort", "reasoning.effort", "output_config.effort"}
	if format == "claude" {
		paths = []string{"output_config.effort", "reasoning_effort", "reasoning.effort"}
	} else if format == "codex" || format == "openai-response" {
		paths = []string{"reasoning.effort", "reasoning_effort", "output_config.effort"}
	}
	for _, path := range paths {
		if effort := gjson.GetBytes(body, path); effort.Exists() && effort.Type != gjson.Null {
			return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(effort.String())}
		}
	}
	if format == "openai-response" || format == "codex" {
		format = "openai"
	}
	return extractThinkingConfig(body, format)
}
