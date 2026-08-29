package contentaudit

import (
	"slices"
	"strings"
	"testing"
)

func TestExtractJSONRequestScansOnlyUntrustedConversationContent(t *testing.T) {
	body := []byte(`{
  "model": "gpt-test",
  "instructions": "trusted root instruction",
  "system_instruction": {"parts": [{"text": "trusted gemini system text"}]},
  "messages": [
    {"role": "system", "content": "trusted system text"},
    {"role": "developer", "content": "trusted developer text"},
    {"role": "assistant", "content": "trusted assistant text", "tool_calls": [{"type": "function", "function": {"name": "run", "arguments": "untrusted dynamic tool arguments"}}]},
    {"role": "model", "parts": [{"text": "trusted gemini model text"}]},
    {"role": "user", "content": "untrusted user text"},
    {"role": "tool", "content": "untrusted tool result"},
    {"content": "untrusted missing-role text"}
  ]
}`)
	extracted := ExtractJSONRequest(body)
	for _, expected := range []string{"untrusted user text", "untrusted tool result", "untrusted missing-role text", "untrusted dynamic tool arguments"} {
		if !strings.Contains(extracted.Text, expected) {
			t.Fatalf("extracted text %q does not contain %q", extracted.Text, expected)
		}
	}
	for _, excluded := range []string{"trusted root instruction", "trusted gemini system text", "trusted system text", "trusted developer text", "trusted assistant text", "trusted gemini model text"} {
		if strings.Contains(extracted.Text, excluded) {
			t.Fatalf("extracted text %q contains trusted text %q", extracted.Text, excluded)
		}
	}
	if extracted.EnforcementText != "untrusted missing-role text" {
		t.Fatalf("enforcement text = %q, want latest user-like content", extracted.EnforcementText)
	}
	for _, excluded := range []string{"untrusted tool result", "untrusted dynamic tool arguments"} {
		if strings.Contains(extracted.EnforcementText, excluded) {
			t.Fatalf("enforcement text %q contains non-user content %q", extracted.EnforcementText, excluded)
		}
	}
	for term, want := range map[string][]string{
		"untrusted user text":              {"user"},
		"untrusted tool result":            {"tool"},
		"untrusted dynamic tool arguments": {"assistant_tool_call"},
		"untrusted missing-role text":      {"unknown"},
	} {
		if got := extracted.MatchedRoles(term); !slices.Equal(got, want) {
			t.Fatalf("MatchedRoles(%q) = %#v, want %#v", term, got, want)
		}
	}
	if got := extracted.EnforcementMatchedRoles("untrusted tool result"); len(got) != 0 {
		t.Fatalf("EnforcementMatchedRoles(tool output) = %#v, want none", got)
	}
	if got := extracted.EnforcementMatchedRoles("untrusted missing-role text"); !slices.Equal(got, []string{"unknown"}) {
		t.Fatalf("EnforcementMatchedRoles(user-like text) = %#v, want []string{\"unknown\"}", got)
	}
}

func TestExtractJSONRequestIncludesTypedToolOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":"tool output text"}],"contents":[{"role":"user","parts":[{"text":"gemini user text"}]}]}`)
	extracted := ExtractJSONRequest(body)
	if !strings.Contains(extracted.Text, "tool output text") || !strings.Contains(extracted.Text, "gemini user text") {
		t.Fatalf("extracted text = %q", extracted.Text)
	}
	if extracted.EnforcementText != "gemini user text" {
		t.Fatalf("enforcement text = %q, want gemini user text", extracted.EnforcementText)
	}
	if got := extracted.MatchedRoles("tool output text"); !slices.Equal(got, []string{"function_call_output"}) {
		t.Fatalf("MatchedRoles(tool output) = %#v", got)
	}
}

func TestExtractJSONRequestFingerprintIgnoresOpaqueRequestIdentifiers(t *testing.T) {
	first := ExtractJSONRequest([]byte(`{"model":"gpt-a","request_id":"req-a","input":[{"type":"function_call_output","call_id":"call-a","output":"stable result"},{"role":"user","content":"stable prompt"}]}`))
	second := ExtractJSONRequest([]byte(`{"model":"gpt-b","request_id":"req-b","input":[{"type":"function_call_output","call_id":"call-b","output":"stable result"},{"role":"user","content":"stable prompt"}]}`))
	if first.FingerprintMaterial() == "" || first.FingerprintMaterial() != second.FingerprintMaterial() {
		t.Fatalf("fingerprints differ:\n%s\n%s", first.FingerprintMaterial(), second.FingerprintMaterial())
	}
}

func TestExtractJSONRequestIgnoresResponsesTextConfigurationForEnforcement(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"current user prompt"}],"text":{"format":{"type":"text"},"verbosity":"medium"}}`)
	extracted := ExtractJSONRequest(body)
	if extracted.EnforcementText != "current user prompt" {
		t.Fatalf("enforcement text = %q, want current user prompt", extracted.EnforcementText)
	}
}

func TestExtractJSONRequestUsesLatestUserMessageForEnforcement(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"older sensitive phrase"},{"role":"assistant","content":"refusal"},{"role":"tool","content":"tool sensitive phrase"},{"role":"user","content":"ordinary new question"}]}`)
	extracted := ExtractJSONRequest(body)
	if extracted.EnforcementText != "ordinary new question" {
		t.Fatalf("enforcement text = %q", extracted.EnforcementText)
	}
	if extracted.Continuation {
		t.Fatal("Continuation = true, want false")
	}
}

func TestExtractJSONRequestIncludesPreviousUserMessageForContinuation(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"older sensitive phrase"},{"role":"assistant","content":"answer"},{"role":"user","content":"继续上面的内容"}]}`)
	extracted := ExtractJSONRequest(body)
	if !extracted.Continuation {
		t.Fatal("Continuation = false, want true")
	}
	for _, expected := range []string{"older sensitive phrase", "继续上面的内容"} {
		if !strings.Contains(extracted.EnforcementText, expected) {
			t.Fatalf("enforcement text %q does not contain %q", extracted.EnforcementText, expected)
		}
	}
	if len(extracted.EnforcementFields) != 2 {
		t.Fatalf("enforcement fields = %#v", extracted.EnforcementFields)
	}
}

func TestExtractJSONRequestDetectsLongContinuationPrompt(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"older sensitive phrase"},{"role":"assistant","content":"answer"},{"role":"user","content":"请保持前文的剧情设定并继续完成后续章节，同时遵循这里附加的格式规则和角色约束。"}]}`)
	extracted := ExtractJSONRequest(body)
	if !extracted.Continuation || !strings.Contains(extracted.EnforcementText, "older sensitive phrase") {
		t.Fatalf("extracted = %#v", extracted)
	}
}

func TestExtractJSONRequestDoesNotTreatScopedWorkAsContinuation(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"older sensitive phrase"},{"role":"assistant","content":"answer"},{"role":"user","content":"继续修复代码中的并发问题"}]}`)
	extracted := ExtractJSONRequest(body)
	if extracted.Continuation || extracted.EnforcementText != "继续修复代码中的并发问题" {
		t.Fatalf("extracted = %#v", extracted)
	}
}
