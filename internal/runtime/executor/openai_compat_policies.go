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
	openAICompatDeepSeekPolicyID             = "openai_compat.deepseek.request_quirks"
	openAICompatDoubaoPolicyID               = "openai_compat.doubao.request_quirks"
	openAICompatKimiPolicyID                 = "openai_compat.kimi.model_quirks"
	openAICompatMiniMaxPolicyID              = "openai_compat.minimax.request_quirks"
	openAICompatQwen38PolicyID               = "openai_compat.qwen38.thinking"
	openAICompatXiaomiPolicyID               = "openai_compat.xiaomi.request_quirks"
	openAICompatPostConfigRevalidatePolicyID = "openai_compat.post_config_revalidate"
	openAICompatPostConfigRevalidateStage    = "compat/" + string(compat.PostConfigRevalidate)
	openAICompatProviderPreQuirkStage        = "request_plan.openai_compat.provider_pre_quirk_scrub"
	openAICompatProviderPostQuirkStage       = "request_plan.openai_compat.provider_post_quirk_scrub"
	openAICompatProviderPreQuirkPolicy       = "openai_compat.provider_pre_quirk_scrub"
	openAICompatProviderPostQuirkPolicy      = "openai_compat.provider_post_quirk_scrub"

	openAICompatDeepSeekThinkingDowngrade   = "openai_compat.deepseek.thinking_controls_normalized"
	openAICompatDeepSeekToolChoiceDowngrade = "openai_compat.deepseek.tool_choice_removed"
	openAICompatDeepSeekStrictDowngrade     = "openai_compat.deepseek.strict_schema_removed"
	openAICompatDoubaoFieldsDowngrade       = "openai_compat.doubao.unsupported_fields_removed"
	openAICompatDoubaoSeed20Downgrade       = "openai_compat.doubao.seed20_payload_normalized"
	openAICompatKimiToolChoiceDowngrade     = "openai_compat.kimi.tool_choice_normalized"
	openAICompatKimiWebSearchDowngrade      = "openai_compat.kimi.web_search_disables_thinking"
	openAICompatMiniMaxPenaltyDowngrade     = "openai_compat.minimax.penalties_removed"
	openAICompatQwen38ThinkingDowngrade     = "openai_compat.qwen38.disabled_to_low"
	openAICompatXiaomiReasoningDowngrade    = "openai_compat.xiaomi.reasoning_normalized"
	openAICompatXiaomiTokenDowngrade        = "openai_compat.xiaomi.token_limits_normalized"
	openAICompatXiaomiToolSchemaDowngrade   = "openai_compat.xiaomi.tool_schema_normalized"
)

var (
	openAICompatPolicyRegistry, openAICompatPolicyRegistryErr = compat.NewRegistry(
		openAICompatDeepSeekPolicy(),
		openAICompatDoubaoPolicy(),
		openAICompatKimiPolicy(),
		openAICompatMiniMaxPolicy(),
		openAICompatQwen38Policy(),
		openAICompatXiaomiPolicy(),
	)
	openAICompatPolicyPipeline                                        = compat.NewPipeline(openAICompatPolicyRegistry)
	openAICompatPostConfigRegistry, openAICompatPostConfigRegistryErr = compat.NewRegistry(
		openAICompatPostConfigRevalidatePolicy(),
	)
	openAICompatPostConfigPipeline = compat.NewPipeline(openAICompatPostConfigRegistry)
)

type openAICompatPolicyContextKey struct{}
type openAICompatPostConfigContextKey struct{}

type openAICompatPolicyContext struct {
	model   string
	baseURL string
}

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
	payload = scrubOpenAICompatPayloadBeforeRegisteredProviderQuirks(payload, profile, model, baseURL)
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
	ctx = context.WithValue(ctx, openAICompatPolicyContextKey{}, openAICompatPolicyContext{model: model, baseURL: baseURL})
	result, err := openAICompatPolicyPipeline.Apply(ctx, openAICompatPolicyMatchContext(profile, payload, model, match), payload)
	if err != nil {
		return nil, err
	}
	postQuirkStarted := time.Now()
	output := scrubOpenAICompatPayloadAfterRegisteredProviderQuirks(result.Payload, profile, model, baseURL)
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
	ctx = context.WithValue(ctx, openAICompatPolicyContextKey{}, openAICompatPolicyContext{model: model, baseURL: baseURL})
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
			openAICompatDeepSeekThinkingDowngrade,
			openAICompatDeepSeekToolChoiceDowngrade,
			openAICompatDeepSeekStrictDowngrade,
			openAICompatDoubaoFieldsDowngrade,
			openAICompatDoubaoSeed20Downgrade,
			openAICompatKimiToolChoiceDowngrade,
			openAICompatKimiWebSearchDowngrade,
			openAICompatMiniMaxPenaltyDowngrade,
			openAICompatQwen38ThinkingDowngrade,
			openAICompatXiaomiReasoningDowngrade,
			openAICompatXiaomiTokenDowngrade,
			openAICompatXiaomiToolSchemaDowngrade,
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
	case "deepseek":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatDeepSeekPolicyDowngrades(input, output))
	case "doubao":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatDoubaoPolicyDowngrades(input, output, openAICompatPolicyModel(ctx, input)))
	case "kimi":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatKimiPolicyDowngrades(input, output))
	case "minimax":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatMiniMaxPolicyDowngrades(input, output))
	case "qwen":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatQwen38PolicyDowngrades(ctx, input, output))
	case "xiaomi":
		downgrades = appendOpenAICompatDowngrades(downgrades, openAICompatXiaomiPolicyDowngrades(input, output))
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

func openAICompatDeepSeekPolicy() compat.Policy {
	return compat.Policy{
		ID:    openAICompatDeepSeekPolicyID,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "openai-compatibility",
			CompatKind:     "deepseek",
		},
		Phase:    compat.ProviderQuirkPatch,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: "Remove when official DeepSeek endpoints accept canonical reasoning controls, tool choice, and OpenAI tool schemas.",
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: "git:2e769c78",
			Fixture:           "internal/runtime/executor/testdata/compat/deepseek_request_quirks.json",
			UpstreamEvidence:  "DeepSeek requires bounded thinking controls and endpoint-specific strict tool schemas.",
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.messages",
			"body.reasoning",
			"body.reasoning_effort",
			"body.thinking",
			"body.thinking_budget",
			"body.tool_choice",
			"body.tools",
		},
		DowngradeIDs: []string{
			openAICompatDeepSeekThinkingDowngrade,
			openAICompatDeepSeekToolChoiceDowngrade,
			openAICompatDeepSeekStrictDowngrade,
		},
		Apply: applyOpenAICompatDeepSeekPolicy,
	}
}

func openAICompatDoubaoPolicy() compat.Policy {
	return compat.Policy{
		ID:    openAICompatDoubaoPolicyID,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "openai-compatibility",
			CompatKind:     "doubao",
		},
		Phase:    compat.ProviderQuirkPatch,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: "Remove when Doubao Ark accepts canonical OpenAI request fields and Seed 2.0 payload shapes.",
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: "git:d1123231",
			Fixture:           "internal/runtime/executor/testdata/compat/doubao_request_quirks.json",
			UpstreamEvidence:  "Doubao Ark rejects unsupported OpenAI fields and constrains Seed 2.0 sampling, token, and content shapes.",
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.max_completion_tokens",
			"body.max_output_tokens",
			"body.max_tokens",
			"body.messages",
			"body.metadata",
			"body.output_config",
			"body.parallel_tool_calls",
			"body.reasoning",
			"body.reasoning_effort",
			"body.response_format",
			"body.service_tier",
			"body.store",
			"body.stream_options",
			"body.temperature",
			"body.thinking",
			"body.thinking_budget",
			"body.user",
		},
		DowngradeIDs: []string{
			openAICompatDoubaoFieldsDowngrade,
			openAICompatDoubaoSeed20Downgrade,
		},
		Apply: applyOpenAICompatDoubaoPolicy,
	}
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

func openAICompatXiaomiPolicy() compat.Policy {
	return compat.Policy{
		ID:    openAICompatXiaomiPolicyID,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "openai-compatibility",
			CompatKind:     "xiaomi",
		},
		Phase:    compat.ProviderQuirkPatch,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: "Remove when Xiaomi MiMo accepts canonical OpenAI reasoning, sampling, token, and tool schema fields.",
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: "git:5a961947",
			Fixture:           "internal/runtime/executor/testdata/compat/xiaomi_request_quirks.json",
			UpstreamEvidence:  "Xiaomi MiMo requires native thinking controls, fixed thinking sampling, bounded output tokens, and simplified tool schemas.",
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.max_completion_tokens",
			"body.max_output_tokens",
			"body.max_tokens",
			"body.messages",
			"body.reasoning",
			"body.reasoning_effort",
			"body.temperature",
			"body.thinking",
			"body.thinking_budget",
			"body.tools",
			"body.top_p",
		},
		DowngradeIDs: []string{
			openAICompatXiaomiReasoningDowngrade,
			openAICompatXiaomiTokenDowngrade,
			openAICompatXiaomiToolSchemaDowngrade,
		},
		Apply: applyOpenAICompatXiaomiPolicy,
	}
}

func applyOpenAICompatDeepSeekPolicy(ctx context.Context, input []byte) (compat.TransformResult, error) {
	state := openAICompatPolicyState(ctx, input)
	output := scrubDeepSeekThinkingBudgetForCompat(input, state.model, state.baseURL, "deepseek")
	output = scrubDeepSeekThinkingToolChoice(output, state.model, state.baseURL, "deepseek")
	if requiresDeepSeekToolSchemaCompatibility(state.model) {
		output = scrubDeepSeekToolPayload(output, state.baseURL)
	}
	return compat.TransformResult{
		Payload:    output,
		Downgrades: openAICompatDeepSeekPolicyDowngrades(input, output),
	}, nil
}

func applyOpenAICompatDoubaoPolicy(ctx context.Context, input []byte) (compat.TransformResult, error) {
	model := openAICompatPolicyModel(ctx, input)
	output := scrubDoubaoPayloadForModel(input, model)
	return compat.TransformResult{
		Payload:    output,
		Downgrades: openAICompatDoubaoPolicyDowngrades(input, output, model),
	}, nil
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

func applyOpenAICompatXiaomiPolicy(ctx context.Context, input []byte) (compat.TransformResult, error) {
	output := scrubXiaomiPayloadForModel(input, openAICompatPolicyModel(ctx, input))
	return compat.TransformResult{
		Payload:    output,
		Downgrades: openAICompatXiaomiPolicyDowngrades(input, output),
	}, nil
}

func openAICompatPolicyState(ctx context.Context, payload []byte) openAICompatPolicyContext {
	if ctx != nil {
		if state, ok := ctx.Value(openAICompatPolicyContextKey{}).(openAICompatPolicyContext); ok {
			return state
		}
	}
	return openAICompatPolicyContext{model: gjson.GetBytes(payload, "model").String()}
}

func openAICompatPolicyModel(ctx context.Context, payload []byte) string {
	return openAICompatPolicyState(ctx, payload).model
}

func openAICompatDeepSeekPolicyDowngrades(input, output []byte) []string {
	downgrades := make([]string, 0, 3)
	for _, path := range []string{
		"reasoning",
		"reasoning_effort",
		"thinking",
		"thinking_budget",
	} {
		if gjson.GetBytes(input, path).Raw != gjson.GetBytes(output, path).Raw {
			downgrades = append(downgrades, openAICompatDeepSeekThinkingDowngrade)
			break
		}
	}
	if gjson.GetBytes(input, "tool_choice").Exists() && !gjson.GetBytes(output, "tool_choice").Exists() {
		downgrades = append(downgrades, openAICompatDeepSeekToolChoiceDowngrade)
	}
	if deepSeekHasStrictField(input) && !deepSeekHasStrictField(output) {
		downgrades = append(downgrades, openAICompatDeepSeekStrictDowngrade)
	}
	return downgrades
}

func deepSeekHasStrictField(payload []byte) bool {
	for _, path := range []string{"tools.#.strict", "tools.#.function.strict"} {
		if len(gjson.GetBytes(payload, path).Array()) > 0 {
			return true
		}
	}
	return false
}

func openAICompatDoubaoPolicyDowngrades(input, output []byte, model string) []string {
	downgrades := make([]string, 0, 2)
	for _, path := range []string{
		"user",
		"response_format",
		"service_tier",
		"reasoning_effort",
		"thinking",
		"thinking_budget",
		"output_config.effort",
		"reasoning",
	} {
		if gjson.GetBytes(input, path).Raw != gjson.GetBytes(output, path).Raw {
			downgrades = append(downgrades, openAICompatDoubaoFieldsDowngrade)
			break
		}
	}
	if openAICompatHasMessageReasoningContent(input) && !openAICompatHasMessageReasoningContent(output) {
		downgrades = appendOpenAICompatDowngrades(downgrades, []string{openAICompatDoubaoFieldsDowngrade})
	}
	if requiresDoubaoSeed20Compatibility(model) {
		for _, path := range []string{"temperature", "max_tokens", "max_completion_tokens", "max_output_tokens", "messages"} {
			if gjson.GetBytes(input, path).Raw != gjson.GetBytes(output, path).Raw {
				downgrades = append(downgrades, openAICompatDoubaoSeed20Downgrade)
				break
			}
		}
	}
	return downgrades
}

func openAICompatHasMessageReasoningContent(payload []byte) bool {
	for _, message := range gjson.GetBytes(payload, "messages").Array() {
		if message.Get("reasoning_content").Exists() {
			return true
		}
	}
	return false
}

func openAICompatXiaomiPolicyDowngrades(input, output []byte) []string {
	downgrades := make([]string, 0, 3)
	for _, path := range []string{"reasoning", "reasoning_effort", "thinking", "thinking_budget", "temperature", "top_p"} {
		if openAICompatJSONValueChanged(input, output, path) {
			downgrades = append(downgrades, openAICompatXiaomiReasoningDowngrade)
			break
		}
	}
	for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if openAICompatJSONValueChanged(input, output, path) {
			downgrades = append(downgrades, openAICompatXiaomiTokenDowngrade)
			break
		}
	}
	if openAICompatJSONValueChanged(input, output, "tools") {
		downgrades = append(downgrades, openAICompatXiaomiToolSchemaDowngrade)
	}
	return downgrades
}

func openAICompatJSONValueChanged(input, output []byte, path string) bool {
	before := gjson.GetBytes(input, path)
	after := gjson.GetBytes(output, path)
	if before.Exists() != after.Exists() {
		return true
	}
	return !jsonValuesEqual(before.Value(), after.Value())
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
