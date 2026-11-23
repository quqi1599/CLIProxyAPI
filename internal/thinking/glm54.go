package thinking

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// IsGLM54Model matches the published model and Claude Code's 1M hint.
func IsGLM54Model(model string) bool {
	name := strings.ToLower(strings.TrimSpace(ParseSuffix(model).ModelName))
	name = name[strings.LastIndex(name, "/")+1:]
	return name == "glm-5.4" || name == "glm-5.4[1m]"
}

// GLM54ThinkingSupport returns the effort levels documented for the Coding Plan.
func GLM54ThinkingSupport() *registry.ThinkingSupport {
	return &registry.ThinkingSupport{Levels: []string{"low", "high", "max"}}
}

func glm54Level(value string) ThinkingLevel {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "off", "disabled", "false", "minimal", "light", "low":
		return LevelLow
	case "medium", "high":
		return LevelHigh
	default:
		return LevelMax
	}
}

func glm54RequestLevel(body []byte, format string) ThinkingLevel {
	effortPaths := []string{"reasoning_effort", "reasoning.effort", "output_config.effort", "thinking.reasoning_effort"}
	switch format {
	case "claude":
		effortPaths[0], effortPaths[2] = effortPaths[2], effortPaths[0]
	case "codex", "openai-response":
		effortPaths[0], effortPaths[1] = effortPaths[1], effortPaths[0]
	}
	for _, path := range effortPaths {
		if effort := gjson.GetBytes(body, path); effort.Type == gjson.String && effort.String() != "" {
			return glm54Level(effort.String())
		}
	}
	if glm54Level(gjson.GetBytes(body, "thinking.type").String()) == LevelLow || gjson.GetBytes(body, "enable_thinking").Type == gjson.False {
		return LevelLow
	}
	for _, path := range []string{"thinking.budget_tokens", "thinking_budget"} {
		if budget := gjson.GetBytes(body, path); budget.Type == gjson.Number {
			level, _ := ConvertBudgetToLevel(int(budget.Int()))
			return glm54Level(level)
		}
	}
	return LevelMax
}

// Read intent before translation, which can discard a Claude or Responses effort.
func applyGLM54Thinking(body, source []byte, suffix SuffixResult, from, to string, applier ProviderApplier) ([]byte, error) {
	if len(source) == 0 {
		source = body
	}
	controls := source
	switch from {
	case "openai", "claude", "codex", "openai-response":
	default:
		controls = body
	}
	level := glm54RequestLevel(controls, from)
	if suffix.HasSuffix {
		value := suffix.RawSuffix
		if budget, ok := ParseNumericSuffix(value); ok {
			value, _ = ConvertBudgetToLevel(budget)
		}
		level = glm54Level(value)
	}
	info := &registry.ModelInfo{ID: suffix.ModelName, Thinking: GLM54ThinkingSupport()}
	config, err := ValidateConfig(ThinkingConfig{Mode: ModeLevel, Level: level}, info, from, to, suffix.HasSuffix)
	if err != nil {
		return body, err
	}
	// Remove client-only controls. Keep sibling fields and historical messages.
	for _, path := range []string{"enable_thinking", "thinking_budget", "thinking.budget_tokens", "thinking.reasoning_effort", "reasoning.effort"} {
		body, _ = sjson.DeleteBytes(body, path)
	}
	if to == "openai" {
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
		body, _ = sjson.SetBytes(body, "thinking.type", "enabled")
	} else {
		body, _ = sjson.DeleteBytes(body, "reasoning_effort")
	}
	for _, path := range []string{"reasoning", "output_config"} {
		if value := gjson.GetBytes(body, path); value.IsObject() && len(value.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, path)
		}
	}
	if clear := gjson.GetBytes(source, "thinking.clear_thinking"); clear.Type == gjson.False || clear.Type == gjson.True {
		body, _ = sjson.SetBytes(body, "thinking.clear_thinking", clear.Bool())
	}
	return applier.Apply(body, *config, info)
}
