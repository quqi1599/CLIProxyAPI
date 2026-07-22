package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
)

const (
	openAICompatKimiPolicyID    = "openai_compat.kimi.model_quirks"
	openAICompatMiniMaxPolicyID = "openai_compat.minimax.request_quirks"
	openAICompatQwen38PolicyID  = "openai_compat.qwen38.thinking"

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
	openAICompatPolicyPipeline = compat.NewPipeline(openAICompatPolicyRegistry)
)

type openAICompatPolicyModelContextKey struct{}

func scrubOpenAICompatPayloadForModelWithPolicies(ctx context.Context, payload []byte, profile openAICompatProfile, model string, baseURL string) ([]byte, error) {
	payload = scrubOpenAICompatPayloadBeforeProviderQuirks(payload, profile, model, baseURL)
	if openAICompatPolicyRegistryErr != nil {
		return nil, openAICompatPolicyRegistryErr
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, openAICompatPolicyModelContextKey{}, model)
	result, err := openAICompatPolicyPipeline.Apply(ctx, openAICompatPolicyMatchContext(profile, payload, model), payload)
	if err != nil {
		return nil, err
	}
	return scrubOpenAICompatPayloadAfterProviderQuirks(result.Payload, profile, model, baseURL), nil
}

func openAICompatPolicyMatchContext(profile openAICompatProfile, payload []byte, model string) compat.MatchContext {
	compatKind := config.NormalizeOpenAICompatibilityKind(profile.Kind)
	matchModel := normalizedOpenAICompatPolicyModelName(model)
	if compatKind == "qwen" && !isQwen38MaxThinkingModel(matchModel) {
		if payloadModel := normalizedOpenAICompatPolicyModelName(gjson.GetBytes(payload, "model").String()); isQwen38MaxThinkingModel(payloadModel) {
			matchModel = payloadModel
		}
	}
	return compat.MatchContext{
		ProviderFamily: "openai-compatibility",
		CompatKind:     compatKind,
		Model:          matchModel,
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
