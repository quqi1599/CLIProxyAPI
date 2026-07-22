package executor

import (
	"bytes"
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
)

const (
	claudeCompatDeepSeekCapabilityPolicyID = "claude_compat.deepseek.capability_scrub"
	claudeCompatDoubaoCapabilityPolicyID   = "claude_compat.doubao.capability_scrub"
	claudeCompatXiaomiCapabilityPolicyID   = "claude_compat.xiaomi.capability_scrub"
)

var (
	claudeCompatPolicyRegistry, claudeCompatPolicyRegistryErr = compat.NewRegistry(
		newClaudeCompatCapabilityPolicy(
			claudeCompatDeepSeekCapabilityPolicyID,
			"deepseek",
			"git:a1d8ce29",
			"internal/runtime/executor/testdata/compat/claude_deepseek_capabilities.json",
			"DeepSeek Claude-compatible endpoints reject server tools, unsupported content blocks, and extended tool schemas.",
			"Remove when DeepSeek Claude-compatible endpoints accept canonical Claude tools, schemas, and multimodal blocks.",
		),
		newClaudeCompatCapabilityPolicy(
			claudeCompatDoubaoCapabilityPolicyID,
			"doubao",
			"git:801cc6a2",
			"internal/runtime/executor/testdata/compat/claude_doubao_capabilities.json",
			"Doubao Claude-compatible endpoints accept native image blocks but reject image_url and unsupported server-tool content.",
			"Remove when Doubao Claude-compatible endpoints accept canonical Claude server tools and multimodal blocks.",
		),
		newClaudeCompatCapabilityPolicy(
			claudeCompatXiaomiCapabilityPolicyID,
			"xiaomi",
			"git:9674bb13",
			"internal/runtime/executor/testdata/compat/claude_xiaomi_capabilities.json",
			"Xiaomi MiMo v2.5 accepts native image blocks while other Claude-compatible content forms require capability scrubbing.",
			"Remove when Xiaomi Claude-compatible endpoints accept canonical Claude server tools and multimodal blocks across supported models.",
		),
	)
	claudeCompatPolicyPipeline = compat.NewPipeline(claudeCompatPolicyRegistry)
)

type claudeCompatPolicyContextKey struct{}

type claudeCompatPolicyContext struct {
	baseURL string
}

func newClaudeCompatCapabilityPolicy(id, compatKind, introducedVersion, fixture, evidence, removalCondition string) compat.Policy {
	return compat.Policy{
		ID:    id,
		Owner: "runtime/executor",
		Match: compat.MatchSpec{
			ProviderFamily: "claude",
			CompatKind:     compatKind,
		},
		Phase:    compat.ProviderCapabilityScrub,
		Priority: 100,
		Cost: compat.CostContract{
			Complexity:         "O(bytes)",
			MaxExpansionBytes:  internalpayload.DefaultMaxExpansionBytes,
			MaxExpansionRatio:  internalpayload.DefaultMaxExpansionRatio,
			MayCopyLargeFields: true,
		},
		RemovalCondition: removalCondition,
		Lifecycle: compat.LifecycleMetadata{
			IntroducedVersion: introducedVersion,
			Fixture:           fixture,
			UpstreamEvidence:  evidence,
			RetrySemantics:    "Request-local transform; no retry, cooldown, or credential eviction changes.",
			ReviewDate:        "2026-10-22",
		},
		MutatedFields: []string{
			"body.messages",
			"body.tool_choice",
			"body.tools",
		},
		DowngradeIDs: []string{claudeToolSearchCompatibilityDowngrade},
		Apply: func(ctx context.Context, input []byte) (compat.TransformResult, error) {
			return applyClaudeCompatCapabilityPolicy(ctx, input, compatKind)
		},
	}
}

func applyClaudeCompatCapabilityPolicy(ctx context.Context, input []byte, compatKind string) (compat.TransformResult, error) {
	state, ok := ctx.Value(claudeCompatPolicyContextKey{}).(claudeCompatPolicyContext)
	if !ok {
		return compat.TransformResult{}, fmt.Errorf("claude compat policy context is missing")
	}
	output, messagePlaceholders := downgradeClaudeToolSearchForCompatKindWithStats(compatKind, state.baseURL, input, true)
	repairs := 0
	if !bytes.Equal(input, output) {
		var err error
		output, repairs, err = normalizeClaudeEmptyToolResults(output)
		if err != nil {
			return compat.TransformResult{}, err
		}
	}
	result := compat.TransformResult{
		Payload:        output,
		SyntheticBytes: int64(repairs*len(claudeEmptyToolResultText) + messagePlaceholders*len(claudeUnsupportedContentPlaceholderText)),
		PatchedCount:   int64(repairs + messagePlaceholders),
	}
	if !bytes.Equal(input, output) {
		result.Downgrades = []string{claudeToolSearchCompatibilityDowngrade}
	}
	return result, nil
}

func applyClaudeCompatProviderCapabilities(ctx context.Context, input []byte, compatKind, baseURL string, match compat.MatchContext) ([]byte, bool, error) {
	compatKind = config.NormalizeOpenAICompatibilityKind(compatKind)
	if !hasClaudeCompatProviderCapabilityPolicy(compatKind) {
		return input, false, nil
	}
	if claudeCompatPolicyRegistryErr != nil {
		return nil, true, claudeCompatPolicyRegistryErr
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, claudeCompatPolicyContextKey{}, claudeCompatPolicyContext{baseURL: baseURL})
	match.ProviderFamily = "claude"
	match.CompatKind = compatKind
	if match.Model == "" {
		match.Model = gjson.GetBytes(input, "model").String()
	}
	result, err := claudeCompatPolicyPipeline.Apply(ctx, match, input)
	if err != nil {
		return nil, true, err
	}
	return result.Payload, true, nil
}

func hasClaudeCompatProviderCapabilityPolicy(compatKind string) bool {
	switch config.NormalizeOpenAICompatibilityKind(compatKind) {
	case "deepseek", "doubao", "xiaomi":
		return true
	default:
		return false
	}
}

func claudeCompatPolicyInventory() (compat.Report, error) {
	if claudeCompatPolicyRegistryErr != nil {
		return compat.Report{}, claudeCompatPolicyRegistryErr
	}
	return claudeCompatPolicyRegistry.Report(), nil
}
