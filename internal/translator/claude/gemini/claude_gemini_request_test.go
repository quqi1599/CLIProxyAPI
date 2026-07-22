package gemini

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToClaude_PreservesCustomToolIDs(t *testing.T) {
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

			out := ConvertGeminiRequestToClaude("claude-sonnet-4", raw, false)

			gotCallID := gjson.GetBytes(out, "messages.0.content.0.id").String()
			if gotCallID != tt.want {
				t.Fatalf("expected tool_use id %q, got %q; output=%s", tt.want, gotCallID, string(out))
			}

			gotResultID := gjson.GetBytes(out, "messages.1.content.0.tool_use_id").String()
			if gotResultID != tt.want {
				t.Fatalf("expected tool_result tool_use_id %q, got %q; output=%s", tt.want, gotResultID, string(out))
			}
		})
	}
}

func TestConvertGeminiRequestToClaude_NormalizesNestedSchemaTypesWithoutDroppingFields(t *testing.T) {
	raw := []byte(`{
		"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{
			"type":"OBJECT",
			"x-provider-field":{"enabled":true,"limit":9007199254740993},
			"properties":{
				"items":{"type":"ARRAY","items":{"type":"STRING"}},
				"count":{"type":"INTEGER"},
				"nullable":{"type":["STRING","NULL"]}
			},
			"required":["items","count"]
		}}]}]
	}`)
	out := ConvertGeminiRequestToClaude("claude-test", raw, false)
	schema := gjson.GetBytes(out, "tools.0.input_schema")
	if schema.Get("type").String() != "object" || schema.Get("properties.items.type").String() != "array" || schema.Get("properties.items.items.type").String() != "string" || schema.Get("properties.count.type").String() != "integer" {
		t.Fatalf("nested schema types were not normalized: %s", schema.Raw)
	}
	if got := schema.Get("properties.nullable.type.0").String() + "," + schema.Get("properties.nullable.type.1").String(); got != "string,null" {
		t.Fatalf("union schema type = %q", got)
	}
	if !schema.Get("x-provider-field.enabled").Bool() {
		t.Fatalf("unknown schema field was dropped: %s", schema.Raw)
	}
	if got := schema.Get("x-provider-field.limit").Raw; got != "9007199254740993" {
		t.Fatalf("large unknown number = %q", got)
	}
	if got := schema.Get("required.0").String() + "," + schema.Get("required.1").String(); got != "items,count" {
		t.Fatalf("required order = %q", got)
	}
}
