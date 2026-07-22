package responses

import (
	"encoding/json"
	"strings"

	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiResponsesThoughtSignature = "skip_thought_signature_validator"

func ConvertOpenAIResponsesRequestToGemini(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := util.NormalizeOpenAIResponsesRequestJSON(inputRawJSON)

	// Note: modelName and stream parameters are part of the fixed method signature
	_ = modelName // Unused but required by interface
	_ = stream    // Unused but required by interface

	// Base Gemini API template (do not include thinkingConfig by default)
	out := []byte(`{"contents":[]}`)

	root := gjson.ParseBytes(rawJSON)
	systemParts := make([][]byte, 0)
	contentNodes := make([][]byte, 0)

	// Extract system instruction from OpenAI "instructions" field
	if instructions := root.Get("instructions"); instructions.Exists() {
		part := []byte(`{"text":""}`)
		part, _ = sjson.SetBytes(part, "text", instructions.String())
		systemParts = append(systemParts, part)
	}

	// Convert input messages to Gemini contents format
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		items := input.Array()

		// Normalize consecutive function calls and outputs so each call is immediately followed by its response
		normalized := make([]gjson.Result, 0, len(items))
		functionNames := make(map[string]string)
		for i := 0; i < len(items); {
			item := items[i]
			itemType := item.Get("type").String()
			itemRole := item.Get("role").String()
			if itemType == "" && itemRole != "" {
				itemType = "message"
			}

			if itemType == "function_call" {
				var calls []gjson.Result
				var outputs []gjson.Result

				for i < len(items) {
					next := items[i]
					nextType := next.Get("type").String()
					nextRole := next.Get("role").String()
					if nextType == "" && nextRole != "" {
						nextType = "message"
					}
					if nextType != "function_call" {
						break
					}
					calls = append(calls, next)
					i++
				}

				for i < len(items) {
					next := items[i]
					nextType := next.Get("type").String()
					nextRole := next.Get("role").String()
					if nextType == "" && nextRole != "" {
						nextType = "message"
					}
					if nextType != "function_call_output" {
						break
					}
					outputs = append(outputs, next)
					i++
				}

				if len(calls) > 0 {
					outputMap := make(map[string]gjson.Result, len(outputs))
					for _, outItem := range outputs {
						outputMap[outItem.Get("call_id").String()] = outItem
					}
					for _, call := range calls {
						normalized = append(normalized, call)
						callID := call.Get("call_id").String()
						if _, exists := functionNames[callID]; !exists {
							functionNames[callID] = call.Get("name").String()
						}
						if resp, ok := outputMap[callID]; ok {
							normalized = append(normalized, resp)
							delete(outputMap, callID)
						}
					}
					for _, outItem := range outputs {
						if _, ok := outputMap[outItem.Get("call_id").String()]; ok {
							normalized = append(normalized, outItem)
						}
					}
					continue
				}
			}

			if itemType == "function_call_output" {
				normalized = append(normalized, item)
				i++
				continue
			}

			normalized = append(normalized, item)
			i++
		}

		for _, item := range normalized {
			itemType := item.Get("type").String()
			itemRole := item.Get("role").String()
			if itemType == "" && itemRole != "" {
				itemType = "message"
			}

			switch itemType {
			case "message":
				if strings.EqualFold(itemRole, "system") || strings.EqualFold(itemRole, "developer") {
					if contentArray := item.Get("content"); contentArray.Exists() {
						if contentArray.IsArray() {
							contentArray.ForEach(func(_, contentItem gjson.Result) bool {
								part := []byte(`{"text":""}`)
								text := contentItem.Get("text").String()
								part, _ = sjson.SetBytes(part, "text", text)
								systemParts = append(systemParts, part)
								return true
							})
						} else if contentArray.Type == gjson.String {
							part := []byte(`{"text":""}`)
							part, _ = sjson.SetBytes(part, "text", contentArray.String())
							systemParts = append(systemParts, part)
						}
					}
					continue
				}

				// Handle regular messages
				// Note: In Responses format, model outputs may appear as content items with type "output_text"
				// even when the message.role is "user". We split such items into distinct Gemini messages
				// with roles derived from the content type to match docs/convert-2.md.
				if contentArray := item.Get("content"); contentArray.Exists() && contentArray.IsArray() {
					currentRole := ""
					currentParts := make([][]byte, 0)

					flush := func() {
						if currentRole == "" || len(currentParts) == 0 {
							currentParts = currentParts[:0]
							return
						}
						one := []byte(`{"role":"","parts":[]}`)
						one, _ = sjson.SetBytes(one, "role", currentRole)
						one, _ = sjson.SetRawBytes(one, "parts", common.RawJSONArray(currentParts))
						contentNodes = append(contentNodes, one)
						currentParts = currentParts[:0]
					}

					contentArray.ForEach(func(_, contentItem gjson.Result) bool {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}

						effRole := "user"
						if itemRole != "" {
							switch strings.ToLower(itemRole) {
							case "assistant", "model":
								effRole = "model"
							default:
								effRole = strings.ToLower(itemRole)
							}
						}
						if contentType == "output_text" {
							effRole = "model"
						}
						if effRole == "assistant" {
							effRole = "model"
						}

						if currentRole != "" && effRole != currentRole {
							flush()
							currentRole = ""
						}
						if currentRole == "" {
							currentRole = effRole
						}

						var partJSON []byte
						switch contentType {
						case "input_text", "output_text":
							if text := contentItem.Get("text"); text.Exists() {
								partJSON = []byte(`{"text":""}`)
								partJSON, _ = sjson.SetBytes(partJSON, "text", text.String())
							}
						case "input_image":
							if mimeType, data, ok := util.ParseDataURL(util.OpenAIImageURLFromPart(contentItem)); ok {
								partJSON = []byte(`{"inline_data":{"mime_type":"","data":""}}`)
								partJSON, _ = sjson.SetBytes(partJSON, "inline_data.mime_type", mimeType)
								partJSON, _ = sjson.SetBytes(partJSON, "inline_data.data", data)
							}
						case "input_audio":
							audioData := contentItem.Get("data").String()
							audioFormat := contentItem.Get("format").String()
							if audioData != "" {
								audioMimeMap := map[string]string{
									"mp3":       "audio/mpeg",
									"wav":       "audio/wav",
									"ogg":       "audio/ogg",
									"flac":      "audio/flac",
									"aac":       "audio/aac",
									"webm":      "audio/webm",
									"pcm16":     "audio/pcm",
									"g711_ulaw": "audio/basic",
									"g711_alaw": "audio/basic",
								}
								mimeType := "audio/wav"
								if audioFormat != "" {
									if mapped, ok := audioMimeMap[audioFormat]; ok {
										mimeType = mapped
									} else {
										mimeType = "audio/" + audioFormat
									}
								}
								partJSON = []byte(`{"inline_data":{"mime_type":"","data":""}}`)
								partJSON, _ = sjson.SetBytes(partJSON, "inline_data.mime_type", mimeType)
								partJSON, _ = sjson.SetBytes(partJSON, "inline_data.data", audioData)
							}
						}

						if len(partJSON) > 0 {
							currentParts = append(currentParts, partJSON)
						}
						return true
					})

					flush()
				} else if contentArray.Type == gjson.String {
					effRole := "user"
					if itemRole != "" {
						switch strings.ToLower(itemRole) {
						case "assistant", "model":
							effRole = "model"
						default:
							effRole = strings.ToLower(itemRole)
						}
					}

					one := []byte(`{"role":"","parts":[{"text":""}]}`)
					one, _ = sjson.SetBytes(one, "role", effRole)
					one, _ = sjson.SetBytes(one, "parts.0.text", contentArray.String())
					contentNodes = append(contentNodes, one)
				}

			case "function_call":
				// Handle function calls - convert to model message with functionCall
				name := util.SanitizeFunctionName(item.Get("name").String())
				arguments := item.Get("arguments").String()

				modelContent := []byte(`{"role":"model","parts":[]}`)
				functionCall := []byte(`{"functionCall":{"name":"","args":{}}}`)
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.name", name)
				functionCall, _ = sjson.SetBytes(functionCall, "thoughtSignature", geminiResponsesThoughtSignature)
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.id", item.Get("call_id").String())

				// Parse arguments JSON string and set as args object
				if arguments != "" {
					argsResult := gjson.Parse(arguments)
					functionCall, _ = sjson.SetRawBytes(functionCall, "functionCall.args", []byte(argsResult.Raw))
				}

				modelContent, _ = sjson.SetRawBytes(modelContent, "parts", common.RawJSONArray([][]byte{functionCall}))
				contentNodes = append(contentNodes, modelContent)

			case "function_call_output":
				// Handle function call outputs - convert to function message with functionResponse
				callID := item.Get("call_id").String()
				// Use .Raw to preserve the JSON encoding (includes quotes for strings)
				outputRaw := item.Get("output").Str

				functionContent := []byte(`{"role":"function","parts":[]}`)
				functionResponse := []byte(`{"functionResponse":{"name":"","response":{}}}`)

				// We need to extract the function name from the previous function_call
				// For now, we'll use a placeholder or extract from context if available
				functionName := functionNames[callID]
				if functionName == "" {
					functionName = "unknown"
				}
				functionName = util.SanitizeFunctionName(functionName)

				functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.name", functionName)
				functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.id", callID)

				// Set the raw JSON output directly (preserves string encoding)
				if outputRaw != "" && outputRaw != "null" {
					output := gjson.Parse(outputRaw)
					if output.Type == gjson.JSON && json.Valid([]byte(output.Raw)) {
						functionResponse, _ = sjson.SetRawBytes(functionResponse, "functionResponse.response.result", []byte(output.Raw))
					} else {
						functionResponse, _ = sjson.SetBytes(functionResponse, "functionResponse.response.result", outputRaw)
					}
				}
				functionContent, _ = sjson.SetRawBytes(functionContent, "parts", common.RawJSONArray([][]byte{functionResponse}))
				contentNodes = append(contentNodes, functionContent)

			case "reasoning":
				thoughtContent := []byte(`{"role":"model","parts":[]}`)
				thought := []byte(`{"text":"","thoughtSignature":"","thought":true}`)
				thought, _ = sjson.SetBytes(thought, "text", item.Get("summary.0.text").String())
				thought, _ = sjson.SetBytes(thought, "thoughtSignature", openAIResponsesGeminiThoughtSignature(item.Get("encrypted_content").String()))

				thoughtContent, _ = sjson.SetRawBytes(thoughtContent, "parts", common.RawJSONArray([][]byte{thought}))
				contentNodes = append(contentNodes, thoughtContent)
			}
		}
	} else if input.Exists() && input.Type == gjson.String {
		// Simple string input conversion to user message
		userContent := []byte(`{"role":"user","parts":[{"text":""}]}`)
		userContent, _ = sjson.SetBytes(userContent, "parts.0.text", input.String())
		contentNodes = append(contentNodes, userContent)
	}

	// Gemini/Vertex accepts assistant/model turns in history, but some model
	// surfaces reject requests whose final turn is model-authored prefill.
	if len(contentNodes) > 0 {
		last := gjson.ParseBytes(contentNodes[len(contentNodes)-1])
		if last.Get("role").String() == "model" && !openAIResponsesGeminiModelTurnCarriesThought(last) {
			contentNodes = contentNodes[:len(contentNodes)-1]
		}
	}
	if len(systemParts) > 0 {
		systemInstruction := []byte(`{"parts":[]}`)
		systemInstruction, _ = sjson.SetRawBytes(systemInstruction, "parts", common.RawJSONArray(systemParts))
		out, _ = sjson.SetRawBytes(out, "systemInstruction", systemInstruction)
	}
	out, _ = sjson.SetRawBytes(out, "contents", common.RawJSONArray(contentNodes))

	// Convert tools to Gemini functionDeclarations format
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		functionDeclarations := make([][]byte, 0, int(tools.Get("#").Int()))

		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "function" {
				name, ok := util.NormalizeRequestToolName(tool.Get("name").String(), nil)
				if !ok {
					return true
				}
				funcDecl := []byte(`{"name":"","description":"","parametersJsonSchema":{}}`)
				funcDecl, _ = sjson.SetBytes(funcDecl, "name", util.SanitizeFunctionName(name))
				if desc := tool.Get("description"); desc.Exists() {
					funcDecl, _ = sjson.SetBytes(funcDecl, "description", desc.String())
				}
				if params := tool.Get("parameters"); params.Exists() {
					funcDecl, _ = sjson.SetRawBytes(funcDecl, "parametersJsonSchema", []byte(util.CleanJSONSchemaForGemini(params.Raw)))
				}

				functionDeclarations = append(functionDeclarations, funcDecl)
			}
			return true
		})

		// Only add tools if there are function declarations
		if len(functionDeclarations) > 0 {
			functionTool := []byte(`{"functionDeclarations":[]}`)
			functionTool, _ = sjson.SetRawBytes(functionTool, "functionDeclarations", common.RawJSONArray(functionDeclarations))
			out, _ = sjson.SetRawBytes(out, "tools", common.RawJSONArray([][]byte{functionTool}))
		}
	}

	// Handle generation config from OpenAI format
	if maxOutputTokens := root.Get("max_output_tokens"); maxOutputTokens.Exists() {
		genConfig := []byte(`{"maxOutputTokens":0}`)
		genConfig, _ = sjson.SetBytes(genConfig, "maxOutputTokens", maxOutputTokens.Int())
		out, _ = sjson.SetRawBytes(out, "generationConfig", genConfig)
	}

	// Handle temperature if present
	if temperature := root.Get("temperature"); temperature.Exists() {
		if !gjson.GetBytes(out, "generationConfig").Exists() {
			out, _ = sjson.SetRawBytes(out, "generationConfig", []byte(`{}`))
		}
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", temperature.Float())
	}

	// Handle top_p if present
	if topP := root.Get("top_p"); topP.Exists() {
		if !gjson.GetBytes(out, "generationConfig").Exists() {
			out, _ = sjson.SetRawBytes(out, "generationConfig", []byte(`{}`))
		}
		out, _ = sjson.SetBytes(out, "generationConfig.topP", topP.Float())
	}

	// Handle stop sequences
	if stopSequences := root.Get("stop_sequences"); stopSequences.Exists() && stopSequences.IsArray() {
		if !gjson.GetBytes(out, "generationConfig").Exists() {
			out, _ = sjson.SetRawBytes(out, "generationConfig", []byte(`{}`))
		}
		var sequences []string
		stopSequences.ForEach(func(_, seq gjson.Result) bool {
			sequences = append(sequences, seq.String())
			return true
		})
		out, _ = sjson.SetBytes(out, "generationConfig.stopSequences", sequences)
	}

	out = applyOpenAIResponsesTextFormatToGemini(out, root)

	// Apply thinking configuration: convert OpenAI Responses API reasoning.effort to Gemini thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := root.Get("reasoning.effort")
	if re.Exists() {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if effort != "" {
			thinkingPath := "generationConfig.thinkingConfig"
			if effort == "auto" {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingBudget", -1)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", true)
			} else {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingLevel", effort)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", effort != "none")
			}
		}
	}

	result := out
	result = common.AttachDefaultSafetySettings(result, "safetySettings")
	return result
}

func openAIResponsesGeminiThoughtSignature(rawSignature string) string {
	return sigcompat.GeminiReplaySignatureOrBypass(rawSignature, sigcompat.SignatureBlockKindGeminiModelPart)
}

func openAIResponsesGeminiModelTurnCarriesThought(content gjson.Result) bool {
	parts := content.Get("parts")
	if !parts.IsArray() {
		return false
	}
	for _, part := range parts.Array() {
		if part.Get("thought").Bool() || strings.TrimSpace(part.Get("thoughtSignature").String()) != "" {
			return true
		}
	}
	return false
}

func applyOpenAIResponsesTextFormatToGemini(out []byte, root gjson.Result) []byte {
	textFormat := root.Get("text.format")
	if !textFormat.Exists() {
		return out
	}

	formatType := strings.ToLower(strings.TrimSpace(textFormat.Get("type").String()))
	switch formatType {
	case "json_object":
		out = ensureGeminiGenerationConfig(out)
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")
	case "json_schema":
		out = ensureGeminiGenerationConfig(out)
		out, _ = sjson.SetBytes(out, "generationConfig.responseMimeType", "application/json")
		out, _ = sjson.DeleteBytes(out, "generationConfig.responseSchema")

		schema := textFormat.Get("schema")
		if !schema.Exists() {
			schema = textFormat.Get("json_schema.schema")
		}
		if schema.Exists() {
			out, _ = sjson.SetRawBytes(out, "generationConfig.responseJsonSchema", []byte(schema.Raw))
		}
	}

	return out
}

func ensureGeminiGenerationConfig(out []byte) []byte {
	if !gjson.GetBytes(out, "generationConfig").Exists() {
		out, _ = sjson.SetRawBytes(out, "generationConfig", []byte(`{}`))
	}
	return out
}
