package registry

import "strings"

// KimiCodeModelInfo describes published Coding Plan IDs, not Moonshot API aliases.
// ContextLength is the model ceiling; account entitlements may impose a lower limit.
// Source: https://www.kimi.com/code/docs/kimi-code/models.html
func KimiCodeModelInfo(model string) *ModelInfo {
	name := strings.ToLower(strings.TrimSpace(model))
	base := strings.TrimSuffix(name, "[1m]")
	info := &ModelInfo{ID: name, Object: "model", OwnedBy: "moonshot", Type: "kimi",
		ContextLength: 1048576, SupportedInputModalities: []string{"text", "image", "video"},
		Thinking: &ThinkingSupport{Levels: []string{"low", "high", "max"}, DynamicAllowed: true, ZeroAllowed: true}}
	switch base {
	case "k3.1-pro":
		info.DisplayName = "Kimi K3.1 Pro"
		info.Thinking.ZeroAllowed = false
	case "k3.1-pro-highspeed":
		info.DisplayName = "Kimi K3.1 Pro Highspeed"
		info.Thinking.ZeroAllowed = false
	case "k3":
		info.DisplayName = "Kimi K3"
	case "k3-256k":
		info.DisplayName = "Kimi K3 256K"
		info.ContextLength = 262144
		info.SupportedInputModalities = []string{"text", "image"}
	case "kimi-for-coding":
		info.DisplayName = "Kimi K2.8 Preview"
	case "kimi-for-coding-highspeed":
		info.DisplayName = "Kimi K2.7 Code Highspeed"
		info.ContextLength = 262144
		info.Thinking.Levels = nil
	default:
		return nil
	}
	if base != name && info.ContextLength < 1048576 {
		return nil
	}
	return info
}

func withKimiCodeBuiltins(models []*ModelInfo) []*ModelInfo {
	for _, id := range []string{"k3.1-pro", "k3.1-pro-highspeed", "k3", "k3-256k", "kimi-for-coding", "kimi-for-coding-highspeed"} {
		models = upsertModelInfos(models, KimiCodeModelInfo(id))
	}
	return models
}
