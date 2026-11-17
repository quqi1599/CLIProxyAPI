package helps

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestKimiNativeAuthEndpointAndCredentialBoundary(t *testing.T) {
	for _, tc := range []struct {
		base, model, key, source, alt string
		want                          bool
	}{
		{"https://api.kimi.com/coding", "k3.1-pro", "key", "openai", "", true},
		{"https://api.kimi.ai/coding/v1/", "k3.1-pro-highspeed[1m](low)", "key", "openai-response", "", true},
		{"https://gateway.test/coding", "k3.1-pro", "key", "openai", "", false},
		{"https://api.moonshot.cn/v1", "k3.1-pro", "key", "openai", "", false},
		{"http://api.kimi.com/coding", "k3.1-pro", "key", "openai", "", false},
		{"https://api.kimi.com/coding/other", "k3.1-pro", "key", "openai", "", false},
		{"https://api.kimi.com/coding?route=other", "k3.1-pro", "key", "openai", "", false},
		{"https://user@api.kimi.com/coding", "k3.1-pro", "key", "openai", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro", "", "openai", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro", "key", "claude", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro", "key", "openai-response", "responses/compact", false},
		{"https://api.kimi.com/coding", "kimi-k3", "key", "openai", "", false},
		{"https://api.kimi.com/coding", "kimi-for-coding-highspeed", "key", "openai", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro-unknown", "key", "openai", "", false},
	} {
		t.Run(tc.base+"/"+tc.model+"/"+tc.source+"/"+tc.alt+"/"+tc.key, func(t *testing.T) {
			auth := &cliproxyauth.Auth{ID: "same-id", Provider: "claude", ProxyURL: "http://proxy.test:8080", Attributes: map[string]string{"base_url": tc.base, "api_key": tc.key, "header:X-Test": "value"}, Metadata: map[string]any{"access_token": "oauth-token"}}
			adapted := KimiCodeNativeOpenAIAuth(auth, tc.model, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.source), Alt: tc.alt})
			if (adapted != nil) != tc.want {
				t.Fatalf("native adaptation=%t want=%t", adapted != nil, tc.want)
			}
			if adapted == nil {
				return
			}
			if adapted == auth || adapted.ID != auth.ID || adapted.Provider != auth.Provider || adapted.ProxyURL != auth.ProxyURL || adapted.Attributes["api_key"] != tc.key || adapted.Attributes["header:X-Test"] != "value" {
				t.Fatal("credential identity, proxy or configured headers changed")
			}
			if auth.Attributes["base_url"] != tc.base || auth.Attributes["compat_kind"] != "" {
				t.Fatal("stored credential mutated")
			}
		})
	}
}
