package util

import (
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/protocol/contentpart"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func NormalizeOpenAIResponsesRequestJSON(input []byte) []byte {
	if len(input) == 0 || !gjson.ValidBytes(input) {
		return input
	}
	root := gjson.ParseBytes(input)
	in := root.Get("input")
	if !in.Exists() || !in.IsArray() {
		return input
	}

	normalized := normalizeResponsesInputArray(in.Array())
	if len(normalized) == 0 {
		return input
	}
	out, err := sjson.SetRawBytes(input, "input", normalized)
	if err != nil {
		return input
	}
	return out
}

func NormalizeOpenAIChatRequestJSON(input []byte) []byte {
	if len(input) == 0 || !gjson.ValidBytes(input) {
		return input
	}
	root := gjson.ParseBytes(input)
	msgs := root.Get("messages")
	if !msgs.Exists() || !msgs.IsArray() {
		return input
	}

	normalized := normalizeChatMessagesArray(msgs.Array())
	if len(normalized) == 0 {
		return input
	}
	out, err := sjson.SetRawBytes(input, "messages", normalized)
	if err != nil {
		return input
	}
	return out
}

func normalizeResponsesInputArray(items []gjson.Result) []byte {
	normalizedItems := make([]string, 0, len(items))
	changed := false

	for _, item := range items {
		itemType := item.Get("type").String()
		itemRole := item.Get("role").String()
		parsed := contentpart.Parse(item)
		if itemType == "" && itemRole != "" {
			itemType = "message"
		}

		switch itemType {
		case "message":
			msgRaw, extra := normalizeResponsesMessageItem(item)
			if msgRaw != "" {
				normalizedItems = append(normalizedItems, msgRaw)
				if msgRaw != item.Raw {
					changed = true
				}
			}
			for _, extraItem := range extra {
				normalizedItems = append(normalizedItems, extraItem)
				changed = true
			}
		case "tool_use":
			call := buildResponsesFunctionCall(
				strings.TrimSpace(parsed.ToolCall.ID),
				strings.TrimSpace(parsed.ToolCall.Name),
				parsed.ToolCall.Arguments,
			)
			normalizedItems = append(normalizedItems, call)
			changed = true
		case "tool_result":
			result := buildResponsesFunctionCallOutput(
				strings.TrimSpace(parsed.ToolResult.CallID),
				parsed.ToolResult.Output,
			)
			normalizedItems = append(normalizedItems, result)
			changed = true
		default:
			normalizedItems = append(normalizedItems, item.Raw)
		}
	}

	if !changed {
		return nil
	}
	return internalpayload.BuildRaw(normalizedItems)
}

func normalizeResponsesMessageItem(item gjson.Result) (string, []string) {
	original := item.Raw
	itemType := item.Get("type")
	roleValue := item.Get("role")
	role := strings.TrimSpace(roleValue.String())
	if role == "" {
		role = "user"
	}

	content := item.Get("content")
	contentItems := make([]string, 0)
	extra := make([]string, 0)
	reasoningValue := item.Get("reasoning_content")
	reasoning := strings.TrimSpace(reasoningValue.String())
	contentChanged := false
	if content.IsArray() {
		contentItems = make([]string, 0, len(content.Array()))
		for _, part := range content.Array() {
			parsed := contentpart.Parse(part)
			switch parsed.Kind {
			case contentpart.Text:
				if parsed.SourceType != "text" {
					contentItems = append(contentItems, part.Raw)
					break
				}
				normalizedType := "input_text"
				if role == "assistant" || role == "model" {
					normalizedType = "output_text"
				}
				textPart := []byte(`{}`)
				textPart, _ = sjson.SetBytes(textPart, "type", normalizedType)
				textPart, _ = sjson.SetBytes(textPart, "text", parsed.Text)
				contentItems = append(contentItems, string(textPart))
				contentChanged = true
			case contentpart.Image:
				detail := parsed.Image.Detail
				if parsed.SourceType == "image" {
					detail = ""
				}
				if imagePart := buildResponsesInputImagePart(parsed.Image.URL, detail); imagePart != nil {
					normalizedPart := string(imagePart)
					contentItems = append(contentItems, normalizedPart)
					contentChanged = contentChanged || normalizedPart != part.Raw
				} else {
					contentChanged = true
				}
			case contentpart.ToolCall:
				if parsed.SourceType != "tool_use" {
					contentItems = append(contentItems, part.Raw)
					break
				}
				extra = append(extra, buildResponsesFunctionCall(strings.TrimSpace(parsed.ToolCall.ID), strings.TrimSpace(parsed.ToolCall.Name), parsed.ToolCall.Arguments))
				contentChanged = true
			case contentpart.ToolResult:
				if parsed.SourceType != "tool_result" {
					contentItems = append(contentItems, part.Raw)
					break
				}
				extra = append(extra, buildResponsesFunctionCallOutput(strings.TrimSpace(parsed.ToolResult.CallID), parsed.ToolResult.Output))
				contentChanged = true
			case contentpart.Reasoning:
				if parsed.SourceType != "thinking" {
					contentItems = append(contentItems, part.Raw)
					break
				}
				if reasoning == "" {
					reasoning = parsed.Reasoning.Text
				}
				contentChanged = true
			default:
				contentItems = append(contentItems, part.Raw)
			}
		}
	} else if content.Exists() && content.Type == gjson.String {
		textPart := []byte(`{}`)
		partType := "input_text"
		if role == "assistant" || role == "model" {
			partType = "output_text"
		}
		textPart, _ = sjson.SetBytes(textPart, "type", partType)
		textPart, _ = sjson.SetBytes(textPart, "text", content.String())
		contentItems = append(contentItems, string(textPart))
		contentChanged = true
	} else {
		contentChanged = true
	}

	toolCalls := item.Get("tool_calls")
	if toolCalls.Exists() && toolCalls.IsArray() {
		for _, call := range toolCalls.Array() {
			if call.Get("type").String() != "function" {
				continue
			}
			callID := strings.TrimSpace(call.Get("id").String())
			name := strings.TrimSpace(call.Get("function.name").String())
			args := call.Get("function.arguments").String()
			extra = append(extra, buildResponsesFunctionCall(callID, name, args))
		}
	}

	typeChanged := !itemType.Exists() || itemType.String() != "message"
	roleChanged := !roleValue.Exists() || roleValue.String() != role
	reasoningChanged := reasoningValue.Exists() && reasoning == "" ||
		(reasoning != "" && (!reasoningValue.Exists() || reasoningValue.String() != reasoning))
	if !typeChanged && !roleChanged && !contentChanged && !toolCalls.Exists() && !reasoningChanged && len(extra) == 0 {
		return original, nil
	}

	msg := []byte(original)
	if typeChanged {
		msg, _ = sjson.SetBytes(msg, "type", "message")
	}
	if roleChanged {
		msg, _ = sjson.SetBytes(msg, "role", role)
	}
	if contentChanged {
		msg, _ = sjson.SetRawBytes(msg, "content", internalpayload.BuildRaw(contentItems))
	}
	if toolCalls.Exists() {
		msg, _ = sjson.DeleteBytes(msg, "tool_calls")
	}
	if reasoning != "" {
		if reasoningChanged {
			msg, _ = sjson.SetBytes(msg, "reasoning_content", reasoning)
		}
	} else if reasoningValue.Exists() {
		msg, _ = sjson.DeleteBytes(msg, "reasoning_content")
	}
	return string(msg), extra
}

func normalizeChatMessagesArray(messages []gjson.Result) []byte {
	normalizedMessages := make([]string, 0, len(messages))
	changed := false

	for _, message := range messages {
		before, msg, after := normalizeChatMessage(message)
		for _, extraMsg := range before {
			normalizedMessages = append(normalizedMessages, extraMsg)
			changed = true
		}
		if msg != "" {
			normalizedMessages = append(normalizedMessages, msg)
			if msg != message.Raw {
				changed = true
			}
		}
		for _, extraMsg := range after {
			normalizedMessages = append(normalizedMessages, extraMsg)
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return internalpayload.BuildRaw(normalizedMessages)
}

func normalizeChatMessage(message gjson.Result) ([]string, string, []string) {
	msg := []byte(message.Raw)
	role := strings.TrimSpace(message.Get("role").String())
	content := message.Get("content")
	if !content.IsArray() {
		return nil, string(msg), nil
	}

	normalizedContentItems := make([]string, 0, len(content.Array()))
	before := make([]string, 0)
	contentChanged := false
	reasoning := strings.TrimSpace(message.Get("reasoning_content").String())
	toolCalls := message.Get("tool_calls")
	hasToolCalls := toolCalls.IsArray()
	toolCallItems := make([]string, 0)
	if hasToolCalls {
		toolCallItems = make([]string, 0, len(toolCalls.Array()))
		for _, call := range toolCalls.Array() {
			toolCallItems = append(toolCallItems, call.Raw)
		}
	}
	hasContentParts := false

	for _, part := range content.Array() {
		parsed := contentpart.Parse(part)
		switch parsed.Kind {
		case contentpart.Text:
			if parsed.SourceType == "text" {
				normalizedContentItems = append(normalizedContentItems, part.Raw)
				hasContentParts = true
				break
			}
			textPart := []byte(`{"type":"text","text":""}`)
			textPart, _ = sjson.SetBytes(textPart, "text", parsed.Text)
			normalizedContentItems = append(normalizedContentItems, string(textPart))
			contentChanged = true
			hasContentParts = true
		case contentpart.Image:
			detail := parsed.Image.Detail
			if parsed.SourceType == "image" {
				detail = ""
			}
			if imagePart := buildChatImageURLPart(parsed.Image.URL, detail); imagePart != nil {
				normalizedPart := string(imagePart)
				normalizedContentItems = append(normalizedContentItems, normalizedPart)
				contentChanged = contentChanged || normalizedPart != part.Raw
				hasContentParts = true
			}
		case contentpart.ToolCall:
			if parsed.SourceType != "tool_use" {
				normalizedContentItems = append(normalizedContentItems, part.Raw)
				hasContentParts = true
				break
			}
			call := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
			call, _ = sjson.SetBytes(call, "id", parsed.ToolCall.ID)
			call, _ = sjson.SetBytes(call, "function.name", parsed.ToolCall.Name)
			call, _ = sjson.SetBytes(call, "function.arguments", parsed.ToolCall.Arguments)
			if !hasToolCalls {
				hasToolCalls = true
			}
			toolCallItems = append(toolCallItems, string(call))
			contentChanged = true
		case contentpart.ToolResult:
			if parsed.SourceType != "tool_result" {
				normalizedContentItems = append(normalizedContentItems, part.Raw)
				hasContentParts = true
				break
			}
			toolMsg := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
			toolMsg, _ = sjson.SetBytes(toolMsg, "tool_call_id", parsed.ToolResult.CallID)
			toolMsg, _ = sjson.SetBytes(toolMsg, "content", parsed.ToolResult.Output)
			before = append(before, string(toolMsg))
			contentChanged = true
		case contentpart.Reasoning:
			if parsed.SourceType != "thinking" {
				normalizedContentItems = append(normalizedContentItems, part.Raw)
				hasContentParts = true
				break
			}
			if role == "assistant" && reasoning == "" {
				reasoning = parsed.Reasoning.Text
			}
			contentChanged = true
		default:
			normalizedContentItems = append(normalizedContentItems, part.Raw)
			hasContentParts = true
		}
	}

	if !contentChanged {
		return nil, string(msg), nil
	}
	if hasContentParts {
		msg, _ = sjson.SetRawBytes(msg, "content", internalpayload.BuildRaw(normalizedContentItems))
	} else if role == "assistant" && hasToolCalls {
		// OpenAI-compatible backends often expect assistant tool-call messages
		// to keep an explicit empty content field instead of an empty array.
		msg, _ = sjson.SetBytes(msg, "content", "")
	} else {
		msg = nil
	}
	if hasToolCalls {
		msg, _ = sjson.SetRawBytes(msg, "tool_calls", internalpayload.BuildRaw(toolCallItems))
	}
	if reasoning != "" {
		msg, _ = sjson.SetBytes(msg, "reasoning_content", reasoning)
	}
	return before, string(msg), nil
}

func buildResponsesFunctionCall(callID, name, args string) string {
	item := []byte(`{"type":"function_call","call_id":"","name":"","arguments":"{}"}`)
	item, _ = sjson.SetBytes(item, "call_id", callID)
	item, _ = sjson.SetBytes(item, "name", name)
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	item, _ = sjson.SetBytes(item, "arguments", args)
	return string(item)
}

func buildResponsesFunctionCallOutput(callID, output string) string {
	if strings.TrimSpace(output) == "" {
		output = "(empty)"
	}
	item := []byte(`{"type":"function_call_output","call_id":"","output":""}`)
	item, _ = sjson.SetBytes(item, "call_id", callID)
	item, _ = sjson.SetBytes(item, "output", output)
	return string(item)
}

// OpenAIImageURLFromPart extracts the image URL/data URL from common OpenAI
// Chat, OpenAI Responses, and Claude-style image content parts.
func OpenAIImageURLFromPart(part gjson.Result) string {
	return contentpart.ImageFrom(part).URL
}

// ParseDataURL splits a data URL into MIME type and data payload.
func ParseDataURL(dataURL string) (string, string, bool) {
	trimmed := strings.TrimSpace(dataURL)
	if len(trimmed) < len("data:,") || !strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		return "", "", false
	}

	mediaAndData := strings.SplitN(trimmed[len("data:"):], ",", 2)
	if len(mediaAndData) != 2 {
		return "", "", false
	}

	mimeType := strings.TrimSpace(strings.SplitN(mediaAndData[0], ";", 2)[0])
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	data := strings.TrimSpace(mediaAndData[1])
	if data == "" {
		return "", "", false
	}
	return mimeType, data, true
}

// GeminiInlineDataFromPart returns either camelCase or snake_case Gemini inline
// data from a part.
func GeminiInlineDataFromPart(part gjson.Result) gjson.Result {
	inlineData := part.Get("inlineData")
	if inlineData.Exists() {
		return inlineData
	}
	return part.Get("inline_data")
}

// GeminiInlineDataMimeType extracts the MIME type from either Gemini JSON
// spelling.
func GeminiInlineDataMimeType(inlineData gjson.Result) string {
	mimeType := strings.TrimSpace(inlineData.Get("mimeType").String())
	if mimeType == "" {
		mimeType = strings.TrimSpace(inlineData.Get("mime_type").String())
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return mimeType
}

func buildResponsesInputImagePart(imageURL, detail string) []byte {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return nil
	}
	imagePart := []byte(`{"type":"input_image","image_url":""}`)
	imagePart, _ = sjson.SetBytes(imagePart, "image_url", imageURL)
	if detail = strings.TrimSpace(detail); detail != "" {
		imagePart, _ = sjson.SetBytes(imagePart, "detail", detail)
	}
	return imagePart
}

func buildChatImageURLPart(imageURL, detail string) []byte {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return nil
	}
	imagePart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
	imagePart, _ = sjson.SetBytes(imagePart, "image_url.url", imageURL)
	if detail = strings.TrimSpace(detail); detail != "" {
		imagePart, _ = sjson.SetBytes(imagePart, "image_url.detail", detail)
	}
	return imagePart
}
