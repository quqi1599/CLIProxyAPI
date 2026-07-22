package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/provideridentity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestOpenAICompatResolveProfileUsesProviderIdentityPrecedence(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		auth *cliproxyauth.Auth
		want provideridentity.Identity
	}{
		{
			name: "explicit config wins",
			cfg: &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "configured-route", Kind: "kimi", BaseURL: "https://api.minimaxi.com/v1"},
			}},
			auth: &cliproxyauth.Auth{
				Provider: "openai-compatible-configured-route",
				Attributes: map[string]string{
					"provider_family": "openai-compatibility",
					"compat_name":     "configured-route",
					"compat_kind":     "minimax",
					"base_url":        "https://api.deepseek.com/v1",
				},
			},
			want: provideridentity.Identity{
				CanonicalProvider: "kimi",
				ExecutorKey:       "openai-compatible-configured-route",
				ProviderFamily:    "openai-compatibility",
				CompatName:        "configured-route",
				Kind:              "kimi",
				Source:            provideridentity.SourceCompatConfig,
				BaseHost:          "api.deepseek.com",
			},
		},
		{
			name: "auth attribute wins over URL",
			auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"compat_kind": "minimax",
				"base_url":    "https://api.deepseek.com/v1",
			}},
			want: provideridentity.Identity{CanonicalProvider: "minimax", ExecutorKey: "openai-compatibility", Kind: "minimax", Source: provideridentity.SourceAttribute, BaseHost: "api.deepseek.com"},
		},
		{
			name: "config base URL fallback",
			cfg: &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "configured-route", BaseURL: "https://api.deepseek.com/v1"},
			}},
			auth: &cliproxyauth.Auth{Provider: "openai-compatible-configured-route", Attributes: map[string]string{
				"compat_name": "configured-route",
			}},
			want: provideridentity.Identity{CanonicalProvider: "deepseek", ExecutorKey: "openai-compatible-configured-route", CompatName: "configured-route", Kind: "deepseek", Source: provideridentity.SourceBaseURL, BaseHost: "api.deepseek.com"},
		},
		{
			name: "persisted explicit source stays aligned with conductor",
			auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"compat_kind":                        "minimax",
				provideridentity.KindSourceAttribute: string(provideridentity.SourceCompatConfig),
				"base_url":                           "https://api.deepseek.com/v1",
			}},
			want: provideridentity.Identity{CanonicalProvider: "minimax", ExecutorKey: "openai-compatibility", Kind: "minimax", Source: provideridentity.SourceCompatConfig, BaseHost: "api.deepseek.com"},
		},
		{
			name: "stale URL-derived attribute does not override current URL",
			auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"compat_kind":                        "minimax",
				provideridentity.KindSourceAttribute: string(provideridentity.SourceBaseURL),
				"base_url":                           "https://example.com/v1",
			}},
			want: provideridentity.Identity{CanonicalProvider: "openai-compatibility", ExecutorKey: "openai-compatibility", Source: provideridentity.SourceDefault, BaseHost: "example.com"},
		},
		{
			name: "base URL fallback",
			auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"base_url": "https://api.deepseek.com/v1",
			}},
			want: provideridentity.Identity{CanonicalProvider: "deepseek", ExecutorKey: "openai-compatibility", Kind: "deepseek", Source: provideridentity.SourceBaseURL, BaseHost: "api.deepseek.com"},
		},
		{
			name: "legacy compat kind uses shared adapter",
			auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"compat-kind": "minimax",
				"base_url":    "https://api.deepseek.com/v1",
			}},
			want: provideridentity.Identity{CanonicalProvider: "minimax", ExecutorKey: "openai-compatibility", Kind: "minimax", Source: provideridentity.SourceAttribute, BaseHost: "api.deepseek.com"},
		},
		{
			name: "generic fallback",
			auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"provider_key": "pool",
				"compat_name":  "pool",
				"base_url":     "https://example.com/v1",
			}},
			want: provideridentity.Identity{CanonicalProvider: "openai-compatibility", ExecutorKey: "pool", CompatName: "pool", Source: provideridentity.SourceDefault, BaseHost: "example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := NewOpenAICompatExecutor("test-provider", tt.cfg)
			profile := executor.resolveProfile(tt.auth)
			if profile.Kind != tt.want.Kind {
				t.Fatalf("profile.Kind = %q, want %q", profile.Kind, tt.want.Kind)
			}
			if profile.Identity != tt.want {
				t.Fatalf("profile.Identity = %+v, want %+v", profile.Identity, tt.want)
			}
			if profile.Kind != profile.Identity.Kind {
				t.Fatalf("profile kind/identity kind = %q/%q", profile.Kind, profile.Identity.Kind)
			}
			if profile.Kind != "" && openAICompatKindSource(profile) != string(profile.Identity.Source) {
				t.Fatalf("openAICompatKindSource() = %q, want %q", openAICompatKindSource(profile), profile.Identity.Source)
			}
		})
	}
}

func TestOpenAICompatKindSourceStaticProfileUsesConfigSource(t *testing.T) {
	if got := openAICompatKindSource(openAICompatProfileForKind("kimi")); got != string(provideridentity.SourceCompatConfig) {
		t.Fatalf("openAICompatKindSource() = %q, want %q", got, provideridentity.SourceCompatConfig)
	}
}
