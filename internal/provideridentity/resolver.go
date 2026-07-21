// Package provideridentity resolves provider compatibility identity from stable inputs.
package provideridentity

import (
	"net/url"
	"strings"
)

// Source identifies the input that determined a compatibility kind.
type Source string

const (
	SourceCompatConfig Source = "compat_config"
	SourceAttribute    Source = "auth_attribute:compat_kind"
	SourceBaseURL      Source = "base_url_inference"
	SourceDefault      Source = "default"
	SourceGeneric      Source = "generic"
)

// KindSourceAttribute is the auth attribute that preserves compatibility
// identity provenance across configuration synthesis and request execution.
const KindSourceAttribute = "compat_kind_source"

// Input contains the values used to resolve a provider compatibility identity.
type Input struct {
	Provider        string
	ProviderKey     string
	ProviderFamily  string
	CompatName      string
	ExplicitKind    string
	AttributeKind   string
	AttributeSource Source
	BaseURL         string
}

// Identity keeps the canonical provider separate from compatibility metadata.
// An empty Kind keeps native/default provider metadata separate from compatibility identity.
type Identity struct {
	CanonicalProvider string
	ExecutorKey       string
	ProviderFamily    string
	CompatName        string
	Kind              string
	Source            Source
	BaseHost          string
}

// Resolve returns one deterministic compatibility identity. Explicit configuration
// wins over an auth attribute, which wins over base URL inference.
func Resolve(input Input) Identity {
	host, path := parseBaseURL(input.BaseURL)
	provider := NormalizeKind(input.Provider)
	providerKey := NormalizeKind(input.ProviderKey)
	identity := Identity{
		ExecutorKey:    firstNonEmpty(providerKey, provider),
		ProviderFamily: NormalizeKind(input.ProviderFamily),
		CompatName:     strings.TrimSpace(input.CompatName),
		BaseHost:       host,
	}
	if kind := NormalizeKind(input.ExplicitKind); kind != "" {
		return resolveKind(identity, kind, SourceCompatConfig)
	}
	attributeKind := NormalizeKind(input.AttributeKind)
	attributeSource := normalizeAttributeSource(input.AttributeSource)
	if attributeKind != "" && attributeSource != SourceBaseURL {
		return resolveKind(identity, attributeKind, attributeSource)
	}
	if kind := inferIdentityKind(host, path); kind != "" {
		return resolveKind(identity, kind, SourceBaseURL)
	}
	identity.CanonicalProvider = provider
	if identity.ExecutorKey == "" {
		identity.ExecutorKey = identity.CanonicalProvider
	}
	if identity.CanonicalProvider == "" {
		identity.Source = SourceGeneric
	} else {
		identity.Source = SourceDefault
	}
	return identity
}

func resolveKind(identity Identity, kind string, source Source) Identity {
	identity.CanonicalProvider = kind
	identity.Kind = kind
	identity.Source = source
	if identity.ExecutorKey == "" {
		identity.ExecutorKey = kind
	}
	return identity
}

func normalizeAttributeSource(source Source) Source {
	switch source {
	case SourceCompatConfig, SourceAttribute, SourceBaseURL:
		return source
	default:
		return SourceAttribute
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// NormalizeKind canonicalizes a compatibility kind for matching and logging.
func NormalizeKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

// IsXiaomiTokenPlanHost reports whether host is an official MiMo Token Plan host.
func IsXiaomiTokenPlanHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return strings.HasSuffix(host, ".xiaomimimo.com") &&
		(host == "token-plan.xiaomimimo.com" || strings.HasPrefix(host, "token-plan-"))
}

func parseBaseURL(rawBaseURL string) (host, path string) {
	baseURL := strings.TrimSpace(rawBaseURL)
	if baseURL == "" {
		return "", ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", ""
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname())), strings.ToLower(strings.TrimSpace(parsed.Path))
}

// InferEndpointKind preserves the existing path-aware compatibility detection
// used when deciding whether a configured endpoint belongs to a supported
// provider protocol. Identity resolution is deliberately broader: a known
// official host keeps its identity even when the endpoint path is empty.
func InferEndpointKind(rawBaseURL string) string {
	host, path := parseBaseURL(rawBaseURL)
	return inferEndpointKind(host, path)
}

func inferIdentityKind(host, path string) string {
	switch host {
	case "api.moonshot.ai", "api.moonshot.cn", "api.kimi.com":
		return "kimi"
	case "api.minimaxi.com", "api.minimaxi.io", "api.minimax.io":
		return "minimax"
	case "open.bigmodel.cn", "maas-api.lanyun.net", "api.z.ai":
		return "zhipu"
	case "api.deepseek.com":
		return "deepseek"
	case "api.xiaomimimo.com":
		return "xiaomi"
	case "ark.cn-beijing.volces.com":
		return "doubao"
	case "maas-coding-api.cn-huabei-1.xf-yun.com":
		return "xfyun"
	case "qianfan.baidubce.com":
		return "qianfan"
	case "api.stepfun.com":
		return "step"
	case "coding.dashscope.aliyuncs.com":
		return "qwen"
	default:
		if IsXiaomiTokenPlanHost(host) {
			return "xiaomi"
		}
		if strings.HasSuffix(host, ".maas.aliyuncs.com") && pathMatches(path, "/apps/anthropic", "/compatible-mode/v1") {
			return "qwen"
		}
		return ""
	}
}

func inferEndpointKind(host, path string) string {
	switch host {
	case "api.deepseek.com":
		if pathMatches(path, "/anthropic") {
			return "deepseek"
		}
	case "api.minimaxi.com", "api.minimaxi.io", "api.minimax.io":
		if pathMatches(path, "/anthropic") {
			return "minimax"
		}
	case "api.kimi.com":
		if pathMatches(path, "/coding") {
			return "kimi"
		}
	case "open.bigmodel.cn", "maas-api.lanyun.net", "api.z.ai":
		if pathMatches(path, "/api/anthropic", "/anthropic", "/api/coding/paas/v4", "/api/paas/v4") {
			return "zhipu"
		}
	case "maas-coding-api.cn-huabei-1.xf-yun.com":
		if pathMatches(path, "/anthropic") {
			return "xfyun"
		}
	case "token-plan-cn.xiaomimimo.com", "api.xiaomimimo.com":
		if pathMatches(path, "/anthropic", "/v1") {
			return "xiaomi"
		}
	case "coding.dashscope.aliyuncs.com":
		if pathMatches(path, "/apps/anthropic") {
			return "qwen"
		}
	case "ark.cn-beijing.volces.com":
		if pathMatches(path, "/api/coding", "/api/v3") {
			return "doubao"
		}
	case "qianfan.baidubce.com":
		if pathMatches(path, "/anthropic/coding", "/v2/coding") {
			return "qianfan"
		}
	case "api.stepfun.com":
		if pathMatches(path, "/step_plan") {
			return "step"
		}
	}

	if strings.HasSuffix(host, ".maas.aliyuncs.com") && pathMatches(path, "/apps/anthropic", "/compatible-mode/v1") {
		return "qwen"
	}
	if IsXiaomiTokenPlanHost(host) && pathMatches(path, "/anthropic", "/v1") {
		return "xiaomi"
	}
	return ""
}

func pathMatches(path string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
