// Package openai provides request translation functionality for OpenAI to Gemini API compatibility.
// It converts OpenAI Chat Completions requests into Gemini compatible JSON using gjson/sjson only.
package chat_completions

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const geminiFunctionThoughtSignature = "skip_thought_signature_validator"

// ConvertOpenAIRequestToGemini converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertOpenAIRequestToGemini(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := util.NormalizeOpenAIChatRequestJSON(inputRawJSON)
	// Base envelope (no default thinkingConfig)
	out := []byte(`{"contents":[]}`)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Let user-provided generationConfig pass through
	if genConfig := gjson.GetBytes(rawJSON, "generationConfig"); genConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "generationConfig", []byte(genConfig.Raw))
	}

	// Apply thinking configuration: convert OpenAI reasoning_effort to Gemini thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
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

	// Temperature/top_p/top_k
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topK", tkr.Num)
	}

	// OpenAI max_tokens / max_completion_tokens -> Gemini generationConfig.maxOutputTokens
	if mt := gjson.GetBytes(rawJSON, "max_tokens"); mt.Exists() && mt.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.maxOutputTokens", mt.Num)
	} else if mct := gjson.GetBytes(rawJSON, "max_completion_tokens"); mct.Exists() && mct.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.maxOutputTokens", mct.Num)
	}

	// Candidate count (OpenAI 'n' parameter)
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == gjson.Number {
		if val := n.Int(); val > 1 {
			out, _ = sjson.SetBytes(out, "generationConfig.candidateCount", val)
		}
	}

	// Map OpenAI modalities -> Gemini generationConfig.responseModalities
	// e.g. "modalities": ["image", "text"] -> ["IMAGE", "TEXT"]
	if mods := gjson.GetBytes(rawJSON, "modalities"); mods.Exists() && mods.IsArray() {
		var responseMods []string
		for _, m := range mods.Array() {
			switch strings.ToLower(m.String()) {
			case "text":
				responseMods = append(responseMods, "TEXT")
			case "image":
				responseMods = append(responseMods, "IMAGE")
			}
		}
		if len(responseMods) > 0 {
			out, _ = sjson.SetBytes(out, "generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into generationConfig.imageConfig.aspectRatio.
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		// First pass: assistant tool_calls id->name map
		tcID2Name := map[string]string{}
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			if m.Get("role").String() == "assistant" {
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() == "function" {
							id := tc.Get("id").String()
							name := tc.Get("function.name").String()
							if id != "" && name != "" {
								tcID2Name[id] = name
							}
						}
					}
				}
			}
		}

		// Second pass build systemInstruction/tool responses cache
		toolResponses := map[string]string{} // tool_call_id -> response text
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()
			if role == "tool" {
				toolCallID := m.Get("tool_call_id").String()
				if toolCallID != "" {
					c := m.Get("content")
					toolResponses[toolCallID] = c.Raw
				}
			}
		}

		systemParts := make([][]byte, 0)
		contentNodes := make([][]byte, 0, len(arr))
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")

			if (role == "system" || role == "developer") && len(arr) > 1 {
				// system -> systemInstruction as a user message style
				if content.Type == gjson.String {
					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", content.String())
					systemParts = append(systemParts, part)
				} else if content.IsObject() && content.Get("type").String() == "text" {
					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", content.Get("text").String())
					systemParts = append(systemParts, part)
				} else if content.IsArray() {
					contents := content.Array()
					for j := 0; j < len(contents); j++ {
						part := []byte(`{"text":""}`)
						part, _ = sjson.SetBytes(part, "text", contents[j].Get("text").String())
						systemParts = append(systemParts, part)
					}
				}
			} else if role == "user" || ((role == "system" || role == "developer") && len(arr) == 1) {
				// Build single user content node to avoid splitting into multiple contents
				node := []byte(`{"role":"user","parts":[]}`)
				parts := make([][]byte, 0)
				if content.Type == gjson.String {
					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", content.String())
					parts = append(parts, part)
				} else if content.IsArray() {
					items := content.Array()
					for _, item := range items {
						switch item.Get("type").String() {
						case "text":
							text := item.Get("text").String()
							if text != "" {
								part := []byte(`{"text":""}`)
								part, _ = sjson.SetBytes(part, "text", text)
								parts = append(parts, part)
							}
						case "image_url":
							imageURL := util.OpenAIImageURLFromPart(item)
							if mime, data, ok := util.ParseDataURL(imageURL); ok {
								part := []byte(`{"inlineData":{"mime_type":"","data":""},"thoughtSignature":""}`)
								part, _ = sjson.SetBytes(part, "inlineData.mime_type", mime)
								part, _ = sjson.SetBytes(part, "inlineData.data", data)
								part, _ = sjson.SetBytes(part, "thoughtSignature", geminiFunctionThoughtSignature)
								parts = append(parts, part)
							}
						case "video_url":
							videoURL := item.Get("video_url.url").String()
							if len(videoURL) > 5 {
								pieces := strings.SplitN(videoURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									part := []byte(`{"inlineData":{"mime_type":"","data":""}}`)
									part, _ = sjson.SetBytes(part, "inlineData.mime_type", mime)
									part, _ = sjson.SetBytes(part, "inlineData.data", data)
									parts = append(parts, part)
								}
							}
						case "file":
							filename := item.Get("file.filename").String()
							fileData := item.Get("file.file_data").String()
							ext := ""
							if sp := strings.Split(filename, "."); len(sp) > 1 {
								ext = sp[len(sp)-1]
							}
							if mimeType, ok := misc.MimeTypes[ext]; ok {
								part := []byte(`{"inlineData":{"mime_type":"","data":""}}`)
								part, _ = sjson.SetBytes(part, "inlineData.mime_type", mimeType)
								part, _ = sjson.SetBytes(part, "inlineData.data", fileData)
								parts = append(parts, part)
							} else {
								log.Warnf("Unknown file name extension '%s' in user message, skip", ext)
							}
						case "input_audio":
							audioData := item.Get("input_audio.data").String()
							if audioData != "" {
								mimeType := openAIInputAudioMimeType(item.Get("input_audio.format").String())
								part := []byte(`{"inlineData":{"mime_type":"","data":""}}`)
								part, _ = sjson.SetBytes(part, "inlineData.mime_type", mimeType)
								part, _ = sjson.SetBytes(part, "inlineData.data", audioData)
								parts = append(parts, part)
							}
						}
					}
				}
				node, _ = sjson.SetRawBytes(node, "parts", common.RawJSONArray(parts))
				contentNodes = append(contentNodes, node)
			} else if role == "assistant" {
				node := []byte(`{"role":"model","parts":[]}`)
				parts := make([][]byte, 0)
				if reasoningContent := m.Get("reasoning_content"); reasoningContent.Type == gjson.String && reasoningContent.String() != "" {
					part := []byte(`{"text":"","thought":true,"thoughtSignature":""}`)
					part, _ = sjson.SetBytes(part, "text", reasoningContent.String())
					part, _ = sjson.SetBytes(part, "thoughtSignature", geminiFunctionThoughtSignature)
					parts = append(parts, part)
				}
				if content.Type == gjson.String && content.String() != "" {
					// Assistant text -> single model content
					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", content.String())
					parts = append(parts, part)
				} else if content.IsArray() {
					// Assistant multimodal content (e.g. text + image) -> single model content with parts
					for _, item := range content.Array() {
						switch item.Get("type").String() {
						case "text":
							text := item.Get("text").String()
							if text != "" {
								part := []byte(`{"text":""}`)
								part, _ = sjson.SetBytes(part, "text", text)
								parts = append(parts, part)
							}
						case "image_url":
							// If the assistant returned an inline data URL, preserve it for history fidelity.
							imageURL := util.OpenAIImageURLFromPart(item)
							if mime, data, ok := util.ParseDataURL(imageURL); ok {
								part := []byte(`{"inlineData":{"mime_type":"","data":""},"thoughtSignature":""}`)
								part, _ = sjson.SetBytes(part, "inlineData.mime_type", mime)
								part, _ = sjson.SetBytes(part, "inlineData.data", data)
								part, _ = sjson.SetBytes(part, "thoughtSignature", geminiFunctionThoughtSignature)
								parts = append(parts, part)
							}
						}
					}
				}

				// Tool calls -> single model content with functionCall parts
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					fIDs := make([]string, 0)
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() != "function" {
							continue
						}
						fid := tc.Get("id").String()
						fname := util.SanitizeFunctionName(tc.Get("function.name").String())
						if fname == "" {
							continue
						}
						fargs := tc.Get("function.arguments").String()
						part := []byte(`{"functionCall":{"name":"","args":{}},"thoughtSignature":""}`)
						part, _ = sjson.SetBytes(part, "functionCall.name", fname)
						part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(fargs))
						part, _ = sjson.SetBytes(part, "thoughtSignature", openAIToolCallGeminiThoughtSignature(tc))
						parts = append(parts, part)
						if fid != "" {
							fIDs = append(fIDs, fid)
						}
					}
					if len(parts) > 0 {
						node, _ = sjson.SetRawBytes(node, "parts", common.RawJSONArray(parts))
						contentNodes = append(contentNodes, node)
					}

					// Append a single tool content combining name + response per function
					toolNode := []byte(`{"role":"user","parts":[]}`)
					toolParts := make([][]byte, 0, len(fIDs))
					for _, fid := range fIDs {
						if name, ok := tcID2Name[fid]; ok {
							part := []byte(`{"functionResponse":{"name":"","response":{"result":null}}}`)
							part, _ = sjson.SetBytes(part, "functionResponse.name", util.SanitizeFunctionName(name))
							resp := toolResponses[fid]
							if resp == "" {
								resp = "{}"
							}
							part, _ = sjson.SetBytes(part, "functionResponse.response.result", []byte(resp))
							toolParts = append(toolParts, part)
						}
					}
					if len(toolParts) > 0 {
						toolNode, _ = sjson.SetRawBytes(toolNode, "parts", common.RawJSONArray(toolParts))
						contentNodes = append(contentNodes, toolNode)
					}
				} else if len(parts) > 0 {
					node, _ = sjson.SetRawBytes(node, "parts", common.RawJSONArray(parts))
					contentNodes = append(contentNodes, node)
				}
			}
		}

		if len(systemParts) > 0 {
			systemInstruction := []byte(`{"role":"user","parts":[]}`)
			systemInstruction, _ = sjson.SetRawBytes(systemInstruction, "parts", common.RawJSONArray(systemParts))
			out, _ = sjson.SetRawBytes(out, "systemInstruction", systemInstruction)
		}
		if len(contentNodes) > 0 && gjson.GetBytes(contentNodes[len(contentNodes)-1], "role").String() == "model" {
			contentNodes = contentNodes[:len(contentNodes)-1]
		}
		out, _ = sjson.SetRawBytes(out, "contents", common.RawJSONArray(contentNodes))
	}

	// tools -> tools[].functionDeclarations + tools[].googleSearch/codeExecution/urlContext passthrough
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		functionDeclarations := make([][]byte, 0)
		googleSearchNodes := make([][]byte, 0)
		codeExecutionNodes := make([][]byte, 0)
		urlContextNodes := make([][]byte, 0)
		for _, t := range tools.Array() {
			if t.Get("type").String() == "function" {
				fn := t.Get("function")
				if fn.Exists() && fn.IsObject() {
					fnRaw := fn.Raw
					if fn.Get("parameters").Exists() {
						cleaned := util.CleanJSONSchemaForStrictUpstream(fn.Get("parameters").Raw)
						var errSet error
						fnRawBytes := []byte(fnRaw)
						fnRawBytes, errSet = sjson.DeleteBytes(fnRawBytes, "parameters")
						if errSet != nil {
							log.Warnf("Failed to delete parameters for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRawBytes, errSet = sjson.SetRawBytes(fnRawBytes, "parametersJsonSchema", []byte(cleaned))
						if errSet != nil {
							log.Warnf("Failed to set cleaned schema for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw = string(fnRawBytes)
					} else {
						var errSet error
						fnRawBytes := []byte(fnRaw)
						fnRawBytes, errSet = sjson.SetRawBytes(fnRawBytes, "parametersJsonSchema", []byte(util.CleanJSONSchemaForStrictUpstream("")))
						if errSet != nil {
							log.Warnf("Failed to set default schema for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw = string(fnRawBytes)
					}
					fnRawBytes := []byte(fnRaw)
					fnRawBytes, _ = sjson.SetBytes(fnRawBytes, "name", util.SanitizeFunctionName(fn.Get("name").String()))
					fnRaw = string(fnRawBytes)
					if parameters := gjson.Get(fnRaw, "parametersJsonSchema"); parameters.Exists() {
						fnRaw, _ = sjson.SetRaw(fnRaw, "parametersJsonSchema", util.CleanJSONSchemaForGemini(parameters.Raw))
					}
					fnRaw, _ = sjson.Delete(fnRaw, "strict")
					functionDeclarations = append(functionDeclarations, []byte(fnRaw))
				}
			}
			if gs := t.Get("google_search"); gs.Exists() {
				googleToolNode := []byte(`{}`)
				var errSet error
				googleToolNode, errSet = sjson.SetRawBytes(googleToolNode, "googleSearch", []byte(gs.Raw))
				if errSet != nil {
					log.Warnf("Failed to set googleSearch tool: %v", errSet)
					continue
				}
				googleSearchNodes = append(googleSearchNodes, googleToolNode)
			}
			if ce := t.Get("code_execution"); ce.Exists() {
				codeToolNode := []byte(`{}`)
				var errSet error
				codeToolNode, errSet = sjson.SetRawBytes(codeToolNode, "codeExecution", []byte(ce.Raw))
				if errSet != nil {
					log.Warnf("Failed to set codeExecution tool: %v", errSet)
					continue
				}
				codeExecutionNodes = append(codeExecutionNodes, codeToolNode)
			}
			if uc := t.Get("url_context"); uc.Exists() {
				urlToolNode := []byte(`{}`)
				var errSet error
				urlToolNode, errSet = sjson.SetRawBytes(urlToolNode, "urlContext", []byte(uc.Raw))
				if errSet != nil {
					log.Warnf("Failed to set urlContext tool: %v", errSet)
					continue
				}
				urlContextNodes = append(urlContextNodes, urlToolNode)
			}
		}
		if len(functionDeclarations) > 0 || len(googleSearchNodes) > 0 || len(codeExecutionNodes) > 0 || len(urlContextNodes) > 0 {
			toolNodes := make([][]byte, 0, 1+len(googleSearchNodes)+len(codeExecutionNodes)+len(urlContextNodes))
			if len(functionDeclarations) > 0 {
				functionToolNode := []byte(`{"functionDeclarations":[]}`)
				functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations", common.RawJSONArray(functionDeclarations))
				toolNodes = append(toolNodes, functionToolNode)
			}
			toolNodes = append(toolNodes, googleSearchNodes...)
			toolNodes = append(toolNodes, codeExecutionNodes...)
			toolNodes = append(toolNodes, urlContextNodes...)
			out, _ = sjson.SetRawBytes(out, "tools", common.RawJSONArray(toolNodes))
		}
	}

	out = common.AttachDefaultSafetySettings(out, "safetySettings")

	return out
}

func openAIToolCallGeminiThoughtSignature(toolCall gjson.Result) string {
	for _, path := range []string{
		"extra_content.google.thought_signature",
		"function.extra_content.google.thought_signature",
		"thoughtSignature",
		"thought_signature",
	} {
		if signatureResult := toolCall.Get(path); signatureResult.Exists() {
			return sigcompat.GeminiReplaySignatureOrBypass(signatureResult.String(), sigcompat.SignatureBlockKindGeminiFunctionCall)
		}
	}
	return geminiFunctionThoughtSignature
}

func openAIInputAudioMimeType(audioFormat string) string {
	switch audioFormat {
	case "", "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	case "webm":
		return "audio/webm"
	case "pcm16":
		return "audio/pcm"
	case "g711_ulaw", "g711_alaw":
		return "audio/basic"
	default:
		return "audio/" + audioFormat
	}
}
