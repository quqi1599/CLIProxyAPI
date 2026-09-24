package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	gptLargeToolHistoryMultiplier          = 3
	gptLargeToolHistoryMessages            = gptLargeToolHistoryMultiplier * 100
	gptLargeToolHistoryTools               = gptLargeToolHistoryMultiplier * 40
	gptWorkBuddyToolHistoryMessages        = 240
	gptWorkBuddyToolHistoryInteractions    = 8
	gptWorkBuddyToolHistoryDeclaredTools   = 8
	gptLargeToolHistoryMaxRetryCredentials = 5 // Six total attempts: initial credential plus five fallbacks.
)

type gptLargeToolHistoryFallbackGuard struct {
	enabled             bool
	attemptedRouteGroup map[string]struct{}
}

func newGPTLargeToolHistoryFallbackGuard(providers []string, routeModel string, opts cliproxyexecutor.Options) *gptLargeToolHistoryFallbackGuard {
	if !isGPTLargeToolHistoryResponsesRequest(providers, routeModel, opts) {
		return nil
	}
	return &gptLargeToolHistoryFallbackGuard{
		enabled:             true,
		attemptedRouteGroup: make(map[string]struct{}),
	}
}

func (g *gptLargeToolHistoryFallbackGuard) effectiveMaxRetryCredentials(current int) int {
	if g == nil || !g.enabled {
		return current
	}
	if current == 0 || current > gptLargeToolHistoryMaxRetryCredentials {
		return gptLargeToolHistoryMaxRetryCredentials
	}
	return current
}

func (g *gptLargeToolHistoryFallbackGuard) shouldSkipAuth(auth *Auth) bool {
	if g == nil || !g.enabled || !isCodexAuth(auth) {
		return false
	}
	group := explicitAuthRoutingGroup(auth)
	if group == "" {
		return false
	}
	_, ok := g.attemptedRouteGroup[group]
	return ok
}

func (g *gptLargeToolHistoryFallbackGuard) markAuth(auth *Auth) {
	if g == nil || !g.enabled || !isCodexAuth(auth) {
		return
	}
	group := explicitAuthRoutingGroup(auth)
	if group == "" {
		return
	}
	g.attemptedRouteGroup[group] = struct{}{}
}

func isGPTLargeToolHistoryResponsesRequest(providers []string, routeModel string, opts cliproxyexecutor.Options) bool {
	if len(providers) != 1 || !isCodexProviderName(providers[0]) {
		return false
	}
	if !isGPTLargeToolHistoryResponsesModel(requestedModelAliasFromOptions(opts, routeModel)) &&
		!isGPTLargeToolHistoryResponsesModel(routeModel) {
		return false
	}
	if !isResponsesEndpointPath(metadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)) {
		return false
	}
	shape := requestShapeFromOptions(opts)
	if shape.MessageCount >= gptLargeToolHistoryMessages || shape.ToolCount >= gptLargeToolHistoryTools {
		return true
	}
	profile := strings.ToLower(strings.TrimSpace(metadataString(opts.Metadata, cliproxyexecutor.ClientProfileMetadataKey)))
	if profile != "workbuddy" && profile != "codex" && profile != "codex_cli" && profile != "codex_tui" {
		return false
	}
	if shape.MessageCount < gptWorkBuddyToolHistoryMessages {
		return false
	}
	toolInteractions := intMetadataValue(opts.Metadata[cliproxyexecutor.ToolInteractionCountMetadataKey])
	declaredTools := intMetadataValue(opts.Metadata[cliproxyexecutor.DeclaredToolCountMetadataKey])
	return toolInteractions >= gptWorkBuddyToolHistoryInteractions ||
		declaredTools >= gptWorkBuddyToolHistoryDeclaredTools ||
		shape.ToolCount >= gptWorkBuddyToolHistoryInteractions
}

func isGPTLargeToolHistoryResponsesModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	switch model {
	case "gpt-5.5", "gpt-5.4", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra":
		return true
	default:
		return strings.HasPrefix(model, "gpt-6-")
	}
}

// preferGPTNativeResponsesAuths filters by request compatibility, not by
// credential origin or health. A Codex-only transform rejection excludes Codex
// candidates even if they declare native support. Unknown routes must not become
// a fallback merely because compatible credentials are unavailable or tried.
func preferGPTNativeResponsesAuths(auths []*Auth, providers []string, model string, opts cliproxyexecutor.Options) ([]*Auth, int) {
	if !requiresCodexToolHistoryRouting(providers, opts) {
		return auths, 0
	}
	preflightRejected := codexToolHistoryPreflightError(opts) != nil
	requireNative := requiresNativeResponsesToolHistoryRouting(providers, opts)
	native := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || (isCodexAuth(auth) && (preflightRejected || (requireNative && !SupportsNativeResponses(auth)))) {
			continue
		}
		native = append(native, auth)
	}
	return native, len(auths) - len(native)
}

func requiresCodexToolHistoryRouting(providers []string, opts cliproxyexecutor.Options) bool {
	return codexToolHistoryPreflightError(opts) != nil || requiresNativeResponsesToolHistoryRouting(providers, opts)
}

func requiresNativeResponsesToolHistoryRouting(providers []string, opts cliproxyexecutor.Options) bool {
	if opts.TokenCount {
		return false
	}
	for _, provider := range providers {
		if isCodexProviderName(provider) {
			return RequiresNativeResponsesToolHistory(opts.Metadata, opts.OriginalRequest)
		}
	}
	return false
}

func nativeResponsesToolHistorySelectionError() error {
	return &Error{
		Code:       "request_feature_unsupported",
		Message:    "codex_tool_history_too_large: no configured route is verified to support this long tool history. Start a new conversation, summarize tool history, or configure native-responses on a verified compatible route.",
		HTTPStatus: http.StatusBadRequest,
	}
}

// SupportsNativeResponses is the shared dispatch/execution capability contract.
// Explicit declarations override defaults, including false on official routes.
// A custom endpoint is unknown until its native Responses/tool-history support
// has been verified and declared; websockets alone do not prove that support.
func SupportsNativeResponses(auth *Auth) bool {
	if auth == nil || !isCodexAuth(auth) {
		return false
	}
	if auth.Attributes != nil {
		declaration := strings.ToLower(strings.TrimSpace(auth.Attributes["native_responses"]))
		switch declaration {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
		if declaration != "" {
			return false
		}
	}
	baseURL := ""
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if baseURL == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Hostname())) {
	case "api.openai.com", "chatgpt.com", "chat.openai.com":
		return true
	default:
		return false
	}
}

func isResponsesEndpointPath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	if idx := strings.Index(path, " "); idx >= 0 {
		path = strings.TrimSpace(path[idx+1:])
	}
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = strings.TrimSpace(path[:idx])
	}
	return path == "/v1/responses"
}

func metadataString(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func explicitAuthRoutingGroup(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	for _, key := range []string{"routing_group", "routing-group"} {
		if value := normalizeRoutingGroupKey(auth.Attributes[key]); value != "" {
			return value
		}
	}
	return ""
}

func contextWithSelectedAuthRoutingGroup(ctx context.Context, auth *Auth) context.Context {
	if auth == nil {
		return ctx
	}
	return coreusage.WithRoutingGroup(ctx, authRoutingGroup(auth))
}
