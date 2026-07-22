package history

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRepairOpenAICompleteHistory(t *testing.T) {
	body := []byte(`{"reasoning_effort":"high","messages":[{"role":"assistant","content":"previous answer"},{"role":"user","content":"continue"}]}`)
	result, err := Repair(body, FormatOpenAI, true)
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if !result.Changed || result.Downgraded || result.Report.PatchedCount != 1 {
		t.Fatalf("result = %+v", result)
	}
	if got := gjson.GetBytes(result.Payload, "messages.0.reasoning_content").String(); got != "previous answer" {
		t.Fatalf("reasoning_content = %q", got)
	}
}

func TestRepairClaudeToolHistory(t *testing.T) {
	body := []byte(`{"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"text","text":"plan"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}]}`)
	result, err := Repair(body, FormatClaude, false)
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if !result.Changed || result.Downgraded || result.Report.PatchedCount != 1 {
		t.Fatalf("result = %+v", result)
	}
	if got := gjson.GetBytes(result.Payload, "messages.0.content.0.thinking").String(); got != "plan" {
		t.Fatalf("thinking = %q", got)
	}
}

func TestRepairOpenAIUnrepairableHistory(t *testing.T) {
	body := []byte(`{"reasoning_effort":"high","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`)
	result, err := Repair(body, FormatOpenAI, false)
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if !result.Changed || !result.Downgraded || result.Report.PatchedCount != 0 || result.Report.DowngradeReason != UnrepairableReason {
		t.Fatalf("result = %+v", result)
	}
	if gjson.GetBytes(result.Payload, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort was not stripped: %s", result.Payload)
	}
}

func TestRepairOpenAIUnrepairableNestedReasoning(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"high","summary":"auto"},"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`)
	result, err := Repair(body, FormatOpenAI, false)
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if !result.Changed || !result.Downgraded || result.Report.DowngradeReason != UnrepairableReason {
		t.Fatalf("result = %+v", result)
	}
	if gjson.GetBytes(result.Payload, "reasoning.effort").Exists() || gjson.GetBytes(result.Payload, "reasoning.summary").String() != "auto" {
		t.Fatalf("nested reasoning downgrade = %s", result.Payload)
	}
}

func TestRepairOpenAIBudgetDowngradeDisablesNativeThinking(t *testing.T) {
	body := buildOpenAIHistoryBody(strings.Repeat("r", MaxSyntheticItemBytes), 9)
	body, _ = sjson.DeleteBytes(body, "reasoning_effort")
	body, _ = sjson.SetBytes(body, "thinking.type", "enabled")
	result, err := Repair(body, FormatOpenAI, true)
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if !result.Changed || !result.Downgraded || result.Report.DowngradeReason != BudgetDowngradeReason {
		t.Fatalf("result = %+v", result)
	}
	if got := gjson.GetBytes(result.Payload, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, result.Payload)
	}
}

func TestRepairSyntheticBudgetIsBounded(t *testing.T) {
	body := buildOpenAIHistoryBody(strings.Repeat("r", MaxSyntheticItemBytes), 9)
	result, err := Repair(body, FormatOpenAI, false)
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if !result.Changed || !result.Downgraded || result.Report.PatchedCount != 9 || result.Report.DowngradeReason != BudgetDowngradeReason {
		t.Fatalf("result = %+v", result)
	}
	if result.Report.SyntheticBytes > MaxSyntheticTotalBytes {
		t.Fatalf("synthetic bytes = %d", result.Report.SyntheticBytes)
	}
	if got := gjson.GetBytes(result.Payload, "messages.9.reasoning_content").String(); got != OpenAIUnavailableValue {
		t.Fatalf("last reasoning_content = %q", got)
	}
}

func TestRepairUnknownFormatIsNoop(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":"unchanged"}]}`)
	result, err := Repair(body, Format("unknown"), true)
	if err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if result.Changed || result.Downgraded || result.Report.PatchedCount != 0 || !bytes.Equal(result.Payload, body) {
		t.Fatalf("result = %+v", result)
	}
}

func BenchmarkRepairOpenAIHistory(b *testing.B) {
	reasoning := strings.Repeat("r", MaxSyntheticItemBytes)
	for _, missingMessages := range []int{0, 16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("items_%d", missingMessages), func(b *testing.B) {
			body := buildOpenAIHistoryBody(reasoning, missingMessages)
			baseline, err := Repair(body, FormatOpenAI, false)
			invalidPatchedCount := baseline.Report.PatchedCount < 0 || baseline.Report.PatchedCount > missingMessages || missingMessages > 0 && baseline.Report.PatchedCount == 0
			if err != nil || invalidPatchedCount || baseline.Report.SyntheticBytes > MaxSyntheticTotalBytes {
				b.Fatalf("baseline report = %+v for %d items; error=%v", baseline.Report, missingMessages, err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for range b.N {
				result, err := Repair(body, FormatOpenAI, false)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkResult = result
			}
		})
	}
}

var benchmarkResult Result

func buildOpenAIHistoryBody(reasoning string, missingMessages int) []byte {
	var builder strings.Builder
	builder.Grow(len(reasoning) + missingMessages*160)
	builder.WriteString(`{"reasoning_effort":"high","messages":[{"role":"assistant","content":"plan","reasoning_content":`)
	fmt.Fprintf(&builder, "%q", reasoning)
	builder.WriteByte('}')
	for idx := 0; idx < missingMessages; idx++ {
		fmt.Fprintf(&builder, `,{"role":"assistant","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"tool","arguments":"{}"}}]}`, idx)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}
