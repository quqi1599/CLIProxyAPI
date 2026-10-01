package thinking

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
)

// IsQwen38Model identifies the verified hybrid-thinking models, not the preview.
func IsQwen38Model(model string) bool {
	switch strings.ToLower(strings.TrimSpace(ParseSuffix(strings.TrimSpace(model)).ModelName)) {
	case "qwen3.8-max", "qwen3.8-max-0902", "qwen3.8-flash":
		return true
	default:
		return false
	}
}

// applyQwenClaudeThinking preserves level intent before a lossy Claude budget
// conversion. Qwen accepts enabled + output_config.effort, not Claude adaptive.
// Contract: https://help.aliyun.com/zh/model-studio/anthropic-api-messages
func applyQwenClaudeThinking(body []byte, suffix SuffixResult, fromFormat string, sourceBody []byte, applier ProviderApplier) ([]byte, error) {
	var config ThinkingConfig
	if suffix.HasSuffix {
		config = parseSuffixToConfig(suffix.RawSuffix, "claude", suffix.ModelName)
	} else {
		if fromFormat == "openai-response" {
			fromFormat = "openai"
		}
		config = extractThinkingConfig(sourceBody, fromFormat)
		if !hasThinkingConfig(config) {
			config = extractClaudeConfig(body)
		}
	}
	if !hasThinkingConfig(config) {
		return body, nil
	}
	if config.Mode == ModeLevel {
		switch config.Level {
		case LevelMinimal:
			config.Level = LevelLow
		case LevelHigh, LevelMax:
			config.Level = LevelXHigh
		}
	}
	info := &registry.ModelInfo{ID: suffix.ModelName, Thinking: &registry.ThinkingSupport{
		Levels: []string{"low", "medium", "xhigh"}, ZeroAllowed: true, DynamicAllowed: true,
	}}
	if config.Mode == ModeBudget {
		// Keep explicit legacy budgets numeric. No undocumented model-wide range
		// is invented; the Claude applier enforces budget < the caller's max_tokens.
		info.Thinking.Levels = nil
		if maxTokens := gjson.GetBytes(body, "max_tokens").Int(); config.Budget > 0 && maxTokens == 1 {
			return body, NewThinkingErrorWithModel(ErrBudgetOutOfRange,
				"a positive thinking budget requires max_tokens greater than 1", suffix.ModelName)
		}
	}
	validated, err := ValidateConfig(config, info, fromFormat, "claude", suffix.HasSuffix)
	if err != nil {
		return body, err
	}
	cleaned, changed, _ := stripThinkingObject(gjson.ParseBytes(body), []thinkingStripPath{
		{segments: []string{"thinking"}},
		{segments: []string{"thinking_budget"}},
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
