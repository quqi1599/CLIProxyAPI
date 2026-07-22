package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIThinkingHistoryRepairsFromPreviousReasoning(t *testing.T) {
	body := []byte(`{
		"reasoning_effort":"high",
		"messages":[
			{"role":"assistant","content":"plan","reasoning_content":"r1"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, _, downgraded, err := normalizeThinkingHistory(body, "openai")
	if err != nil {
		t.Fatalf("normalizeThinkingHistory() error = %v", err)
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistory() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != "r1" {
		t.Fatalf("messages.1.reasoning_content = %q, want %q", got, "r1")
	}
}

func TestNormalizeOpenAIThinkingHistoryBoundsSyntheticReasoning(t *testing.T) {
	largeReasoning := strings.Repeat("r", maxSyntheticThinkingHistoryBytes+1)
	body := []byte(fmt.Sprintf(`{
		"reasoning_effort":"high",
		"messages":[
			{"role":"assistant","content":"plan","reasoning_content":%q},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`, largeReasoning))

	out, changed, downgraded, err := normalizeThinkingHistory(body, "openai")
	if err != nil {
		t.Fatalf("normalizeThinkingHistory() error = %v", err)
	}
	if !changed || downgraded {
		t.Fatalf("normalizeThinkingHistory() changed=%v downgraded=%v", changed, downgraded)
	}
	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != "[reasoning unavailable]" {
		t.Fatalf("messages.1.reasoning_content = %q, want bounded placeholder", got)
	}
	if len(out) > len(body)+256 {
		t.Fatalf("normalized body expanded unexpectedly: input=%d output=%d", len(body), len(out))
	}
}

func TestNormalizeOpenAIThinkingHistoryDowngradesWhenUnrepairable(t *testing.T) {
	body := []byte(`{
		"reasoning_effort":"high",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, _, downgraded, err := normalizeThinkingHistory(body, "openai")
	if err != nil {
		t.Fatalf("normalizeThinkingHistory() error = %v", err)
	}
	if !downgraded {
		t.Fatalf("normalizeThinkingHistory() should downgrade thinking")
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be removed")
	}
}

func TestNormalizeOpenAIThinkingHistoryDeepSeekRepairsPlainAssistant(t *testing.T) {
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
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should change DeepSeek history")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "previous answer" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "previous answer")
	}
}

func TestNormalizeOpenAIThinkingHistoryDeepSeekSkipsWithoutThinkingRequest(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"previous answer"},
			{"role":"user","content":"continue"}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if changed || downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() changed DeepSeek history unexpectedly without thinking request: changed=%v downgraded=%v body=%s", changed, downgraded, string(out))
	}
	if gjson.GetBytes(out, "messages.0.reasoning_content").Exists() {
		t.Fatalf("messages.0.reasoning_content should not be added without thinking request")
	}
}

func TestNormalizeOpenAIThinkingHistoryDeepSeekRepairsPartialHistoryWithoutThinkingRequest(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","reasoning_content":"first plan","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"},
			{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{}"}}]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should repair partial DeepSeek history")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.2.reasoning_content").String(); got != "first plan" {
		t.Fatalf("messages.2.reasoning_content = %q, want %q", got, "first plan")
	}
}

func TestNormalizeOpenAIThinkingHistoryDeepSeekRepairsToolHistoryWithoutReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should repair DeepSeek tool history")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "[reasoning unavailable]" {
		t.Fatalf("messages.0.reasoning_content = %q, want placeholder", got)
	}
}

func TestNormalizeOpenAIThinkingHistoryDeepSeekRepairsWhenThinkingTypeEnabled(t *testing.T) {
	body := []byte(`{
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":"previous answer"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should change DeepSeek history when thinking.type is enabled")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "previous answer" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "previous answer")
	}
	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != "previous answer" {
		t.Fatalf("messages.1.reasoning_content = %q, want %q", got, "previous answer")
	}
}

func TestNormalizeOpenAIThinkingHistoryKeepsPlainAssistantForOtherModels(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"previous answer"},
			{"role":"user","content":"continue"}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "openai", "gpt-5")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if changed || downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() changed generic history unexpectedly: changed=%v downgraded=%v body=%s", changed, downgraded, string(out))
	}
	if gjson.GetBytes(out, "messages.0.reasoning_content").Exists() {
		t.Fatalf("messages.0.reasoning_content should not be added for generic models")
	}
}

func TestNormalizeClaudeThinkingHistoryRepairsFromText(t *testing.T) {
	body := []byte(`{
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"plan"},
				{"type":"tool_use","id":"toolu_1","name":"list_directory","input":{}}
			]}
		]
	}`)

	out, _, downgraded, err := normalizeThinkingHistory(body, "claude")
	if err != nil {
		t.Fatalf("normalizeThinkingHistory() error = %v", err)
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistory() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "thinking" {
		t.Fatalf("messages.0.content.0.type = %q, want %q", got, "thinking")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.thinking").String(); got != "plan" {
		t.Fatalf("messages.0.content.0.thinking = %q, want %q", got, "plan")
	}
}

func TestNormalizeClaudeThinkingHistoryDowngradesWhenUnrepairable(t *testing.T) {
	body := []byte(`{
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_1","name":"list_directory","input":{}}
			]}
		]
	}`)

	out, _, downgraded, err := normalizeThinkingHistory(body, "claude")
	if err != nil {
		t.Fatalf("normalizeThinkingHistory() error = %v", err)
	}
	if !downgraded {
		t.Fatalf("normalizeThinkingHistory() should downgrade thinking")
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking should be removed")
	}
}

func TestNormalizeClaudeThinkingHistoryDeepSeekRepairsPlainTextBlock(t *testing.T) {
	body := []byte(`{
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"previous answer"}]},
			{"role":"user","content":[{"type":"text","text":"continue"}]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "claude", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should change DeepSeek history")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "thinking" {
		t.Fatalf("messages.0.content.0.type = %q, want %q", got, "thinking")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.thinking").String(); got != "previous answer" {
		t.Fatalf("messages.0.content.0.thinking = %q, want %q", got, "previous answer")
	}
}

func TestNormalizeClaudeThinkingHistoryDeepSeekSkipsWithoutThinkingRequest(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"previous answer"}]},
			{"role":"user","content":[{"type":"text","text":"continue"}]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "claude", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if changed || downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() changed DeepSeek Claude history unexpectedly without thinking request: changed=%v downgraded=%v body=%s", changed, downgraded, string(out))
	}
	if gjson.GetBytes(out, "messages.0.content.0.thinking").Exists() {
		t.Fatalf("messages.0.content.0.thinking should not be added without thinking request")
	}
}

func TestNormalizeClaudeThinkingHistoryDeepSeekRepairsPartialHistoryWithoutThinkingRequest(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"first plan"},{"type":"tool_use","id":"call_1","name":"list_directory","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_2","name":"read_file","input":{}}]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "claude", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should repair partial DeepSeek Claude history")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.thinking").String(); got != "first plan" {
		t.Fatalf("messages.2.content.0.thinking = %q, want %q", got, "first plan")
	}
}

func TestNormalizeClaudeThinkingHistoryDeepSeekRepairsToolHistoryWithoutThinking(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"list_directory","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "claude", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should repair DeepSeek Claude tool history")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.thinking").String(); got != "[thinking unavailable]" {
		t.Fatalf("messages.0.content.0.thinking = %q, want placeholder", got)
	}
}

func TestNormalizeClaudeThinkingHistoryDeepSeekConvertsStringContent(t *testing.T) {
	body := []byte(`{
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":"previous answer"}
		]
	}`)

	out, changed, downgraded, err := normalizeThinkingHistoryForModel(body, "claude", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryForModel() error = %v", err)
	}
	if !changed {
		t.Fatalf("normalizeThinkingHistoryForModel() should change DeepSeek string content history")
	}
	if downgraded {
		t.Fatalf("normalizeThinkingHistoryForModel() downgraded unexpectedly")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.thinking").String(); got != "previous answer" {
		t.Fatalf("messages.0.content.0.thinking = %q, want %q", got, "previous answer")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.text").String(); got != "previous answer" {
		t.Fatalf("messages.0.content.1.text = %q, want %q", got, "previous answer")
	}
}

func TestNormalizeOpenAIThinkingHistorySyntheticBudgetBoundary(t *testing.T) {
	reasoning := strings.Repeat("r", maxSyntheticThinkingHistoryBytes)
	body := buildOpenAIThinkingBudgetBody(reasoning, 8)

	out, changed, downgraded, report, err := normalizeThinkingHistoryWithReport(body, "openai")
	if err != nil {
		t.Fatalf("normalizeThinkingHistoryWithReport() error = %v", err)
	}
	if !changed || downgraded {
		t.Fatalf("normalizeThinkingHistoryWithReport() changed=%v downgraded=%v", changed, downgraded)
	}
	if report.InputBytes != len(body) || report.OutputBytes != len(out) {
		t.Fatalf("report bytes = input:%d output:%d, want input:%d output:%d", report.InputBytes, report.OutputBytes, len(body), len(out))
	}
	if report.SyntheticBytes != maxSyntheticThinkingHistoryTotalBytes {
		t.Fatalf("SyntheticBytes = %d, want %d", report.SyntheticBytes, maxSyntheticThinkingHistoryTotalBytes)
	}
	if report.PatchedCount != 8 || report.DowngradeReason != "" {
		t.Fatalf("report = %+v, want 8 patches without downgrade", report)
	}
	if got := gjson.GetBytes(out, "messages.8.reasoning_content").String(); got != reasoning {
		t.Fatalf("last reasoning length = %d, want %d", len(got), len(reasoning))
	}
}

func TestNormalizeThinkingHistorySyntheticBudgetDowngradesMultipleMessages(t *testing.T) {
	reasoning := strings.Repeat("r", maxSyntheticThinkingHistoryBytes)
	tests := []struct {
		name              string
		provider          string
		body              []byte
		thinkingPath      string
		lastSyntheticPath string
		placeholder       string
	}{
		{
			name:              "openai",
			provider:          "openai",
			body:              buildOpenAIThinkingBudgetBody(reasoning, 9),
			thinkingPath:      "reasoning_effort",
			lastSyntheticPath: "messages.9.reasoning_content",
			placeholder:       openAIReasoningUnavailablePlaceholder,
		},
		{
			name:              "claude",
			provider:          "claude",
			body:              buildClaudeThinkingBudgetBody(reasoning, 9),
			thinkingPath:      "thinking",
			lastSyntheticPath: "messages.9.content.0.thinking",
			placeholder:       claudeThinkingUnavailablePlaceholder,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := bytes.Clone(tt.body)
			out, changed, downgraded, report, err := normalizeThinkingHistoryWithReport(tt.body, tt.provider)
			if err != nil {
				t.Fatalf("normalizeThinkingHistoryWithReport() error = %v", err)
			}
			if !changed || !downgraded {
				t.Fatalf("normalizeThinkingHistoryWithReport() changed=%v downgraded=%v", changed, downgraded)
			}
			if !bytes.Equal(tt.body, original) {
				t.Fatal("normalizer mutated its input")
			}
			if report.InputBytes != len(tt.body) || report.OutputBytes != len(out) {
				t.Fatalf("report bytes = input:%d output:%d, want input:%d output:%d", report.InputBytes, report.OutputBytes, len(tt.body), len(out))
			}
			if report.SyntheticBytes > maxSyntheticThinkingHistoryTotalBytes {
				t.Fatalf("SyntheticBytes = %d, exceeds %d", report.SyntheticBytes, maxSyntheticThinkingHistoryTotalBytes)
			}
			if report.PatchedCount != 9 || report.DowngradeReason != thinkingHistoryBudgetDowngradeReason {
				t.Fatalf("report = %+v, want 9 patches and budget downgrade", report)
			}
			if gjson.GetBytes(out, tt.thinkingPath).Exists() {
				t.Fatalf("%s should be stripped after budget downgrade", tt.thinkingPath)
			}
			if got := gjson.GetBytes(out, tt.lastSyntheticPath).String(); got != tt.placeholder {
				t.Fatalf("last synthetic history = %q, want %q", got, tt.placeholder)
			}

			ctx := internalpayload.WithTransformReport(context.Background(), int64(len(tt.body)))
			const stageDuration = 2 * time.Millisecond
			if errRecord := enforceThinkingHistoryTransform(ctx, tt.provider, report, stageDuration); errRecord != nil {
				t.Fatalf("enforceThinkingHistoryTransform() error = %v", errRecord)
			}
			transformReport, ok := internalpayload.TransformReportFromContext(ctx)
			if !ok || len(transformReport.Stages) != 1 {
				t.Fatalf("transform report = %#v", transformReport)
			}
			stage := transformReport.Stages[0]
			wantStage := openAIThinkingHistoryTransformStage
			if tt.provider == "claude" {
				wantStage = claudeThinkingHistoryTransformStage
			}
			if stage.Stage != wantStage || stage.InputBytes != int64(report.InputBytes) || stage.OutputBytes != int64(report.OutputBytes) || stage.SyntheticBytes != int64(report.SyntheticBytes) || stage.Duration != stageDuration {
				t.Fatalf("thinking history stage = %#v", stage)
			}
			if len(stage.AppliedPolicies) != 2 || stage.AppliedPolicies[0] != thinkingHistoryPlaceholderPolicy || stage.AppliedPolicies[1] != thinkingHistorySyntheticBudgetPolicy {
				t.Fatalf("applied policies = %v", stage.AppliedPolicies)
			}
			if len(stage.Downgrades) != 1 || stage.Downgrades[0] != thinkingHistoryBudgetDowngradeReason {
				t.Fatalf("downgrades = %v", stage.Downgrades)
			}
		})
	}
}

func TestNormalizeThinkingHistoryBudgetDowngradeIsIdempotent(t *testing.T) {
	body := buildOpenAIThinkingBudgetBody(strings.Repeat("r", maxSyntheticThinkingHistoryBytes), 9)
	original := bytes.Clone(body)

	out, changed, downgraded, firstReport, err := normalizeThinkingHistoryForModelWithReport(body, "openai", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("first normalizeThinkingHistoryForModelWithReport() error = %v", err)
	}
	if !changed || !downgraded || firstReport.DowngradeReason != thinkingHistoryBudgetDowngradeReason {
		t.Fatalf("first normalization changed=%v downgraded=%v report=%+v", changed, downgraded, firstReport)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("first normalization mutated its input")
	}

	second, changed, downgraded, secondReport, err := normalizeThinkingHistoryForModelWithReport(out, "openai", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("second normalizeThinkingHistoryForModelWithReport() error = %v", err)
	}
	if changed || downgraded || !bytes.Equal(second, out) {
		t.Fatalf("second normalization changed=%v downgraded=%v", changed, downgraded)
	}
	if secondReport.InputBytes != len(out) || secondReport.OutputBytes != len(out) || secondReport.SyntheticBytes != 0 || secondReport.PatchedCount != 0 || secondReport.PlaceholderCount != 0 || secondReport.DowngradeReason != "" {
		t.Fatalf("second report = %+v, want unchanged report", secondReport)
	}
}

func BenchmarkNormalizeOpenAIThinkingHistoryWithReport(b *testing.B) {
	reasoning := strings.Repeat("r", maxSyntheticThinkingHistoryBytes)
	for _, missingMessages := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("missing_%d", missingMessages), func(b *testing.B) {
			body := buildOpenAIThinkingBudgetBody(reasoning, missingMessages)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for range b.N {
				out, _, _, report, err := normalizeThinkingHistoryWithReport(body, "openai")
				if err != nil {
					b.Fatal(err)
				}
				benchmarkThinkingHistoryOutput = out
				benchmarkThinkingHistoryReport = report
			}
		})
	}
}

var benchmarkThinkingHistoryOutput []byte
var benchmarkThinkingHistoryReport thinkingHistoryTransformReport

func buildOpenAIThinkingBudgetBody(reasoning string, missingMessages int) []byte {
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

func buildClaudeThinkingBudgetBody(thinkingText string, missingMessages int) []byte {
	var builder strings.Builder
	builder.Grow(len(thinkingText) + missingMessages*140)
	builder.WriteString(`{"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":`)
	fmt.Fprintf(&builder, "%q", thinkingText)
	builder.WriteString(`}]}`)
	for idx := 0; idx < missingMessages; idx++ {
		fmt.Fprintf(&builder, `,{"role":"assistant","content":[{"type":"tool_use","id":"tool_%d","name":"tool","input":{}}]}`, idx)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}
