package thinking

import "testing"

func TestExtractOpenAIStyleThinkingBooleanToggle(t *testing.T) {
	for _, tt := range []struct {
		body    string
		mode    ThinkingMode
		present bool
	}{
		{`{"enable_thinking":false}`, ModeNone, true},
		{`{"enable_thinking":false,"reasoning_effort":"high"}`, ModeNone, true},
		{`{"enable_thinking":false,"thinking":{"type":"enabled"}}`, ModeNone, true},
		{`{"enable_thinking":true,"thinking":{"type":"disabled"}}`, ModeNone, true},
		{`{"enable_thinking":true}`, ModeAuto, true},
		{`{"enable_thinking":true,"reasoning_effort":"high"}`, ModeLevel, true},
		{`{}`, 0, false},
		{`{"enable_thinking":null}`, 0, false},
		{`{"enable_thinking":"false"}`, 0, false},
		{`{"enable_thinking":0}`, 0, false},
	} {
		t.Run(tt.body, func(t *testing.T) {
			got, ok := ExtractOpenAIStyleThinkingConfig([]byte(tt.body))
			if ok != tt.present || got.Mode != tt.mode {
				t.Fatalf("got %+v, %t; want mode=%q present=%t", got, ok, tt.mode, tt.present)
			}
		})
	}
}
