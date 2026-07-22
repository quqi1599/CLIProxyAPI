package contentpart

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// Kind identifies the semantic family of a content or Responses input part.
type Kind uint8

const (
	Unknown Kind = iota
	Text
	Image
	ToolCall
	ToolResult
	Reasoning
)

// Part is a read-only view of a protocol content part. Raw JSON is intentionally
// left with the caller so unknown fields can be preserved.
type Part struct {
	Kind       Kind
	SourceType string
	Text       string
	Image      ImageValue
	ToolCall   ToolCallValue
	ToolResult ToolResultValue
	Reasoning  ReasoningValue
}

type ImageValue struct {
	URL    string
	Detail string
}

type ToolCallValue struct {
	ID        string
	Name      string
	Arguments string
	Input     string
}

type ToolResultValue struct {
	CallID string
	Output string
}

type ReasoningValue struct {
	Text      string
	Available bool
}

// Parse classifies OpenAI Chat, OpenAI Responses, and Claude-compatible part
// spellings without mutating or re-encoding the input.
func Parse(part gjson.Result) Part {
	sourceType := strings.TrimSpace(part.Get("type").String())
	parsed := Part{SourceType: sourceType}

	switch sourceType {
	case "text", "input_text", "output_text":
		parsed.Kind = Text
		parsed.Text = part.Get("text").String()
	case "image", "image_url", "input_image":
		parsed.Kind = Image
		parsed.Image = ImageFrom(part)
	case "tool_use":
		parsed.Kind = ToolCall
		parsed.ToolCall = ToolCallValue{
			ID:        part.Get("id").String(),
			Name:      part.Get("name").String(),
			Arguments: jsonValueToString(part.Get("input").Value(), "{}"),
		}
	case "function_call":
		parsed.Kind = ToolCall
		parsed.ToolCall = ToolCallValue{
			ID:        part.Get("call_id").String(),
			Name:      part.Get("name").String(),
			Arguments: part.Get("arguments").String(),
		}
	case "custom_tool_call":
		parsed.Kind = ToolCall
		parsed.ToolCall = ToolCallValue{
			ID:    part.Get("call_id").String(),
			Name:  part.Get("name").String(),
			Input: part.Get("input").String(),
		}
	case "tool_result":
		parsed.Kind = ToolResult
		parsed.ToolResult = ToolResultValue{
			CallID: part.Get("tool_use_id").String(),
			Output: claudeToolResultText(part.Get("content")),
		}
	case "function_call_output":
		parsed.Kind = ToolResult
		parsed.ToolResult = ToolResultValue{
			CallID: part.Get("call_id").String(),
			Output: part.Get("output").String(),
		}
	case "custom_tool_call_output":
		parsed.Kind = ToolResult
		parsed.ToolResult = ToolResultValue{
			CallID: part.Get("call_id").String(),
			Output: responsesToolOutputText(part.Get("output")),
		}
	case "thinking":
		parsed.Kind = Reasoning
		parsed.Reasoning.Text = strings.TrimSpace(part.Get("thinking").String())
		parsed.Reasoning.Available = parsed.Reasoning.Text != ""
	case "reasoning":
		parsed.Kind = Reasoning
		parsed.Reasoning = responsesReasoning(part)
	}

	return parsed
}

// ImageFrom extracts image URL and detail aliases without requiring a type
// field. This keeps legacy callers compatible while sharing one extraction rule.
func ImageFrom(part gjson.Result) ImageValue {
	image := ImageValue{}
	for _, path := range []string{"image_url.url", "image_url", "url"} {
		value := part.Get(path)
		if value.Exists() && value.Type == gjson.String {
			if imageURL := strings.TrimSpace(value.String()); imageURL != "" {
				image.URL = imageURL
				break
			}
		}
	}
	if image.URL == "" {
		image.URL = claudeImageSourceToURL(part.Get("source"))
	}
	image.Detail = strings.TrimSpace(part.Get("detail").String())
	if image.Detail == "" {
		image.Detail = strings.TrimSpace(part.Get("image_url.detail").String())
	}
	return image
}

func responsesReasoning(part gjson.Result) ReasoningValue {
	var text strings.Builder
	if summary := part.Get("summary"); summary.Exists() && summary.IsArray() {
		summary.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "summary_text" {
				text.WriteString(item.Get("text").String())
			}
			return true
		})
	}
	return ReasoningValue{Text: text.String(), Available: text.Len() > 0}
}

func jsonValueToString(value any, fallback string) string {
	if value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return fallback
		}
		return typed
	default:
		raw, err := json.Marshal(value)
		if err != nil || len(raw) == 0 {
			return fallback
		}
		return string(raw)
	}
}

func claudeToolResultText(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			if item.Get("type").String() == "text" {
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return content.Raw
}

func responsesToolOutputText(output gjson.Result) string {
	if output.Type == gjson.String {
		return output.String()
	}
	if output.IsArray() {
		var text strings.Builder
		output.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				text.WriteString(item.String())
				return true
			}
			if value := item.Get("text"); value.Exists() {
				text.WriteString(value.String())
			}
			return true
		})
		return text.String()
	}
	if output.Exists() {
		return output.Raw
	}
	return ""
}

func claudeImageSourceToURL(source gjson.Result) string {
	if !source.Exists() {
		return ""
	}
	switch source.Get("type").String() {
	case "base64":
		mediaType := strings.TrimSpace(source.Get("media_type").String())
		data := strings.TrimSpace(source.Get("data").String())
		if mediaType == "" || data == "" {
			return ""
		}
		return "data:" + mediaType + ";base64," + data
	case "url":
		return strings.TrimSpace(source.Get("url").String())
	default:
		return ""
	}
}
