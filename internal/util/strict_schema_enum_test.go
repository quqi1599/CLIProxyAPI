package util

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStrictSchemaPreservesScalarEnums(t *testing.T) {
	cleaners := []struct {
		name  string
		clean func(string) string
	}{
		{"strict", CleanJSONSchemaForStrictUpstream},
		{"structured-output", CleanJSONSchemaForOpenAIStructuredOutput},
	}
	cases := []struct {
		name     string
		schema   string
		path     string
		wantType string
		wantEnum string
	}{
		{"integer", `{"type":"integer","enum":[7]}`, "", "integer", "[7]"},
		{"large-integer", `{"type":"integer","enum":[9007199254740993]}`, "", "integer", "[9007199254740993]"},
		{"decimal", `{"type":"number","enum":[0.1234567890123456789]}`, "", "number", "[0.1234567890123456789]"},
		{"boolean", `{"type":"boolean","enum":[true,false]}`, "", "boolean", "[true,false]"},
		{"null", `{"type":"null","enum":[null]}`, "", "null", "[null]"},
		{"string", `{"type":"string","enum":["7"]}`, "", "string", `["7"]`},
		{"mixed", `{"enum":[7,"7",true,null]}`, "", "", `[7,"7",true,null]`},
		{"const", `{"type":"integer","const":7}`, "", "integer", "[7]"},
		{"items", `{"type":"array","items":{"type":"integer","enum":[7]}}`, "items.", "integer", "[7]"},
		{"one-of", `{"oneOf":[{"type":"integer","enum":[7]},{"type":"null"}]}`, "", "integer", "[7]"},
		{"property-named-type", `{"type":"object","properties":{"type":{"type":"integer","enum":[7]}}}`, "properties.type.", "integer", "[7]"},
		{"property-named-enum", `{"type":"object","properties":{"enum":{"type":"boolean","enum":[false]}}}`, "properties.enum.", "boolean", "[false]"},
	}
	for _, cleaner := range cleaners {
		for _, tc := range cases {
			t.Run(cleaner.name+"/"+tc.name, func(t *testing.T) {
				input := fmt.Sprintf(`{"type":"object","properties":{"value":%s},"required":["value"]}`, tc.schema)
				result := cleaner.clean(input)
				path := "properties.value." + tc.path
				if got := gjson.Get(result, path+"type").String(); got != tc.wantType {
					t.Fatalf("type = %q, want %q: %s", got, tc.wantType, result)
				}
				if got := gjson.Get(result, path+"enum").Raw; got != tc.wantEnum {
					t.Fatalf("enum = %s, want %s: %s", got, tc.wantEnum, result)
				}
				if second := cleaner.clean(result); second != result {
					t.Fatalf("schema cleanup is not idempotent:\nfirst: %s\nsecond: %s", result, second)
				}
			})
		}
	}
}

func TestGeminiSchemaKeepsScalarEnumStringification(t *testing.T) {
	input := `{"type":"object","properties":{"n":{"type":"integer","enum":[7]},"flag":{"type":"boolean","enum":[true,false]}}}`
	for _, cleaner := range []struct {
		name  string
		clean func(string) string
	}{
		{"gemini", CleanJSONSchemaForGemini},
		{"antigravity", CleanJSONSchemaForAntigravity},
	} {
		t.Run(cleaner.name, func(t *testing.T) {
			result := cleaner.clean(input)
			for _, path := range []string{"properties.n.type", "properties.flag.type"} {
				if got := gjson.Get(result, path).String(); got != "string" {
					t.Fatalf("%s = %q, want string: %s", path, got, result)
				}
			}
			if got := gjson.Get(result, "properties.n.enum").Raw; got != `["7"]` {
				t.Fatalf("integer enum = %s, want string values: %s", got, result)
			}
			if got := gjson.Get(result, "properties.flag.enum").Raw; got != `["true","false"]` {
				t.Fatalf("boolean enum = %s, want string values: %s", got, result)
			}
		})
	}
}
