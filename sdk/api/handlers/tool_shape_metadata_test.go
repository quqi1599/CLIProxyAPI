package handlers

import (
	"fmt"
	"strings"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSetRequestShapeAndToolMetadataForChatPayload(t *testing.T) {
	meta := make(map[string]any)
	rawJSON := []byte(`{
		"messages":[
			{"role":"assistant","content":"calling","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"},
			{"role":"user","content":"hi"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
	}`)

	setRequestShapeAndToolMetadata(meta, rawJSON)

	if got := meta[coreexecutor.RequestBodyBytesMetadataKey]; got != len(rawJSON) {
		t.Fatalf("request_body_bytes = %#v, want %d", got, len(rawJSON))
	}
	if got := meta[coreexecutor.RequestWireBytesMetadataKey]; got != int64(len(rawJSON)) {
		t.Fatalf("request_wire_bytes = %#v, want %d", got, len(rawJSON))
	}
	if got := meta[coreexecutor.MessageCountMetadataKey]; got != 3 {
		t.Fatalf("message_count = %#v, want 3", got)
	}
	if got := meta[coreexecutor.ToolCountMetadataKey]; got != 2 {
		t.Fatalf("tool_count = %#v, want 2", got)
	}
	if got := meta[coreexecutor.DeclaredToolCountMetadataKey]; got != 1 {
		t.Fatalf("declared_tool_count = %#v, want 1", got)
	}
	if got := meta[coreexecutor.ToolInteractionCountMetadataKey]; got != 2 {
		t.Fatalf("tool_interaction_count = %#v, want 2", got)
	}
	if got := meta[coreexecutor.ContentPartCountMetadataKey]; got != 3 {
		t.Fatalf("content_part_count = %#v, want 3", got)
	}
}

func TestSetRequestShapeAndToolMetadataForResponsesInput(t *testing.T) {
	meta := make(map[string]any)
	rawJSON := []byte(`{
		"input":[
			{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools":[{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.test"}]
	}`)

	setRequestShapeAndToolMetadata(meta, rawJSON)

	if got := meta[coreexecutor.RequestBodyBytesMetadataKey]; got != len(rawJSON) {
		t.Fatalf("request_body_bytes = %#v, want %d", got, len(rawJSON))
	}
	if got := meta[coreexecutor.RequestWireBytesMetadataKey]; got != int64(len(rawJSON)) {
		t.Fatalf("request_wire_bytes = %#v, want %d", got, len(rawJSON))
	}
	if got := meta[coreexecutor.MessageCountMetadataKey]; got != 3 {
		t.Fatalf("message_count = %#v, want 3", got)
	}
	if got := meta[coreexecutor.ToolCountMetadataKey]; got != 2 {
		t.Fatalf("tool_count = %#v, want 2", got)
	}
	if got := meta[coreexecutor.DeclaredToolCountMetadataKey]; got != 1 {
		t.Fatalf("declared_tool_count = %#v, want 1", got)
	}
	if got := meta[coreexecutor.ToolInteractionCountMetadataKey]; got != 2 {
		t.Fatalf("tool_interaction_count = %#v, want 2", got)
	}
	if got := meta[coreexecutor.MCPToolCountMetadataKey]; got != 1 {
		t.Fatalf("mcp_tool_count = %#v, want 1", got)
	}
	if got := meta[coreexecutor.ContentPartCountMetadataKey]; got != 1 {
		t.Fatalf("content_part_count = %#v, want 1", got)
	}
}

func TestInspectRequestComplexitySingleTraversalParity(t *testing.T) {
	rawJSON := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{},{}],"function_call":{"name":"legacy"},"content":[{"type":"tool_result"},{"type":"text"}]},
			{"role":"tool","content":null}
		],
		"input":[
			{"type":"function_call","role":"tool","content":[{"type":"function_call_output"}]},
			{"type":"message","content":"scalar"}
		],
		"tools":[{"type":"function"},{"type":"web_search"},{"type":"mcp","server_label":"docs"}]
	}`)

	vector, ok := inspectRequestComplexity(rawJSON)
	if !ok {
		t.Fatal("inspectRequestComplexity() rejected valid JSON")
	}
	if vector.DecodedBytes != int64(len(rawJSON)) {
		t.Fatalf("DecodedBytes = %d, want %d", vector.DecodedBytes, len(rawJSON))
	}
	if vector.MessageCount != 2 {
		t.Fatalf("MessageCount = %d, want messages precedence count 2", vector.MessageCount)
	}
	if vector.DeclaredToolCount != 3 {
		t.Fatalf("DeclaredToolCount = %d, want 3", vector.DeclaredToolCount)
	}
	if vector.InteractionCount != 7 {
		t.Fatalf("InteractionCount = %d, want 7", vector.InteractionCount)
	}
	if vector.ContentPartCount != 4 {
		t.Fatalf("ContentPartCount = %d, want 4", vector.ContentPartCount)
	}
	if vector.MCPToolCount != 1 || vector.BuiltinToolCount != 1 {
		t.Fatalf("tool shape counts = mcp:%d builtin:%d, want 1/1", vector.MCPToolCount, vector.BuiltinToolCount)
	}
}

func TestInspectRequestComplexityKeepsLegacyFirstKeyAndFallbackSemantics(t *testing.T) {
	rawJSON := []byte(`{
		"messages":[],
		"messages":[{"role":"user","content":"ignored"}],
		"input":[{"type":"function_call","name":"lookup"}],
		"tools":[],
		"tools":[{"type":"function","function":{"name":"ignored"}}]
	}`)

	vector, ok := inspectRequestComplexity(rawJSON)
	if !ok {
		t.Fatal("inspectRequestComplexity() rejected valid JSON")
	}
	if vector.MessageCount != 0 {
		t.Fatalf("MessageCount = %d, want first empty messages array to win", vector.MessageCount)
	}
	if vector.DeclaredToolCount != 0 {
		t.Fatalf("DeclaredToolCount = %d, want first empty tools array to win", vector.DeclaredToolCount)
	}
	if vector.InteractionCount != 1 {
		t.Fatalf("InteractionCount = %d, want input interaction to remain included", vector.InteractionCount)
	}
	if vector.ContentPartCount != 0 {
		t.Fatalf("ContentPartCount = %d, want 0", vector.ContentPartCount)
	}
}

func TestSetRequestShapeAndToolMetadataRejectsInvalidJSONWithoutMutation(t *testing.T) {
	meta := map[string]any{"existing": "value"}
	setRequestShapeAndToolMetadata(meta, []byte(`{"messages":[}`))

	if len(meta) != 1 || meta["existing"] != "value" {
		t.Fatalf("metadata changed for invalid JSON: %#v", meta)
	}
}

func TestInspectRequestComplexityProtocolFixtures(t *testing.T) {
	tests := []struct {
		name             string
		sourceFormat     string
		endpoint         string
		stream           bool
		body             string
		messages         int
		parts            int
		toolCalls        int
		toolOutputs      int64
		inlineImages     int64
		reasoning        int64
		declaredTools    int
		builtinTools     int
		toolInteractions int
	}{
		{
			name:         "openai_chat",
			sourceFormat: "openai",
			endpoint:     "chat",
			body: `{"messages":[
				{"role":"assistant","reasoning_content":"why","content":[{"type":"reasoning","text":"think"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YWJj"}}],"tool_calls":[{"type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","name":"lookup","content":"result"}
			]}`,
			messages: 2, parts: 3, toolCalls: 1, toolOutputs: 6, inlineImages: 3,
			reasoning: 8, toolInteractions: 2,
		},
		{
			name:         "openai_responses",
			sourceFormat: "openai-response",
			endpoint:     "responses",
			stream:       true,
			body: `{"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/jpeg;base64,YWJj"}]},
				{"type":"function_call","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_1","output":"tool-output"},
				{"type":"reasoning","encrypted_content":"enc","summary":[{"type":"summary_text","text":"sum"}]}
			]}`,
			messages: 4, parts: 2, toolCalls: 1, toolOutputs: 11, inlineImages: 3,
			reasoning: 6, toolInteractions: 2,
		},
		{
			name:         "claude",
			sourceFormat: "claude",
			endpoint:     "chat",
			body: `{"system":[{"type":"text","text":"system"}],"messages":[
				{"role":"assistant","content":[{"type":"thinking","thinking":"abcd"},{"type":"tool_use","id":"tool_1","name":"lookup","input":{}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJjZA=="}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"result"}]}
			]}`,
			messages: 2, parts: 5, toolCalls: 1, toolOutputs: 6, inlineImages: 4,
			reasoning: 4, toolInteractions: 2,
		},
		{
			name:         "gemini",
			sourceFormat: "gemini",
			endpoint:     "chat",
			body: `{"systemInstruction":{"parts":[{"text":"system"}]},"contents":[
				{"role":"model","parts":[{"thought":true,"text":"brain"},{"functionCall":{"name":"lookup","args":{}}},{"inlineData":{"mimeType":"image/png","data":"YWJj"}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}
			],"tools":[{"functionDeclarations":[{"name":"lookup"},{"name":"write"}]},{"googleSearch":{},"codeExecution":{}}]}`,
			messages: 2, parts: 5, toolCalls: 1, toolOutputs: 15, inlineImages: 3,
			reasoning: 5, declaredTools: 4, builtinTools: 2, toolInteractions: 2,
		},
		{
			name:         "codex",
			sourceFormat: "codex",
			endpoint:     "responses",
			stream:       true,
			body: `{"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"local_shell_call","name":"shell","arguments":"{}"},
				{"type":"local_shell_call_output","output":"xyz"},
				{"type":"reasoning","encrypted_content":"sealed","summary":[{"type":"summary_text","text":"brief"}]}
			]}`,
			messages: 4, parts: 1, toolCalls: 1, toolOutputs: 3,
			reasoning: 11, toolInteractions: 2,
		},
		{
			name:         "images",
			sourceFormat: "openai",
			endpoint:     "images",
			body: `{"images":[
				{"image_url":"data:image/png;base64,YWJj"},
				{"b64_json":"YWJjZA=="}
			],"mask":{"image_url":"data:image;base64,YQ=="}}`,
			inlineImages: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			vector, ok := inspectRequestComplexityWithDimensions(body, complexityDimensions{
				SourceFormat: tt.sourceFormat,
				Endpoint:     tt.endpoint,
				Stream:       tt.stream,
			})
			if !ok {
				t.Fatal("inspectRequestComplexityWithDimensions() rejected valid JSON")
			}
			if vector.DecodedBytes != int64(len(body)) || vector.MessageCount != tt.messages ||
				vector.ContentPartCount != tt.parts || vector.ToolCallCount != tt.toolCalls ||
				vector.ToolOutputBytes != tt.toolOutputs || vector.InlineImageBytes != tt.inlineImages ||
				vector.ReasoningBytes != tt.reasoning || vector.DeclaredToolCount != tt.declaredTools ||
				vector.BuiltinToolCount != tt.builtinTools || vector.InteractionCount != tt.toolInteractions {
				t.Fatalf("complexity vector = %+v", vector)
			}
			if vector.SourceFormat != tt.sourceFormat || vector.Endpoint != tt.endpoint || vector.Stream != tt.stream {
				t.Fatalf("dimensions = %q/%q/%t, want %q/%q/%t", vector.SourceFormat, vector.Endpoint, vector.Stream, tt.sourceFormat, tt.endpoint, tt.stream)
			}
		})
	}
}

func TestRequestComplexityMetadataUsesBoundedDimensionsWithoutContent(t *testing.T) {
	const secret = "must-not-enter-metadata"
	body := []byte(`{"messages":[{"role":"tool","content":"` + secret + `"}]}`)
	vector, ok := inspectRequestComplexityWithDimensions(body, complexityDimensions{
		SourceFormat: "untrusted-client-format",
		Endpoint:     "untrusted-endpoint",
		Stream:       true,
	})
	if !ok {
		t.Fatal("inspectRequestComplexityWithDimensions() rejected valid JSON")
	}
	meta := make(map[string]any)
	setRequestShapeAndToolMetadataFromComplexity(meta, vector)

	if got := meta[coreexecutor.RequestSourceFormatMetadataKey]; got != "unknown" {
		t.Fatalf("request_source_format = %#v, want unknown", got)
	}
	if got := meta[coreexecutor.RequestEndpointMetadataKey]; got != "unknown" {
		t.Fatalf("request_endpoint = %#v, want unknown", got)
	}
	if got := meta[coreexecutor.RequestStreamMetadataKey]; got != true {
		t.Fatalf("request_stream = %#v, want true", got)
	}
	if got := meta[coreexecutor.ToolOutputBytesMetadataKey]; got != int64(len(secret)) {
		t.Fatalf("tool_output_bytes = %#v, want %d", got, len(secret))
	}
	for key, value := range meta {
		if strings.Contains(fmt.Sprint(value), secret) {
			t.Fatalf("metadata %q leaked request content", key)
		}
	}
}

func TestComplexityExecutionDimensionsOverrideShapeInference(t *testing.T) {
	vector, ok := inspectRequestComplexity([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if !ok || vector.SourceFormat != "gemini" {
		t.Fatalf("inferred vector = %+v, ok = %t", vector, ok)
	}
	vector.applyDimensions(complexityDimensions{SourceFormat: "claude", Endpoint: "chat", Stream: true})
	if vector.SourceFormat != "claude" || vector.Endpoint != "chat" || !vector.Stream {
		t.Fatalf("authoritative dimensions were not applied: %+v", vector)
	}
}

func TestInspectRequestComplexityRejectsExcessiveJSONDepth(t *testing.T) {
	deepJSON := []byte(strings.Repeat("[", maxRequestJSONNestingDepth+1) + "null" + strings.Repeat("]", maxRequestJSONNestingDepth+1))
	if requestJSONDepthAllowed(deepJSON, maxRequestJSONNestingDepth) {
		t.Fatal("depth guard accepted a body above the nesting limit")
	}
	if _, ok := inspectRequestComplexity(deepJSON); ok {
		t.Fatal("complexity inspection accepted a body above the nesting limit")
	}
	if !requestJSONDepthAllowed([]byte(`{"text":"[[[{{{","value":1}`), maxRequestJSONNestingDepth) {
		t.Fatal("depth guard counted delimiters inside a JSON string")
	}
}

func TestInspectRequestComplexityBoundsNamespaceTraversal(t *testing.T) {
	tool := `{"type":"function","name":"leaf"}`
	for range maxToolShapeNamespaceDepth + 8 {
		tool = `{"type":"namespace","name":"nested","tools":[` + tool + `]}`
	}
	body := []byte(`{"tools":[` + tool + `]}`)
	vector, ok := inspectRequestComplexity(body)
	if !ok {
		t.Fatal("bounded namespace fixture was rejected as invalid JSON")
	}
	if vector.toolShapeNodes != maxToolShapeNamespaceDepth+1 {
		t.Fatalf("visited tool nodes = %d, want %d", vector.toolShapeNodes, maxToolShapeNamespaceDepth+1)
	}
}

func TestInspectRequestComplexityBoundsFunctionDeclarationTraversal(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"tools":[{"functionDeclarations":[`)
	for idx := 0; idx < maxToolShapeNodes+128; idx++ {
		if idx > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"name":"f"}`)
	}
	body.WriteString(`]}]}`)

	vector, valid := inspectRequestComplexity([]byte(body.String()))
	if !valid {
		t.Fatal("function declaration fixture was rejected as invalid JSON")
	}
	if vector.toolShapeNodes != maxToolShapeNodes {
		t.Fatalf("visited tool nodes = %d, want %d", vector.toolShapeNodes, maxToolShapeNodes)
	}
	if vector.DeclaredToolCount != maxToolShapeNodes-1 {
		t.Fatalf("declared tools = %d, want %d", vector.DeclaredToolCount, maxToolShapeNodes-1)
	}
}

func TestInspectRequestComplexityCountsSupportedToolFamilies(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"custom_tool_call","name":"patch"},
		{"type":"custom_tool_call_output","output":"custom"},
		{"type":"mcp_tool_use","name":"read"},
		{"type":"mcp_tool_result","content":"mcp"},
		{"type":"web_search_tool_result","content":"web"},
		{"type":"code_execution_tool_result","content":"code"},
		{"type":"tool_search_tool_result","content":"search"},
		{"type":"future_tool_result","content":"future"},
		{"type":"message","content":[
			{"executableCode":{"language":"PYTHON","code":"print(1)"}},
			{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"gemini"}}
		]}
	]}`)

	vector, ok := inspectRequestComplexity(body)
	if !ok {
		t.Fatal("inspectRequestComplexity() rejected tool family fixture")
	}
	if vector.MessageCount != 9 || vector.ContentPartCount != 7 || vector.ToolCallCount != 3 ||
		vector.InteractionCount != 10 || vector.ToolOutputBytes != 34 || vector.MCPToolCount != 2 ||
		vector.BuiltinToolCount != 5 {
		t.Fatalf("tool family vector = %+v", vector)
	}
}

func TestInspectRequestComplexityCountsImagesJSONReferenceMatrix(t *testing.T) {
	body := []byte(`{
		"image":{"data_url":"data:image/png;base64,YWJj"},
		"images":[
			"iVBORw0KGgo=",
			{"base64":"YW Jj\nZA=="},
			{"b64_json":"YWJj"},
			{"image_url":"data:image/png;base64,YQ=="},
			{"url":"https://example.com/image.png"}
		]
	}`)

	vector, ok := inspectRequestComplexity(body)
	if !ok {
		t.Fatal("inspectRequestComplexity() rejected Images JSON fixture")
	}
	if vector.InlineImageBytes != 19 {
		t.Fatalf("InlineImageBytes = %d, want 19", vector.InlineImageBytes)
	}
}

func TestToolShapeMetadataBoundsUnknownClientTypes(t *testing.T) {
	const secret = "secret_marker_123"
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_` + secret + `"}]}],"tools":[{"type":"mcp_` + secret + `"}]}`)
	meta := make(map[string]any)
	setRequestShapeAndToolMetadata(meta, body)

	got, _ := meta[coreexecutor.ToolShapeTypesMetadataKey].(string)
	if got != "other_tool" {
		t.Fatalf("tool_types = %q, want bounded other_tool", got)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("tool_types leaked client-controlled type %q", secret)
	}
}

func TestInspectRequestComplexityMalformedJSONKeepsOnlySafeDimensions(t *testing.T) {
	body := []byte(`{"messages":[{"content":"secret"}`)
	vector, ok := inspectRequestComplexityWithDimensions(body, complexityDimensions{
		SourceFormat: "claude",
		Endpoint:     "chat",
		Stream:       true,
	})
	if ok {
		t.Fatal("inspectRequestComplexityWithDimensions() accepted malformed JSON")
	}
	if vector.DecodedBytes != int64(len(body)) || vector.SourceFormat != "claude" || vector.Endpoint != "chat" || !vector.Stream {
		t.Fatalf("safe malformed vector = %+v", vector)
	}
	if vector.MessageCount != 0 || vector.ContentPartCount != 0 || vector.ToolOutputBytes != 0 || vector.ReasoningBytes != 0 {
		t.Fatalf("malformed payload exposed partial structure: %+v", vector)
	}
}

func TestRefineComplexityDimensionsUsesLowCardinalityEndpoint(t *testing.T) {
	tests := map[string]string{
		"/v1/messages/count_tokens": "count",
		"/v1/responses/compact":     "compact",
		"/v1/images/generations":    "images",
		"/v1/videos":                "videos",
		"/v1/raw/search":            "raw_search",
		"/v1/responses":             "responses",
		"/v1/chat/completions":      "chat",
	}
	for path, want := range tests {
		got := refineComplexityDimensions(complexityDimensions{SourceFormat: "openai", Endpoint: "chat"}, path)
		if got.Endpoint != want {
			t.Fatalf("path %q endpoint = %q, want %q", path, got.Endpoint, want)
		}
	}
}

func TestExecutionComplexityDimensionsUseBoundedDefaults(t *testing.T) {
	tests := []struct {
		name         string
		sourceFormat string
		alt          string
		stream       bool
		image        bool
		count        bool
		wantSource   string
		wantEndpoint string
	}{
		{name: "chat", sourceFormat: "openai", wantSource: "openai", wantEndpoint: "chat"},
		{name: "responses", sourceFormat: "codex", stream: true, wantSource: "codex", wantEndpoint: "responses"},
		{name: "compact", sourceFormat: "openai-response", alt: "responses/compact", wantSource: "openai-response", wantEndpoint: "compact"},
		{name: "images", sourceFormat: "openai-image", image: true, wantSource: "openai", wantEndpoint: "images"},
		{name: "videos", sourceFormat: "openai-video", wantSource: "openai", wantEndpoint: "videos"},
		{name: "count", sourceFormat: "claude", count: true, wantSource: "claude", wantEndpoint: "count"},
		{name: "unknown", sourceFormat: "custom-client", wantSource: "unknown", wantEndpoint: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := executionComplexityDimensions(tt.sourceFormat, tt.alt, tt.stream, tt.image, tt.count)
			if got.SourceFormat != tt.wantSource || got.Endpoint != tt.wantEndpoint || got.Stream != tt.stream {
				t.Fatalf("dimensions = %+v, want source=%q endpoint=%q stream=%t", got, tt.wantSource, tt.wantEndpoint, tt.stream)
			}
		})
	}
}

func BenchmarkInspectRequestComplexity(b *testing.B) {
	for _, messageCount := range []int{8, 16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("messages_%d", messageCount), func(b *testing.B) {
			rawJSON := buildComplexityBenchmarkBody(messageCount)
			b.ReportAllocs()
			b.SetBytes(int64(len(rawJSON)))
			b.ResetTimer()
			for range b.N {
				vector, ok := inspectRequestComplexity(rawJSON)
				if !ok {
					b.Fatal("inspectRequestComplexity() rejected benchmark payload")
				}
				benchmarkRequestComplexity = vector
			}
		})
	}

	cases := []struct {
		name       string
		body       []byte
		dimensions complexityDimensions
	}{
		{
			name:       "responses_tool_outputs_64x16KiB",
			body:       buildToolOutputBenchmarkBody(64, 16<<10),
			dimensions: complexityDimensions{SourceFormat: "openai-response", Endpoint: "responses"},
		},
		{
			name:       "claude_inline_images_8x256KiB",
			body:       buildInlineImageBenchmarkBody(8, 256<<10),
			dimensions: complexityDimensions{SourceFormat: "claude", Endpoint: "chat"},
		},
		{
			name:       "codex_reasoning_64x16KiB",
			body:       buildReasoningBenchmarkBody(64, 16<<10),
			dimensions: complexityDimensions{SourceFormat: "codex", Endpoint: "responses", Stream: true},
		},
	}
	for _, tt := range cases {
		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tt.body)))
			b.ResetTimer()
			for range b.N {
				vector, ok := inspectRequestComplexityWithDimensions(tt.body, tt.dimensions)
				if !ok {
					b.Fatal("inspectRequestComplexityWithDimensions() rejected benchmark payload")
				}
				benchmarkRequestComplexity = vector
			}
		})
	}

	b.Run("malformed", func(b *testing.B) {
		body := []byte(`{"input":[{"type":"message"}`)
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for range b.N {
			vector, ok := inspectRequestComplexity(body)
			if ok {
				b.Fatal("inspectRequestComplexity() accepted malformed benchmark payload")
			}
			benchmarkRequestComplexity = vector
		}
	})
}

var benchmarkRequestComplexity complexityVector

func buildComplexityBenchmarkBody(messageCount int) []byte {
	var builder strings.Builder
	builder.Grow(messageCount * 256)
	builder.WriteString(`{"messages":[`)
	for idx := 0; idx < messageCount; idx++ {
		if idx > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"role":"assistant","content":[{"type":"text","text":"message-%d"},{"type":"tool_result","tool_call_id":"call-%d","content":"ok"}],"tool_calls":[{"id":"call-%d","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, idx, idx, idx)
	}
	builder.WriteString(`],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	return []byte(builder.String())
}

func buildToolOutputBenchmarkBody(outputCount, outputBytes int) []byte {
	var builder strings.Builder
	builder.Grow(outputCount * (outputBytes + 64))
	builder.WriteString(`{"input":[`)
	output := strings.Repeat("x", outputBytes)
	for index := 0; index < outputCount; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"type":"function_call_output","call_id":"call-%d","output":"%s"}`, index, output)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func buildInlineImageBenchmarkBody(imageCount, encodedBytes int) []byte {
	var builder strings.Builder
	builder.Grow(imageCount * (encodedBytes + 128))
	builder.WriteString(`{"messages":[{"role":"user","content":[`)
	data := strings.Repeat("A", encodedBytes)
	for index := 0; index < imageCount; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}`, data)
	}
	builder.WriteString(`]}]}`)
	return []byte(builder.String())
}

func buildReasoningBenchmarkBody(itemCount, reasoningBytes int) []byte {
	var builder strings.Builder
	builder.Grow(itemCount * (reasoningBytes + 96))
	builder.WriteString(`{"input":[`)
	reasoning := strings.Repeat("r", reasoningBytes)
	for index := 0; index < itemCount; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"type":"reasoning","encrypted_content":"%s","summary":[{"type":"summary_text","text":"item-%d"}]}`, reasoning, index)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}
