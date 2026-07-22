// Package gemini provides request translation functionality for Antigravity to Gemini API compatibility.
// It handles parsing and transforming Antigravity API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Antigravity API format and Gemini API's expected format.
package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToAntigravity parses and transforms a Antigravity API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini API format
// 3. Converts system instructions to the expected format
// 4. Fixes CLI tool response format and grouping
//
// Parameters:
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertGeminiRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	hasWebSearchTool := false
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.Exists() && tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("googleSearch").Exists() {
				hasWebSearchTool = true
				break
			}
		}
	}
	templateStr := `{"project":"","request":{},"model":""}`
	templateBytes, _ := sjson.SetRawBytes([]byte(templateStr), "request", rawJSON)
	templateBytes, _ = sjson.SetBytes(templateBytes, "model", modelName)
	template := string(templateBytes)
	template, _ = sjson.Delete(template, "request.model")
	if hasWebSearchTool {
		template, _ = sjson.Set(template, "requestType", "web_search")
	}

	template, errFixCLIToolResponse := fixCLIToolResponse(template)
	if errFixCLIToolResponse != nil {
		return []byte{}
	}

	systemInstructionResult := gjson.Get(template, "request.system_instruction")
	if systemInstructionResult.Exists() {
		templateBytes, _ = sjson.SetRawBytes([]byte(template), "request.systemInstruction", []byte(systemInstructionResult.Raw))
		template = string(templateBytes)
		template, _ = sjson.Delete(template, "request.system_instruction")
	}
	rawJSON = []byte(template)

	// Normalize roles in request.contents: default to valid values if missing/invalid
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if contents.IsArray() {
		prevRole := ""
		normalizedContents := make([]json.RawMessage, 0, len(contents.Array()))
		for _, value := range contents.Array() {
			role := value.Get("role").String()
			valid := role == "user" || role == "model"
			content := []byte(value.Raw)
			if role == "" || !valid {
				if prevRole == "" {
					role = "user"
				} else if prevRole == "user" {
					role = "model"
				} else {
					role = "user"
				}
				content, _ = sjson.SetBytes(content, "role", role)
			}
			prevRole = role
			normalizedContents = append(normalizedContents, content)
		}
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "request.contents", internalpayload.BuildRaw(normalizedContents))
	}

	toolsResult := gjson.GetBytes(rawJSON, "request.tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		rewrittenTools := make([]json.RawMessage, 0, len(toolsResult.Array()))
		for _, toolResult := range toolsResult.Array() {
			tool := []byte(toolResult.Raw)
			functionDeclarationsResult := toolResult.Get("function_declarations")
			if functionDeclarationsResult.Exists() && functionDeclarationsResult.IsArray() {
				declarations := make([]json.RawMessage, 0, len(functionDeclarationsResult.Array()))
				for _, declarationResult := range functionDeclarationsResult.Array() {
					declaration := declarationResult.Raw
					if declarationResult.Get("parameters").Exists() {
						declaration, _ = util.RenameKey(declaration, "parameters", "parametersJsonSchema")
					}
					declarations = append(declarations, json.RawMessage(declaration))
				}
				tool, _ = sjson.SetRawBytes(tool, "function_declarations", internalpayload.BuildRaw(declarations))
			}
			rewrittenTools = append(rewrittenTools, tool)
		}
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "request.tools", internalpayload.BuildRaw(rewrittenTools))
	}

	if strings.Contains(strings.ToLower(modelName), "claude") {
		rawJSON = sanitizeAntigravityClaudeGeminiRequestSignatures(modelName, rawJSON)
	} else {
		rawJSON = signature.SanitizeGeminiRequestThoughtSignatures(rawJSON, "request.contents")
	}

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}

func sanitizeAntigravityClaudeGeminiRequestSignatures(modelName string, rawJSON []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(rawJSON, &root); err != nil {
		log.WithError(err).Debug("antigravity gemini translator: failed to parse request for Claude signature sanitize")
		return rawJSON
	}

	request, ok := root["request"].(map[string]any)
	if !ok {
		return rawJSON
	}
	contents, ok := request["contents"].([]any)
	if !ok {
		return rawJSON
	}

	changed := false
	rewrittenContents := make([]any, 0, len(contents))
	for contentIndex, contentValue := range contents {
		content, ok := contentValue.(map[string]any)
		if !ok {
			rewrittenContents = append(rewrittenContents, contentValue)
			continue
		}

		parts, ok := content["parts"].([]any)
		if !ok {
			rewrittenContents = append(rewrittenContents, content)
			continue
		}

		isModelTurn := content["role"] == "model"
		rewrittenParts := make([]any, 0, len(parts))
		for partIndex, partValue := range parts {
			part, ok := partValue.(map[string]any)
			if !ok {
				rewrittenParts = append(rewrittenParts, partValue)
				continue
			}

			rawSignature, hasSignature := antigravityClaudeGeminiPartThoughtSignature(part)
			if hasFunctionResponsePart(part) {
				if hasSignature {
					changed = true
					deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "functionResponse parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
				}
				rewrittenParts = append(rewrittenParts, part)
				continue
			}
			if !isModelTurn {
				if hasSignature {
					changed = true
					deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-model parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
				}
				rewrittenParts = append(rewrittenParts, part)
				continue
			}

			if part["thought"] == true {
				normalized, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
				if !compatible {
					changed = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "missing_or_incompatible_signature", contentIndex, partIndex, rawSignature)
					continue
				}
				if text, _ := part["text"].(string); strings.TrimSpace(text) == "" {
					changed = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "empty_thinking_text", contentIndex, partIndex, rawSignature)
					continue
				}
				if normalized != rawSignature {
					changed = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "normalize_signature", "compatible_claude_signature", contentIndex, partIndex, rawSignature)
				}
				deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				part["thoughtSignature"] = normalized
				rewrittenParts = append(rewrittenParts, part)
				continue
			}

			if hasSignature {
				changed = true
				deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-thinking parts should not carry Claude thinking signatures", contentIndex, partIndex, rawSignature)
			}
			rewrittenParts = append(rewrittenParts, part)
		}

		if len(rewrittenParts) == 0 {
			changed = true
			continue
		}
		content["parts"] = rewrittenParts
		rewrittenContents = append(rewrittenContents, content)
	}

	if !changed {
		return rawJSON
	}
	request["contents"] = rewrittenContents
	out, err := json.Marshal(root)
	if err != nil {
		log.WithError(err).Debug("antigravity gemini translator: failed to marshal Claude signature sanitize")
		return rawJSON
	}
	return out
}

func antigravityClaudeGeminiPartThoughtSignature(part map[string]any) (string, bool) {
	for _, path := range [][]string{
		{"thoughtSignature"},
		{"thought_signature"},
		{"functionCall", "thoughtSignature"},
		{"functionCall", "thought_signature"},
		{"functionResponse", "thoughtSignature"},
		{"functionResponse", "thought_signature"},
		{"extra_content", "google", "thought_signature"},
	} {
		if value, ok := stringAtPath(part, path...); ok {
			return value, true
		}
	}
	return "", false
}

func deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part map[string]any) {
	for _, path := range [][]string{
		{"thoughtSignature"},
		{"thought_signature"},
		{"functionCall", "thoughtSignature"},
		{"functionCall", "thought_signature"},
		{"functionResponse", "thoughtSignature"},
		{"functionResponse", "thought_signature"},
		{"extra_content", "google", "thought_signature"},
	} {
		deleteAtPath(part, path...)
	}
}

func hasFunctionResponsePart(part map[string]any) bool {
	_, ok := part["functionResponse"]
	if ok {
		return true
	}
	_, ok = part["function_response"]
	return ok
}

func stringAtPath(value map[string]any, path ...string) (string, bool) {
	var current any = value
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = m[key]
		if !ok {
			return "", false
		}
	}
	s, ok := current.(string)
	return s, ok
}

func deleteAtPath(value map[string]any, path ...string) {
	if len(path) == 0 {
		return
	}
	current := value
	for _, key := range path[:len(path)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			return
		}
		current = next
	}
	delete(current, path[len(path)-1])
}

func logAntigravityClaudeGeminiSignatureSanitize(modelName, action, reason string, contentIndex, partIndex int, rawSignature string) {
	fields := log.Fields{
		"component":         "signature_sanitizer",
		"translator":        "antigravity_gemini",
		"target_provider":   string(signature.SignatureProviderClaude),
		"action":            action,
		"reason":            reason,
		"model":             modelName,
		"content_index":     contentIndex,
		"part_index":        partIndex,
		"has_signature":     strings.TrimSpace(rawSignature) != "",
		"signature_length":  len(strings.TrimSpace(rawSignature)),
		"detected_provider": string(signature.DetectSignatureProviderForBlock(rawSignature, signature.SignatureBlockKindClaudeThinking)),
	}
	log.WithFields(fields).Debug("antigravity gemini translator: sanitized Claude target thoughtSignature before upstream")
}

// FunctionCallGroup represents a group of function calls and their responses
type FunctionCallGroup struct {
	ResponsesNeeded int
	CallNames       []string // ordered function call names for backfilling empty response names
}

// parseFunctionResponseRaw attempts to normalize a function response part into a JSON object string.
// Falls back to a minimal "functionResponse" object when parsing fails.
// fallbackName is used when the response's own name is empty.
func parseFunctionResponseRaw(response gjson.Result, fallbackName string) string {
	if response.IsObject() && gjson.Valid(response.Raw) {
		raw := response.Raw
		name := response.Get("functionResponse.name").String()
		if strings.TrimSpace(name) == "" && fallbackName != "" {
			updated, _ := sjson.SetBytes([]byte(raw), "functionResponse.name", fallbackName)
			raw = string(updated)
		}
		return raw
	}

	log.Debugf("parse function response failed, using fallback")
	funcResp := response.Get("functionResponse")
	if funcResp.Exists() {
		fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
		name := funcResp.Get("name").String()
		if strings.TrimSpace(name) == "" {
			name = fallbackName
		}
		fr, _ = sjson.SetBytes(fr, "functionResponse.name", name)
		fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", funcResp.Get("response").String())
		if id := funcResp.Get("id").String(); id != "" {
			fr, _ = sjson.SetBytes(fr, "functionResponse.id", id)
		}
		return string(fr)
	}

	useName := fallbackName
	if useName == "" {
		useName = "unknown"
	}
	fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
	fr, _ = sjson.SetBytes(fr, "functionResponse.name", useName)
	fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", response.String())
	return string(fr)
}

// fixCLIToolResponse performs sophisticated tool response format conversion and grouping.
// This function transforms the CLI tool response format by intelligently grouping function calls
// with their corresponding responses, ensuring proper conversation flow and API compatibility.
// It converts from a linear format (1.json) to a grouped format (2.json) where function calls
// and their responses are properly associated and structured.
//
// Parameters:
//   - input: The input JSON string to be processed
//
// Returns:
//   - string: The processed JSON string with grouped function calls and responses
//   - error: An error if the processing fails
func fixCLIToolResponse(input string) (string, error) {
	// Parse the input JSON to extract the conversation structure
	parsed := gjson.Parse(input)

	// Extract the contents array which contains the conversation messages
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		// log.Debugf(input)
		return input, fmt.Errorf("contents not found in input")
	}

	var pendingGroups []*FunctionCallGroup
	var collectedResponses []gjson.Result
	outputContents := make([]json.RawMessage, 0, len(contents.Array()))
	for _, value := range contents.Array() {
		role := value.Get("role").String()
		parts := value.Get("parts")

		var responsePartsInThisContent []gjson.Result
		parts.ForEach(func(_, part gjson.Result) bool {
			if part.Get("functionResponse").Exists() {
				responsePartsInThisContent = append(responsePartsInThisContent, part)
			}
			return true
		})

		if len(responsePartsInThisContent) > 0 {
			collectedResponses = append(collectedResponses, responsePartsInThisContent...)
			for len(pendingGroups) > 0 && len(collectedResponses) >= pendingGroups[0].ResponsesNeeded {
				group := pendingGroups[0]
				pendingGroups = pendingGroups[1:]
				groupResponses := collectedResponses[:group.ResponsesNeeded]
				collectedResponses = collectedResponses[group.ResponsesNeeded:]
				if functionResponseContent := buildFunctionResponseContent(groupResponses, group.CallNames); functionResponseContent != nil {
					outputContents = append(outputContents, functionResponseContent)
				}
			}
			continue
		}

		if role == "model" {
			var callNames []string
			parts.ForEach(func(_, part gjson.Result) bool {
				if part.Get("functionCall").Exists() {
					callNames = append(callNames, part.Get("functionCall.name").String())
				}
				return true
			})

			if len(callNames) > 0 {
				if !value.IsObject() {
					log.Warnf("failed to parse model content")
					continue
				}
				outputContents = append(outputContents, json.RawMessage(value.Raw))
				pendingGroups = append(pendingGroups, &FunctionCallGroup{
					ResponsesNeeded: len(callNames),
					CallNames:       callNames,
				})
			} else {
				if !value.IsObject() {
					log.Warnf("failed to parse content")
					continue
				}
				outputContents = append(outputContents, json.RawMessage(value.Raw))
			}
		} else {
			if !value.IsObject() {
				log.Warnf("failed to parse content")
				continue
			}
			outputContents = append(outputContents, json.RawMessage(value.Raw))
		}
	}

	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]
			if functionResponseContent := buildFunctionResponseContent(groupResponses, group.CallNames); functionResponseContent != nil {
				outputContents = append(outputContents, functionResponseContent)
			}
		}
	}

	result, _ := sjson.SetRawBytes([]byte(input), "request.contents", internalpayload.BuildRaw(outputContents))

	return string(result), nil
}

func buildFunctionResponseContent(responses []gjson.Result, callNames []string) json.RawMessage {
	parts := make([]json.RawMessage, 0, len(responses))
	for index, response := range responses {
		partRaw := parseFunctionResponseRaw(response, callNames[index])
		if partRaw != "" {
			parts = append(parts, json.RawMessage(partRaw))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	content := []byte(`{"parts":[],"role":"function"}`)
	content, _ = sjson.SetRawBytes(content, "parts", internalpayload.BuildRaw(parts))
	return content
}
