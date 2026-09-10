package auth

import (
	"context"
	"strings"

	compathistory "github.com/router-for-me/CLIProxyAPI/v7/internal/compat/history"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	claudeSonnet46MiMoHistoryFilterReason         = "mimo_incomplete_reasoning_history"
	claudeSonnet46MiMoHistoryIncompatibleMetadata = "__cliproxy_claude_sonnet46_mimo_history_incompatible"
)

// preferClaudeSonnet46HistoryCompatibleAuths removes Xiaomi MiMo routes that
// cannot safely continue the aggregate model's current thinking history. When
// every candidate has the same limitation, the original set is retained so
// the executor can return the existing actionable MiMo validation error.
func (m *Manager) preferClaudeSonnet46HistoryCompatibleAuths(ctx context.Context, auths []*Auth, routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]*Auth, int) {
	if len(auths) < 2 || !claudeSonnet46MiMoHistoryIncompatible(routeModel, req, opts) {
		return auths, 0
	}

	compatible := make([]*Auth, 0, len(auths))
	excluded := 0
	for _, auth := range auths {
		if m.claudeSonnet46AuthOnlyRoutesToMiMo(auth, routeModel, opts) {
			excluded++
			continue
		}
		compatible = append(compatible, auth)
	}
	if excluded == 0 || len(compatible) == 0 {
		return auths, 0
	}

	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"event":                    "route_compatibility_filter",
		"requested_model":          requestedModelAliasFromOptions(opts, routeModel),
		"compat_kind":              "xiaomi",
		"reason":                   claudeSonnet46MiMoHistoryFilterReason,
		"excluded_candidate_count": excluded,
	}).Info("auth selection: excluded MiMo candidates incompatible with aggregate model history")
	return compatible, excluded
}

// filterClaudeSonnet46MiMoExecutionModels handles a provider-local alias pool.
// It removes MiMo only when another mapped model remains on the same auth.
func filterClaudeSonnet46MiMoExecutionModels(auth *Auth, routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, candidates []string) []string {
	if len(candidates) < 2 || routePlanProviderIdentity(auth, "").CanonicalProvider != "xiaomi" ||
		!claudeSonnet46MiMoHistoryIncompatible(routeModel, req, opts) {
		return candidates
	}

	filtered := make([]string, 0, len(candidates))
	removed := false
	for _, candidate := range candidates {
		if isXiaomiMiMoV25RouteModel(candidate) {
			removed = true
			continue
		}
		filtered = append(filtered, candidate)
	}
	if !removed || len(filtered) == 0 {
		return candidates
	}
	return filtered
}

func (m *Manager) claudeSonnet46AuthOnlyRoutesToMiMo(auth *Auth, routeModel string, opts cliproxyexecutor.Options) bool {
	if m == nil || auth == nil || routePlanProviderIdentity(auth, "").CanonicalProvider != "xiaomi" {
		return false
	}

	models := m.configuredExecutionModelsForCompatibility(auth, requestedModelAliasFromOptions(opts, routeModel))
	if len(models) == 0 {
		return false
	}
	hasMiMo := false
	for _, model := range models {
		if isXiaomiMiMoV25RouteModel(model) {
			hasMiMo = true
			continue
		}
		return false
	}
	return hasMiMo
}

// configuredExecutionModelsForCompatibility mirrors alias resolution without
// rotating a model pool or mutating scheduler state.
func (m *Manager) configuredExecutionModelsForCompatibility(auth *Auth, routeModel string) []string {
	if m == nil || auth == nil {
		return nil
	}
	if auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			return []string{homeModel}
		}
	}

	requestedModel := rewriteModelForAuth(routeModel, auth)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	if requestedModel == "" {
		return nil
	}
	if pool := m.resolveOAuthUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		return pool
	}
	requestedModel = m.applyOAuthModelAlias(auth, requestedModel)
	if pool := m.resolveAPIKeyUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		return pool
	}
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		return pool
	}
	resolved := strings.TrimSpace(m.applyAPIKeyModelAlias(auth, requestedModel))
	if resolved == "" || strings.EqualFold(resolved, routeModel) {
		return nil
	}
	return []string{resolved}
}

func claudeSonnet46MiMoHistoryIncompatible(routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	if incompatible, ok := opts.Metadata[claudeSonnet46MiMoHistoryIncompatibleMetadata].(bool); ok {
		return incompatible
	}
	requestedModel := requestedModelAliasFromOptions(opts, routeModel)
	if !isClaudeSonnet46FallbackModel(requestedModel) {
		return false
	}
	payload := opts.OriginalRequest
	if len(payload) == 0 {
		payload = req.Payload
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) || !aggregateThinkingExplicitlyEnabled(payload, requestedModel, opts) {
		return false
	}

	formats := []compathistory.Format{compathistory.FormatOpenAI, compathistory.FormatClaude}
	switch opts.SourceFormat {
	case sdktranslator.FormatOpenAI:
		formats = formats[:1]
	case sdktranslator.FormatClaude:
		formats = formats[1:]
	}
	for _, format := range formats {
		if _, err := compathistory.Validate(payload, format, true); err != nil {
			return true
		}
	}
	return false
}

func classifyClaudeSonnet46MiMoHistory(routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}
	if _, ok := opts.Metadata[claudeSonnet46MiMoHistoryIncompatibleMetadata].(bool); ok {
		return opts
	}
	opts.Metadata[claudeSonnet46MiMoHistoryIncompatibleMetadata] = claudeSonnet46MiMoHistoryIncompatible(routeModel, req, opts)
	return opts
}

func aggregateThinkingExplicitlyEnabled(payload []byte, requestedModel string, opts cliproxyexecutor.Options) bool {
	effort := strings.ToLower(strings.TrimSpace(thinking.ExtractReasoningEffort(payload, opts.SourceFormat.String(), requestedModel)))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(metadataString(opts.Metadata, cliproxyexecutor.ReasoningEffortOriginalMetadataKey)))
	}
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(reasoningEffortFromOptions(opts)))
	}
	if effort != "" {
		switch effort {
		case "none", "disabled", "disable", "off", "false":
			return false
		default:
			return true
		}
	}
	return gjson.GetBytes(payload, "thinking_budget").Exists() ||
		gjson.GetBytes(payload, "thinking.budget_tokens").Exists()
}

func isXiaomiMiMoV25RouteModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = model[slash+1:]
	}
	return model == "mimo-v2.5" || strings.HasPrefix(model, "mimo-v2.5-pro")
}
