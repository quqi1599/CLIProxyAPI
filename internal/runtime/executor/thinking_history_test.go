package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

func TestNormalizeThinkingHistoryDeepSeekIgnoresPlainAssistantTurn(t *testing.T) {
	body := []byte(`{
		"reasoning_effort":"high",
		"messages":[
			{"role":"assistant","content":"previous answer"},
			{"role":"user","content":"continue"}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if changed || downgraded || !bytes.Equal(out, body) {
		t.Fatalf("plain assistant turn changed=%v downgraded=%v body=%s", changed, downgraded, out)
	}
}

func TestNormalizeThinkingHistoryDeepSeekKeepsRealOpenAIReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","reasoning_content":"original reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		]
	}`)

	out, changed, downgraded, report, err := normalizeThinkingHistoryForModelWithReport(body, "openai", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModelWithReport() error = %v", err)
	}
	if changed || downgraded || !bytes.Equal(out, body) {
		t.Fatalf("real reasoning changed=%v downgraded=%v body=%s", changed, downgraded, out)
	}
	if report.CheckedToolCallTurns != 1 || report.InputBytes != len(body) || report.OutputBytes != len(body) {
		t.Fatalf("report = %+v", report)
	}
}

func TestNormalizeThinkingHistoryDeepSeekRejectsMissingOpenAIReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"visible answer","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "deepseek-v4-pro")
	if changed || downgraded || !bytes.Equal(out, body) {
		t.Fatalf("invalid history changed=%v downgraded=%v body=%s", changed, downgraded, out)
	}
	assertMissingReasoningHistoryError(t, err, "reasoning_content")
}

func TestNormalizeThinkingHistoryDeepSeekRejectsMissingClaudeThinking(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"visible answer"},
				{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}
			]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "claude", "deepseek-v4-flash")
	if changed || downgraded || !bytes.Equal(out, body) {
		t.Fatalf("invalid history changed=%v downgraded=%v body=%s", changed, downgraded, out)
	}
	assertMissingReasoningHistoryError(t, err, "thinking block")
}

func TestNormalizeThinkingHistoryDoesNotEnforceDeepSeekRulesForOtherModels(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "gpt-5")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if changed || downgraded || !bytes.Equal(out, body) {
		t.Fatalf("generic history changed=%v downgraded=%v body=%s", changed, downgraded, out)
	}
}

func TestEnforceThinkingHistoryTransformReportsValidationWithoutMutation(t *testing.T) {
	ctx, releaseReport := retainExecutorTransformReport(context.Background(), 128)
	defer releaseReport()
	report := thinkingHistoryTransformReport{InputBytes: 128, OutputBytes: 128, CheckedToolCallTurns: 2}

	if err := enforceThinkingHistoryTransform(ctx, "openai", report, time.Millisecond); err != nil {
		t.Fatalf("enforceThinkingHistoryTransform() error = %v", err)
	}
	transformReport, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || len(transformReport.Stages) != 1 {
		t.Fatalf("transform report = %#v", transformReport)
	}
	stage := transformReport.Stages[0]
	if stage.Stage != openAIThinkingHistoryTransformStage || !stage.ReusedInput || stage.SyntheticBytes != 0 || stage.PatchedCount != 0 {
		t.Fatalf("thinking history stage = %#v", stage)
	}
	if !containsTransformMetadataID(stage.AppliedPolicies, thinkingHistoryValidationPolicy) {
		t.Fatalf("applied policies = %v", stage.AppliedPolicies)
	}
}

func assertMissingReasoningHistoryError(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected missing reasoning history error")
	}
	var status statusErr
	if !errors.As(err, &status) {
		t.Fatalf("error type = %T, want statusErr: %v", err, err)
	}
	if status.StatusCode() != http.StatusBadRequest || status.ErrorCode() != "missing_reasoning_history" {
		t.Fatalf("status = %d code = %q", status.StatusCode(), status.ErrorCode())
	}
	for _, fragment := range []string{"missing_real_reasoning_history", wantField, "will not fabricate"} {
		if !strings.Contains(status.Error(), fragment) {
			t.Fatalf("error = %q, want fragment %q", status.Error(), fragment)
		}
	}
}
