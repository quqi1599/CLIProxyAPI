// Package openai provides request translation functionality for OpenAI to Antigravity API compatibility.
// It converts OpenAI Chat Completions requests into Antigravity compatible JSON using gjson/sjson only.
package chat_completions

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const antigravityFunctionThoughtSignature = "skip_thought_signature_validator"

// ConvertOpenAIRequestToAntigravity converts an OpenAI Chat Completions request (raw JSON)
// into a complete Antigravity request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Antigravity API format
func ConvertOpenAIRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := util.NormalizeOpenAIChatRequestJSON(inputRawJSON)
	hasWebSearchTool := false
	// Base envelope (no default thinkingConfig)
	out := []byte(`{"project":"","request":{"contents":[]},"model":"gemini-2.5-pro"}`)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Let user-provided generationConfig pass through
	if genConfig := gjson.GetBytes(rawJSON, "generationConfig"); genConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "request.generationConfig", []byte(genConfig.Raw))
	}

	// Apply thinking configuration: convert OpenAI reasoning_effort to Antigravity thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
	if re.Exists() {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if effort != "" {
			thinkingPath := "request.generationConfig.thinkingConfig"
			if effort == "auto" {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingBudget", -1)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", true)
			} else {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingLevel", effort)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", effort != "none")
			}
		}
	}

	// Temperature/top_p/top_k/max_tokens
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topK", tkr.Num)
	}
	if maxTok := gjson.GetBytes(rawJSON, "max_tokens"); maxTok.Exists() && maxTok.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", maxTok.Num)
	}

	// Candidate count (OpenAI 'n' parameter)
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == gjson.Number {
		if val := n.Int(); val > 1 {
			out, _ = sjson.SetBytes(out, "request.generationConfig.candidateCount", val)
		}
	}

	// Map OpenAI modalities -> Antigravity request.generationConfig.responseModalities
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
			out, _ = sjson.SetBytes(out, "request.generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into request.generationConfig.imageConfig.aspectRatio.
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		tcID2Name := make(map[string]string)
		toolResponses := make(map[string]string)
		for _, message := range arr {
			if message.Get("role").String() == "assistant" {
				for _, toolCall := range message.Get("tool_calls").Array() {
					if toolCall.Get("type").String() != "function" {
						continue
					}
					id := toolCall.Get("id").String()
					name := toolCall.Get("function.name").String()
					if id != "" && name != "" {
						tcID2Name[id] = name
					}
				}
			}
			if message.Get("role").String() == "tool" {
				if id := message.Get("tool_call_id").String(); id != "" {
					toolResponses[id] = message.Get("content").Raw
				}
			}
		}

		systemParts := make([][]byte, 0)
		requestContents := make([][]byte, 0, len(arr))
		for _, message := range arr {
			role := message.Get("role").String()
			content := message.Get("content")
			if (role == "system" || role == "developer") && len(arr) > 1 {
				switch {
				case content.Type == gjson.String:
					systemParts = append(systemParts, textPart(content.String()))
				case content.IsObject() && content.Get("type").String() == "text":
					systemParts = append(systemParts, textPart(content.Get("text").String()))
				case content.IsArray():
					for _, item := range content.Array() {
						systemParts = append(systemParts, textPart(item.Get("text").String()))
					}
				}
				continue
			}

			if role == "user" || ((role == "system" || role == "developer") && len(arr) == 1) {
				parts := make([][]byte, 0)
				if content.Type == gjson.String {
					parts = append(parts, textPart(content.String()))
				} else if content.IsArray() {
					for _, item := range content.Array() {
						if part := openAIContentPart(item, false); part != nil {
							parts = append(parts, part)
						}
					}
				}
				requestContents = append(requestContents, contentNode("user", parts))
				continue
			}

			if role != "assistant" {
				continue
			}
			parts := make([][]byte, 0)
			if reasoning := message.Get("reasoning_content"); reasoning.Type == gjson.String && reasoning.String() != "" {
				part := textPart(reasoning.String())
				part, _ = sjson.SetBytes(part, "thought", true)
				part, _ = sjson.SetBytes(part, "thoughtSignature", antigravityFunctionThoughtSignature)
				parts = append(parts, part)
			}
			if content.Type == gjson.String && content.String() != "" {
				parts = append(parts, textPart(content.String()))
			} else if content.IsArray() {
				for _, item := range content.Array() {
					if part := openAIContentPart(item, true); part != nil {
						parts = append(parts, part)
					}
				}
			}

			toolCalls := message.Get("tool_calls")
			if !toolCalls.IsArray() {
				if len(parts) > 0 {
					requestContents = append(requestContents, contentNode("model", parts))
				}
				continue
			}
			functionIDs := make([]string, 0)
			for _, toolCall := range toolCalls.Array() {
				if toolCall.Get("type").String() != "function" {
					continue
				}
				name := util.SanitizeFunctionName(toolCall.Get("function.name").String())
				if name == "" {
					continue
				}
				id := toolCall.Get("id").String()
				arguments := toolCall.Get("function.arguments").String()
				part := []byte(`{"functionCall":{"id":"","name":""},"thoughtSignature":"skip_thought_signature_validator"}`)
				part, _ = sjson.SetBytes(part, "functionCall.id", id)
				part, _ = sjson.SetBytes(part, "functionCall.name", name)
				if gjson.Valid(arguments) {
					part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(arguments))
				} else {
					part, _ = sjson.SetBytes(part, "functionCall.args.params", []byte(arguments))
				}
				parts = append(parts, part)
				if id != "" {
					functionIDs = append(functionIDs, id)
				}
			}
			if len(parts) > 0 {
				requestContents = append(requestContents, contentNode("model", parts))
			}

			responseParts := make([][]byte, 0, len(functionIDs))
			for _, id := range functionIDs {
				name, ok := tcID2Name[id]
				if !ok {
					continue
				}
				response := toolResponses[id]
				if response == "" {
					response = "{}"
				}
				part := []byte(`{"functionResponse":{"id":"","name":"","response":{}}}`)
				part, _ = sjson.SetBytes(part, "functionResponse.id", id)
				part, _ = sjson.SetBytes(part, "functionResponse.name", util.SanitizeFunctionName(name))
				if response != "null" {
					parsed := gjson.Parse(response)
					if parsed.Type == gjson.JSON {
						part, _ = sjson.SetRawBytes(part, "functionResponse.response.result", []byte(parsed.Raw))
					} else {
						part, _ = sjson.SetBytes(part, "functionResponse.response.result", response)
					}
				}
				responseParts = append(responseParts, part)
			}
			if len(responseParts) > 0 {
				requestContents = append(requestContents, contentNode("user", responseParts))
			}
		}
		if len(systemParts) > 0 {
			out, _ = sjson.SetRawBytes(out, "request.systemInstruction", contentNode("user", systemParts))
		}
		out, _ = sjson.SetRawBytes(out, "request.contents", internalpayload.BuildRaw(requestContents))
	}

	// tools -> request.tools[].functionDeclarations + request.tools[].googleSearch/codeExecution/urlContext passthrough
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
						renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
						if errRename != nil {
							log.Warnf("Failed to rename parameters for tool '%s': %v", fn.Get("name").String(), errRename)
							var errSet error
							fnRawBytes, errSet := sjson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
							if errSet != nil {
								log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
								continue
							}
							fnRaw = string(fnRawBytes)
							fnRawBytes, errSet = sjson.SetRawBytes([]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`))
							if errSet != nil {
								log.Warnf("Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(), errSet)
								continue
							}
							fnRaw = string(fnRawBytes)
						} else {
							fnRaw = renamed
						}
					} else {
						var errSet error
						fnRawBytes, errSet := sjson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
						if errSet != nil {
							log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw = string(fnRawBytes)
						fnRawBytes, errSet = sjson.SetRawBytes([]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`))
						if errSet != nil {
							log.Warnf("Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw = string(fnRawBytes)
					}
					fnRawBytes := []byte(fnRaw)
					fnRawBytes, _ = sjson.SetBytes(fnRawBytes, "name", util.SanitizeFunctionName(fn.Get("name").String()))
					fnRaw, _ = sjson.Delete(string(fnRawBytes), "strict")
					functionDeclarations = append(functionDeclarations, []byte(fnRaw))
				}
			}
			if t.Get("type").String() == "web_search" {
				hasWebSearchTool = true
				googleSearchNodes = append(googleSearchNodes, []byte(`{"googleSearch":{}}`))
			}
			if gs := t.Get("google_search"); gs.Exists() {
				hasWebSearchTool = true
				googleToolNode := []byte(`{}`)
				googleToolNode, errSet := sjson.SetRawBytes(googleToolNode, "googleSearch", []byte(gs.Raw))
				if errSet != nil {
					log.Warnf("Failed to set googleSearch tool: %v", errSet)
					continue
				}
				googleSearchNodes = append(googleSearchNodes, googleToolNode)
			}
			if ce := t.Get("code_execution"); ce.Exists() {
				codeToolNode := []byte(`{}`)
				codeToolNode, errSet := sjson.SetRawBytes(codeToolNode, "codeExecution", []byte(ce.Raw))
				if errSet != nil {
					log.Warnf("Failed to set codeExecution tool: %v", errSet)
					continue
				}
				codeExecutionNodes = append(codeExecutionNodes, codeToolNode)
			}
			if uc := t.Get("url_context"); uc.Exists() {
				urlToolNode := []byte(`{}`)
				urlToolNode, errSet := sjson.SetRawBytes(urlToolNode, "urlContext", []byte(uc.Raw))
				if errSet != nil {
					log.Warnf("Failed to set urlContext tool: %v", errSet)
					continue
				}
				urlContextNodes = append(urlContextNodes, urlToolNode)
			}
		}
		toolNodes := make([][]byte, 0, 1+len(googleSearchNodes)+len(codeExecutionNodes)+len(urlContextNodes))
		if len(functionDeclarations) > 0 {
			functionToolNode := []byte(`{"functionDeclarations":[]}`)
			functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations", internalpayload.BuildRaw(functionDeclarations))
			toolNodes = append(toolNodes, functionToolNode)
		}
		toolNodes = append(toolNodes, googleSearchNodes...)
		toolNodes = append(toolNodes, codeExecutionNodes...)
		toolNodes = append(toolNodes, urlContextNodes...)
		if len(toolNodes) > 0 {
			out, _ = sjson.SetRawBytes(out, "request.tools", internalpayload.BuildRaw(toolNodes))
		}
	}

	if hasWebSearchTool {
		out, _ = sjson.SetBytes(out, "model", "gemini-2.5-flash")
		out, _ = sjson.SetBytes(out, "request.generationConfig.candidateCount", 1)
		out, _ = sjson.SetBytes(out, "requestType", "web_search")
	}

	return common.AttachDefaultSafetySettings(out, "request.safetySettings")
}

func textPart(text string) []byte {
	part := []byte(`{"text":""}`)
	part, _ = sjson.SetBytes(part, "text", text)
	return part
}

func contentNode(role string, parts [][]byte) []byte {
	node := []byte(`{"role":"","parts":[]}`)
	node, _ = sjson.SetBytes(node, "role", role)
	node, _ = sjson.SetRawBytes(node, "parts", internalpayload.BuildRaw(parts))
	return node
}

func openAIContentPart(item gjson.Result, assistant bool) []byte {
	switch item.Get("type").String() {
	case "text":
		if text := item.Get("text").String(); text != "" {
			return textPart(text)
		}
	case "image_url":
		imageURL := item.Get("image_url.url").String()
		if len(imageURL) <= 5 {
			return nil
		}
		pieces := strings.SplitN(imageURL[5:], ";", 2)
		if len(pieces) != 2 || len(pieces[1]) <= 7 {
			return nil
		}
		part := []byte(`{"inlineData":{"mimeType":"","data":""},"thoughtSignature":"skip_thought_signature_validator"}`)
		part, _ = sjson.SetBytes(part, "inlineData.mimeType", pieces[0])
		part, _ = sjson.SetBytes(part, "inlineData.data", pieces[1][7:])
		return part
	case "file":
		if assistant {
			return nil
		}
		filename := item.Get("file.filename").String()
		fileData := item.Get("file.file_data").String()
		extension := ""
		if pieces := strings.Split(filename, "."); len(pieces) > 1 {
			extension = pieces[len(pieces)-1]
		}
		mimeType, ok := misc.MimeTypes[extension]
		if !ok {
			log.Warnf("Unknown file name extension '%s' in user message, skip", extension)
			return nil
		}
		part := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
		part, _ = sjson.SetBytes(part, "inlineData.mimeType", mimeType)
		part, _ = sjson.SetBytes(part, "inlineData.data", fileData)
		return part
	case "input_audio":
		if assistant {
			return nil
		}
		audioData := item.Get("input_audio.data").String()
		if audioData == "" {
			return nil
		}
		audioFormat := item.Get("input_audio.format").String()
		mimeType := audioMIMEType(audioFormat)
		part := []byte(`{"inlineData":{"mime_type":"","data":""}}`)
		part, _ = sjson.SetBytes(part, "inlineData.mime_type", mimeType)
		part, _ = sjson.SetBytes(part, "inlineData.data", audioData)
		return part
	}
	return nil
}

func audioMIMEType(format string) string {
	switch format {
	case "":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
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
		return "audio/" + format
	}
}
