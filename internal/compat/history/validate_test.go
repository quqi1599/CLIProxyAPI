package history

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateOpenAIOnlyRequiresReasoningOnToolCallTurns(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":"plain answer"},
		{"role":"assistant","reasoning_content":"real tool reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"}
	]}`)
	report, err := Validate(body, FormatOpenAI, true)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if report.CheckedToolCallTurns != 1 || report.InputBytes != len(body) || report.OutputBytes != len(body) {
		t.Fatalf("report = %+v", report)
	}
}

func TestValidateOpenAIRejectsMissingOrLegacyPlaceholderReasoning(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":"copied answer","tool_calls":[{"id":"call_1"}]},
		{"role":"assistant","reasoning_content":"[reasoning unavailable]","tool_calls":[{"id":"call_2"}]}
	]}`)
	report, err := Validate(body, FormatOpenAI, true)
	var missing *MissingReasoningError
	if err == nil || !errors.As(err, &missing) {
		t.Fatalf("report=%+v error=%v", report, err)
	}
	if report.CheckedToolCallTurns != 2 || len(missing.MessageIndexes) != 2 || !strings.Contains(err.Error(), "reasoning_content") {
		t.Fatalf("report=%+v error=%v", report, err)
	}
}

func TestValidateClaudeRequiresRealThinkingOnToolUseTurns(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"text","text":"plain answer"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"real tool reasoning"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}
	]}`)
	report, err := Validate(body, FormatClaude, true)
	if err != nil || report.CheckedToolCallTurns != 1 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
}

func TestValidateClaudeDoesNotCopyTextIntoThinking(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"visible answer"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}]}`)
	_, err := Validate(body, FormatClaude, true)
	var missing *MissingReasoningError
	if err == nil || !errors.As(err, &missing) || missing.Format != FormatClaude || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("error = %#v", err)
	}
}
