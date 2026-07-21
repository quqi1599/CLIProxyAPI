package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/provideridentity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestOpenAICompatResolveProfileUsesProviderIdentityPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		auth       *cliproxyauth.Auth
		wantKind   string
		wantSource provideridentity.Source
	}{
		{
			name: "explicit config wins",
			cfg: &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "configured-route", Kind: "kimi", BaseURL: "https://api.deepseek.com/v1"},
			}},
			auth: &cliproxyauth.Auth{
				Provider: "openai-compatible-configured-route",
				Attributes: map[string]string{
					"compat_name": "configured-route",
					"compat_kind": "minimax",
					"base_url":    "https://api.deepseek.com/v1",
				},
			},
			wantKind:   "kimi",
			wantSource: provideridentity.SourceCompatConfig,
		},
		{
			name: "auth attribute wins over URL",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"compat_kind": "minimax",
				"base_url":    "https://api.deepseek.com/v1",
			}},
			wantKind:   "minimax",
			wantSource: provideridentity.SourceAttribute,
		},
		{
			name: "persisted explicit source stays aligned with conductor",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"compat_kind":                        "minimax",
				provideridentity.KindSourceAttribute: string(provideridentity.SourceCompatConfig),
				"base_url":                           "https://api.deepseek.com/v1",
			}},
			wantKind:   "minimax",
			wantSource: provideridentity.SourceCompatConfig,
		},
		{
			name: "stale URL-derived attribute does not override current URL",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"compat_kind":                        "minimax",
				provideridentity.KindSourceAttribute: string(provideridentity.SourceBaseURL),
				"base_url":                           "https://example.com/v1",
			}},
			wantSource: provideridentity.SourceGeneric,
		},
		{
			name: "base URL fallback",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": "https://api.deepseek.com/v1",
			}},
			wantKind:   "deepseek",
			wantSource: provideridentity.SourceBaseURL,
		},
		{
			name: "generic fallback",
			auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"provider_key": "pool",
				"compat_name":  "pool",
				"base_url":     "https://example.com/v1",
			}},
			wantSource: provideridentity.SourceGeneric,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := NewOpenAICompatExecutor("test-provider", tt.cfg)
			profile := executor.resolveProfile(tt.auth)
			if profile.Kind != tt.wantKind {
				t.Fatalf("profile.Kind = %q, want %q", profile.Kind, tt.wantKind)
			}
			if profile.KindSource != tt.wantSource {
				t.Fatalf("profile.KindSource = %q, want %q", profile.KindSource, tt.wantSource)
			}
		})
	}
}
