package contentaudit

import (
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
}

func TestExtractJSONRequestIncludesTypedToolOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":"tool output text"}],"contents":[{"role":"user","parts":[{"text":"gemini user text"}]}]}`)
	extracted := ExtractJSONRequest(body)
	if !strings.Contains(extracted.Text, "tool output text") || !strings.Contains(extracted.Text, "gemini user text") {
		t.Fatalf("extracted text = %q", extracted.Text)
	}
}
