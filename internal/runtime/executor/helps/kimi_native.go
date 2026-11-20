package helps

import (
	"net/url"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// KimiCodeNativeOpenAIAuth keeps K3.1 requests and K3 video Chat in their original protocol when
// an official Coding Plan API key was configured under claude-api-key. Both
// protocol surfaces use the same key and host. Never derive an API key from
// OAuth metadata or infer an OpenAI endpoint for a third-party gateway.
func KimiCodeNativeOpenAIAuth(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) *cliproxyauth.Auth {
	if auth == nil || opts.Alt != "" {
		return nil
	}
	if opts.SourceFormat != sdktranslator.FormatOpenAI && opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		return nil
	}
	if !KimiCodeRequiresThinking(req.Model) && !(IsKimiVideoChat(req, opts) && IsKimiVideoBase(auth.Attributes["base_url"])) {
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

// NormalizeKimiChatSampling runs after thinking and payload overrides. For
// official K3.1 Chat, let the upstream choose its fixed sampling defaults.
// Invalid types and out-of-range values remain available to its validator.
func NormalizeKimiChatSampling(body []byte, baseModel, baseURL string) ([]byte, error) {
	switch strings.ToLower(baseModel) {
	case "k3", "k3[1m]":
		switch gjson.GetBytes(body, "reasoning_effort").String() {
		case "low", "high", "max":
			return sjson.DeleteBytes(body, "top_p")
		}
	case "k3.1-pro", "k3.1-pro[1m]", "k3.1-pro-highspeed", "k3.1-pro-highspeed[1m]":
		endpoint, err := url.Parse(baseURL)
		if err != nil || !IsKimiCodeEndpoint(endpoint) {
			return body, nil
		}
		for _, field := range []struct {
			name     string
			min, max float64
		}{
			{"temperature", 0, 2},
			{"top_p", 0, 1},
			{"presence_penalty", -2, 2},
			{"frequency_penalty", -2, 2},
		} {
			v := gjson.GetBytes(body, field.name)
			if v.Type == gjson.Number && v.Float() >= field.min && v.Float() <= field.max {
				body, err = sjson.DeleteBytes(body, field.name)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return body, nil
}
