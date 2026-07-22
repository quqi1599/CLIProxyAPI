package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/sjson"
)

func TestPrepareClaudeRequestReportsThinkingHistoryPolicy(t *testing.T) {
	payload := buildClaudeThinkingBudgetBody(strings.Repeat("r", maxSyntheticThinkingHistoryBytes), 9)
	payload = mustSetSemanticReportFixture(t, payload, "tools", `[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}]`)
	payload = mustSetSemanticReportFixture(t, payload, "output_config.format", `{"type":"json_schema","schema":{"type":"object"}}`)
	ctx, releaseReport := retainExecutorTransformReport(context.Background(), len(payload))
	executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":     "test-key",
		"base_url":    "https://api.example.com",
		"compat_kind": "minimax",
	}}
	plan, err := executor.prepareClaudeRequest(ctx, auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "deepseek-v4-pro", false)
	if err != nil {
		t.Fatalf("prepareClaudeRequest() error = %v", err)
	}
	assertThinkingHistoryProductionStage(t, ctx, claudeThinkingHistoryTransformStage)
	assertSemanticRequestStages(t, ctx, []semanticStageExpectation{
		{id: claudeProviderConfigTransformStage, policy: claudeProviderConfigPolicy},
		{id: claudeProviderCompatibilityTransformStage, policy: claudeProviderCompatibilityPolicy},
		{id: claudeToolHistoryTransformStage, policy: claudeToolHistoryPolicy, downgrade: claudeToolSearchCompatibilityDowngrade},
		{id: claudeFinalSanitizeTransformStage, policy: claudeFinalSanitizePolicy, downgrade: claudeStructuredOutputCompatibilityDowngrade},
	}, claudeFinalSanitizeTransformStage, len(plan.bodyForUpstream))
	releaseReport()
	assertFinalSemanticReportOutput(t, ctx, len(plan.bodyForUpstream))
}

func TestPrepareOpenAICompatRequestReportsThinkingHistoryPolicy(t *testing.T) {
	payload := buildOpenAIThinkingBudgetBody(strings.Repeat("r", maxSyntheticThinkingHistoryBytes), 9)
	payload = mustSetSemanticReportFixture(t, payload, "metadata", `{"tenant":"test"}`)
	payload = mustSetSemanticReportFixture(t, payload, "store", `true`)
	payload = mustSetSemanticReportFixture(t, payload, "parallel_tool_calls", `true`)
	payload = mustSetSemanticReportFixture(t, payload, "stream_options", `{"include_usage":true}`)
	ctx, releaseReport := retainExecutorTransformReport(context.Background(), len(payload))
	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	plan, err := executor.prepareOpenAICompatRequest(
		ctx,
		nil,
		cliproxyexecutor.Request{Model: "test-model", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI},
		"https://api.example.com/v1",
		"test-model",
		openAICompatProfileForKind("newapi"),
		false,
	)
	if err != nil {
		t.Fatalf("prepareOpenAICompatRequest() error = %v", err)
	}
	assertThinkingHistoryProductionStage(t, ctx, openAIThinkingHistoryTransformStage)
	reused := true
	changed := false
	assertSemanticRequestStages(t, ctx, []semanticStageExpectation{
		{id: openAICompatProviderResolveTransformStage, policy: openAICompatProviderResolvePolicy, reused: &changed},
		{id: openAICompatProviderConfigTransformStage, policy: openAICompatProviderConfigPolicy, reused: &changed},
		{id: openAICompatToolHistoryTransformStage, policy: openAICompatToolHistoryPolicy, reused: &reused},
		{id: openAICompatProviderPreQuirkStage, policy: openAICompatProviderPreQuirkPolicy, downgrade: openAICompatMetadataRemovedDowngrade, reused: &changed},
		{id: openAICompatProviderPostQuirkStage, policy: openAICompatProviderPostQuirkPolicy, reused: &reused},
		{id: openAICompatFinalSanitizeTransformStage, policy: openAICompatFinalSanitizePolicy, reused: &reused},
	}, openAICompatFinalSanitizeTransformStage, len(plan.body))
	releaseReport()
	assertFinalSemanticReportOutput(t, ctx, len(plan.body))
}

type semanticStageExpectation struct {
	id        string
	policy    string
	downgrade string
	reused    *bool
}

func mustSetSemanticReportFixture(t *testing.T, payload []byte, path, raw string) []byte {
	t.Helper()
	updated, err := sjson.SetRawBytes(payload, path, []byte(raw))
	if err != nil {
		t.Fatalf("set fixture path %q: %v", path, err)
	}
	return updated
}

func assertSemanticRequestStages(t *testing.T, ctx context.Context, expected []semanticStageExpectation, finalStage string, finalBytes int) {
	t.Helper()
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("transform report missing")
	}
	stages := make(map[string][]internalpayload.TransformStageReport, len(report.Stages))
	for _, stage := range report.Stages {
		stages[stage.Stage] = append(stages[stage.Stage], stage)
	}
	for _, want := range expected {
		matches := stages[want.id]
		if len(matches) != 1 {
			t.Fatalf("semantic stage %q count = %d, want 1; report=%#v", want.id, len(matches), report)
		}
		stage := matches[0]
		if stage.InputBytes <= 0 || stage.OutputBytes <= 0 || stage.Duration < 0 {
			t.Fatalf("semantic stage %q has invalid measurements: %#v", want.id, stage)
		}
		if !containsTransformMetadataID(stage.AppliedPolicies, want.policy) {
			t.Fatalf("semantic stage %q policies = %v, want %q", want.id, stage.AppliedPolicies, want.policy)
		}
		if want.downgrade != "" && !containsTransformMetadataID(stage.Downgrades, want.downgrade) {
			t.Fatalf("semantic stage %q downgrades = %v, want %q", want.id, stage.Downgrades, want.downgrade)
		}
		if want.reused != nil && stage.ReusedInput != *want.reused {
			t.Fatalf("semantic stage %q reused_input = %v, want %v", want.id, stage.ReusedInput, *want.reused)
		}
	}
	if len(report.Stages) == 0 || report.Stages[len(report.Stages)-1].Stage != finalStage {
		t.Fatalf("last semantic stage = %#v, want %q", report.Stages, finalStage)
	}
	if report.OutputBytes != int64(finalBytes) || report.Stages[len(report.Stages)-1].OutputBytes != int64(finalBytes) {
		t.Fatalf("final semantic output = %d/%d, want %d", report.OutputBytes, report.Stages[len(report.Stages)-1].OutputBytes, finalBytes)
	}
}

func assertFinalSemanticReportOutput(t *testing.T, ctx context.Context, finalBytes int) {
	t.Helper()
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || !report.Finalized || report.OutputBytes != int64(finalBytes) || report.FinalAmplification.OutputBytes != int64(finalBytes) {
		t.Fatalf("finalized semantic report = %#v, want output bytes %d", report, finalBytes)
	}
}

func assertThinkingHistoryProductionStage(t *testing.T, ctx context.Context, wantStage string) {
	t.Helper()
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("transform report missing")
	}
	for _, stage := range report.Stages {
		if stage.Stage != wantStage {
			continue
		}
		if stage.InputBytes <= 0 || stage.OutputBytes <= 0 || stage.SyntheticBytes <= 0 || stage.PatchedCount != 9 || stage.Duration < 0 {
			t.Fatalf("thinking history stage bytes = %#v", stage)
		}
		if report.PatchedCount != 9 {
			t.Fatalf("thinking history report patched count = %d", report.PatchedCount)
		}
		if len(stage.AppliedPolicies) != 2 || stage.AppliedPolicies[0] != thinkingHistoryPlaceholderPolicy || stage.AppliedPolicies[1] != thinkingHistorySyntheticBudgetPolicy {
			t.Fatalf("thinking history policies = %v", stage.AppliedPolicies)
		}
		if len(stage.Downgrades) != 1 || stage.Downgrades[0] != thinkingHistoryBudgetDowngradeReason {
			t.Fatalf("thinking history downgrades = %v", stage.Downgrades)
		}
		if !stage.Amplification.OverrideApplied || stage.Amplification.PolicyID != thinkingHistorySyntheticBudgetPolicy || stage.Amplification.Exceeded {
			t.Fatalf("thinking history amplification = %#v", stage.Amplification)
		}
		return
	}
	t.Fatalf("thinking history stage %q missing: %#v", wantStage, report)
}
