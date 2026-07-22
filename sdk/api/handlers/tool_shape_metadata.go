package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unsafe"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	maxToolShapeTypes          = 32
	maxToolShapeNameHashes     = 32
	maxToolShapeNodes          = 4096
	maxToolShapeNamespaceDepth = 32
	maxRequestJSONNestingDepth = 128
)

type toolShapeTelemetry struct {
	DeclaredToolCount  int
	InteractionCount   int
	MCPToolCount       int
	BuiltinToolCount   int
	ToolTypes          []string
	ToolNameHashes     []string
	toolTypesSeen      map[string]struct{}
	toolNameHashesSeen map[string]struct{}
}

// complexityVector contains the request-shape metrics available from the same
// gjson walk. Maximum depth is intentionally omitted because gjson does not
// expose it without a separate recursive parse.
type complexityVector struct {
	WireBytes        int64
	DecodedBytes     int64
	MessageCount     int
	ContentPartCount int
	ToolCallCount    int
	ToolOutputBytes  int64
	InlineImageBytes int64
	ReasoningBytes   int64
	SourceFormat     string
	Endpoint         string
	Stream           bool
	toolShapeTelemetry
	toolCompatibility
	responsesChatCompatibility toolCompatibility
	toolShapeNodes             int
}

type toolCompatibility struct {
	hasBuiltinImageGeneration bool
	hasSearchTool             bool
	hasNonSearchTool          bool
}

type complexityDimensions struct {
	SourceFormat string
	Endpoint     string
	Stream       bool
}

func setToolShapeMetadata(meta map[string]any, vector complexityVector) {
	if meta == nil {
		return
	}
	shape := vector.toolShapeTelemetry
	if !shape.hasData() {
		return
	}
	meta[coreexecutor.DeclaredToolCountMetadataKey] = shape.DeclaredToolCount
	meta[coreexecutor.ToolInteractionCountMetadataKey] = shape.InteractionCount
	meta[coreexecutor.MCPToolCountMetadataKey] = shape.MCPToolCount
	meta[coreexecutor.BuiltinToolCountMetadataKey] = shape.BuiltinToolCount
	if len(shape.ToolTypes) > 0 {
		meta[coreexecutor.ToolShapeTypesMetadataKey] = strings.Join(shape.ToolTypes, ",")
	}
	if len(shape.ToolNameHashes) > 0 {
		meta[coreexecutor.ToolNameHashesMetadataKey] = strings.Join(shape.ToolNameHashes, ",")
	}
}

func setRequestShapeMetadata(meta map[string]any, vector complexityVector) {
	if meta == nil {
		return
	}
	if vector.DecodedBytes > 0 {
		meta[coreexecutor.RequestBodyBytesMetadataKey] = int(vector.DecodedBytes)
	}
	if vector.WireBytes > 0 {
		meta[coreexecutor.RequestWireBytesMetadataKey] = vector.WireBytes
	}
	if vector.MessageCount > 0 {
		meta[coreexecutor.MessageCountMetadataKey] = vector.MessageCount
	}
	if vector.ContentPartCount > 0 {
		meta[coreexecutor.ContentPartCountMetadataKey] = vector.ContentPartCount
	}
	if vector.ToolCallCount > 0 {
		meta[coreexecutor.ToolCallCountMetadataKey] = vector.ToolCallCount
	}
	if vector.ToolOutputBytes > 0 {
		meta[coreexecutor.ToolOutputBytesMetadataKey] = vector.ToolOutputBytes
	}
	if vector.InlineImageBytes > 0 {
		meta[coreexecutor.InlineImageBytesMetadataKey] = vector.InlineImageBytes
	}
	if vector.ReasoningBytes > 0 {
		meta[coreexecutor.ReasoningBytesMetadataKey] = vector.ReasoningBytes
	}
	if vector.SourceFormat != "" {
		meta[coreexecutor.RequestSourceFormatMetadataKey] = vector.SourceFormat
	}
	if vector.Endpoint != "" {
		meta[coreexecutor.RequestEndpointMetadataKey] = vector.Endpoint
	}
	if vector.SourceFormat != "" || vector.Endpoint != "" {
		meta[coreexecutor.RequestStreamMetadataKey] = vector.Stream
	}
	toolCount := vector.DeclaredToolCount
	if vector.InteractionCount > toolCount {
		toolCount = vector.InteractionCount
	}
	if toolCount > 0 {
		meta[coreexecutor.ToolCountMetadataKey] = toolCount
	}
}

func setRequestShapeAndToolMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil {
		return
	}
	vector, ok := inspectRequestComplexity(rawJSON)
	if !ok {
		return
	}
	setRequestShapeAndToolMetadataFromComplexity(meta, vector)
}

func setRequestShapeAndToolMetadataFromComplexity(meta map[string]any, vector complexityVector) {
	if meta == nil {
		return
	}
	setRequestShapeMetadata(meta, vector)
	setToolShapeMetadata(meta, vector)
}

func inspectRequestComplexity(rawJSON []byte) (complexityVector, bool) {
	return inspectRequestComplexityWithDimensions(rawJSON, complexityDimensions{})
}

func inspectRequestComplexityWithDimensions(rawJSON []byte, dimensions complexityDimensions) (complexityVector, bool) {
	vector := complexityVector{
		WireBytes:    int64(len(rawJSON)),
		DecodedBytes: int64(len(rawJSON)),
		SourceFormat: normalizeComplexitySourceFormat(dimensions.SourceFormat),
		Endpoint:     normalizeComplexityEndpoint(dimensions.Endpoint),
		Stream:       dimensions.Stream,
	}
	if len(rawJSON) == 0 || !requestJSONDepthAllowed(rawJSON, maxRequestJSONNestingDepth) || !gjson.ValidBytes(rawJSON) {
		return vector, false
	}

	// The request body is immutable for this synchronous inspection. Keep the
	// gjson view zero-copy so large accepted requests are not duplicated.
	root := gjson.Parse(unsafe.String(unsafe.SliceData(rawJSON), len(rawJSON)))
	payload := root
	wrappedRequest := false
	if request := root.Get("request"); request.IsObject() && !hasComplexityPayload(root) {
		payload = request
		wrappedRequest = true
	}

	var messages, input, contents, tools, instructions, system, systemInstruction, images, image, mask gjson.Result
	var messagesSeen, inputSeen, contentsSeen, toolsSeen, instructionsSeen, systemSeen, systemInstructionSeen, imagesSeen, imageSeen, maskSeen bool
	payload.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "messages":
			if !messagesSeen {
				messages, messagesSeen = value, true
			}
		case "input":
			if !inputSeen {
				input, inputSeen = value, true
			}
		case "tools":
			if !toolsSeen {
				tools, toolsSeen = value, true
			}
		case "instructions":
			if !instructionsSeen {
				instructions, instructionsSeen = value, true
			}
		case "contents":
			if !contentsSeen {
				contents, contentsSeen = value, true
			}
		case "system":
			if !systemSeen {
				system, systemSeen = value, true
			}
		case "systemInstruction", "system_instruction":
			if !systemInstructionSeen {
				systemInstruction, systemInstructionSeen = value, true
			}
		case "images":
			if !imagesSeen {
				images, imagesSeen = value, true
			}
		case "image":
			if !imageSeen {
				image, imageSeen = value, true
			}
		case "mask":
			if !maskSeen {
				mask, maskSeen = value, true
			}
		}
		return true
	})
	if vector.SourceFormat == "" {
		if wrappedRequest {
			vector.SourceFormat = "antigravity"
		} else if !messages.Exists() && (input.Exists() || instructions.Exists()) {
			vector.SourceFormat = "openai-response"
		} else if !messages.Exists() && contents.Exists() {
			vector.SourceFormat = "gemini"
		}
	}

	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			return vector.addDeclaredTool(tool)
		})
	}

	hasMessages := messages.IsArray()
	if hasMessages {
		messages.ForEach(func(_, message gjson.Result) bool {
			vector.MessageCount++
			vector.addMessage(message, true)
			return true
		})
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if !hasMessages {
				vector.MessageCount++
			}
			vector.addInput(item)
			return true
		})
	} else if input.Exists() && input.Type != gjson.Null && !hasMessages {
		vector.MessageCount++
		vector.ContentPartCount++
	}
	if !hasMessages && !input.Exists() && contents.IsArray() {
		contents.ForEach(func(_, content gjson.Result) bool {
			vector.MessageCount++
			vector.addMessage(content, true)
			return true
		})
	}
	vector.addSystemContent(system)
	vector.addSystemContent(systemInstruction)
	vector.addSystemContent(instructions)
	vector.addImageContainer(images)
	vector.addImageContainer(image)
	vector.addImageContainer(mask)

	vector.finish()
	return vector, true
}

func requestJSONDepthAllowed(rawJSON []byte, maxDepth int) bool {
	if maxDepth < 1 {
		return false
	}
	depth := 0
	inString := false
	escaped := false
	for _, value := range rawJSON {
		if inString {
			switch {
			case escaped:
				escaped = false
			case value == '\\':
				escaped = true
			case value == '"':
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !inString && !escaped
}

func hasComplexityPayload(root gjson.Result) bool {
	return root.Get("messages").Exists() || root.Get("input").Exists() || root.Get("contents").Exists()
}

func (s toolShapeTelemetry) hasData() bool {
	return s.DeclaredToolCount > 0 ||
		s.InteractionCount > 0 ||
		s.MCPToolCount > 0 ||
		s.BuiltinToolCount > 0 ||
		len(s.ToolTypes) > 0 ||
		len(s.ToolNameHashes) > 0
}

func isResponsesToolItemType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "function_call", "function_call_output", "tool_call", "tool_result",
		"computer_call", "computer_call_output", "local_shell_call",
		"local_shell_call_output", "mcp_call", "mcp_call_output",
		"web_search_call", "web_search_call_output", "file_search_call",
		"file_search_call_output", "code_interpreter_call", "code_interpreter_call_output",
		"image_generation_call", "image_generation_call_output":
		return true
	default:
		return false
	}
}

func (v *complexityVector) addMessage(message gjson.Result, countRoleInteraction bool) {
	if v == nil {
		return
	}
	if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() {
		toolCalls.ForEach(func(_, call gjson.Result) bool {
			v.InteractionCount++
			v.ToolCallCount++
			callType := normalizeToolShapeType(toolShapeType(call, "tool_call"))
			if callType == "function" {
				callType = "tool_call"
			}
			v.addToolShape(callType, toolShapeName(call), false)
			return true
		})
	}
	if functionCall := message.Get("function_call"); functionCall.Exists() && functionCall.Raw != "null" {
		v.InteractionCount++
		v.ToolCallCount++
		v.addToolShape("function_call", toolShapeName(functionCall), false)
	}
	role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
	if role == "tool" || role == "function" {
		if countRoleInteraction {
			v.InteractionCount++
		}
		v.addToolShape(role+"_result", toolShapeName(message), false)
		v.ToolOutputBytes += jsonContentBytes(message.Get("content"))
	}
	v.ReasoningBytes += firstContentBytes(message, "reasoning_content", "reasoningContent")
	if thinking := message.Get("thinking"); thinking.Type == gjson.String {
		v.ReasoningBytes += int64(len(thinking.String()))
	}
	content := message.Get("content")
	if content.IsArray() {
		content.ForEach(func(_, item gjson.Result) bool {
			v.addContentPart(item, true)
			return true
		})
	} else if content.Exists() && content.Type != gjson.Null {
		v.ContentPartCount++
	}
	if parts := message.Get("parts"); parts.IsArray() {
		parts.ForEach(func(_, part gjson.Result) bool {
			v.addContentPart(part, true)
			return true
		})
	}
}

func (v *complexityVector) addInput(item gjson.Result) {
	if v == nil {
		return
	}
	itemType := normalizeToolShapeType(toolShapeType(item, ""))
	if itemType == "additional_tools" {
		v.addToolShape("additional_tools", "", true)
		if tools := item.Get("tools"); tools.IsArray() {
			tools.ForEach(func(_, tool gjson.Result) bool {
				return v.addDeclaredTool(tool)
			})
		}
		return
	}
	if itemType == "message" || item.Get("role").Exists() {
		if itemType != "message" {
			v.addTypedItemWithType(item, false, itemType)
		}
		v.addMessage(item, false)
		return
	}
	v.addTypedItemWithType(item, true, itemType)
	if content := item.Get("content"); isToolOutputType(itemType) && content.Exists() && content.Type != gjson.Null && !content.IsArray() {
		v.ContentPartCount++
	}
}

func (v *complexityVector) addDeclaredTool(tool gjson.Result) bool {
	if v == nil {
		return false
	}
	if v.toolShapeNodes >= maxToolShapeNodes {
		return false
	}
	if v.SourceFormat == "openai-response" {
		v.addResponsesChatToolDefinition(tool)
	}
	return v.addDeclaredToolStructure(tool, 0)
}

func (v *complexityVector) addDeclaredToolStructure(tool gjson.Result, depth int) bool {
	if v == nil {
		return false
	}
	if v.toolShapeNodes >= maxToolShapeNodes {
		return false
	}
	v.toolShapeNodes++
	rawToolType := normalizeToolShapeType(tool.Get("type").String())
	if rawToolType == "namespace" {
		v.addToolShape("namespace", toolShapeName(tool), true)
		if depth < maxToolShapeNamespaceDepth {
			children := tool.Get("tools")
			if !children.IsArray() {
				return true
			}
			children.ForEach(func(_, child gjson.Result) bool {
				return v.addDeclaredToolStructure(child, depth+1)
			})
		}
		return v.toolShapeNodes < maxToolShapeNodes
	}
	declarations := tool.Get("functionDeclarations")
	if !declarations.IsArray() {
		declarations = tool.Get("function_declarations")
	}
	if declarations.IsArray() {
		declarations.ForEach(func(_, declaration gjson.Result) bool {
			if v.toolShapeNodes >= maxToolShapeNodes {
				return false
			}
			v.toolShapeNodes++
			v.DeclaredToolCount++
			v.addToolShape("function", toolShapeName(declaration), true)
			v.addDeclaredToolCompatibility("function")
			return v.toolShapeNodes < maxToolShapeNodes
		})
	}
	declared := declarations.IsArray()
	for _, candidate := range []struct {
		camelCase string
		snakeCase string
		toolType  string
	}{
		{camelCase: "googleSearch", snakeCase: "google_search", toolType: "google_search"},
		{camelCase: "codeExecution", snakeCase: "code_execution", toolType: "code_execution"},
		{camelCase: "urlContext", snakeCase: "url_context", toolType: "url_context"},
	} {
		if tool.Get(candidate.camelCase).Exists() || tool.Get(candidate.snakeCase).Exists() {
			declared = true
			v.DeclaredToolCount++
			v.addToolShape(candidate.toolType, "", true)
			v.addDeclaredToolCompatibility(candidate.toolType)
		}
	}
	if declared {
		return v.toolShapeNodes < maxToolShapeNodes
	}
	v.DeclaredToolCount++
	toolType := toolShapeType(tool, "tool")
	toolName := toolShapeName(tool)
	v.addToolShape(toolType, toolName, true)
	v.addDeclaredToolCompatibility(toolType)
	return v.toolShapeNodes < maxToolShapeNodes
}

func (v *complexityVector) addResponsesChatToolDefinition(tool gjson.Result) {
	if v == nil {
		return
	}
	toolType := normalizeToolShapeType(tool.Get("type").String())
	if toolType != "namespace" {
		v.addResponsesChatToolCompatibility(toolType, responsesChatToolName(tool))
		return
	}
	if children := tool.Get("tools"); children.IsArray() {
		visited := 0
		children.ForEach(func(_, child gjson.Result) bool {
			if visited >= maxToolShapeNodes {
				return false
			}
			visited++
			v.addResponsesChatToolCompatibility(child.Get("type").String(), responsesChatToolName(child))
			return visited < maxToolShapeNodes
		})
	}
}

func responsesChatToolName(tool gjson.Result) string {
	return firstString(tool, "name", "function.name")
}

func (v *complexityVector) addDeclaredToolCompatibility(toolType string) {
	if v == nil {
		return
	}
	switch normalizeToolShapeType(toolType) {
	case "image_generation":
		v.hasBuiltinImageGeneration = true
	case "google_search", "web_search":
		v.hasSearchTool = true
	case "function", "custom", "code_execution", "url_context":
		v.hasNonSearchTool = true
	}
}

func (v *complexityVector) addResponsesChatToolCompatibility(toolType, toolName string) {
	if v == nil || strings.TrimSpace(toolName) == "" {
		return
	}
	switch normalizeToolShapeType(toolType) {
	case "", "function", "custom":
		v.responsesChatCompatibility.hasNonSearchTool = true
	}
}

func (v *complexityVector) useResponsesChatToolCompatibility() {
	if v == nil {
		return
	}
	v.toolCompatibility = v.responsesChatCompatibility
}

func (v *complexityVector) addSystemContent(system gjson.Result) {
	if v == nil || !system.Exists() || system.Type == gjson.Null {
		return
	}
	if parts := system.Get("parts"); parts.IsArray() {
		parts.ForEach(func(_, part gjson.Result) bool {
			v.addContentPart(part, false)
			return true
		})
		return
	}
	if system.IsArray() {
		system.ForEach(func(_, part gjson.Result) bool {
			v.addContentPart(part, false)
			return true
		})
		return
	}
	v.ContentPartCount++
}

func (v *complexityVector) addContentPart(item gjson.Result, inspectNested bool) {
	if v == nil {
		return
	}
	v.ContentPartCount++
	v.addTypedItem(item, inspectNested)
}

func (v *complexityVector) addTypedItem(item gjson.Result, inspectNested bool) {
	if v == nil || !item.Exists() {
		return
	}
	itemType := normalizeToolShapeType(toolShapeType(item, ""))
	v.addTypedItemWithType(item, inspectNested, itemType)
}

func (v *complexityVector) addTypedItemWithType(item gjson.Result, inspectNested bool, itemType string) {
	if v == nil || !item.Exists() {
		return
	}
	if isToolCallType(itemType) {
		v.InteractionCount++
		v.ToolCallCount++
		v.addToolShape(itemType, toolShapeName(item), false)
	} else if isToolOutputType(itemType) {
		v.InteractionCount++
		v.addToolShape(itemType, toolShapeName(item), false)
		v.ToolOutputBytes += toolOutputBytes(item)
		if inspectNested {
			v.addNestedToolContent(item.Get("content"))
		}
	} else if isToolShapeInteractionType(itemType) {
		v.InteractionCount++
		v.addToolShape(itemType, toolShapeName(item), false)
	}

	if functionCall := item.Get("functionCall"); functionCall.IsObject() {
		v.InteractionCount++
		v.ToolCallCount++
		v.addToolShape("function_call", toolShapeName(functionCall), false)
	}
	if functionResponse := item.Get("functionResponse"); functionResponse.IsObject() {
		v.InteractionCount++
		v.addToolShape("function_response", toolShapeName(functionResponse), false)
		v.ToolOutputBytes += jsonContentBytes(functionResponse.Get("response"))
	}
	if executableCode := firstObject(item, "executableCode", "executable_code"); executableCode.Exists() {
		v.InteractionCount++
		v.ToolCallCount++
		v.addToolShape("code_execution", "", false)
	}
	if codeResult := firstObject(item, "codeExecutionResult", "code_execution_result"); codeResult.Exists() {
		v.InteractionCount++
		v.addToolShape("code_execution_tool_result", "", false)
		v.ToolOutputBytes += toolOutputBytes(codeResult)
	}

	v.ReasoningBytes += reasoningItemBytes(item, itemType)
	v.InlineImageBytes += inlineImageBytes(item)
}

func (v *complexityVector) addNestedToolContent(content gjson.Result) {
	if v == nil || !content.IsArray() {
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		v.addContentPart(part, false)
		return true
	})
}

func (v *complexityVector) addImageContainer(value gjson.Result) {
	if v == nil || !value.Exists() || value.Type == gjson.Null {
		return
	}
	if value.IsArray() {
		value.ForEach(func(_, item gjson.Result) bool {
			v.InlineImageBytes += inlineImageValueBytes(item)
			return true
		})
		return
	}
	v.InlineImageBytes += inlineImageValueBytes(value)
}

func normalizeComplexitySourceFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai", "openai-chat", "openai-image", "openai-video":
		return "openai"
	case "openai-response", "responses":
		return "openai-response"
	case "claude", "anthropic":
		return "claude"
	case "gemini":
		return "gemini"
	case "codex":
		return "codex"
	case "antigravity":
		return "antigravity"
	case "":
		return ""
	default:
		return "unknown"
	}
}

func normalizeComplexityEndpoint(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "chat", "responses", "compact", "count", "images", "videos", "raw_search":
		return strings.ToLower(strings.TrimSpace(value))
	case "":
		return ""
	default:
		return "unknown"
	}
}

func executionComplexityDimensions(sourceFormat, alt string, stream, imageEndpoint, countEndpoint bool) complexityDimensions {
	normalizedSource := normalizeComplexitySourceFormat(sourceFormat)
	dimensions := complexityDimensions{SourceFormat: normalizedSource, Stream: stream}
	switch {
	case countEndpoint:
		dimensions.Endpoint = "count"
	case imageEndpoint, strings.EqualFold(sourceFormat, "openai-image"):
		dimensions.Endpoint = "images"
	case strings.EqualFold(sourceFormat, "openai-video"):
		dimensions.Endpoint = "videos"
	case strings.EqualFold(strings.TrimSpace(alt), "responses/compact"):
		dimensions.Endpoint = "compact"
	case normalizedSource == "openai-response", normalizedSource == "codex":
		dimensions.Endpoint = "responses"
	case normalizedSource == "", normalizedSource == "unknown":
		dimensions.Endpoint = "unknown"
	default:
		dimensions.Endpoint = "chat"
	}
	return dimensions
}

func refineComplexityDimensions(dimensions complexityDimensions, requestPath string) complexityDimensions {
	path := strings.ToLower(strings.TrimSpace(requestPath))
	switch {
	case strings.Contains(path, "count_tokens") || strings.Contains(path, "counttokens"):
		dimensions.Endpoint = "count"
	case strings.Contains(path, "/responses/compact"):
		dimensions.Endpoint = "compact"
	case strings.Contains(path, "/images"):
		dimensions.Endpoint = "images"
	case strings.Contains(path, "/videos"):
		dimensions.Endpoint = "videos"
	case strings.Contains(path, "/search") || strings.HasSuffix(path, "search"):
		dimensions.Endpoint = "raw_search"
	case strings.Contains(path, "/responses"):
		dimensions.Endpoint = "responses"
	case strings.Contains(path, "/chat/") || strings.Contains(path, "/messages") ||
		strings.Contains(path, "generatecontent"):
		dimensions.Endpoint = "chat"
	}
	dimensions.SourceFormat = normalizeComplexitySourceFormat(dimensions.SourceFormat)
	dimensions.Endpoint = normalizeComplexityEndpoint(dimensions.Endpoint)
	return dimensions
}

func (v *complexityVector) applyDimensions(dimensions complexityDimensions) {
	if v == nil {
		return
	}
	if sourceFormat := normalizeComplexitySourceFormat(dimensions.SourceFormat); sourceFormat != "" {
		v.SourceFormat = sourceFormat
	}
	v.Endpoint = normalizeComplexityEndpoint(dimensions.Endpoint)
	v.Stream = dimensions.Stream
}

func requestPathMetadata(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	value, _ := meta[coreexecutor.RequestPathMetadataKey].(string)
	return value
}

func isToolCallType(value string) bool {
	switch normalizeToolShapeType(value) {
	case "function_call", "custom_tool_call", "tool_call", "tool_use", "server_tool_use", "mcp_tool_use",
		"computer_call", "local_shell_call", "mcp_call", "web_search_call",
		"file_search_call", "code_interpreter_call", "image_generation_call",
		"code_execution":
		return true
	default:
		return false
	}
}

func isToolOutputType(value string) bool {
	value = normalizeToolShapeType(value)
	switch value {
	case "function_call_output", "custom_tool_call_output", "function_response", "tool_result", "tool_output", "mcp_tool_result",
		"computer_call_output", "local_shell_call_output", "mcp_call_output",
		"web_search_call_output", "file_search_call_output",
		"code_interpreter_call_output", "image_generation_call_output",
		"web_search_tool_result", "code_execution_tool_result", "tool_search_tool_result":
		return true
	default:
		return strings.HasSuffix(value, "_tool_result")
	}
}

func toolOutputBytes(item gjson.Result) int64 {
	for _, path := range []string{"output", "content", "result", "response"} {
		if value := item.Get(path); value.Exists() && value.Type != gjson.Null {
			return jsonContentBytes(value)
		}
	}
	return 0
}

func jsonContentBytes(value gjson.Result) int64 {
	if !value.Exists() || value.Type == gjson.Null {
		return 0
	}
	if value.Type == gjson.String {
		return int64(len(value.String()))
	}
	return int64(len(value.Raw))
}

func firstContentBytes(item gjson.Result, paths ...string) int64 {
	for _, path := range paths {
		if value := item.Get(path); value.Exists() && value.Type != gjson.Null {
			return jsonContentBytes(value)
		}
	}
	return 0
}

func firstObject(item gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if value := item.Get(path); value.IsObject() {
			return value
		}
	}
	return gjson.Result{}
}

func reasoningItemBytes(item gjson.Result, itemType string) int64 {
	var total int64
	switch itemType {
	case "thinking", "thinking_text":
		total += firstContentBytes(item, "thinking", "text")
	case "redacted_thinking":
		total += firstContentBytes(item, "data")
	case "reasoning", "reasoning_text":
		total += firstContentBytes(item, "text", "reasoning_content", "encrypted_content")
		if encrypted := item.Get("encrypted_content"); encrypted.Exists() && item.Get("text").Exists() {
			total += jsonContentBytes(encrypted)
		}
		if summary := item.Get("summary"); summary.IsArray() {
			summary.ForEach(func(_, part gjson.Result) bool {
				total += firstContentBytes(part, "text", "content")
				return true
			})
		}
	}
	if item.Get("thought").Bool() {
		total += firstContentBytes(item, "text")
	}
	return total
}

func inlineImageBytes(item gjson.Result) int64 {
	var total int64
	for _, path := range []string{"image_url.url", "image_url", "url", "data_url"} {
		value := item.Get(path)
		if value.Type == gjson.String {
			if size := dataImageBytes(value.String()); size > 0 {
				total += size
				break
			}
		}
	}
	if source := item.Get("source"); source.IsObject() {
		mediaType := firstString(source, "media_type", "mime_type")
		if strings.EqualFold(source.Get("type").String(), "base64") && isImageMediaType(mediaType) {
			total += decodedBase64Bytes(source.Get("data").String())
		}
	}
	for _, path := range []string{"inlineData", "inline_data"} {
		if inline := item.Get(path); inline.IsObject() {
			if isImageMediaType(firstString(inline, "mimeType", "mime_type")) {
				total += decodedBase64Bytes(inline.Get("data").String())
			}
		}
	}
	for _, path := range []string{"b64_json", "base64"} {
		if value := item.Get(path); value.Type == gjson.String {
			total += encodedImageBytes(value.String())
		}
	}
	return total
}

func inlineImageValueBytes(value gjson.Result) int64 {
	if value.Type == gjson.String {
		encoded := value.String()
		if size := dataImageBytes(encoded); size > 0 {
			return size
		}
		if looksLikeBase64Image(encoded) {
			return decodedBase64Bytes(encoded)
		}
		return 0
	}
	return inlineImageBytes(value)
}

func encodedImageBytes(value string) int64 {
	if size := dataImageBytes(value); size > 0 {
		return size
	}
	return decodedBase64Bytes(value)
}

func looksLikeBase64Image(value string) bool {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{
		"iVBORw0KGgo", // PNG
		"/9j/",        // JPEG
		"R0lGOD",      // GIF
		"UklGR",       // WebP
		"Qk",          // BMP
		"SUkq",        // little-endian TIFF
		"TU0A",        // big-endian TIFF
	} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func firstString(item gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(item.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func dataImageBytes(value string) int64 {
	value = strings.TrimSpace(value)
	comma := strings.IndexByte(value, ',')
	if comma <= 0 {
		return 0
	}
	header := strings.ToLower(value[:comma])
	if (!strings.HasPrefix(header, "data:image/") && !strings.HasPrefix(header, "data:image;")) ||
		!strings.Contains(header, ";base64") {
		return 0
	}
	return decodedBase64Bytes(value[comma+1:])
}

func decodedBase64Bytes(value string) int64 {
	encodedBytes := int64(0)
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			encodedBytes++
		}
	}
	if encodedBytes == 0 {
		return 0
	}
	padding := 0
	for index := len(value) - 1; index >= 0 && padding < 2; index-- {
		switch value[index] {
		case ' ', '\n', '\r', '\t':
			continue
		case '=':
			padding++
			continue
		}
		break
	}
	decoded := ((encodedBytes + 3) / 4 * 3) - int64(padding)
	if decoded < 0 {
		return 0
	}
	return decoded
}

func isImageMediaType(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "image/")
}

func (s *toolShapeTelemetry) addToolShape(toolType, toolName string, declared bool) {
	if s == nil {
		return
	}
	toolType = normalizeToolShapeType(toolType)
	toolName = strings.TrimSpace(toolName)
	if toolType == "" && toolName == "" {
		return
	}
	if toolType != "" {
		s.addToolType(toolType)
	}
	if toolName != "" {
		s.addToolNameHash(toolName)
	}
	if isMCPToolShape(toolType, toolName) {
		s.MCPToolCount++
	}
	if isBuiltinToolShape(toolType, toolName, declared) {
		s.BuiltinToolCount++
	}
}

func (s *toolShapeTelemetry) addToolType(toolType string) {
	if s == nil || toolType == "" {
		return
	}
	toolType = metadataToolShapeType(toolType)
	if s.toolTypesSeen == nil {
		s.toolTypesSeen = make(map[string]struct{})
	}
	if _, ok := s.toolTypesSeen[toolType]; ok {
		return
	}
	if len(s.toolTypesSeen) >= maxToolShapeTypes {
		return
	}
	s.toolTypesSeen[toolType] = struct{}{}
}

func metadataToolShapeType(value string) string {
	value = normalizeToolShapeType(value)
	switch value {
	case "tool", "function", "custom", "namespace", "additional_tools", "mcp", "builtin",
		"function_call", "custom_tool_call", "tool_call", "tool_use", "server_tool_use", "mcp_tool_use",
		"computer_call", "local_shell_call", "mcp_call", "web_search_call", "file_search_call",
		"code_interpreter_call", "image_generation_call", "code_execution",
		"function_call_output", "custom_tool_call_output", "function_response", "function_result",
		"tool_result", "tool_output", "mcp_tool_result", "computer_call_output", "local_shell_call_output",
		"mcp_call_output", "web_search_call_output", "file_search_call_output",
		"code_interpreter_call_output", "image_generation_call_output",
		"web_search_tool_result", "code_execution_tool_result", "tool_search_tool_result",
		"web_search", "web_search_preview", "google_search", "url_context", "code_interpreter",
		"file_search", "image_generation":
		return value
	default:
		return "other_tool"
	}
}

func (s *toolShapeTelemetry) addToolNameHash(toolName string) {
	if s == nil || toolName == "" {
		return
	}
	hash := toolShapeNameHash(toolName)
	if hash == "" {
		return
	}
	if s.toolNameHashesSeen == nil {
		s.toolNameHashesSeen = make(map[string]struct{})
	}
	if _, ok := s.toolNameHashesSeen[hash]; ok {
		return
	}
	if len(s.toolNameHashesSeen) >= maxToolShapeNameHashes {
		return
	}
	s.toolNameHashesSeen[hash] = struct{}{}
}

func (s *toolShapeTelemetry) finish() {
	if s == nil {
		return
	}
	s.ToolTypes = sortedToolShapeKeys(s.toolTypesSeen)
	s.ToolNameHashes = sortedToolShapeKeys(s.toolNameHashesSeen)
}

func sortedToolShapeKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func toolShapeType(item gjson.Result, fallback string) string {
	for _, path := range []string{"type", "tool_type", "function.type"} {
		if value := strings.TrimSpace(item.Get(path).String()); value != "" {
			return value
		}
	}
	if item.Get("function").Exists() {
		return "function"
	}
	return fallback
}

func toolShapeName(item gjson.Result) string {
	for _, path := range []string{
		"function.name",
		"name",
		"tool_name",
		"server_label",
		"server_name",
	} {
		if value := strings.TrimSpace(item.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func normalizeToolShapeType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(value))
	lastUnderscore := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastUnderscore = false
		case r == '-' || r == '_':
			builder.WriteRune(r)
			lastUnderscore = r == '_'
		default:
			if !lastUnderscore {
				builder.WriteByte('_')
				lastUnderscore = true
			}
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return strings.Trim(builder.String(), "_-")
}

func toolShapeNameHash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func isToolShapeInteractionType(value string) bool {
	value = normalizeToolShapeType(value)
	if value == "" {
		return false
	}
	if isResponsesToolItemType(value) {
		return true
	}
	return strings.Contains(value, "tool") ||
		strings.Contains(value, "mcp") ||
		strings.Contains(value, "function_call") ||
		strings.Contains(value, "web_search") ||
		strings.Contains(value, "code_interpreter") ||
		strings.Contains(value, "code_execution") ||
		strings.Contains(value, "file_search") ||
		strings.Contains(value, "computer_call")
}

func isMCPToolShape(toolType, toolName string) bool {
	toolType = normalizeToolShapeType(toolType)
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	return strings.Contains(toolType, "mcp") ||
		strings.HasPrefix(toolName, "mcp__") ||
		strings.Contains(toolName, ".mcp.") ||
		strings.Contains(toolName, "/mcp")
}

func isBuiltinToolShape(toolType, toolName string, declared bool) bool {
	toolType = normalizeToolShapeType(toolType)
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	if strings.Contains(toolType, "builtin") ||
		strings.Contains(toolName, "$web_search") {
		return true
	}
	switch toolType {
	case "web_search", "web_search_preview", "web_search_call", "web_search_tool_result",
		"google_search", "url_context", "code_execution", "code_execution_tool_result", "tool_search_tool_result", "code_interpreter",
		"computer_call", "file_search", "image_generation":
		return true
	default:
		return declared && strings.HasPrefix(toolName, "$")
	}
}
