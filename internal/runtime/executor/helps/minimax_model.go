package helps

import (
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

const MiniMaxM31OfficialModel = "MiniMax-M3.1-Flash-Preview"

// The official Chat API rejects the legacy short names, while Anthropic can
// accept them as M3. Both protocols must address the same documented M3.1 model.
// This normalization is restricted to the official HTTPS SDK endpoints.
func OfficialMiniMaxModel(model, baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return model
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.minimaxi.com", "api.minimax.cn", "api.minimax.io":
	default:
		return model
	}
	switch strings.TrimRight(u.Path, "/") {
	case "", "/v1", "/anthropic", "/anthropic/v1", "/v1/chat/completions", "/anthropic/v1/messages", "/v1/messages", "/anthropic/v1/messages/count_tokens":
	default:
		return model
	}
	suffix := thinking.ParseSuffix(model)
	switch strings.ToLower(strings.TrimSpace(suffix.ModelName)) {
	case "minimax-m3.1", "minimax-m3.1-flash", "minimax-m3.1-flash-preview":
		mapped := MiniMaxM31OfficialModel
		if suffix.HasSuffix {
			mapped += "(" + suffix.RawSuffix + ")"
		}
		return mapped
	default:
		return model
	}
}
