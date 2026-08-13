// Package history validates reasoning history required by upstream providers.
package history

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const maxReportedMissingIndexes = 8

// Format identifies the canonical history representation to validate.
type Format string

const (
	FormatOpenAI Format = "openai"
	FormatClaude Format = "claude"
)

// Report contains payload-free accounting for one history validation.
type Report struct {
	InputBytes           int
	OutputBytes          int
	CheckedToolCallTurns int
}

// MissingReasoningError reports assistant tool-call turns that do not contain
// the original reasoning field required by the upstream protocol.
type MissingReasoningError struct {
	Format         Format
	MessageIndexes []int
}

func (e *MissingReasoningError) Error() string {
	if e == nil {
		return "missing real reasoning history"
	}
	field := "reasoning_content"
	if e.Format == FormatClaude {
		field = "thinking"
	}
	indexes := make([]string, 0, min(len(e.MessageIndexes), maxReportedMissingIndexes))
	for _, index := range e.MessageIndexes {
		if len(indexes) >= maxReportedMissingIndexes {
			break
		}
		indexes = append(indexes, strconv.Itoa(index))
	}
	suffix := ""
	if remaining := len(e.MessageIndexes) - len(indexes); remaining > 0 {
		suffix = fmt.Sprintf(" and %d more", remaining)
	}
	return fmt.Sprintf("assistant tool-call history is missing original %s at message indexes [%s]%s", field, strings.Join(indexes, ","), suffix)
}

// Validate checks only assistant turns that contain tool calls. It never adds,
// copies, or replaces reasoning content. Validation is disabled when the
// selected upstream does not require returned tool-call reasoning history.
func Validate(body []byte, format Format, requireToolCallReasoning bool) (Report, error) {
	report := Report{InputBytes: len(body), OutputBytes: len(body)}
	if !requireToolCallReasoning || len(body) == 0 || !gjson.ValidBytes(body) {
		return report, nil
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return report, nil
	}

	format = Format(strings.ToLower(strings.TrimSpace(string(format))))
	missing := make([]int, 0)
	for index, message := range messages.Array() {
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		switch format {
		case FormatOpenAI:
			if !hasOpenAIToolCalls(message) {
				continue
			}
			report.CheckedToolCallTurns++
			if !isRealReasoningValue(message.Get("reasoning_content"), "[reasoning unavailable]") {
				missing = append(missing, index)
			}
		case FormatClaude:
			if !hasClaudeToolUse(message) {
				continue
			}
			report.CheckedToolCallTurns++
			if !hasRealClaudeThinking(message) {
				missing = append(missing, index)
			}
		default:
			return report, nil
		}
	}
	if len(missing) > 0 {
		return report, &MissingReasoningError{Format: format, MessageIndexes: missing}
	}
	return report, nil
}

func hasOpenAIToolCalls(message gjson.Result) bool {
	toolCalls := message.Get("tool_calls")
	return toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0
}

func hasClaudeToolUse(message gjson.Result) bool {
	content := message.Get("content")
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "tool_use") {
			return true
		}
	}
	return false
}

func hasRealClaudeThinking(message gjson.Result) bool {
	for _, part := range message.Get("content").Array() {
		if !strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "thinking") {
			continue
		}
		if isRealReasoningValue(part.Get("thinking"), "[thinking unavailable]") {
			return true
		}
	}
	return false
}

func isRealReasoningValue(value gjson.Result, legacyPlaceholder string) bool {
	if !value.Exists() || value.Type != gjson.String {
		return false
	}
	text := strings.TrimSpace(value.String())
	return text != "" && !strings.EqualFold(text, legacyPlaceholder)
}
