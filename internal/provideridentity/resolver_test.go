package provideridentity

import "testing"

func TestInputFromAttributes(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		attributes map[string]string
		want       Input
	}{
		{
			name:     "nil attributes",
			provider: "claude",
			want:     Input{Provider: "claude"},
		},
		{
			name:     "canonical attributes",
			provider: "openai-compatibility",
			attributes: map[string]string{
				"provider_key":      "configured-route",
				"provider_family":   "openai-compatibility",
				"compat_name":       "Configured Route",
				"compat_kind":       "MiniMax",
				KindSourceAttribute: string(SourceCompatConfig),
				"base_url":          "https://api.deepseek.com/v1",
			},
			want: Input{
				Provider:        "openai-compatibility",
				ProviderKey:     "configured-route",
				ProviderFamily:  "openai-compatibility",
				CompatName:      "Configured Route",
				AttributeKind:   "MiniMax",
				AttributeSource: SourceCompatConfig,
				BaseURL:         "https://api.deepseek.com/v1",
			},
		},
		{
			name: "legacy compat kind",
			attributes: map[string]string{
				"compat-kind": "MiniMax",
			},
			want: Input{AttributeKind: "MiniMax"},
		},
		{
			name: "canonical compat kind wins",
			attributes: map[string]string{
				"compat_kind": " Kimi ",
				"compat-kind": "MiniMax",
			},
			want: Input{AttributeKind: "Kimi"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InputFromAttributes(tt.provider, tt.attributes); got != tt.want {
				t.Fatalf("InputFromAttributes() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestInputFromAttributesReResolvesURLDerivedKind(t *testing.T) {
	input := InputFromAttributes("openai-compatibility", map[string]string{
		"provider_key":      "configured-route",
		"compat_kind":       "minimax",
		KindSourceAttribute: string(SourceBaseURL),
		"base_url":          "https://api.deepseek.com/v1",
	})
	identity := Resolve(input)
	if identity.Kind != "deepseek" || identity.Source != SourceBaseURL || identity.ExecutorKey != "configured-route" {
		t.Fatalf("Resolve(InputFromAttributes()) = %+v", identity)
	}
}

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name  string
		input Input
		want  Identity
	}{
		{
			name: "explicit config wins",
			input: Input{
				Provider:      "openai-compatibility",
				ProviderKey:   "openai-compatible-qwen-route",
				ExplicitKind:  " KIMI ",
				AttributeKind: "minimax",
				BaseURL:       "https://api.deepseek.com/v1",
			},
			want: Identity{CanonicalProvider: "kimi", ExecutorKey: "openai-compatible-qwen-route", Kind: "kimi", Source: SourceCompatConfig, BaseHost: "api.deepseek.com"},
		},
		{
			name: "attribute wins over URL",
			input: Input{
				AttributeKind: " MiniMax ",
				BaseURL:       "https://api.deepseek.com/v1",
			},
			want: Identity{CanonicalProvider: "minimax", ExecutorKey: "minimax", Kind: "minimax", Source: SourceAttribute, BaseHost: "api.deepseek.com"},
		},
		{
			name: "persisted URL source is re-resolved",
			input: Input{
				ProviderKey:     "openai-compatible-route",
				AttributeKind:   "minimax",
				AttributeSource: SourceBaseURL,
				BaseURL:         "https://api.deepseek.com/v1",
			},
			want: Identity{CanonicalProvider: "deepseek", ExecutorKey: "openai-compatible-route", Kind: "deepseek", Source: SourceBaseURL, BaseHost: "api.deepseek.com"},
		},
		{
			name: "stale persisted URL kind is not reused",
			input: Input{
				Provider:        "openai-compatibility",
				ProviderKey:     "openai-compatible-route",
				AttributeKind:   "minimax",
				AttributeSource: SourceBaseURL,
				BaseURL:         "https://example.com/v1",
			},
			want: Identity{CanonicalProvider: "openai-compatibility", ExecutorKey: "openai-compatible-route", Source: SourceDefault, BaseHost: "example.com"},
		},
		{
			name:  "URL inference",
			input: Input{BaseURL: "https://API.DEEPSEEK.COM:443/v1/chat/completions"},
			want:  Identity{CanonicalProvider: "deepseek", ExecutorKey: "deepseek", Kind: "deepseek", Source: SourceBaseURL, BaseHost: "api.deepseek.com"},
		},
		{
			name:  "generic",
			input: Input{BaseURL: "https://example.com/v1"},
			want:  Identity{Source: SourceGeneric, BaseHost: "example.com"},
		},
		{
			name:  "invalid URL keeps explicit identity",
			input: Input{ExplicitKind: "QWEN", BaseURL: "://bad"},
			want:  Identity{CanonicalProvider: "qwen", ExecutorKey: "qwen", Kind: "qwen", Source: SourceCompatConfig},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.input); got != tt.want {
				t.Fatalf("Resolve(%+v) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveCarriesCanonicalRouteMetadata(t *testing.T) {
	identity := Resolve(Input{
		Provider:        "openai-compatible-configured-route",
		ProviderKey:     "openai-compatible-configured-route",
		ProviderFamily:  " OpenAI-Compatibility ",
		CompatName:      " Configured Route ",
		AttributeKind:   " KIMI ",
		AttributeSource: SourceCompatConfig,
		BaseURL:         "https://api.deepseek.com/v1",
	})
	want := Identity{
		CanonicalProvider: "kimi",
		ExecutorKey:       "openai-compatible-configured-route",
		ProviderFamily:    "openai-compatibility",
		CompatName:        "Configured Route",
		Kind:              "kimi",
		Source:            SourceCompatConfig,
		BaseHost:          "api.deepseek.com",
	}
	if identity != want {
		t.Fatalf("Resolve() = %+v, want %+v", identity, want)
	}
}

func TestResolvePreservesFallbackBehavior(t *testing.T) {
	tests := []struct {
		name  string
		input Input
		want  Identity
	}{
		{
			name:  "provider key stays executor metadata",
			input: Input{Provider: "openai-compatibility", ProviderKey: "custom-route"},
			want:  Identity{CanonicalProvider: "openai-compatibility", ExecutorKey: "custom-route", Source: SourceDefault},
		},
		{
			name:  "compat name stays diagnostic metadata",
			input: Input{Provider: "openai-compatibility", ProviderKey: "pool", CompatName: "pool"},
			want:  Identity{CanonicalProvider: "openai-compatibility", ExecutorKey: "pool", CompatName: "pool", Source: SourceDefault},
		},
		{
			name:  "compat name cannot create a provider identity",
			input: Input{CompatName: "pool"},
			want:  Identity{CompatName: "pool", Source: SourceGeneric},
		},
		{
			name:  "legacy provider key does not become compatibility identity",
			input: Input{Provider: "openai-compatibility", ProviderKey: "deepseek"},
			want:  Identity{CanonicalProvider: "openai-compatibility", ExecutorKey: "deepseek", Source: SourceDefault},
		},
		{
			name:  "non compatibility provider",
			input: Input{Provider: " Claude "},
			want:  Identity{CanonicalProvider: "claude", ExecutorKey: "claude", Source: SourceDefault},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.input); got != tt.want {
				t.Fatalf("Resolve(%+v) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveBaseURLIdentity(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "deepseek openai", baseURL: "https://api.deepseek.com/v1", want: "deepseek"},
		{name: "deepseek anthropic", baseURL: "https://api.deepseek.com/anthropic/v1/messages", want: "deepseek"},
		{name: "deepseek root", baseURL: "https://api.deepseek.com", want: "deepseek"},
		{name: "deepseek unrelated path keeps identity", baseURL: "https://api.deepseek.com/dashboard", want: "deepseek"},
		{name: "minimax openai", baseURL: "https://api.minimaxi.com/v1", want: "minimax"},
		{name: "minimax anthropic", baseURL: "https://api.minimax.io/anthropic", want: "minimax"},
		{name: "minimax root", baseURL: "https://api.minimaxi.com", want: "minimax"},
		{name: "minimax unrelated path keeps identity", baseURL: "https://api.minimaxi.com/account", want: "minimax"},
		{name: "kimi moonshot", baseURL: "https://api.moonshot.ai/v1", want: "kimi"},
		{name: "kimi coding", baseURL: "https://api.kimi.com/coding/v1", want: "kimi"},
		{name: "zhipu", baseURL: "https://open.bigmodel.cn/api/coding/paas/v4", want: "zhipu"},
		{name: "xfyun", baseURL: "https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic", want: "xfyun"},
		{name: "xiaomi", baseURL: "https://token-plan-sgp.xiaomimimo.com/v1", want: "xiaomi"},
		{name: "qwen", baseURL: "https://workspace.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions", want: "qwen"},
		{name: "doubao", baseURL: "https://ark.cn-beijing.volces.com/api/v3", want: "doubao"},
		{name: "qianfan", baseURL: "https://qianfan.baidubce.com/v2/coding", want: "qianfan"},
		{name: "step", baseURL: "https://api.stepfun.com/step_plan", want: "step"},
		{name: "unknown", baseURL: "https://example.com/v1", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := Resolve(Input{BaseURL: tt.baseURL})
			if identity.Kind != tt.want {
				t.Fatalf("Resolve(%q).Kind = %q, want %q", tt.baseURL, identity.Kind, tt.want)
			}
			wantSource := SourceGeneric
			if tt.want != "" {
				wantSource = SourceBaseURL
			}
			if identity.Source != wantSource {
				t.Fatalf("Resolve(%q).Source = %q, want %q", tt.baseURL, identity.Source, wantSource)
			}
		})
	}
}

func TestInferEndpointKindKeepsLegacyPathSemantics(t *testing.T) {
	tests := []struct {
		baseURL string
		want    string
	}{
		{baseURL: "https://api.deepseek.com/anthropic", want: "deepseek"},
		{baseURL: "https://api.deepseek.com/v1", want: ""},
		{baseURL: "https://api.minimaxi.com/anthropic", want: "minimax"},
		{baseURL: "https://api.minimaxi.com/v1", want: ""},
		{baseURL: "https://api.kimi.com/coding", want: "kimi"},
		{baseURL: "https://api.kimi.com/v1", want: ""},
		{baseURL: "https://api.moonshot.ai/v1", want: ""},
		{baseURL: "https://ark.cn-beijing.volces.com/api/v3", want: "doubao"},
		{baseURL: "https://ark.cn-beijing.volces.com", want: ""},
	}

	for _, tt := range tests {
		if got := InferEndpointKind(tt.baseURL); got != tt.want {
			t.Fatalf("InferEndpointKind(%q) = %q, want %q", tt.baseURL, got, tt.want)
		}
	}
}
