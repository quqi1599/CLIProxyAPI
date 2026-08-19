package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	compathistory "github.com/router-for-me/CLIProxyAPI/v7/internal/compat/history"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxSyntheticThinkingHistoryBytes      = compathistory.MaxSyntheticItemBytes
	maxSyntheticThinkingHistoryTotalBytes = compathistory.MaxSyntheticTotalBytes

	thinkingHistoryBudgetDowngradeReason  = compathistory.BudgetDowngradeReason
	thinkingHistoryUnrepairableReason     = compathistory.UnrepairableReason
	thinkingHistoryClientDowngradeReason  = "thinking_history_downgraded"
	thinkingHistorySyntheticBudgetPolicy  = "thinking_history.synthetic_budget"
	thinkingHistoryPlaceholderPolicy      = "thinking_history.placeholder"
	thinkingHistoryValidationPolicy       = "thinking_history.real_reasoning_validation"
	openAIThinkingHistoryTransformStage   = "normalize.thinking_history.openai"
	claudeThinkingHistoryTransformStage   = "normalize.thinking_history.claude"
	openAIReasoningUnavailablePlaceholder = compathistory.OpenAIUnavailableValue
	claudeThinkingUnavailablePlaceholder  = compathistory.ClaudeUnavailableValue
	xiaomiMimoInvalidThinkingHistoryCode  = "mimo_incomplete_reasoning_history"
	deepSeekInvalidThinkingHistoryCode    = "missing_reasoning_history"
	zhipuGLM53InvalidThinkingHistoryCode  = "glm53_missing_reasoning_history"
)

const xiaomiMimoInvalidThinkingHistoryMessage = "MiMo 工具调用历史缺少真实 reasoning_content，CPA 无法可靠还原。请新建会话、关闭思考或清理工具历史后重试；系统不会伪造思考内容、删除工具或跨渠道重试。"
const deepSeekInvalidThinkingHistoryMessage = "DeepSeek 兼容性提示：当前会话使用过文件读取、搜索、命令执行等工具，但会话历史未保留 DeepSeek 继续思考所需的原始推理内容。Codex 中重复重试或 /compact 无法修复；请切换到 OpenAI 原生 GPT 模型后继续并执行 /compact，或新建 DeepSeek 会话。若客户端支持，也可关闭思考模式后继续；这不是账号额度或网络错误。"
const workBuddyDeepSeekInvalidThinkingHistoryMessage = "当前对话的深度思考记录不完整，暂时无法继续。请任选一种方式处理：\n1. 推荐：在 WorkBuddy 中点击“新建对话”，然后重新发送刚才的问题。\n2. 或关闭深度思考：打开 WorkBuddy 的“设置” → 进入“模型设置”或“自定义模型” → 选择当前 DeepSeek 模型 → 关闭“深度思考”“Thinking”或“Reasoning”开关 → 返回对话重试。不同版本的菜单名称可能略有不同。\n3. 如果仍然无法使用，请在模型列表中切换到 OpenAI 原生 GPT 模型，再重新发送。\n这不是账号余额或网络问题。"
const zhipuGLM53InvalidThinkingHistoryMessage = "GLM-5.3 保留式思考的工具调用历史缺少原始 reasoning_content，CPA 不会伪造或重写思考内容。请新建对话，或确保客户端完整保留并按原顺序回传 reasoning_content 后重试；GLM-5.3 不支持通过关闭思考来绕过此问题。"

type deepSeekThinkingIntent uint8

const (
	deepSeekThinkingIntentDefault deepSeekThinkingIntent = iota
	deepSeekThinkingIntentDisabled
	deepSeekThinkingIntentEnabled
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

	appliedPolicies := make([]string, 0, 2)
	if report.PlaceholderCount > 0 {
		appliedPolicies = append(appliedPolicies, thinkingHistoryPlaceholderPolicy)
	}
	if report.CheckedToolCallTurns > 0 {
		appliedPolicies = append(appliedPolicies, thinkingHistoryValidationPolicy)
	}
	downgrades := make([]string, 0, 1)
	switch report.DowngradeReason {
	case thinkingHistoryBudgetDowngradeReason, thinkingHistoryUnrepairableReason, thinkingHistoryClientDowngradeReason:
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
	return normalizeThinkingHistoryForModelWithReportForClient(body, provider, model, "")
}

func normalizeThinkingHistoryForModelWithReportForClient(body []byte, provider string, model string, clientProfile string) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	report := thinkingHistoryTransformReport{InputBytes: len(body), OutputBytes: len(body)}
	if requiresReturnedThinkingHistory(model) {
		if !deepSeekThinkingHistoryRequiredForRequest(body, provider) {
			return body, false, false, report, nil
		}
		var err error
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "openai":
			report, err = compathistory.Validate(body, compathistory.FormatOpenAI, true)
		case "claude":
			report, err = compathistory.Validate(body, compathistory.FormatClaude, true)
		default:
			return body, false, false, report, nil
		}
		if err == nil {
			return body, false, false, report, nil
		}
		clientProfile = strings.ToLower(strings.TrimSpace(clientProfile))
		workBuddyDowngrade := clientProfile == "workbuddy"
		if deepSeekThinkingHistoryIntent(body, provider) == deepSeekThinkingIntentDefault || workBuddyDowngrade {
			out, errDisable := disableDeepSeekThinkingForIncompleteHistory(body, provider)
			if errDisable != nil {
				if workBuddyDowngrade {
					return body, false, false, report, missingReasoningHistoryStatusErrorWithMessage(err, workBuddyDeepSeekInvalidThinkingHistoryMessage)
				}
				return body, false, false, report, errDisable
			}
			report.OutputBytes = len(out)
			if workBuddyDowngrade {
				report.DowngradeReason = thinkingHistoryClientDowngradeReason
			} else {
				report.DowngradeReason = thinkingHistoryUnrepairableReason
			}
			return out, !bytes.Equal(out, body), true, report, nil
		}
		return body, false, false, report, missingReasoningHistoryStatusError(err)
	}
	if preservesOnlyRealReasoningHistory(model) {
		return body, false, false, report, nil
	}
	requested := thinkingHistoryRequested(body, provider)
	if !requested {
		return body, false, false, report, nil
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return normalizeOpenAIThinkingHistoryWithReport(body, false)
	case "claude":
		return normalizeClaudeThinkingHistoryWithReport(body, false)
	default:
		return body, false, false, report, nil
	}
}

func preservesOnlyRealReasoningHistory(model string) bool {
	modelName := normalizedOpenAICompatPolicyModelName(model)
	return strings.HasPrefix(modelName, "qwen") || isXiaomiMimoV25Model(modelName) || isZhipuGLM53Model(modelName)
}

func validateZhipuGLM53PreservedThinkingHistory(body []byte, compatKind, baseURL, model string) error {
	kind := config.NormalizeOpenAICompatibilityKind(compatKind)
	if kind == "" {
		kind = config.InferCompatKindFromBaseURL(baseURL)
	}
	if kind != "zhipu" || !isZhipuGLM53Model(model) || !zhipuGLM53PreservedThinkingEnabled(body, baseURL) {
		return nil
	}

	_, err := compathistory.Validate(body, compathistory.FormatOpenAI, true)
	if err == nil {
		return nil
	}
	return missingReasoningHistoryStatusErrorWithCode(err, zhipuGLM53InvalidThinkingHistoryMessage, zhipuGLM53InvalidThinkingHistoryCode)
}

func zhipuGLM53PreservedThinkingEnabled(body []byte, baseURL string) bool {
	enabled := strings.Contains(strings.ToLower(strings.TrimSpace(baseURL)), "/api/coding/")
	clearThinking := gjson.GetBytes(body, "thinking.clear_thinking")
	if clearThinking.Exists() && clearThinking.Type != gjson.Null {
		enabled = !clearThinking.Bool()
	}
	return enabled
}

func isXiaomiMimoV25Model(model string) bool {
	modelName := normalizedOpenAICompatPolicyModelName(model)
	return modelName == "mimo-v2.5" || strings.HasPrefix(modelName, "mimo-v2.5-pro")
}

func normalizeXiaomiMimoThinkingHistory(body []byte, compatKind string, model string) ([]byte, bool, error) {
	if config.NormalizeOpenAICompatibilityKind(compatKind) != "xiaomi" || !isXiaomiMimoV25Model(model) {
		return body, false, nil
	}
	if countXiaomiMimoToolCallsMissingReasoning(body) == 0 {
		return body, false, nil
	}

	switch xiaomiMimoThinkingIntent(body) {
	case "enabled":
		return body, false, &failurecontract.Failure{
			Kind:          failurecontract.InvalidThinkingHistory,
			Scope:         failurecontract.ScopeRequest,
			HTTPStatus:    http.StatusBadRequest,
			ProviderCode:  xiaomiMimoInvalidThinkingHistoryCode,
			Retryable:     false,
			PublicMessage: xiaomiMimoInvalidThinkingHistoryMessage,
		}
	case "disabled":
		updated, err := setXiaomiMimoThinkingDisabled(body)
		return updated, false, err
	default:
		updated, err := setXiaomiMimoThinkingDisabled(body)
		return updated, err == nil, err
	}
}

func countXiaomiMimoToolCallsMissingReasoning(body []byte) int {
	missing := 0
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		toolCalls := message.Get("tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			continue
		}
		reasoning := message.Get("reasoning_content")
		if reasoning.Type != gjson.String || strings.TrimSpace(reasoning.String()) == "" {
			missing++
		}
	}
	return missing
}

func xiaomiMimoThinkingIntent(body []byte) string {
	if thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())); thinkingType != "" {
		if isDisabledThinkingType(thinkingType) {
			return "disabled"
		}
		return "enabled"
	}
	if reasoningIntent := xiaomiThinkingTypeFromReasoning(body); reasoningIntent != "" {
		if isDisabledThinkingType(reasoningIntent) {
			return "disabled"
		}
		return "enabled"
	}
	for _, path := range []string{"thinking_budget", "thinking.budget_tokens"} {
		if gjson.GetBytes(body, path).Exists() {
			return "enabled"
		}
	}
	return "default"
}

func isDisabledThinkingType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "off", "false", "disabled", "disable":
		return true
	default:
		return false
	}
}

func setXiaomiMimoThinkingDisabled(body []byte) ([]byte, error) {
	out := thinking.StripThinkingConfig(body, "openai")
	updated, err := sjson.SetBytes(out, "thinking.type", "disabled")
	if err != nil {
		return body, fmt.Errorf("set MiMo thinking.type disabled: %w", err)
	}
	return updated, nil
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

func deepSeekThinkingHistoryRequiredForRequest(body []byte, provider string) bool {
	return deepSeekThinkingHistoryIntent(body, provider) != deepSeekThinkingIntentDisabled
}

func deepSeekThinkingHistoryIntent(body []byte, provider string) deepSeekThinkingIntent {
	if thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())); thinkingType != "" {
		if isDisabledThinkingType(thinkingType) {
			return deepSeekThinkingIntentDisabled
		}
		return deepSeekThinkingIntentEnabled
	}

	effortPaths := []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"}
	if strings.EqualFold(strings.TrimSpace(provider), "claude") {
		effortPaths = []string{"output_config.effort", "reasoning.effort"}
	}
	for _, path := range effortPaths {
		effort := gjson.GetBytes(body, path)
		if !effort.Exists() {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(effort.String()))
		if value == "" {
			continue
		}
		if isDisabledThinkingType(value) {
			return deepSeekThinkingIntentDisabled
		}
		return deepSeekThinkingIntentEnabled
	}

	for _, path := range []string{"thinking_budget", "thinking.budget_tokens"} {
		budget := gjson.GetBytes(body, path)
		if !budget.Exists() {
			continue
		}
		if value, ok := deepSeekThinkingBudgetValue(budget); ok && value == 0 {
			return deepSeekThinkingIntentDisabled
		}
		return deepSeekThinkingIntentEnabled
	}
	return deepSeekThinkingIntentDefault
}

func disableDeepSeekThinkingForIncompleteHistory(body []byte, provider string) ([]byte, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "openai" && provider != "claude" {
		return body, nil
	}
	out := thinking.StripThinkingConfig(body, provider)
	updated, err := sjson.SetBytes(out, "thinking.type", "disabled")
	if err != nil {
		return body, fmt.Errorf("disable DeepSeek thinking for incomplete history: %w", err)
	}
	return updated, nil
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

func missingReasoningHistoryStatusError(err error) error {
	return missingReasoningHistoryStatusErrorWithMessage(err, deepSeekInvalidThinkingHistoryMessage)
}

func missingReasoningHistoryStatusErrorWithMessage(err error, publicMessage string) error {
	return missingReasoningHistoryStatusErrorWithCode(err, publicMessage, deepSeekInvalidThinkingHistoryCode)
}

func missingReasoningHistoryStatusErrorWithCode(err error, publicMessage, errorCode string) error {
	var missing *compathistory.MissingReasoningError
	if !errors.As(err, &missing) {
		return err
	}
	publicMessage = strings.TrimSpace(publicMessage)
	if publicMessage == "" {
		publicMessage = deepSeekInvalidThinkingHistoryMessage
	}
	errorCode = strings.TrimSpace(errorCode)
	if errorCode == "" {
		errorCode = deepSeekInvalidThinkingHistoryCode
	}
	failure := &failurecontract.Failure{
		Kind:          failurecontract.InvalidThinkingHistory,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusBadRequest,
		ProviderCode:  errorCode,
		SemanticCode:  errorCode,
		Retryable:     false,
		Cause:         missing,
		PublicMessage: publicMessage,
	}
	return statusErr{
		code:      http.StatusBadRequest,
		errorCode: errorCode,
		msg:       publicMessage,
		failure:   failure,
	}
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
