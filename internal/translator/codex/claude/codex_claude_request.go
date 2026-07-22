// Package claude provides request translation functionality for Claude Code API compatibility.
// It handles parsing and transforming Claude Code API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude Code API format and the internal client's expected format.
package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeRequestToCodex parses and transforms a Claude Code API request into the internal client format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
// The function performs the following transformations:
// 1. Sets up a template with the model name and empty instructions field
// 2. Processes system messages and converts them to developer input content
// 3. Transforms message contents (text, image, tool_use, tool_result) to appropriate formats
// 4. Converts tools declarations to the expected format
// 5. Adds additional configuration parameters for the Codex API
// 6. Maps Claude thinking configuration to Codex reasoning settings
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Claude Code API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in internal client format
func ConvertClaudeRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	template := []byte(`{"model":"","instructions":"","input":[]}`)

	rootResult := gjson.ParseBytes(rawJSON)
	toolNameMap := buildReverseMapFromClaudeOriginalToShort(rawJSON)
	template, _ = sjson.SetBytes(template, "model", modelName)
	inputItems := make([]json.RawMessage, 0)

	// Process system messages and convert them to input content format.
	systemsResult := rootResult.Get("system")
	if systemsResult.Exists() {
		message := []byte(`{"type":"message","role":"developer","content":[]}`)
		contentItems := make([]json.RawMessage, 0)

		appendSystemText := func(text string) {
			if text == "" || util.IsClaudeCodeAttributionSystemText(text) {
				return
			}

			part := []byte(`{"type":"input_text","text":""}`)
			part, _ = sjson.SetBytes(part, "text", text)
			contentItems = append(contentItems, json.RawMessage(part))
		}

		if systemsResult.Type == gjson.String {
			appendSystemText(systemsResult.String())
		} else if systemsResult.IsArray() {
			systemResults := systemsResult.Array()
			for i := 0; i < len(systemResults); i++ {
				systemResult := systemResults[i]
				if systemResult.Get("type").String() == "text" {
					appendSystemText(systemResult.Get("text").String())
				}
			}
		}

		if len(contentItems) > 0 {
			content, _ := json.Marshal(contentItems)
			message, _ = sjson.SetRawBytes(message, "content", content)
			inputItems = append(inputItems, json.RawMessage(message))
		}
	}

	// Process messages and transform their contents to appropriate formats.
	messagesResult := rootResult.Get("messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()

		for i := 0; i < len(messageResults); i++ {
			messageResult := messageResults[i]
			messageRole := messageResult.Get("role").String()
			if messageRole == "system" {
				if reminderText, ok := translatorcommon.ClaudeMessageSystemReminderText(messageResult.Get("content")); ok {
					message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
					message, _ = sjson.SetBytes(message, "content.0.text", reminderText)
					inputItems = append(inputItems, json.RawMessage(message))
				}
				continue
			}

			newMessage := func() []byte {
				msg := []byte(`{"type":"message","role":"","content":[]}`)
				msg, _ = sjson.SetBytes(msg, "role", messageRole)
				return msg
			}

			message := newMessage()
			contentItems := make([]json.RawMessage, 0)

			flushMessage := func() {
				if len(contentItems) > 0 {
					content, _ := json.Marshal(contentItems)
					message, _ = sjson.SetRawBytes(message, "content", content)
					inputItems = append(inputItems, json.RawMessage(message))
					message = newMessage()
					contentItems = contentItems[:0]
				}
			}

			appendTextContent := func(text string) {
				partType := "input_text"
				if messageRole == "assistant" {
					partType = "output_text"
				}
				part := []byte(`{"type":"","text":""}`)
				part, _ = sjson.SetBytes(part, "type", partType)
				part, _ = sjson.SetBytes(part, "text", text)
				contentItems = append(contentItems, json.RawMessage(part))
			}

			appendImageContent := func(dataURL string) {
				part := []byte(`{"type":"input_image","image_url":""}`)
				part, _ = sjson.SetBytes(part, "image_url", dataURL)
				contentItems = append(contentItems, json.RawMessage(part))
			}

			appendReasoningContent := func(part gjson.Result) {
				if messageRole != "assistant" {
					return
				}

				signature, ok := sigcompat.CompatibleSignatureForProvider(sigcompat.SignatureProviderGPT, part.Get("signature").String())
				if !ok {
					return
				}

				flushMessage()
				reasoningItem := []byte(`{"type":"reasoning","summary":[],"content":null}`)
				reasoningItem, _ = sjson.SetBytes(reasoningItem, "encrypted_content", signature)
				inputItems = append(inputItems, json.RawMessage(reasoningItem))
			}

			messageContentsResult := messageResult.Get("content")
			if messageContentsResult.IsArray() {
				messageContentResults := messageContentsResult.Array()
				for j := 0; j < len(messageContentResults); j++ {
					messageContentResult := messageContentResults[j]
					contentType := messageContentResult.Get("type").String()

					switch contentType {
					case "text":
						appendTextContent(messageContentResult.Get("text").String())
					case "thinking":
						appendReasoningContent(messageContentResult)
					case "image":
						sourceResult := messageContentResult.Get("source")
						if sourceResult.Exists() {
							data := sourceResult.Get("data").String()
							if data == "" {
								data = sourceResult.Get("base64").String()
							}
							if data != "" {
								mediaType := sourceResult.Get("media_type").String()
								if mediaType == "" {
									mediaType = sourceResult.Get("mime_type").String()
								}
								if mediaType == "" {
									mediaType = "application/octet-stream"
								}
								dataURL := fmt.Sprintf("data:%s;base64,%s", mediaType, data)
								appendImageContent(dataURL)
							}
						}
					case "tool_use":
						flushMessage()
						functionCallMessage := []byte(`{"type":"function_call"}`)
						functionCallMessage, _ = sjson.SetBytes(functionCallMessage, "call_id", shortenCodexCallIDIfNeeded(messageContentResult.Get("id").String()))
						{
							name, ok := util.NormalizeRequestToolName(messageContentResult.Get("name").String(), nil)
							if !ok {
								break
							}
							if short, ok := toolNameMap[name]; ok {
								name = short
							} else {
								name = shortenNameIfNeeded(name)
							}
							functionCallMessage, _ = sjson.SetBytes(functionCallMessage, "name", name)
						}
						functionCallMessage, _ = sjson.SetBytes(functionCallMessage, "arguments", messageContentResult.Get("input").Raw)
						inputItems = append(inputItems, json.RawMessage(functionCallMessage))
					case "tool_result":
						flushMessage()
						functionCallOutputMessage := []byte(`{"type":"function_call_output"}`)
						functionCallOutputMessage, _ = sjson.SetBytes(functionCallOutputMessage, "call_id", shortenCodexCallIDIfNeeded(messageContentResult.Get("tool_use_id").String()))

						contentResult := messageContentResult.Get("content")
						if contentResult.IsArray() {
							toolResultContent := make([]json.RawMessage, 0)
							contentResults := contentResult.Array()
							for k := 0; k < len(contentResults); k++ {
								toolResultContentType := contentResults[k].Get("type").String()
								if toolResultContentType == "image" {
									sourceResult := contentResults[k].Get("source")
									if sourceResult.Exists() {
										data := sourceResult.Get("data").String()
										if data == "" {
											data = sourceResult.Get("base64").String()
										}
										if data != "" {
											mediaType := sourceResult.Get("media_type").String()
											if mediaType == "" {
												mediaType = sourceResult.Get("mime_type").String()
											}
											if mediaType == "" {
												mediaType = "application/octet-stream"
											}
											dataURL := fmt.Sprintf("data:%s;base64,%s", mediaType, data)

											part := []byte(`{"type":"input_image","image_url":""}`)
											part, _ = sjson.SetBytes(part, "image_url", dataURL)
											toolResultContent = append(toolResultContent, json.RawMessage(part))
										}
									}
								} else if toolResultContentType == "text" {
									part := []byte(`{"type":"input_text","text":""}`)
									part, _ = sjson.SetBytes(part, "text", contentResults[k].Get("text").String())
									toolResultContent = append(toolResultContent, json.RawMessage(part))
								}
							}
							if len(toolResultContent) > 0 {
								output, _ := json.Marshal(toolResultContent)
								functionCallOutputMessage, _ = sjson.SetRawBytes(functionCallOutputMessage, "output", output)
							} else {
								functionCallOutputMessage, _ = sjson.SetBytes(functionCallOutputMessage, "output", messageContentResult.Get("content").String())
							}
						} else {
							functionCallOutputMessage, _ = sjson.SetBytes(functionCallOutputMessage, "output", messageContentResult.Get("content").String())
						}

						inputItems = append(inputItems, json.RawMessage(functionCallOutputMessage))
					}
				}
				flushMessage()
			} else if messageContentsResult.Type == gjson.String {
				appendTextContent(messageContentsResult.String())
				flushMessage()
			}
		}

	}
	input, _ := json.Marshal(inputItems)
	template, _ = sjson.SetRawBytes(template, "input", input)

	// Convert tools declarations to the expected format for the Codex API.
	toolsResult := rootResult.Get("tools")
	if toolsResult.IsArray() {
		webSearchToolNames := buildClaudeWebSearchToolNameSet(toolsResult)
		template, _ = sjson.SetRawBytes(template, "tool_choice", convertClaudeToolChoiceToCodex(rootResult.Get("tool_choice"), toolNameMap, webSearchToolNames))
		toolItems := make([]json.RawMessage, 0)
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			toolResult := toolResults[i]
			// Special handling: map Claude web search tool to Codex web_search
			if isClaudeWebSearchToolType(toolResult.Get("type").String()) {
				toolItems = append(toolItems, json.RawMessage(convertClaudeWebSearchToolToCodex(toolResult)))
				continue
			}
			tool := []byte(toolResult.Raw)
			tool, _ = sjson.SetBytes(tool, "type", "function")
			name, ok := util.NormalizeRequestToolName(toolResult.Get("name").String(), nil)
			if !ok {
				continue
			}
			if short, ok := toolNameMap[name]; ok {
				name = short
			} else {
				name = shortenNameIfNeeded(name)
			}
			tool, _ = sjson.SetBytes(tool, "name", name)
			tool, _ = sjson.SetRawBytes(tool, "parameters", []byte(normalizeToolParameters(toolResult.Get("input_schema").Raw)))
			tool, _ = sjson.DeleteBytes(tool, "input_schema")
			tool, _ = sjson.DeleteBytes(tool, "parameters.$schema")
			tool, _ = sjson.DeleteBytes(tool, "cache_control")
			tool, _ = sjson.DeleteBytes(tool, "defer_loading")
			tool, _ = sjson.SetBytes(tool, "strict", false)
			toolItems = append(toolItems, json.RawMessage(tool))
		}
		tools, _ := json.Marshal(toolItems)
		template, _ = sjson.SetRawBytes(template, "tools", tools)
	}

	// Default to parallel tool calls unless tool_choice explicitly disables them.
	parallelToolCalls := true
	if disableParallelToolUse := rootResult.Get("tool_choice.disable_parallel_tool_use"); disableParallelToolUse.Exists() {
		parallelToolCalls = !disableParallelToolUse.Bool()
	}

	// Add additional configuration parameters for the Codex API.
	template, _ = sjson.SetBytes(template, "parallel_tool_calls", parallelToolCalls)

	// Convert thinking.budget_tokens to reasoning.effort.
	reasoningEffort := "medium"
	if thinkingConfig := rootResult.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch thinkingConfig.Get("type").String() {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				budget := int(budgetTokens.Int())
				if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
					reasoningEffort = effort
				}
			}
		case "adaptive", "auto":
			// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
			// Pass through directly; ApplyThinking handles clamping to target model's levels.
			effort := ""
			if v := rootResult.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
				effort = strings.ToLower(strings.TrimSpace(v.String()))
			}
			if effort != "" {
				reasoningEffort = effort
			} else {
				reasoningEffort = string(thinking.LevelXHigh)
			}
		case "disabled":
			if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
				reasoningEffort = effort
			}
		}
	}
	template, _ = sjson.SetBytes(template, "reasoning.effort", reasoningEffort)
	template, _ = sjson.SetBytes(template, "reasoning.summary", "auto")
	if serviceTier := normalizeCodexServiceTier(rootResult.Get("service_tier")); serviceTier != "" {
		template, _ = sjson.SetBytes(template, "service_tier", serviceTier)
	}
	template, _ = sjson.SetBytes(template, "stream", true)
	template, _ = sjson.SetBytes(template, "store", false)
	template, _ = sjson.SetBytes(template, "include", []string{"reasoning.encrypted_content"})

	return template
}

func normalizeCodexServiceTier(result gjson.Result) string {
	if !result.Exists() || result.Type != gjson.String {
		return ""
	}

	switch strings.ToLower(strings.TrimSpace(result.String())) {
	case "fast", "priority":
		return "priority"
	default:
		return ""
	}
}

// shortenCodexCallIDIfNeeded keeps Claude tool IDs within the OpenAI Responses
// API call_id limit while preserving a stable, low-collision mapping.
func shortenCodexCallIDIfNeeded(id string) string {
	const limit = 64
	if len(id) <= limit {
		return id
	}

	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLen := limit - len(suffix)
	if prefixLen <= 0 {
		return suffix[len(suffix)-limit:]
	}
	return id[:prefixLen] + suffix
}

func isClaudeWebSearchToolType(toolType string) bool {
	return toolType == "web_search_20250305" || toolType == "web_search_20260209"
}

func buildClaudeWebSearchToolNameSet(tools gjson.Result) map[string]struct{} {
	names := map[string]struct{}{}
	if !tools.IsArray() {
		return names
	}

	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := tool.Get("type").String()
		if !isClaudeWebSearchToolType(toolType) {
			return true
		}

		if name := tool.Get("name").String(); name != "" {
			names[name] = struct{}{}
		}
		return true
	})

	return names
}

func convertClaudeToolChoiceToCodex(toolChoice gjson.Result, toolNameMap map[string]string, webSearchToolNames map[string]struct{}) []byte {
	if !toolChoice.Exists() || toolChoice.Type == gjson.Null {
		return []byte(`"auto"`)
	}

	choiceType := toolChoice.Get("type").String()
	if choiceType == "" && toolChoice.Type == gjson.String {
		choiceType = toolChoice.String()
	}

	switch choiceType {
	case "auto", "":
		return []byte(`"auto"`)
	case "any":
		return []byte(`"required"`)
	case "none":
		return []byte(`"none"`)
	case "tool":
		name := toolChoice.Get("name").String()
		if _, ok := webSearchToolNames[name]; ok {
			return []byte(`{"type":"web_search"}`)
		}
		if short, ok := toolNameMap[name]; ok {
			name = short
		} else {
			name = shortenNameIfNeeded(name)
		}
		if name == "" {
			return []byte(`"auto"`)
		}

		choice := []byte(`{"type":"function","name":""}`)
		choice, _ = sjson.SetBytes(choice, "name", name)
		return choice
	default:
		return []byte(`"auto"`)
	}
}

func convertClaudeWebSearchToolToCodex(tool gjson.Result) []byte {
	out := []byte(`{"type":"web_search"}`)
	if allowedDomains := tool.Get("allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() {
		out, _ = sjson.SetRawBytes(out, "filters.allowed_domains", []byte(allowedDomains.Raw))
	}
	if userLocation := tool.Get("user_location"); userLocation.Exists() && userLocation.IsObject() {
		out, _ = sjson.SetRawBytes(out, "user_location", []byte(userLocation.Raw))
	}
	return out
}

// shortenNameIfNeeded applies a simple shortening rule for a single name.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > limit {
				return cand[:limit]
			}
			return cand
		}
	}
	return name[:limit]
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// buildReverseMapFromClaudeOriginalToShort builds original->short map, used to map tool_use names to short.
func buildReverseMapFromClaudeOriginalToShort(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	m := map[string]string{}
	if !tools.IsArray() {
		return m
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		if n, ok := util.NormalizeRequestToolName(arr[i].Get("name").String(), nil); ok {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m = buildShortNameMap(names)
	}
	return m
}

// normalizeToolParameters ensures object schemas contain at least an empty properties map.
func normalizeToolParameters(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	result := gjson.Parse(raw)
	schema := []byte(raw)
	schemaType := result.Get("type").String()
	if schemaType == "" {
		schema, _ = sjson.SetBytes(schema, "type", "object")
		schemaType = "object"
	}
	if schemaType == "object" && !result.Get("properties").Exists() {
		schema, _ = sjson.SetRawBytes(schema, "properties", []byte(`{}`))
	}
	return string(schema)
}
