package contentpart

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestParseContentPartMatrix(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		kind      Kind
		assertion func(*testing.T, Part)
	}{
		{
			name: "chat text",
			raw:  `{"type":"text","text":"hello","vendor":{"keep":true}}`,
			kind: Text,
			assertion: func(t *testing.T, part Part) {
				if part.Text != "hello" {
					t.Fatalf("text = %q", part.Text)
				}
			},
		},
		{
			name: "responses image",
			raw:  `{"type":"input_image","image_url":"https://example.com/a.png","detail":"high"}`,
			kind: Image,
			assertion: func(t *testing.T, part Part) {
				if part.Image.URL != "https://example.com/a.png" || part.Image.Detail != "high" {
					t.Fatalf("image = %+v", part.Image)
				}
			},
		},
		{
			name: "claude image",
			raw:  `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`,
			kind: Image,
			assertion: func(t *testing.T, part Part) {
				if part.Image.URL != "data:image/png;base64,AAAA" {
					t.Fatalf("image URL = %q", part.Image.URL)
				}
			},
		},
		{
			name: "claude tool call",
			raw:  `{"type":"tool_use","id":"call_1","name":"lookup","input":{"path":"README.md"}}`,
			kind: ToolCall,
			assertion: func(t *testing.T, part Part) {
				if part.ToolCall.ID != "call_1" || part.ToolCall.Name != "lookup" || part.ToolCall.Arguments != `{"path":"README.md"}` {
					t.Fatalf("tool call = %+v", part.ToolCall)
				}
			},
		},
		{
			name: "responses custom tool result",
			raw:  `{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"done"},"!"]}`,
			kind: ToolResult,
			assertion: func(t *testing.T, part Part) {
				if part.ToolResult.CallID != "call_2" || part.ToolResult.Output != "done!" {
					t.Fatalf("tool result = %+v", part.ToolResult)
				}
			},
		},
		{
			name: "claude reasoning",
			raw:  `{"type":"thinking","thinking":" plan "}`,
			kind: Reasoning,
			assertion: func(t *testing.T, part Part) {
				if !part.Reasoning.Available || part.Reasoning.Text != "plan" {
					t.Fatalf("reasoning = %+v", part.Reasoning)
				}
			},
		},
		{
			name: "responses reasoning",
			raw:  `{"type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"other","text":"drop"},{"type":"summary_text","text":"second"}]}`,
			kind: Reasoning,
			assertion: func(t *testing.T, part Part) {
				if !part.Reasoning.Available || part.Reasoning.Text != "firstsecond" {
					t.Fatalf("reasoning = %+v", part.Reasoning)
				}
			},
		},
		{
			name:      "unknown stays unknown",
			raw:       `{"type":"vendor_blob","payload":{"keep":true}}`,
			kind:      Unknown,
			assertion: func(*testing.T, Part) {},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			part := Parse(gjson.Parse(test.raw))
			if part.Kind != test.kind {
				t.Fatalf("kind = %v, want %v", part.Kind, test.kind)
			}
			test.assertion(t, part)
		})
	}
}

func TestImageFromAcceptsUntypedLegacyPart(t *testing.T) {
	image := ImageFrom(gjson.Parse(`{"image_url":{"url":"https://example.com/a.png","detail":"low"}}`))
	if image.URL != "https://example.com/a.png" || image.Detail != "low" {
		t.Fatalf("image = %+v", image)
	}
}
