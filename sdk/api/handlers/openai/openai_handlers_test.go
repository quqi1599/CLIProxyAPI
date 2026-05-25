package openai

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCompletionsRequestToChatCompletions_StripsLegacyOnlyFields(t *testing.T) {
	raw := []byte(`{
		"model":"kimi-k2.6",
		"prompt":"Complete this",
		"max_tokens":64,
		"temperature":0,
		"logprobs":true,
		"top_logprobs":5,
		"echo":true
	}`)

	out := convertCompletionsRequestToChatCompletions(raw)

	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "Complete this" {
		t.Fatalf("messages.0.content = %q, want prompt text: %s", got, string(out))
	}
	for _, path := range []string{"logprobs", "top_logprobs", "echo"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be stripped from synthetic chat request: %s", path, string(out))
		}
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0 {
		t.Fatalf("temperature = %v, want 0: %s", got, string(out))
	}
}
