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
		{"role":"assistant","content":"copied answer","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"assistant","reasoning_content":"[reasoning unavailable]","tool_calls":[{"id":"call_2","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"assistant","reasoning_content":123,"tool_calls":[{"id":"call_3","type":"function","function":{"name":"write","arguments":"{}"}}]}
	]}`)
	report, err := Validate(body, FormatOpenAI, true)
	if err == nil {
		t.Fatal("expected missing reasoning error")
	}
	var missing *MissingReasoningError
	if !errors.As(err, &missing) {
		t.Fatalf("error type = %T", err)
	}
	if report.CheckedToolCallTurns != 3 || len(missing.MessageIndexes) != 3 || missing.MessageIndexes[0] != 0 || missing.MessageIndexes[1] != 1 || missing.MessageIndexes[2] != 2 {
		t.Fatalf("report=%+v error=%+v", report, missing)
	}
	if !strings.Contains(err.Error(), "reasoning_content") {
		t.Fatalf("error = %q", err)
	}
}

func TestValidateClaudeRequiresRealThinkingOnlyOnToolUseTurns(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"text","text":"plain answer"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"real tool reasoning"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}
	]}`)
	report, err := Validate(body, FormatClaude, true)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if report.CheckedToolCallTurns != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestValidateClaudeDoesNotCopyTextIntoThinking(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"visible answer"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}]}`)
	_, err := Validate(body, FormatClaude, true)
	if err == nil {
		t.Fatal("expected missing thinking error")
	}
	var missing *MissingReasoningError
	if !errors.As(err, &missing) || missing.Format != FormatClaude {
		t.Fatalf("error = %#v", err)
	}
	if !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("error = %q", err)
	}
}

func TestValidateIsNoopWhenUpstreamDoesNotRequireHistory(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1"}]}]}`)
	report, err := Validate(body, FormatOpenAI, false)
	if err != nil || report.CheckedToolCallTurns != 0 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
}
