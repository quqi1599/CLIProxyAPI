package gemini

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var benchmarkGeminiToOpenAIOutput []byte

func TestConvertGeminiRequestToOpenAI_FunctionResponsesConsumeToolCallIDsFIFO(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "read_file", "args": {"path": "a.txt"}}},
					{"functionCall": {"name": "grep", "args": {"pattern": "needle"}}},
					{"functionCall": {"name": "list_dir", "args": {"path": "."}}}
				]
			},
			{
				"role": "function",
				"parts": [
					{"functionResponse": {"name": "read_file", "response": {"result": "a"}}},
					{"functionResponse": {"name": "grep", "response": {"result": "b"}}},
					{"functionResponse": {"name": "list_dir", "response": {"result": "c"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	firstID := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	secondID := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String()
	thirdID := gjson.GetBytes(out, "messages.0.tool_calls.2.id").String()

	if firstID == "" || secondID == "" || thirdID == "" {
		t.Fatalf("expected all assistant tool call IDs to be set. Output: %s", string(out))
	}
	if firstID == secondID || secondID == thirdID || firstID == thirdID {
		t.Fatalf("expected distinct assistant tool call IDs, got %q, %q, %q", firstID, secondID, thirdID)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != firstID {
		t.Fatalf("messages.1.tool_call_id = %q, want %q. Output: %s", got, firstID, string(out))
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != secondID {
		t.Fatalf("messages.2.tool_call_id = %q, want %q. Output: %s", got, secondID, string(out))
	}
	if got := gjson.GetBytes(out, "messages.3.tool_call_id").String(); got != thirdID {
		t.Fatalf("messages.3.tool_call_id = %q, want %q. Output: %s", got, thirdID, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_FunctionResponseWithoutPriorCallGetsFallbackID(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "function",
				"parts": [
					{"functionResponse": {"name": "read_file", "response": {"result": "ok"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	toolCallID := gjson.GetBytes(out, "messages.0.tool_call_id").String()
	if !strings.HasPrefix(toolCallID, "call_") {
		t.Fatalf("fallback tool_call_id = %q, want call_ prefix. Output: %s", toolCallID, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_ExtraFunctionResponsesUseFallbackID(t *testing.T) {
	inputJSON := []byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "read_file", "args": {"path": "a.txt"}}}
				]
			},
			{
				"role": "function",
				"parts": [
					{"functionResponse": {"name": "read_file", "response": {"result": "a"}}},
					{"functionResponse": {"name": "read_file", "response": {"result": "extra"}}}
				]
			}
		]
	}`)

	out := ConvertGeminiRequestToOpenAI("test-model", inputJSON, false)
	callID := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	firstResponseID := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	extraResponseID := gjson.GetBytes(out, "messages.2.tool_call_id").String()

	if firstResponseID != callID {
		t.Fatalf("messages.1.tool_call_id = %q, want %q. Output: %s", firstResponseID, callID, string(out))
	}
	if !strings.HasPrefix(extraResponseID, "call_") {
		t.Fatalf("extra response fallback tool_call_id = %q, want call_ prefix. Output: %s", extraResponseID, string(out))
	}
	if extraResponseID == callID {
		t.Fatalf("extra response reused consumed tool_call_id %q. Output: %s", extraResponseID, string(out))
	}
}

func TestConvertGeminiRequestToOpenAI_PayloadGrowthPreservesOrderAndInput(t *testing.T) {
	for _, size := range []int{16, 64, 256, 1024} {
		t.Run(fmt.Sprintf("items_%d", size), func(t *testing.T) {
			input := buildGeminiToOpenAIGrowthPayload(size)
			original := bytes.Clone(input)

			out := ConvertGeminiRequestToOpenAI("growth-model", input, true)

			if !bytes.Equal(input, original) {
				t.Fatal("converter mutated the input payload")
			}
			if !gjson.ValidBytes(out) {
				t.Fatalf("converter returned invalid JSON: %s", string(out))
			}

			root := gjson.ParseBytes(out)
			if got := root.Get("model").String(); got != "growth-model" {
				t.Fatalf("model = %q, want growth-model", got)
			}
			if !root.Get("stream").Bool() {
				t.Fatal("stream = false, want true")
			}
			if got := root.Get("tool_choice").String(); got != "auto" {
				t.Fatalf("tool_choice = %q, want auto", got)
			}

			messages := root.Get("messages").Array()
			if len(messages) != size+2 {
				t.Fatalf("messages count = %d, want %d", len(messages), size+2)
			}

			systemParts := messages[0].Get("content").Array()
			if len(systemParts) != size {
				t.Fatalf("system content count = %d, want %d", len(systemParts), size)
			}
			for idx, part := range systemParts {
				want := fmt.Sprintf("system-%04d", idx)
				if got := part.Get("text").String(); got != want {
					t.Fatalf("system content %d = %q, want %q", idx, got, want)
				}
			}

			var joinedText strings.Builder
			for idx := 0; idx < size; idx++ {
				fmt.Fprintf(&joinedText, "part-%04d|", idx)
			}
			if got, want := messages[1].Get("content").String(), joinedText.String(); got != want {
				t.Fatalf("joined text content differs: got %d bytes, want %d", len(got), len(want))
			}

			for idx := 0; idx < size; idx++ {
				message := messages[idx+2]
				wantRole := "user"
				if idx%2 == 1 {
					wantRole = "assistant"
				}
				if got := message.Get("role").String(); got != wantRole {
					t.Fatalf("message %d role = %q, want %q", idx, got, wantRole)
				}
				wantText := fmt.Sprintf("message-%04d", idx)
				if got := message.Get("content").String(); got != wantText {
					t.Fatalf("message %d content = %q, want %q", idx, got, wantText)
				}
			}

			tools := root.Get("tools").Array()
			if len(tools) != size {
				t.Fatalf("tools count = %d, want %d", len(tools), size)
			}
			for idx, tool := range tools {
				wantName := fmt.Sprintf("tool_%04d", idx)
				if got := tool.Get("function.name").String(); got != wantName {
					t.Fatalf("tool %d name = %q, want %q", idx, got, wantName)
				}
				metadata := tool.Get("function.parameters.vendorMetadata.ordinal")
				if !metadata.Exists() || metadata.Int() != int64(idx) {
					got := metadata.Int()
					t.Fatalf("tool %d unknown schema field = %d, want %d", idx, got, idx)
				}
				if got := tool.Get("function.parameters.properties.value.vendorMarker").String(); got != "keep" {
					t.Fatalf("tool %d nested unknown schema field = %q, want keep", idx, got)
				}
			}
		})
	}
}

func BenchmarkPayloadGrowthGeminiToOpenAIRequest(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("items_%d", size), func(b *testing.B) {
			input := buildGeminiToOpenAIGrowthPayload(size)
			out := ConvertGeminiRequestToOpenAI("growth-model", input, true)
			if !gjson.ValidBytes(out) {
				b.Fatal("converter returned invalid JSON")
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for b.Loop() {
				benchmarkGeminiToOpenAIOutput = ConvertGeminiRequestToOpenAI("growth-model", input, true)
			}
		})
	}
}

func buildGeminiToOpenAIGrowthPayload(size int) []byte {
	var payload strings.Builder
	payload.Grow(size * 480)
	payload.WriteString(`{"unknownTopLevel":{"marker":"preserve-source"},"generationConfig":{"temperature":0.5,"unknownGenerationField":true},"systemInstruction":{"unknownSystemField":"source","parts":[`)
	for idx := 0; idx < size; idx++ {
		if idx > 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `{"text":"system-%04d","unknownPart":"source"}`, idx)
	}

	payload.WriteString(`]},"contents":[{"role":"user","unknownContentField":"source","parts":[`)
	for idx := 0; idx < size; idx++ {
		if idx > 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `{"text":"part-%04d|","unknownPart":"source"}`, idx)
	}
	payload.WriteString(`]}`)

	for idx := 0; idx < size; idx++ {
		role := "user"
		if idx%2 == 1 {
			role = "model"
		}
		fmt.Fprintf(&payload, `,{"role":%q,"unknownContentField":"source","parts":[{"text":"message-%04d","unknownPart":"source"}]}`, role, idx)
	}

	payload.WriteString(`],"tools":[`)
	const declarationsPerTool = 4
	for groupStart := 0; groupStart < size; groupStart += declarationsPerTool {
		if groupStart > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"unknownToolField":"source","functionDeclarations":[`)
		for offset := 0; offset < declarationsPerTool && groupStart+offset < size; offset++ {
			if offset > 0 {
				payload.WriteByte(',')
			}
			idx := groupStart + offset
			fmt.Fprintf(&payload, `{"name":"tool_%04d","description":"tool %04d","unknownDeclarationField":"source","parameters":{"type":"object","vendorMetadata":{"ordinal":%d},"properties":{"value":{"type":"string","vendorMarker":"keep"}}}}`, idx, idx, idx)
		}
		payload.WriteString(`]}`)
	}
	payload.WriteString(`],"toolConfig":{"functionCallingConfig":{"mode":"AUTO","unknownChoiceField":"source"}}}`)
	return []byte(payload.String())
}
