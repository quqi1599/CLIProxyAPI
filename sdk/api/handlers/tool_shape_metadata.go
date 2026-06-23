package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	maxToolShapeTypes      = 32
	maxToolShapeNameHashes = 32
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

func setToolShapeMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil || len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
		return
	}
	shape := requestToolShapeTelemetry(rawJSON)
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

func requestToolShapeTelemetry(rawJSON []byte) toolShapeTelemetry {
	shape := newToolShapeTelemetry()
	shape.DeclaredToolCount = requestDeclaredToolCount(rawJSON)
	shape.InteractionCount = requestToolInteractionCount(rawJSON)

	if tools := gjson.GetBytes(rawJSON, "tools"); tools.IsArray() {
		for _, tool := range tools.Array() {
			shape.addToolShape(toolShapeType(tool, "tool"), toolShapeName(tool), true)
		}
	}
	if messages := gjson.GetBytes(rawJSON, "messages"); messages.IsArray() {
		for _, message := range messages.Array() {
			shape.addMessageToolShapes(message)
		}
	}
	if input := gjson.GetBytes(rawJSON, "input"); input.IsArray() {
		for _, item := range input.Array() {
			shape.addInputToolShapes(item)
		}
	}

	shape.finish()
	return shape
}

func newToolShapeTelemetry() toolShapeTelemetry {
	return toolShapeTelemetry{
		toolTypesSeen:      make(map[string]struct{}),
		toolNameHashesSeen: make(map[string]struct{}),
	}
}

func (s toolShapeTelemetry) hasData() bool {
	return s.DeclaredToolCount > 0 ||
		s.InteractionCount > 0 ||
		s.MCPToolCount > 0 ||
		s.BuiltinToolCount > 0 ||
		len(s.ToolTypes) > 0 ||
		len(s.ToolNameHashes) > 0
}

func requestDeclaredToolCount(rawJSON []byte) int {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.IsArray() {
		return 0
	}
	return len(tools.Array())
}

func requestToolInteractionCount(rawJSON []byte) int {
	count := 0
	if messages := gjson.GetBytes(rawJSON, "messages"); messages.IsArray() {
		for _, message := range messages.Array() {
			count += toolInteractionsInObject(message)
			role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
			if role == "tool" || role == "function" {
				count++
			}
		}
	}
	if input := gjson.GetBytes(rawJSON, "input"); input.IsArray() {
		for _, item := range input.Array() {
			count += toolInteractionsInObject(item)
			if isResponsesToolItemType(item.Get("type").String()) {
				count++
			}
		}
	}
	return count
}

func toolInteractionsInObject(value gjson.Result) int {
	count := 0
	if toolCalls := value.Get("tool_calls"); toolCalls.IsArray() {
		count += len(toolCalls.Array())
	}
	if functionCall := value.Get("function_call"); functionCall.Exists() && functionCall.Raw != "null" {
		count++
	}
	content := value.Get("content")
	if content.IsArray() {
		for _, item := range content.Array() {
			if isResponsesToolItemType(item.Get("type").String()) {
				count++
			}
		}
	}
	return count
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

func (s *toolShapeTelemetry) addMessageToolShapes(message gjson.Result) {
	if s == nil {
		return
	}
	if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() {
		for _, call := range toolCalls.Array() {
			callType := normalizeToolShapeType(toolShapeType(call, "tool_call"))
			if callType == "function" {
				callType = "tool_call"
			}
			s.addToolShape(callType, toolShapeName(call), false)
		}
	}
	if functionCall := message.Get("function_call"); functionCall.Exists() && functionCall.Raw != "null" {
		s.addToolShape("function_call", toolShapeName(functionCall), false)
	}
	role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
	if role == "tool" || role == "function" {
		s.addToolShape(role+"_result", toolShapeName(message), false)
	}
	content := message.Get("content")
	if content.IsArray() {
		for _, item := range content.Array() {
			if itemType := toolShapeType(item, ""); isToolShapeInteractionType(itemType) {
				s.addToolShape(itemType, toolShapeName(item), false)
			}
		}
	}
}

func (s *toolShapeTelemetry) addInputToolShapes(item gjson.Result) {
	if s == nil {
		return
	}
	itemType := toolShapeType(item, "")
	if isToolShapeInteractionType(itemType) {
		s.addToolShape(itemType, toolShapeName(item), false)
	}
	s.addMessageToolShapes(item)
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
	if _, ok := s.toolTypesSeen[toolType]; ok {
		return
	}
	if len(s.toolTypesSeen) >= maxToolShapeTypes {
		return
	}
	s.toolTypesSeen[toolType] = struct{}{}
}

func (s *toolShapeTelemetry) addToolNameHash(toolName string) {
	if s == nil || toolName == "" {
		return
	}
	hash := toolShapeNameHash(toolName)
	if hash == "" {
		return
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
		"google_search", "url_context", "code_execution", "code_interpreter",
		"computer_call", "file_search", "image_generation":
		return true
	default:
		return declared && strings.HasPrefix(toolName, "$")
	}
}
