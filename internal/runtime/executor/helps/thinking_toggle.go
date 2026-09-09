package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeOpenAICompatDisabledThinking preserves an explicit off switch before
// providers remove unsupported OpenAI effort fields. Model-specific policies
// still decide how thinking-only models handle the request.
func NormalizeOpenAICompatDisabledThinking(body []byte, kind string) []byte {
	switch kind {
	case "qwen", "zhipu", "doubao":
	default:
		return body
	}
	config, ok := thinking.ExtractOpenAIStyleThinkingConfig(body)
	if !ok || config.Mode != thinking.ModeNone {
		return body
	}
	if kind == "qwen" {
		body, _ = sjson.SetBytes(body, "enable_thinking", false)
	} else {
		body, _ = sjson.SetBytes(body, "thinking.type", "disabled")
		body, _ = sjson.DeleteBytes(body, "thinking.budget_tokens")
	}
	return body
}

// DoubaoSeedThinkingType retains the native toggle for known hybrid Seed models.
// Hosted third-party models keep their separate, existing Ark contract.
func DoubaoSeedThinkingType(body []byte, model string) string {
	name := strings.ToLower(thinking.ParseSuffix(model).ModelName)
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	knownHybrid := false
	for _, prefix := range []string{"doubao-seed-2.0-pro", "doubao-seed-2.0-lite", "doubao-seed-2.0-mini", "doubao-seed-2.1-pro", "doubao-seed-2.1-turbo", "doubao-seed-2-0-pro", "doubao-seed-2-0-lite", "doubao-seed-2-0-mini", "doubao-seed-2-1-pro", "doubao-seed-2-1-turbo"} {
		if name == prefix || strings.HasPrefix(name, prefix+"-") {
			knownHybrid = true
			break
		}
	}
	if !knownHybrid {
		return ""
	}
	config, ok := thinking.ExtractOpenAIStyleThinkingConfig(body)
	if ok && config.Mode == thinking.ModeNone {
		return "disabled"
	}
	switch value := strings.ToLower(gjson.GetBytes(body, "thinking.type").String()); value {
	case "enabled", "disabled", "auto":
		return value
	}
	if ok {
		return "enabled"
	}
	return ""
}

// NormalizeDeepSeekResponsesThinking restores the Responses-specific off switch
// after shared compatibility code has normalized it to the Chat toggle.
func NormalizeDeepSeekResponsesThinking(body []byte) []byte {
	config, ok := thinking.ExtractOpenAIStyleThinkingConfig(body)
	if !ok || config.Mode != thinking.ModeNone {
		return body
	}
	body, _ = sjson.SetBytes(body, "reasoning.effort", "none")
	for _, field := range []string{"thinking", "enable_thinking", "thinking_budget", "reasoning_effort"} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	return body
}
