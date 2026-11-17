package helps

import (
	"net/url"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// KimiCodeNativeOpenAIAuth keeps K3.1 requests in their original protocol when
// an official Coding Plan API key was configured under claude-api-key. Both
// protocol surfaces use the same key and host. Never derive an API key from
// OAuth metadata or infer an OpenAI endpoint for a third-party gateway.
func KimiCodeNativeOpenAIAuth(auth *cliproxyauth.Auth, model string, opts cliproxyexecutor.Options) *cliproxyauth.Auth {
	if auth == nil || !KimiCodeRequiresThinking(model) || opts.Alt != "" {
		return nil
	}
	if opts.SourceFormat != sdktranslator.FormatOpenAI && opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		return nil
	}
	if strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return nil
	}
	endpoint, err := url.Parse(strings.TrimSpace(auth.Attributes["base_url"]))
	if err != nil || !IsKimiCodeEndpoint(endpoint) || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil
	}
	switch strings.TrimRight(endpoint.Path, "/") {
	case "/coding", "/coding/v1":
	default:
		return nil
	}
	endpoint.Path, endpoint.RawPath = "/coding/v1", ""
	adapted := auth.Clone()
	adapted.Attributes["base_url"] = endpoint.String()
	adapted.Attributes["compat_kind"] = "kimi"
	return adapted
}
