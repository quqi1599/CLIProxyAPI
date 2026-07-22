// Package history repairs reasoning history required by upstream providers.
package history

import (
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// MaxSyntheticItemBytes bounds one copied reasoning or thinking value.
	MaxSyntheticItemBytes = 8 * 1024
	// MaxSyntheticTotalBytes bounds all synthetic history added to one request.
	MaxSyntheticTotalBytes = 64 * 1024

	BudgetDowngradeReason  = "synthetic_history_budget_exceeded"
	UnrepairableReason     = "unrepairable_history"
	OpenAIUnavailableValue = "[reasoning unavailable]"
	ClaudeUnavailableValue = "[thinking unavailable]"
)

// Format identifies the canonical history representation to repair.
type Format string

const (
	FormatOpenAI Format = "openai"
	FormatClaude Format = "claude"
)

// Report contains payload-free accounting for one history repair.
type Report struct {
	InputBytes       int
	OutputBytes      int
	SyntheticBytes   int
	PatchedCount     int
	PlaceholderCount int
	DowngradeReason  string
}

// Result contains the repaired payload and its bounded accounting metadata.
type Result struct {
	Payload    []byte
	Changed    bool
	Downgraded bool
	Report     Report
}

type syntheticBudget struct {
	used         int
	exceeded     bool
	placeholders int
}

func (b *syntheticBudget) add(value, placeholder string, moreMessages bool) (string, bool) {
	if value == "" {
		return "", false
	}
	if b.exceeded {
		value = placeholder
	}
	if len(value) > MaxSyntheticItemBytes {
		value = placeholder
	}

	limit := MaxSyntheticTotalBytes
	if moreMessages && value != placeholder {
		limit -= len(placeholder)
	}
	if b.used+len(value) > limit {
		b.exceeded = true
		value = placeholder
	}
	if b.used+len(value) > MaxSyntheticTotalBytes {
		return "", false
	}
	b.used += len(value)
	if value == placeholder {
		b.placeholders++
	}
	return value, true
}

// Repair applies the format-specific history algorithm. The caller owns the
// provider capability decision expressed by requireCompleteHistory.
func Repair(body []byte, format Format, requireCompleteHistory bool) (Result, error) {
	switch Format(strings.ToLower(strings.TrimSpace(string(format)))) {
	case FormatOpenAI:
		return repairOpenAI(body, requireCompleteHistory)
	case FormatClaude:
		return repairClaude(body, requireCompleteHistory)
	default:
		return unchanged(body), nil
	}
}

func unchanged(body []byte) Result {
	return Result{
		Payload: body,
		Report: Report{
			InputBytes:  len(body),
			OutputBytes: len(body),
		},
	}
}

func repairOpenAI(body []byte, requireCompleteHistory bool) (Result, error) {
	result := unchanged(body)
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return result, nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return result, nil
	}

	messageItems := messages.Array()
	normalizedMessages := make([]string, len(messageItems))
	for idx, message := range messageItems {
		normalizedMessages[idx] = message.Raw
	}
	latestReasoning := ""
	latestReasoningAvailable := false
	patched := 0
	unrepaired := 0
	budget := syntheticBudget{}

	for idx, message := range messageItems {
		if strings.TrimSpace(message.Get("role").String()) != "assistant" {
			continue
		}
		reasoning := strings.TrimSpace(message.Get("reasoning_content").String())
		if reasoning != "" {
			latestReasoning = bounded(reasoning)
			latestReasoningAvailable = true
		}
		hasToolCalls := false
		if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0 {
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
			if assistantText := openAIAssistantText(message); assistantText != "" {
				fallback = bounded(assistantText)
				fallbackAvailable = true
			}
		}
		if fallback == "" && (requireCompleteHistory || fallbackAvailable) {
			fallback = OpenAIUnavailableValue
		}
		if fallback == "" {
			unrepaired++
			continue
		}
		fallback, ok := budget.add(fallback, OpenAIUnavailableValue, idx+1 < len(messageItems))
		if !ok {
			unrepaired++
			continue
		}
		next, err := sjson.SetBytes([]byte(message.Raw), "reasoning_content", fallback)
		if err != nil {
			result.Report.SyntheticBytes = budget.used
			result.Report.PatchedCount = patched
			result.Report.PlaceholderCount = budget.placeholders
			return result, err
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
			result.Report.SyntheticBytes = budget.used
			result.Report.PatchedCount = patched
			result.Report.PlaceholderCount = budget.placeholders
			return result, err
		}
	}

	downgraded := budget.exceeded
	if budget.exceeded {
		result.Report.DowngradeReason = BudgetDowngradeReason
	}
	if (budget.exceeded || unrepaired > 0) && openAIThinkingEnabled(out) {
		out = thinking.StripThinkingConfig(out, "openai")
		out = disableOpenAIThinkingType(out)
		downgraded = true
		if result.Report.DowngradeReason == "" {
			result.Report.DowngradeReason = UnrepairableReason
		}
	}
	result.Payload = out
	result.Changed = patched > 0 || downgraded
	result.Downgraded = downgraded
	result.Report.OutputBytes = len(out)
	result.Report.SyntheticBytes = budget.used
	result.Report.PatchedCount = patched
	result.Report.PlaceholderCount = budget.placeholders
	if result.Changed {
		log.WithFields(log.Fields{
			"patched_reasoning_messages": patched,
			"downgraded_thinking":        downgraded,
			"synthetic_history_bytes":    result.Report.SyntheticBytes,
			"downgrade_reason":           result.Report.DowngradeReason,
		}).Debug("executor: normalized openai thinking history")
	}
	return result, nil
}

func repairClaude(body []byte, requireCompleteHistory bool) (Result, error) {
	result := unchanged(body)
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return result, nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return result, nil
	}

	messageItems := messages.Array()
	normalizedMessages := make([]string, len(messageItems))
	for idx, message := range messageItems {
		normalizedMessages[idx] = message.Raw
	}
	latestThinking := ""
	latestThinkingAvailable := false
	patched := 0
	unrepaired := 0
	budget := syntheticBudget{}

	for idx, message := range messageItems {
		if strings.TrimSpace(message.Get("role").String()) != "assistant" {
			continue
		}
		content := message.Get("content")
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
				fallback = bounded(text)
				fallbackAvailable = true
			}
			if fallback == "" && (requireCompleteHistory || fallbackAvailable) {
				fallback = ClaudeUnavailableValue
			}
			fallback, ok := budget.add(fallback, ClaudeUnavailableValue, idx+1 < len(messageItems))
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
			next, err := sjson.SetRawBytes([]byte(message.Raw), "content", internalpayload.BuildRaw(rebuiltItems))
			if err != nil {
				result.Report.SyntheticBytes = budget.used
				result.Report.PatchedCount = patched
				result.Report.PlaceholderCount = budget.placeholders
				return result, err
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
					latestThinking = bounded(thinkingText)
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
			fallback = bounded(strings.Join(textParts, "\n"))
			fallbackAvailable = true
		}
		if fallback == "" && (requireCompleteHistory || fallbackAvailable) {
			fallback = ClaudeUnavailableValue
		}
		if fallback == "" {
			unrepaired++
			continue
		}
		fallback, ok := budget.add(fallback, ClaudeUnavailableValue, idx+1 < len(messageItems))
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
		next, err := sjson.SetRawBytes([]byte(message.Raw), "content", internalpayload.BuildRaw(rebuiltItems))
		if err != nil {
			result.Report.SyntheticBytes = budget.used
			result.Report.PatchedCount = patched
			result.Report.PlaceholderCount = budget.placeholders
			return result, err
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
			result.Report.SyntheticBytes = budget.used
			result.Report.PatchedCount = patched
			result.Report.PlaceholderCount = budget.placeholders
			return result, err
		}
	}

	downgraded := budget.exceeded
	if budget.exceeded {
		result.Report.DowngradeReason = BudgetDowngradeReason
	}
	if (budget.exceeded || unrepaired > 0) && claudeThinkingEnabled(out) {
		out = thinking.StripThinkingConfig(out, "claude")
		downgraded = true
		if result.Report.DowngradeReason == "" {
			result.Report.DowngradeReason = UnrepairableReason
		}
	}
	result.Payload = out
	result.Changed = patched > 0 || downgraded
	result.Downgraded = downgraded
	result.Report.OutputBytes = len(out)
	result.Report.SyntheticBytes = budget.used
	result.Report.PatchedCount = patched
	result.Report.PlaceholderCount = budget.placeholders
	if result.Changed {
		log.WithFields(log.Fields{
			"patched_thinking_messages": patched,
			"downgraded_thinking":       downgraded,
			"synthetic_history_bytes":   result.Report.SyntheticBytes,
			"downgrade_reason":          result.Report.DowngradeReason,
		}).Debug("executor: normalized claude thinking history")
	}
	return result, nil
}

func bounded(value string) string {
	if len(value) > MaxSyntheticItemBytes {
		return ""
	}
	return value
}

func openAIAssistantText(message gjson.Result) string {
	content := message.Get("content")
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	if !content.IsArray() {
		return ""
	}
	parts := make([]string, 0, len(content.Array()))
	for _, item := range content.Array() {
		text := strings.TrimSpace(item.Get("text").String())
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func openAIThinkingEnabled(body []byte) bool {
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "disabled", "disable", "none", "off", "false":
		return false
	case "enabled", "enable", "auto", "adaptive", "true", "low", "medium", "high", "max":
		return true
	}
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"} {
		if strings.TrimSpace(gjson.GetBytes(body, path).String()) != "" {
			return true
		}
	}
	return false
}

func disableOpenAIThinkingType(body []byte) []byte {
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "enable", "auto", "adaptive", "true", "low", "medium", "high", "max":
		if updated, err := sjson.SetBytes(body, "thinking.type", "disabled"); err == nil {
			return updated
		}
	}
	return body
}

func claudeThinkingEnabled(body []byte) bool {
	thinkingType := strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())
	if thinkingType != "" && thinkingType != "disabled" {
		return true
	}
	return strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String()) != ""
}
