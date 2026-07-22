package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

const (
	openAICompatKimiPolicyID                 = "openai_compat.kimi.model_quirks"
	openAICompatMiniMaxPolicyID              = "openai_compat.minimax.request_quirks"
	openAICompatQwen38PolicyID               = "openai_compat.qwen38.thinking"
	openAICompatPostConfigRevalidatePolicyID = "openai_compat.post_config_revalidate"
	openAICompatPostConfigRevalidateStage    = "compat/" + string(compat.PostConfigRevalidate)
	openAICompatProviderPreQuirkStage        = "request_plan.openai_compat.provider_pre_quirk_scrub"
	openAICompatProviderPostQuirkStage       = "request_plan.openai_compat.provider_post_quirk_scrub"
	openAICompatProviderPreQuirkPolicy       = "openai_compat.provider_pre_quirk_scrub"
	openAICompatProviderPostQuirkPolicy      = "openai_compat.provider_post_quirk_scrub"

	openAICompatKimiToolChoiceDowngrade = "openai_compat.kimi.tool_choice_normalized"
	openAICompatKimiWebSearchDowngrade  = "openai_compat.kimi.web_search_disables_thinking"
	openAICompatMiniMaxPenaltyDowngrade = "openai_compat.minimax.penalties_removed"
	openAICompatQwen38ThinkingDowngrade = "openai_compat.qwen38.disabled_to_low"
)

var (
	openAICompatPolicyRegistry, openAICompatPolicyRegistryErr = compat.NewRegistry(
		openAICompatKimiPolicy(),
		openAICompatMiniMaxPolicy(),
		openAICompatQwen38Policy(),
	)
	openAICompatPolicyPipeline                                        = compat.NewPipeline(openAICompatPolicyRegistry)
	openAICompatPostConfigRegistry, openAICompatPostConfigRegistryErr = compat.NewRegistry(
		openAICompatPostConfigRevalidatePolicy(),
	)
	openAICompatPostConfigPipeline = compat.NewPipeline(openAICompatPostConfigRegistry)
)

type openAICompatPolicyModelContextKey struct{}
type openAICompatPostConfigContextKey struct{}

type openAICompatPostConfigContext struct {
	profile openAICompatProfile
	model   string
	baseURL string
}

func scrubOpenAICompatPayloadForModelWithPolicies(ctx context.Context, payload []byte, profile openAICompatProfile, model string, baseURL string, match compat.MatchContext) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	preQuirkStarted := time.Now()
	preQuirkInput := payload
	payload = scrubOpenAICompatPayloadBeforeProviderQuirks(payload, profile, model, baseURL)
	if err := helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatProviderPreQuirkStage,
		preQuirkInput,
		payload,
		preQuirkStarted,
		[]string{openAICompatProviderPreQuirkPolicy},
		openAICompatCompatibilityDowngrades(preQuirkInput, payload),
		internalpayload.AmplificationOverride{},
	); err != nil {
		return nil, err
	}
	if openAICompatPolicyRegistryErr != nil {
		return nil, openAICompatPolicyRegistryErr
	}
	ctx = context.WithValue(ctx, openAICompatPolicyModelContextKey{}, model)
	result, err := openAICompatPolicyPipeline.Apply(ctx, openAICompatPolicyMatchContext(profile, payload, model, match), payload)
	if err != nil {
		return nil, err
	}
	postQuirkStarted := time.Now()
	output := scrubOpenAICompatPayloadAfterProviderQuirks(result.Payload, profile, model, baseURL)
	if err = helps.EnforceSemanticTransformStage(
		ctx,
		openAICompatProviderPostQuirkStage,
		result.Payload,
		output,
		postQuirkStarted,
		[]string{openAICompatProviderPostQuirkPolicy},
		openAICompatCompatibilityDowngrades(result.Payload, output),
		internalpayload.AmplificationOverride{},
	); err != nil {
		return nil, err
	}
	return output, nil
}

func openAICompatPolicyMatchContext(profile openAICompatProfile, payload []byte, model string, match compat.MatchContext) compat.MatchContext {
	compatKind := config.NormalizeOpenAICompatibilityKind(profile.Kind)
	matchModel := normalizedOpenAICompatPolicyModelName(model)
	if compatKind == "qwen" && !isQwen38MaxThinkingModel(matchModel) {
		if payloadModel := normalizedOpenAICompatPolicyModelName(gjson.GetBytes(payload, "model").String()); isQwen38MaxThinkingModel(payloadModel) {
			matchModel = payloadModel
		} else {
			matchModel = "__no_qwen38_match__"
		}
	}
	match.ProviderFamily = "openai-compatibility"
	match.CompatKind = compatKind
	match.Model = matchModel
	return match
}

func revalidateOpenAICompatPayloadAfterConfig(ctx context.Context, payload []byte, profile openAICompatProfile, model, baseURL string, match compat.MatchContext) ([]byte, error) {
	if openAICompatPostConfigRegistryErr != nil {
		return nil, openAICompatPostConfigRegistryErr
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, openAICompatPolicyModelContextKey{}, model)
	ctx = context.WithValue(ctx, openAICompatPostConfigContextKey{}, openAICompatPostConfigContext{
		profile: profile,
		model:   model,
		baseURL: baseURL,
	})
	result, err := openAICompatPostConfigPipeline.Apply(ctx, openAICompatPolicyMatchContext(profile, payload, model, match), payload)
	if err != nil {
		return nil, err
	}
	return result.Payload, nil
}

func openAICompatPolicyInventory() (compat.Report, error) {
	if openAICompatPolicyRegistryErr != nil {
		return compat.Report{}, openAICompatPolicyRegistryErr
	}
	if openAICompatPostConfigRegistryErr != nil {
		return compat.Report{}, openAICompatPostConfigRegistryErr
	}
	primary := openAICompatPolicyRegistry.Report()
	postConfig := openAICompatPostConfigRegistry.Report()
	primary.Policies = append(primary.Policies, postConfig.Policies...)
	return primary, nil
}

func openAICompatPostConfigRevalidatePolicy() compat.Policy {
	return compat.Policy{
		ID:    openAICompatPostConfigRevalidatePolicyID,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "openai-compatibility",
		},
		Phase:    compat.PostConfigRevalidate,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: "Remove when user payload configuration cannot reintroduce fields rejected or normalized by OpenAI-compatible providers.",
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: "git:2007a895",
			Fixture:           "internal/runtime/executor/testdata/compat/post_config_revalidate.json",
			UpstreamEvidence:  "Payload overrides can reintroduce provider-incompatible fields after the initial compatibility scrub.",
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.enable_thinking",
			"body.frequency_penalty",
			"body.max_completion_tokens",
			"body.max_output_tokens",
			"body.max_tokens",
			"body.messages",
			"body.metadata",
			"body.n",
			"body.output_config",
			"body.parallel_tool_calls",
			"body.presence_penalty",
			"body.reasoning",
			"body.reasoning_effort",
			"body.response_format",
			"body.service_tier",
			"body.store",
			"body.stream_options",
			"body.temperature",
			"body.thinking",
			"body.thinking_budget",
			"body.tool_choice",
			"body.tools",
			"body.top_p",
			"body.user",
		},
		DowngradeIDs: []string{
			openAICompatMetadataRemovedDowngrade,
			openAICompatStoreRemovedDowngrade,
			openAICompatParallelToolsRemovedDowngrade,
			openAICompatReasoningRemovedDowngrade,
			openAICompatStreamUsageRemovedDowngrade,
			openAICompatKimiToolChoiceDowngrade,
			openAICompatKimiWebSearchDowngrade,
			openAICompatMiniMaxPenaltyDowngrade,
			openAICompatQwen38ThinkingDowngrade,
		},
		Apply: applyOpenAICompatPostConfigRevalidatePolicy,
	}
}

func applyOpenAICompatPostConfigRevalidatePolicy(ctx context.Context, input []byte) (compat.TransformResult, error) {
	state, ok := ctx.Value(openAICompatPostConfigContextKey{}).(openAICompatPostConfigContext)
	if !ok {
		return compat.TransformResult{}, fmt.Errorf("openai compat post-config context is missing")
	}
	output := scrubOpenAICompatPostConfigPayload(input, state.profile, state.model, state.baseURL)
	return compat.TransformResult{
		Payload:    output,
		Downgrades: openAICompatPostConfigDowngrades(ctx, input, output, state.profile),
	}, nil
}

func openAICompatPostConfigDowngrades(ctx context.Context, input, output []byte, profile openAICompatProfile) []string {
	downgrades := appendOpenAICompatDowngrades(nil, openAICompatCompatibilityDowngrades(input, output))
	switch config.NormalizeOpenAICompatibilityKind(profile.Kind) {
	case "kimi":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatKimiPolicyDowngrades(input, output))
	case "minimax":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatMiniMaxPolicyDowngrades(input, output))
	case "qwen":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatQwen38PolicyDowngrades(ctx, input, output))
	}
	return downgrades
}

func appendOpenAICompatDowngrades(existing []string, groups ...[]string) []string {
	for _, group := range groups {
		for _, downgrade := range group {
			seen := false
			for _, current := range existing {
				if current == downgrade {
					seen = true
					break
				}
			}
			if !seen {
				existing = append(existing, downgrade)
			}
		}
	}
	return existing
}

func openAICompatKimiPolicy() compat.Policy {
	return compat.Policy{
		ID:    openAICompatKimiPolicyID,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "openai-compatibility",
			CompatKind:     "kimi",
		},
		Phase:    compat.ProviderQuirkPatch,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: "Remove when supported Kimi models accept canonical OpenAI thinking, sampling, and tool-choice fields.",
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: "git:a6adf256",
			Fixture:           "internal/runtime/executor/testdata/compat/kimi_model_quirks.json",
			UpstreamEvidence:  "Kimi model families require distinct thinking, sampling, and tool-choice payload shapes.",
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.frequency_penalty",
			"body.messages",
			"body.n",
			"body.presence_penalty",
			"body.reasoning",
			"body.reasoning_effort",
			"body.temperature",
			"body.thinking",
			"body.thinking_budget",
			"body.tool_choice",
			"body.top_p",
		},
		DowngradeIDs: []string{
			openAICompatKimiToolChoiceDowngrade,
			openAICompatKimiWebSearchDowngrade,
		},
		Apply: applyOpenAICompatKimiPolicy,
	}
}

func openAICompatMiniMaxPolicy() compat.Policy {
	return compat.Policy{
		ID:    openAICompatMiniMaxPolicyID,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "openai-compatibility",
			CompatKind:     "minimax",
		},
		Phase:    compat.ProviderQuirkPatch,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: "Remove when MiniMax accepts canonical system messages, thinking controls, penalties, and tool arguments.",
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: "git:65b0ea98",
			Fixture:           "internal/runtime/executor/testdata/compat/minimax_request_quirks.json",
			UpstreamEvidence:  "MiniMax rejects OpenAI penalty fields and requires normalized thinking and tool-call arguments.",
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.frequency_penalty",
			"body.messages",
			"body.presence_penalty",
			"body.thinking",
		},
		DowngradeIDs: []string{openAICompatMiniMaxPenaltyDowngrade},
		Apply:        applyOpenAICompatMiniMaxPolicy,
	}
}

func openAICompatQwen38Policy() compat.Policy {
	return compat.Policy{
		ID:    openAICompatQwen38PolicyID,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "openai-compatibility",
			CompatKind:     "qwen",
			ModelPattern:   "qwen3.8-max*",
		},
		Phase:    compat.ProviderQuirkPatch,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: "Remove when Qwen 3.8 Max supports canonical optional thinking controls.",
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: "git:d4254b33",
			Fixture:           "internal/runtime/executor/testdata/compat/qwen38_thinking.json",
			UpstreamEvidence:  "Qwen 3.8 Max requires thinking mode and one canonical effort or budget control.",
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.enable_thinking",
			"body.reasoning",
			"body.reasoning_effort",
			"body.thinking",
			"body.thinking_budget",
		},
		DowngradeIDs: []string{openAICompatQwen38ThinkingDowngrade},
		Apply:        applyOpenAICompatQwen38Policy,
	}
}

func applyOpenAICompatKimiPolicy(ctx context.Context, input []byte) (compat.TransformResult, error) {
	output := scrubKimiPayloadForModel(input, openAICompatPolicyModel(ctx, input))
	return compat.TransformResult{
		Payload:    output,
		Downgrades: openAICompatKimiPolicyDowngrades(input, output),
	}, nil
}

func applyOpenAICompatMiniMaxPolicy(ctx context.Context, input []byte) (compat.TransformResult, error) {
	output := normalizeMiniMaxSystemMessages(input)
	output = normalizeMiniMaxM3Thinking(output, openAICompatPolicyModel(ctx, input))
	output = mutateOpenAICompatJSON(output, []string{"frequency_penalty", "presence_penalty"}, nil)
	output = normalizeOpenAICompatToolCallArguments(output)
	return compat.TransformResult{
		Payload:    output,
		Downgrades: openAICompatMiniMaxPolicyDowngrades(input, output),
	}, nil
}

func applyOpenAICompatQwen38Policy(ctx context.Context, input []byte) (compat.TransformResult, error) {
	output := normalizeQwen38MaxThinking(input, openAICompatPolicyModel(ctx, input))
	return compat.TransformResult{
		Payload:    output,
		Downgrades: openAICompatQwen38PolicyDowngrades(ctx, input, output),
	}, nil
}

func openAICompatPolicyModel(ctx context.Context, payload []byte) string {
	if ctx != nil {
		if model, ok := ctx.Value(openAICompatPolicyModelContextKey{}).(string); ok && model != "" {
			return model
		}
	}
	return gjson.GetBytes(payload, "model").String()
}

func openAICompatKimiPolicyDowngrades(input, output []byte) []string {
	downgrades := make([]string, 0, 2)
	beforeToolChoice := gjson.GetBytes(input, "tool_choice")
	afterToolChoice := gjson.GetBytes(output, "tool_choice")
	if beforeToolChoice.Exists() && beforeToolChoice.Raw != afterToolChoice.Raw {
		downgrades = append(downgrades, openAICompatKimiToolChoiceDowngrade)
	}
	if kimiPayloadHasOfficialWebSearch(input) && kimiThinkingEnabled(input) && !kimiThinkingEnabled(output) {
		downgrades = append(downgrades, openAICompatKimiWebSearchDowngrade)
	}
	return downgrades
}

func openAICompatMiniMaxPolicyDowngrades(input, output []byte) []string {
	for _, path := range []string{"frequency_penalty", "presence_penalty"} {
		if gjson.GetBytes(input, path).Exists() && !gjson.GetBytes(output, path).Exists() {
			return []string{openAICompatMiniMaxPenaltyDowngrade}
		}
	}
	return nil
}

func openAICompatQwen38PolicyDowngrades(ctx context.Context, input, output []byte) []string {
	if !requiresQwen38MaxThinking(input, openAICompatPolicyModel(ctx, input)) {
		return nil
	}
	_, _, disabledEffort := qwen38MaxReasoningEffort(input)
	budget, hasBudget := firstOpenAICompatIntegerValue(input, "thinking_budget", "thinking.budget_tokens")
	disabled := disabledEffort || qwen38MaxThinkingDisabled(input) || hasBudget && budget <= 0
	if disabled && gjson.GetBytes(output, "enable_thinking").Bool() && gjson.GetBytes(output, "reasoning_effort").String() == "low" {
		return []string{openAICompatQwen38ThinkingDowngrade}
	}
	return nil
}
