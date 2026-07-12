package executor

import (
	"testing"

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
