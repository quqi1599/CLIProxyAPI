package executor

import (
	"context"
	"strings"
	"time"

	compathistory "github.com/router-for-me/CLIProxyAPI/v7/internal/compat/history"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

const (
	maxSyntheticThinkingHistoryBytes      = compathistory.MaxSyntheticItemBytes
	maxSyntheticThinkingHistoryTotalBytes = compathistory.MaxSyntheticTotalBytes

	thinkingHistoryBudgetDowngradeReason  = compathistory.BudgetDowngradeReason
	thinkingHistoryUnrepairableReason     = compathistory.UnrepairableReason
	thinkingHistorySyntheticBudgetPolicy  = "thinking_history.synthetic_budget"
	thinkingHistoryPlaceholderPolicy      = "thinking_history.placeholder"
	openAIThinkingHistoryTransformStage   = "normalize.thinking_history.openai"
	claudeThinkingHistoryTransformStage   = "normalize.thinking_history.claude"
	openAIReasoningUnavailablePlaceholder = compathistory.OpenAIUnavailableValue
	claudeThinkingUnavailablePlaceholder  = compathistory.ClaudeUnavailableValue
)

type thinkingHistoryTransformReport = compathistory.Report

func enforceThinkingHistoryTransform(ctx context.Context, provider string, report thinkingHistoryTransformReport, duration time.Duration) error {
	stage := ""
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		stage = openAIThinkingHistoryTransformStage
	case "claude":
		stage = claudeThinkingHistoryTransformStage
	default:
		return nil
	}

	appliedPolicies := make([]string, 0, 1)
	if report.PlaceholderCount > 0 {
		appliedPolicies = append(appliedPolicies, thinkingHistoryPlaceholderPolicy)
	}
	downgrades := make([]string, 0, 1)
	switch report.DowngradeReason {
	case thinkingHistoryBudgetDowngradeReason, thinkingHistoryUnrepairableReason:
		downgrades = append(downgrades, report.DowngradeReason)
	}
	override := internalpayload.AmplificationOverride{}
	if report.PatchedCount > 0 {
		override = internalpayload.AmplificationOverride{
			PolicyID:          thinkingHistorySyntheticBudgetPolicy,
			MaxExpansionBytes: 2 * maxSyntheticThinkingHistoryTotalBytes,
			MaxExpansionRatio: internalpayload.DefaultMaxExpansionRatio,
		}
	}
	return internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:           stage,
		InputBytes:      int64(report.InputBytes),
		OutputBytes:     int64(report.OutputBytes),
		SyntheticBytes:  int64(report.SyntheticBytes),
		PatchedCount:    int64(report.PatchedCount),
		Duration:        duration,
		AppliedPolicies: appliedPolicies,
		Downgrades:      downgrades,
		ReusedInput:     report.PatchedCount == 0 && report.SyntheticBytes == 0 && report.InputBytes == report.OutputBytes,
	}, override)
}

func normalizeThinkingHistory(body []byte, provider string) ([]byte, bool, bool, error) {
	return normalizeThinkingHistoryForModel(body, provider, "")
}

func normalizeThinkingHistoryForModel(body []byte, provider string, model string) ([]byte, bool, bool, error) {
	out, changed, downgraded, _, err := normalizeThinkingHistoryForModelWithReport(body, provider, model)
	return out, changed, downgraded, err
}

func normalizeThinkingHistoryWithReport(body []byte, provider string) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	return normalizeThinkingHistoryForModelWithReport(body, provider, "")
}

func normalizeThinkingHistoryForModelWithReport(body []byte, provider string, model string) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	report := thinkingHistoryTransformReport{InputBytes: len(body), OutputBytes: len(body)}
	requested := thinkingHistoryRequested(body, provider)
	if !requested && requiresReturnedThinkingHistory(model) {
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "openai":
			requested = deepSeekOpenAIThinkingEnabled(body) || openAIHistoryNeedsThinkingNormalization(body)
		case "claude":
			requested = claudeHistoryNeedsThinkingNormalization(body)
		}
	}
	if !requested {
		return body, false, false, report, nil
	}
	requireCompleteHistory := requiresReturnedThinkingHistory(model)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return normalizeOpenAIThinkingHistoryWithReport(body, requireCompleteHistory)
	case "claude":
		return normalizeClaudeThinkingHistoryWithReport(body, requireCompleteHistory)
	default:
		return body, false, false, report, nil
	}
}

func openAIHistoryNeedsThinkingNormalization(body []byte) bool {
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		if strings.TrimSpace(message.Get("reasoning_content").String()) != "" {
			return true
		}
		if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() && len(toolCalls.Array()) > 0 {
			return true
		}
	}
	return false
}

func claudeHistoryNeedsThinkingNormalization(body []byte) bool {
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		for _, part := range message.Get("content").Array() {
			if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "thinking") && strings.TrimSpace(part.Get("thinking").String()) != "" {
				return true
			}
			if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "tool_use") {
				return true
			}
		}
	}
	return false
}

func requiresReturnedThinkingHistory(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	return strings.HasPrefix(modelName, "deepseek-v4") || strings.Contains(modelName, "deepseek-reasoner")
}

func normalizeOpenAIThinkingHistory(body []byte, requireCompleteHistory bool) ([]byte, bool, bool, error) {
	out, changed, downgraded, _, err := normalizeOpenAIThinkingHistoryWithReport(body, requireCompleteHistory)
	return out, changed, downgraded, err
}

func normalizeOpenAIThinkingHistoryWithReport(body []byte, requireCompleteHistory bool) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	result, err := compathistory.Repair(body, compathistory.FormatOpenAI, requireCompleteHistory)
	return result.Payload, result.Changed, result.Downgraded, result.Report, err
}

func normalizeClaudeThinkingHistory(body []byte, requireCompleteHistory bool) ([]byte, bool, bool, error) {
	out, changed, downgraded, _, err := normalizeClaudeThinkingHistoryWithReport(body, requireCompleteHistory)
	return out, changed, downgraded, err
}

func normalizeClaudeThinkingHistoryWithReport(body []byte, requireCompleteHistory bool) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	result, err := compathistory.Repair(body, compathistory.FormatClaude, requireCompleteHistory)
	return result.Payload, result.Changed, result.Downgraded, result.Report, err
}

func thinkingHistoryRequested(body []byte, provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return openAIThinkingEnabled(body)
	case "claude":
		return claudeThinkingEnabled(body)
	default:
		return false
	}
}

func openAIThinkingEnabled(body []byte) bool {
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"} {
		if strings.TrimSpace(gjson.GetBytes(body, path).String()) != "" {
			return true
		}
	}
	return false
}

func deepSeekOpenAIThinkingEnabled(body []byte) bool {
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "disabled", "disable", "none", "off", "false":
		return false
	case "enabled", "enable", "auto", "adaptive", "true", "low", "medium", "high", "max":
		return true
	}
	if openAIThinkingEnabled(body) {
		return true
	}
	for _, path := range []string{"thinking_budget", "thinking.budget_tokens"} {
		value := gjson.GetBytes(body, path)
		if !value.Exists() {
			continue
		}
		if budget, ok := deepSeekThinkingBudgetValue(value); ok && budget > 0 {
			return true
		}
	}
	return false
}

func claudeThinkingEnabled(body []byte) bool {
	thinkingType := strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())
	if thinkingType != "" && thinkingType != "disabled" {
		return true
	}
	return strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String()) != ""
}
