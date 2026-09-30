package thinking

import "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"

// applyClaudeToGPTThinking translates the original client's intent after the
// aggregate route has selected its upstream model. Translation defaults and the
// public Claude alias must not determine a GPT model's reasoning capabilities.
func applyClaudeToGPTThinking(body, source []byte, suffix SuffixResult, toFormat string, info *registry.ModelInfo, applier ProviderApplier) ([]byte, error) {
	var config ThinkingConfig
	if suffix.HasSuffix {
		config = parseSuffixToConfig(suffix.RawSuffix, toFormat, suffix.ModelName)
	} else {
		config = extractClaudeConfig(source)
	}
	// Omitted/adaptive effort delegates to the selected upstream's own default.
	// Do not manufacture medium/xhigh or send the non-OpenAI enum value "auto".
	if !hasThinkingConfig(config) || config.Mode == ModeAuto || (config.Mode == ModeLevel && config.Level == LevelAuto) {
		return StripThinkingConfig(body, toFormat), nil
	}
	if info != nil && info.Thinking != nil {
		if config.Mode == ModeNone && !info.Thinking.ZeroAllowed && !HasLevel(info.Thinking.Levels, string(LevelNone)) {
			return body, NewThinkingErrorWithModel(ErrLevelNotSupported,
				"the selected upstream model does not support disabling reasoning", suffix.ModelName)
		}
		// Explicit upstream capabilities remain authoritative for configured models.
		// Clone metadata rather than changing shared registry state.
		copyInfo := *info
		copyInfo.UserDefined = false
		info = &copyInfo
		validated, err := ValidateConfig(config, info, "claude", toFormat, suffix.HasSuffix)
		if err != nil {
			return body, err
		}
		config = *validated
	} else if !IsUserDefinedModel(info) {
		return StripThinkingConfig(body, toFormat), nil
	}
	// Unknown target models retain explicit intent for upstream validation; never
	// borrow the public alias's limits or invent a capability table for new GPTs.
	return applier.Apply(StripThinkingConfig(body, toFormat), config, info)
}
