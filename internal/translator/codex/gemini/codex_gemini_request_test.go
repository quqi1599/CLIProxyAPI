package gemini

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToCodex_LargeArraysPreserveOrderAndSchemaFields(t *testing.T) {
	const textParts = 2048
	const toolCount = 512
	var input strings.Builder
	input.WriteString(`{"systemInstruction":{"parts":[{"text":"rules"}]},"contents":[{"role":"model","parts":[`)
	for i := 0; i < textParts; i++ {
		if i > 0 {
			input.WriteByte(',')
		}
		input.WriteString(`{"text":"text-`)
		input.WriteString(strconv.Itoa(i))
		input.WriteString(`"}`)
	}
	input.WriteString(`,{"functionCall":{"name":"tool_0","id":"call-0","args":{"q":"x"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"tool_0","id":"call-0","response":{"result":"ok"}}}]}],"tools":[{"functionDeclarations":[`)
	for i := 0; i < toolCount; i++ {
		if i > 0 {
			input.WriteByte(',')
		}
		input.WriteString(`{"name":"tool_`)
		input.WriteString(strconv.Itoa(i))
		input.WriteString(`","description":"d","parameters":{"type":"OBJECT","properties":{"x":{"type":"STRING","x_unknown":{"type":"NUMBER","keep":true}}}}}`)
	}
	input.WriteString(`]}]}`)

	out := ConvertGeminiRequestToCodex("gpt-5.4", []byte(input.String()), false)
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != textParts+3 {
		t.Fatalf("input item count = %d, want %d", len(items), textParts+3)
	}
	if got := items[0].Get("content.0.text").String(); got != "rules" {
		t.Fatalf("system text = %q", got)
	}
	if got := items[textParts].Get("content.0.text").String(); got != "text-2047" {
		t.Fatalf("last text = %q", got)
	}
	if got := items[textParts+1].Get("call_id").String(); got != "call-0" {
		t.Fatalf("function call id = %q", got)
	}
	if got := items[textParts+2].Get("call_id").String(); got != "call-0" {
		t.Fatalf("function output call id = %q", got)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != toolCount {
		t.Fatalf("tool count = %d, want %d", len(tools), toolCount)
	}
	lastTool := tools[toolCount-1]
	if got := lastTool.Get("name").String(); got != "tool_511" {
		t.Fatalf("last tool name = %q", got)
	}
	if got := lastTool.Get("parameters.properties.x.type").String(); got != "string" {
		t.Fatalf("schema type = %q, want string", got)
	}
	if !lastTool.Get("parameters.properties.x.x_unknown.keep").Bool() {
		t.Fatalf("unknown schema field was lost: %s", lastTool.Raw)
	}
	if got := lastTool.Get("parameters.properties.x.x_unknown.type").String(); got != "number" {
		t.Fatalf("unknown nested type = %q, want number", got)
	}
}

func TestConvertGeminiRequestToCodex_PreservesCustomCallIDs(t *testing.T) {
	tests := []struct {
		name          string
		callField     string
		responseField string
		want          string
	}{
		{
			name:          "id",
			callField:     `"id":"call_gateway_id"`,
			responseField: `"id":"call_gateway_id"`,
			want:          "call_gateway_id",
		},
		{
			name:          "call_id",
			callField:     `"call_id":"call_gateway_call_id"`,
			responseField: `"call_id":"call_gateway_call_id"`,
			want:          "call_gateway_call_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{
				"contents": [
					{
						"role": "model",
						"parts": [
							{"functionCall": {"name": "lookup", %s, "args": {"query": "status"}}}
						]
					},
					{
						"role": "user",
						"parts": [
							{"functionResponse": {"name": "lookup", %s, "response": {"result": "ok"}}}
						]
					}
				]
			}`, tt.callField, tt.responseField))

			out := ConvertGeminiRequestToCodex("gpt-5.1-codex", raw, false)

			gotCallID := gjson.GetBytes(out, "input.0.call_id").String()
			if gotCallID != tt.want {
				t.Fatalf("expected function_call call_id %q, got %q; output=%s", tt.want, gotCallID, string(out))
			}

			gotOutputID := gjson.GetBytes(out, "input.1.call_id").String()
			if gotOutputID != tt.want {
				t.Fatalf("expected function_call_output call_id %q, got %q; output=%s", tt.want, gotOutputID, string(out))
			}
		})
	}
}
