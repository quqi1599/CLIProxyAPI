// Package openai provides response translation functionality for Antigravity to OpenAI API compatibility.
// This package handles the conversion of Antigravity API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"

	. "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/openai/chat-completions"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertCliResponseToOpenAIChatParams holds parameters for response conversion.
type convertCliResponseToOpenAIChatParams struct {
	UnixTimestamp        int64
	FunctionIndex        int
	SawToolCall          bool   // Tracks if any tool call was seen in the entire stream
	UpstreamFinishReason string // Caches the upstream finish reason for final chunk
	SanitizedNameMap     map[string]string
}

// functionCallIDCounter provides a process-wide unique counter for function call identifiers.
var functionCallIDCounter uint64

// ConvertAntigravityResponseToOpenAI translates a single chunk of a streaming response from the
// Antigravity API format to the OpenAI Chat Completions streaming format.
// It processes various Antigravity event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Antigravity API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func ConvertAntigravityResponseToOpenAI(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &convertCliResponseToOpenAIChatParams{
			UnixTimestamp:    0,
			FunctionIndex:    0,
			SanitizedNameMap: util.SanitizedToolNameMap(originalRequestRawJSON),
		}
	}
	if (*param).(*convertCliResponseToOpenAIChatParams).SanitizedNameMap == nil {
		(*param).(*convertCliResponseToOpenAIChatParams).SanitizedNameMap = util.SanitizedToolNameMap(originalRequestRawJSON)
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return [][]byte{}
	}

	// Initialize the OpenAI SSE template.
	template := []byte(`{"id":"","object":"chat.completion.chunk","created":12345,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)

	// Extract and set the model version.
	if modelVersionResult := gjson.GetBytes(rawJSON, "response.modelVersion"); modelVersionResult.Exists() {
		template, _ = sjson.SetBytes(template, "model", modelVersionResult.String())
	}

	// Extract and set the creation timestamp.
	if createTimeResult := gjson.GetBytes(rawJSON, "response.createTime"); createTimeResult.Exists() {
		t, err := time.Parse(time.RFC3339Nano, createTimeResult.String())
		if err == nil {
			(*param).(*convertCliResponseToOpenAIChatParams).UnixTimestamp = t.Unix()
		}
		template, _ = sjson.SetBytes(template, "created", (*param).(*convertCliResponseToOpenAIChatParams).UnixTimestamp)
	} else {
		template, _ = sjson.SetBytes(template, "created", (*param).(*convertCliResponseToOpenAIChatParams).UnixTimestamp)
	}

	// Extract and set the response ID.
	if responseIDResult := gjson.GetBytes(rawJSON, "response.responseId"); responseIDResult.Exists() {
		template, _ = sjson.SetBytes(template, "id", responseIDResult.String())
	}

	// Cache the finish reason - do NOT set it in output yet (will be set on final chunk)
	if finishReasonResult := gjson.GetBytes(rawJSON, "response.candidates.0.finishReason"); finishReasonResult.Exists() {
		(*param).(*convertCliResponseToOpenAIChatParams).UpstreamFinishReason = strings.ToUpper(finishReasonResult.String())
	}

	// Extract and set usage metadata (token counts).
	if usageResult := gjson.GetBytes(rawJSON, "response.usageMetadata"); usageResult.Exists() {
		cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
		if candidatesTokenCountResult := usageResult.Get("candidatesTokenCount"); candidatesTokenCountResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.completion_tokens", candidatesTokenCountResult.Int())
		}
		if totalTokenCountResult := usageResult.Get("totalTokenCount"); totalTokenCountResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokenCountResult.Int())
		}
		promptTokenCount := usageResult.Get("promptTokenCount").Int()
		thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
		template, _ = sjson.SetBytes(template, "usage.prompt_tokens", promptTokenCount)
		if thoughtsTokenCount > 0 {
			template, _ = sjson.SetBytes(template, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
		}
		// Include cached token count if present (indicates prompt caching is working)
		if cachedTokenCount > 0 {
			var err error
			template, err = sjson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
			if err != nil {
				log.Warnf("antigravity openai response: failed to set cached_tokens: %v", err)
			}
		}
	}

	// Process the main content part of the response.
	partsResult := gjson.GetBytes(rawJSON, "response.candidates.0.content.parts")
	if partsResult.IsArray() {
		var content string
		var reasoningContent string
		contentSet := false
		reasoningSet := false
		roleSet := false
		toolCalls := make([][]byte, 0)
		images := make([][]byte, 0)
		for _, partResult := range partsResult.Array() {
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")
			thoughtSignatureResult := partResult.Get("thoughtSignature")
			if !thoughtSignatureResult.Exists() {
				thoughtSignatureResult = partResult.Get("thought_signature")
			}
			inlineDataResult := partResult.Get("inlineData")
			if !inlineDataResult.Exists() {
				inlineDataResult = partResult.Get("inline_data")
			}

			hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
			hasContentPayload := partTextResult.Exists() || functionCallResult.Exists() || inlineDataResult.Exists()

			// Ignore encrypted thoughtSignature but keep any actual content in the same part.
			if hasThoughtSignature && !hasContentPayload {
				continue
			}

			if partTextResult.Exists() {
				textContent := partTextResult.String()
				if partResult.Get("thought").Bool() {
					reasoningContent = textContent
					reasoningSet = true
				} else {
					content = textContent
					contentSet = true
				}
				roleSet = true
			} else if functionCallResult.Exists() {
				// Handle function call content.
				(*param).(*convertCliResponseToOpenAIChatParams).SawToolCall = true // Persist across chunks
				functionCallIndex := (*param).(*convertCliResponseToOpenAIChatParams).FunctionIndex
				(*param).(*convertCliResponseToOpenAIChatParams).FunctionIndex++
				if len(toolCalls) > 0 {
					functionCallIndex = len(toolCalls)
				}

				functionCallTemplate := []byte(`{"id": "","index": 0,"type": "function","function": {"name": "","arguments": ""}}`)
				fcName := util.RestoreSanitizedToolName((*param).(*convertCliResponseToOpenAIChatParams).SanitizedNameMap, functionCallResult.Get("name").String())
				functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "id", fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1)))
				functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "index", functionCallIndex)
				functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.name", fcName)
				if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
					functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.arguments", fcArgsResult.Raw)
				}
				toolCalls = append(toolCalls, functionCallTemplate)
				roleSet = true
			} else if inlineDataResult.Exists() {
				data := inlineDataResult.Get("data").String()
				if data == "" {
					continue
				}
				mimeType := inlineDataResult.Get("mimeType").String()
				if mimeType == "" {
					mimeType = inlineDataResult.Get("mime_type").String()
				}
				if mimeType == "" {
					mimeType = "image/png"
				}
				imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
				imageIndex := len(images)
				imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
				imagePayload, _ = sjson.SetBytes(imagePayload, "index", imageIndex)
				imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
				images = append(images, imagePayload)
				roleSet = true
			}
		}
		if reasoningSet {
			template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", reasoningContent)
		}
		if contentSet {
			template, _ = sjson.SetBytes(template, "choices.0.delta.content", content)
		}
		if len(toolCalls) > 0 {
			template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", internalpayload.BuildRaw(toolCalls))
		}
		if len(images) > 0 {
			template, _ = sjson.SetRawBytes(template, "choices.0.delta.images", internalpayload.BuildRaw(images))
		}
		if roleSet {
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		}
	}

	// Determine finish_reason only on the final chunk (has both finishReason and usage metadata)
	params := (*param).(*convertCliResponseToOpenAIChatParams)
	upstreamFinishReason := params.UpstreamFinishReason
	sawToolCall := params.SawToolCall

	usageExists := gjson.GetBytes(rawJSON, "response.usageMetadata").Exists()
	isFinalChunk := upstreamFinishReason != "" && usageExists

	if isFinalChunk {
		var finishReason string
		if sawToolCall {
			finishReason = "tool_calls"
		} else if upstreamFinishReason == "MAX_TOKENS" {
			finishReason = "max_tokens"
		} else {
			finishReason = "stop"
		}
		template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)
		template, _ = sjson.SetBytes(template, "choices.0.native_finish_reason", strings.ToLower(upstreamFinishReason))
	}

	return [][]byte{template}
}

// ConvertAntigravityResponseToOpenAINonStream converts a non-streaming Antigravity response to a non-streaming OpenAI response.
// This function processes the complete Antigravity response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Antigravity API
//   - param: A pointer to a parameter object for the conversion
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertAntigravityResponseToOpenAINonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	responseResult := gjson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		return ConvertGeminiResponseToOpenAINonStream(ctx, modelName, originalRequestRawJSON, requestRawJSON, []byte(responseResult.Raw), param)
	}
	return []byte{}
}
