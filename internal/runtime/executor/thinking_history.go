package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	compathistory "github.com/router-for-me/CLIProxyAPI/v7/internal/compat/history"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

const (
	thinkingHistoryValidationPolicy     = "thinking_history.real_reasoning_validation"
	openAIThinkingHistoryTransformStage = "normalize.thinking_history.openai"
	claudeThinkingHistoryTransformStage = "normalize.thinking_history.claude"
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

	return internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:           stage,
		InputBytes:      int64(report.InputBytes),
		OutputBytes:     int64(report.OutputBytes),
		Duration:        duration,
		AppliedPolicies: []string{thinkingHistoryValidationPolicy},
		ReusedInput:     true,
	}, internalpayload.AmplificationOverride{})
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
	requireToolCallReasoning := requiresReturnedThinkingHistory(model)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return normalizeOpenAIThinkingHistoryWithReport(body, requireToolCallReasoning)
	case "claude":
		return normalizeClaudeThinkingHistoryWithReport(body, requireToolCallReasoning)
	default:
		report := thinkingHistoryTransformReport{InputBytes: len(body), OutputBytes: len(body)}
		return body, false, false, report, nil
	}
}

func requiresReturnedThinkingHistory(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	return strings.HasPrefix(modelName, "deepseek-v4") || strings.Contains(modelName, "deepseek-reasoner")
}

func normalizeOpenAIThinkingHistory(body []byte, requireToolCallReasoning bool) ([]byte, bool, bool, error) {
	out, changed, downgraded, _, err := normalizeOpenAIThinkingHistoryWithReport(body, requireToolCallReasoning)
	return out, changed, downgraded, err
}

func normalizeOpenAIThinkingHistoryWithReport(body []byte, requireToolCallReasoning bool) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	report, err := compathistory.Validate(body, compathistory.FormatOpenAI, requireToolCallReasoning)
	if err != nil {
		return body, false, false, report, missingReasoningHistoryStatusError(err)
	}
	return body, false, false, report, nil
}

func normalizeClaudeThinkingHistory(body []byte, requireToolCallReasoning bool) ([]byte, bool, bool, error) {
	out, changed, downgraded, _, err := normalizeClaudeThinkingHistoryWithReport(body, requireToolCallReasoning)
	return out, changed, downgraded, err
}

func normalizeClaudeThinkingHistoryWithReport(body []byte, requireToolCallReasoning bool) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	report, err := compathistory.Validate(body, compathistory.FormatClaude, requireToolCallReasoning)
	if err != nil {
		return body, false, false, report, missingReasoningHistoryStatusError(err)
	}
	return body, false, false, report, nil
}

func missingReasoningHistoryStatusError(err error) error {
	var missing *compathistory.MissingReasoningError
	if !errors.As(err, &missing) {
		return err
	}
	field := "reasoning_content"
	if missing.Format == compathistory.FormatClaude {
		field = "thinking block"
	}
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: "missing_reasoning_history",
		msg: fmt.Sprintf(
			"invalid_request_error: missing_real_reasoning_history. %s. CPA will not fabricate, copy, or insert placeholder reasoning. Preserve the original %s returned by DeepSeek on every assistant tool-call turn, or start a new conversation.",
			missing.Error(),
			field,
		),
	}
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

func openAIThinkingEnabled(body []byte) bool {
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"} {
		if strings.TrimSpace(gjson.GetBytes(body, path).String()) != "" {
			return true
		}
	}
	return false
}
