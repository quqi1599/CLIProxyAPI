package gemini

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBackfillEmptyFunctionResponseNames_Single(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"output": "file1.txt"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected backfilled name 'Bash', got '%s'", name)
	}
}

func TestBackfillEmptyFunctionResponseNames_Parallel(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Read", "args": {"path": "/a"}}},
					{"functionCall": {"name": "Grep", "args": {"pattern": "x"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "content a"}}},
					{"functionResponse": {"name": "", "response": {"result": "match x"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name0 := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	name1 := gjson.GetBytes(out, "contents.1.parts.1.functionResponse.name").String()
	if name0 != "Read" {
		t.Errorf("Expected first name 'Read', got '%s'", name0)
	}
	if name1 != "Grep" {
		t.Errorf("Expected second name 'Grep', got '%s'", name1)
	}
}

func TestBackfillEmptyFunctionResponseNames_PreservesExisting(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "Bash", "response": {"result": "ok"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected preserved name 'Bash', got '%s'", name)
	}
}

func TestConvertGeminiRequestToGemini_BackfillsEmptyName(t *testing.T) {
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"output": "file1.txt"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToGemini("", input, false)

	name := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected backfilled name 'Bash', got '%s'", name)
	}
}

func TestBackfillEmptyFunctionResponseNames_MoreResponsesThanCalls(t *testing.T) {
	// Extra responses beyond the call count should not panic and should be left unchanged.
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Bash", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "ok"}}},
					{"functionResponse": {"name": "", "response": {"result": "extra"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name0 := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	if name0 != "Bash" {
		t.Errorf("Expected first name 'Bash', got '%s'", name0)
	}
	// Second response has no matching call, should remain empty
	name1 := gjson.GetBytes(out, "contents.1.parts.1.functionResponse.name").String()
	if name1 != "" {
		t.Errorf("Expected second name to remain empty, got '%s'", name1)
	}
}

func TestBackfillEmptyFunctionResponseNames_MultipleGroups(t *testing.T) {
	// Two sequential call/response groups should each get correct names.
	input := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Read", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "content"}}}
				]
			},
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "Grep", "args": {}}}
				]
			},
			{
				"role": "user",
				"parts": [
					{"functionResponse": {"name": "", "response": {"result": "match"}}}
				]
			}
		]
	}`)

	out := backfillEmptyFunctionResponseNames(input)

	name0 := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String()
	name1 := gjson.GetBytes(out, "contents.3.parts.0.functionResponse.name").String()
	if name0 != "Read" {
		t.Errorf("Expected first group name 'Read', got '%s'", name0)
	}
	if name1 != "Grep" {
		t.Errorf("Expected second group name 'Grep', got '%s'", name1)
	}
}

func TestConvertGeminiRequestToGeminiPayloadGrowth(t *testing.T) {
	for _, size := range []int{16, 64, 256, 1024} {
		t.Run(fmt.Sprintf("groups_%d", size), func(t *testing.T) {
			input := buildGeminiNormalizationGrowthRequest(size)
			original := bytes.Clone(input)

			output := ConvertGeminiRequestToGemini("", input, false)

			if !bytes.Equal(input, original) {
				t.Fatal("converter mutated the input payload")
			}
			if !gjson.GetBytes(output, "unknownTopLevel.keep").Bool() {
				t.Fatalf("top-level unknown field was lost: %s", output)
			}
			contents := gjson.GetBytes(output, "contents").Array()
			if len(contents) != 1+size*2 {
				t.Fatalf("content count = %d, want %d", len(contents), 1+size*2)
			}
			for index := 0; index < size; index++ {
				callContent := contents[1+index*2]
				responseContent := contents[2+index*2]
				wantName := fmt.Sprintf("tool_%04d", index)
				if callContent.Get("role").String() != "model" || responseContent.Get("role").String() != "user" {
					t.Fatalf("group %d role order changed: %q, %q", index, callContent.Get("role").String(), responseContent.Get("role").String())
				}
				if got := responseContent.Get("parts.0.functionResponse.name").String(); got != wantName {
					t.Fatalf("group %d response name = %q, want %q", index, got, wantName)
				}
				if !responseContent.Get("parts.0.functionResponse.response.unknown.keep").Bool() || responseContent.Get("unknownContent.ordinal").Int() != int64(index*2+1) {
					t.Fatalf("group %d unknown response fields were lost: %s", index, responseContent.Raw)
				}
			}

			tools := gjson.GetBytes(output, "tools").Array()
			if len(tools) != size {
				t.Fatalf("tool count = %d, want %d", len(tools), size)
			}
			for index, tool := range tools {
				if tool.Get("functionDeclarations").Exists() {
					t.Fatalf("tool %d retained camel-case declarations: %s", index, tool.Raw)
				}
				declaration := tool.Get("function_declarations.0")
				if got, want := declaration.Get("name").String(), fmt.Sprintf("tool_%04d", index); got != want {
					t.Fatalf("tool %d name = %q, want %q", index, got, want)
				}
				if declaration.Get("parameters").Exists() || declaration.Get("parametersJsonSchema.vendorMetadata.ordinal").Int() != int64(index) {
					t.Fatalf("tool %d schema rename lost unknown fields: %s", index, declaration.Raw)
				}
				if !tool.Get("unknownTool.keep").Bool() || !declaration.Get("unknownDeclaration.keep").Bool() {
					t.Fatalf("tool %d unknown fields were lost: %s", index, tool.Raw)
				}
			}
			if gjson.GetBytes(output, "generationConfig.responseSchema").Exists() || !gjson.GetBytes(output, "generationConfig.responseJsonSchema.vendorMetadata.keep").Bool() {
				t.Fatalf("response schema rename lost unknown fields: %s", output)
			}
		})
	}
}

var benchmarkGeminiNormalizationOutput []byte

func BenchmarkPayloadGrowthGeminiRequestNormalization(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("groups_%d", size), func(b *testing.B) {
			input := buildGeminiNormalizationGrowthRequest(size)
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for b.Loop() {
				benchmarkGeminiNormalizationOutput = ConvertGeminiRequestToGemini("", input, false)
			}
		})
	}
}

func buildGeminiNormalizationGrowthRequest(size int) []byte {
	var input strings.Builder
	input.Grow(size * 520)
	input.WriteString(`{"unknownTopLevel":{"keep":true},"contents":[{"role":"user","parts":[{"text":"seed"}],"unknownContent":{"ordinal":-1}}`)
	for index := 0; index < size; index++ {
		fmt.Fprintf(&input, `,{"role":"invalid","parts":[{"functionCall":{"name":"tool_%04d","args":{"index":%d}},"unknownPart":{"keep":true}}],"unknownContent":{"ordinal":%d}}`, index, index, index*2)
		fmt.Fprintf(&input, `,{"parts":[{"functionResponse":{"name":"","response":{"index":%d,"unknown":{"keep":true}}},"unknownPart":{"keep":true}}],"unknownContent":{"ordinal":%d}}`, index, index*2+1)
	}
	input.WriteString(`],"tools":[`)
	for index := 0; index < size; index++ {
		if index > 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `{"functionDeclarations":[{"name":"tool_%04d","parameters":{"type":"object","vendorMetadata":{"ordinal":%d}},"unknownDeclaration":{"keep":true}}],"unknownTool":{"keep":true}}`, index, index)
	}
	input.WriteString(`],"generationConfig":{"responseSchema":{"type":"object","vendorMetadata":{"keep":true}},"unknownGeneration":{"keep":true}}}`)
	return []byte(input.String())
}
