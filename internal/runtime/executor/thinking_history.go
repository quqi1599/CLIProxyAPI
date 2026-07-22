package executor

import (
	"context"
	"strings"
	"time"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Synthetic history only satisfies upstream compatibility checks. Bounding it
// prevents one large reasoning block from being copied into every later turn.
const (
	maxSyntheticThinkingHistoryBytes      = 8 * 1024
	maxSyntheticThinkingHistoryTotalBytes = 64 * 1024

	thinkingHistoryBudgetDowngradeReason  = "synthetic_history_budget_exceeded"
	thinkingHistoryUnrepairableReason     = "unrepairable_history"
	thinkingHistorySyntheticBudgetPolicy  = "thinking_history.synthetic_budget"
	thinkingHistoryPlaceholderPolicy      = "thinking_history.placeholder"
	openAIThinkingHistoryTransformStage   = "normalize.thinking_history.openai"
	claudeThinkingHistoryTransformStage   = "normalize.thinking_history.claude"
	openAIReasoningUnavailablePlaceholder = "[reasoning unavailable]"
	claudeThinkingUnavailablePlaceholder  = "[thinking unavailable]"
)

type thinkingHistoryTransformReport struct {
	InputBytes       int
	OutputBytes      int
	SyntheticBytes   int
	PatchedCount     int
	PlaceholderCount int
	DowngradeReason  string
}

type syntheticThinkingHistoryBudget struct {
	used         int
	exceeded     bool
	placeholders int
}

func (b *syntheticThinkingHistoryBudget) add(value, placeholder string, moreMessages bool) (string, bool) {
	if value == "" {
		return "", false
	}
	if b.exceeded {
		value = placeholder
	}
	if len(value) > maxSyntheticThinkingHistoryBytes {
		value = placeholder
	}

	limit := maxSyntheticThinkingHistoryTotalBytes
	if moreMessages && value != placeholder {
		limit -= len(placeholder)
	}
	if b.used+len(value) > limit {
		b.exceeded = true
		value = placeholder
	}
	if b.used+len(value) > maxSyntheticThinkingHistoryTotalBytes {
		return "", false
	}
	b.used += len(value)
	if value == placeholder {
		b.placeholders++
	}
	return value, true
}

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
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		if !strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "assistant") {
			continue
		}
		if strings.TrimSpace(msg.Get("reasoning_content").String()) != "" {
			return true
		}
		if toolCalls := msg.Get("tool_calls"); toolCalls.IsArray() && len(toolCalls.Array()) > 0 {
			return true
		}
	}
	return false
}

func claudeHistoryNeedsThinkingNormalization(body []byte) bool {
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		if !strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "assistant") {
			continue
		}
		for _, part := range msg.Get("content").Array() {
			if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "thinking") &&
				strings.TrimSpace(part.Get("thinking").String()) != "" {
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
	report := thinkingHistoryTransformReport{InputBytes: len(body), OutputBytes: len(body)}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false, false, report, nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, false, false, report, nil
	}

	messageItems := messages.Array()
	normalizedMessages := make([]string, len(messageItems))
	for idx, msg := range messageItems {
		normalizedMessages[idx] = msg.Raw
	}
	latestReasoning := ""
	latestReasoningAvailable := false
	patched := 0
	unrepaired := 0
	budget := syntheticThinkingHistoryBudget{}

	for idx, msg := range messageItems {
		if strings.TrimSpace(msg.Get("role").String()) != "assistant" {
			continue
		}
		reasoning := strings.TrimSpace(msg.Get("reasoning_content").String())
		if reasoning != "" {
			latestReasoning = boundedSyntheticThinkingHistory(reasoning)
			latestReasoningAvailable = true
		}
		hasToolCalls := false
		if toolCalls := msg.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0 {
			hasToolCalls = true
		}
		if !requireCompleteHistory && !hasToolCalls {
			continue
		}
		if reasoning != "" {
			continue
		}
		fallback := latestReasoning
		fallbackAvailable := latestReasoningAvailable
		if fallback == "" {
			if assistantText := assistantOpenAIText(msg); assistantText != "" {
				fallback = boundedSyntheticThinkingHistory(assistantText)
				fallbackAvailable = true
			}
		}
		if fallback == "" && (requireCompleteHistory || fallbackAvailable) {
			fallback = openAIReasoningUnavailablePlaceholder
		}
		if fallback == "" {
			unrepaired++
			continue
		}
		fallback, ok := budget.add(fallback, openAIReasoningUnavailablePlaceholder, idx+1 < len(messageItems))
		if !ok {
			unrepaired++
			continue
		}
		next, err := sjson.SetBytes([]byte(msg.Raw), "reasoning_content", fallback)
		if err != nil {
			report.SyntheticBytes = budget.used
			report.PatchedCount = patched
			report.PlaceholderCount = budget.placeholders
			return body, false, false, report, err
		}
		normalizedMessages[idx] = string(next)
		latestReasoning = fallback
		latestReasoningAvailable = true
		patched++
	}

	out := body
	if patched > 0 {
		var err error
		out, err = sjson.SetRawBytes(body, "messages", internalpayload.BuildRaw(normalizedMessages))
		if err != nil {
			report.SyntheticBytes = budget.used
			report.PatchedCount = patched
			report.PlaceholderCount = budget.placeholders
			return body, false, false, report, err
		}
	}

	downgraded := budget.exceeded
	if budget.exceeded {
		report.DowngradeReason = thinkingHistoryBudgetDowngradeReason
	}
	if (budget.exceeded || unrepaired > 0) && openAIThinkingEnabled(out) {
		out = thinking.StripThinkingConfig(out, "openai")
		downgraded = true
		if report.DowngradeReason == "" {
			report.DowngradeReason = thinkingHistoryUnrepairableReason
		}
	}
	report.OutputBytes = len(out)
	report.SyntheticBytes = budget.used
	report.PatchedCount = patched
	report.PlaceholderCount = budget.placeholders
	if patched > 0 || downgraded {
		log.WithFields(log.Fields{
			"patched_reasoning_messages": patched,
			"downgraded_thinking":        downgraded,
			"synthetic_history_bytes":    report.SyntheticBytes,
			"downgrade_reason":           report.DowngradeReason,
		}).Debug("executor: normalized openai thinking history")
	}
	return out, patched > 0 || downgraded, downgraded, report, nil
}

func normalizeClaudeThinkingHistory(body []byte, requireCompleteHistory bool) ([]byte, bool, bool, error) {
	out, changed, downgraded, _, err := normalizeClaudeThinkingHistoryWithReport(body, requireCompleteHistory)
	return out, changed, downgraded, err
}

func normalizeClaudeThinkingHistoryWithReport(body []byte, requireCompleteHistory bool) ([]byte, bool, bool, thinkingHistoryTransformReport, error) {
	report := thinkingHistoryTransformReport{InputBytes: len(body), OutputBytes: len(body)}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false, false, report, nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, false, false, report, nil
	}

	messageItems := messages.Array()
	normalizedMessages := make([]string, len(messageItems))
	for idx, msg := range messageItems {
		normalizedMessages[idx] = msg.Raw
	}
	latestThinking := ""
	latestThinkingAvailable := false
	patched := 0
	unrepaired := 0
	budget := syntheticThinkingHistoryBudget{}

	for idx, msg := range messageItems {
		if strings.TrimSpace(msg.Get("role").String()) != "assistant" {
			continue
		}
		content := msg.Get("content")
		if !content.Exists() {
			continue
		}
		if content.Type == gjson.String {
			if !requireCompleteHistory {
				continue
			}
			fallback := latestThinking
			fallbackAvailable := latestThinkingAvailable
			text := strings.TrimSpace(content.String())
			if fallback == "" && text != "" {
				fallback = boundedSyntheticThinkingHistory(text)
				fallbackAvailable = true
			}
			if fallback == "" && (requireCompleteHistory || fallbackAvailable) {
				fallback = claudeThinkingUnavailablePlaceholder
			}
			fallback, ok := budget.add(fallback, claudeThinkingUnavailablePlaceholder, idx+1 < len(messageItems))
			if !ok {
				unrepaired++
				continue
			}
			block := []byte(`{"type":"thinking","thinking":""}`)
			block, _ = sjson.SetBytes(block, "thinking", fallback)
			rebuiltItems := []string{string(block)}
			if text != "" {
				textBlock := []byte(`{"type":"text","text":""}`)
				textBlock, _ = sjson.SetBytes(textBlock, "text", text)
				rebuiltItems = append(rebuiltItems, string(textBlock))
			}
			next, err := sjson.SetRawBytes([]byte(msg.Raw), "content", internalpayload.BuildRaw(rebuiltItems))
			if err != nil {
				report.SyntheticBytes = budget.used
				report.PatchedCount = patched
				report.PlaceholderCount = budget.placeholders
				return body, false, false, report, err
			}
			normalizedMessages[idx] = string(next)
			latestThinking = fallback
			latestThinkingAvailable = true
			patched++
			continue
		}
		if !content.IsArray() {
			continue
		}

		hasToolUse := false
		hasThinking := false
		textParts := make([]string, 0, len(content.Array()))
		for _, part := range content.Array() {
			switch strings.TrimSpace(part.Get("type").String()) {
			case "thinking":
				thinkingText := strings.TrimSpace(part.Get("thinking").String())
				if thinkingText != "" {
					latestThinking = boundedSyntheticThinkingHistory(thinkingText)
					latestThinkingAvailable = true
					hasThinking = true
				}
			case "text":
				text := strings.TrimSpace(part.Get("text").String())
				if text != "" {
					textParts = append(textParts, text)
				}
			case "tool_use":
				hasToolUse = true
			}
		}
		if hasThinking {
			continue
		}
		if !requireCompleteHistory && !hasToolUse {
			continue
		}
		fallback := latestThinking
		fallbackAvailable := latestThinkingAvailable
		if fallback == "" && len(textParts) > 0 {
			fallback = boundedSyntheticThinkingHistory(strings.Join(textParts, "\n"))
			fallbackAvailable = true
		}
		if fallback == "" && (requireCompleteHistory || fallbackAvailable) {
			fallback = claudeThinkingUnavailablePlaceholder
		}
		if fallback == "" {
			unrepaired++
			continue
		}
		fallback, ok := budget.add(fallback, claudeThinkingUnavailablePlaceholder, idx+1 < len(messageItems))
		if !ok {
			unrepaired++
			continue
		}
		block := []byte(`{"type":"thinking","thinking":""}`)
		block, _ = sjson.SetBytes(block, "thinking", fallback)
		rebuiltItems := make([]string, 0, len(content.Array())+1)
		rebuiltItems = append(rebuiltItems, string(block))
		for _, part := range content.Array() {
			rebuiltItems = append(rebuiltItems, part.Raw)
		}
		next, err := sjson.SetRawBytes([]byte(msg.Raw), "content", internalpayload.BuildRaw(rebuiltItems))
		if err != nil {
			report.SyntheticBytes = budget.used
			report.PatchedCount = patched
			report.PlaceholderCount = budget.placeholders
			return body, false, false, report, err
		}
		normalizedMessages[idx] = string(next)
		latestThinking = fallback
		latestThinkingAvailable = true
		patched++
	}

	out := body
	if patched > 0 {
		var err error
		out, err = sjson.SetRawBytes(body, "messages", internalpayload.BuildRaw(normalizedMessages))
		if err != nil {
			report.SyntheticBytes = budget.used
			report.PatchedCount = patched
			report.PlaceholderCount = budget.placeholders
			return body, false, false, report, err
		}
	}

	downgraded := budget.exceeded
	if budget.exceeded {
		report.DowngradeReason = thinkingHistoryBudgetDowngradeReason
	}
	if (budget.exceeded || unrepaired > 0) && claudeThinkingEnabled(out) {
		out = thinking.StripThinkingConfig(out, "claude")
		downgraded = true
		if report.DowngradeReason == "" {
			report.DowngradeReason = thinkingHistoryUnrepairableReason
		}
	}
	report.OutputBytes = len(out)
	report.SyntheticBytes = budget.used
	report.PatchedCount = patched
	report.PlaceholderCount = budget.placeholders
	if patched > 0 || downgraded {
		log.WithFields(log.Fields{
			"patched_thinking_messages": patched,
			"downgraded_thinking":       downgraded,
			"synthetic_history_bytes":   report.SyntheticBytes,
			"downgrade_reason":          report.DowngradeReason,
		}).Debug("executor: normalized claude thinking history")
	}
	return out, patched > 0 || downgraded, downgraded, report, nil
}

func boundedSyntheticThinkingHistory(value string) string {
	if len(value) > maxSyntheticThinkingHistoryBytes {
		return ""
	}
	return value
}

func assistantOpenAIText(msg gjson.Result) string {
	content := msg.Get("content")
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	if !content.IsArray() {
		return ""
	}
	parts := make([]string, 0, len(content.Array()))
	for _, item := range content.Array() {
		text := strings.TrimSpace(item.Get("text").String())
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
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
