package failure

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

var kimiMessageSizeError = regexp.MustCompile(`^total message size [0-9]+ exceeds limit [0-9]+$`)
var kimiTokenLimitError = regexp.MustCompile(`^(invalid request: )?your request exceeded model token limit: [0-9]+ \(requested: [0-9]+\)$`)
var kimiMissingReasoningError = regexp.MustCompile(`^thinking is enabled but reasoning_content is missing in assistant tool call message at index [0-9]+$`)
var kimiDuplicateToolError = regexp.MustCompile(`^function name [^\r\n]{1,128} is duplicated$`)
var kimiParameterError = regexp.MustCompile("^(?:invalid request: )?(?:invalid value for |unsupported |invalid )?[`'\"]?(temperature|top_p|max_tokens|max_completion_tokens|max_output_tokens|reasoning_effort|response_format|tool_choice)[`'\"]?(?:[ .:=]|$)")

// KimiRejectionDiagnostic recognizes request-validation failures from a bounded,
// unambiguous error envelope. Outputs are fixed labels, not upstream prose,
// parameter values, function names, request content or credentials. Recognition
// is diagnostic only; the caller retains request-scoped, non-retryable errors.
func KimiRejectionDiagnostic(body []byte) (code, param, message string) {
	if !unambiguousDiagnosticJSON(body) {
		return "", "", ""
	}
	root := gjson.ParseBytes(body)
	if envelope := root.Get("error"); envelope.Exists() {
		root = envelope
	} else if envelope := root.Get("body.error"); envelope.Exists() {
		root = envelope
	}
	if !root.IsObject() {
		return "", "", ""
	}
	for _, key := range []string{"code", "type"} {
		if value := root.Get(key); value.Exists() && value.Type != gjson.Null {
			if value.Type != gjson.String {
				return "", "", ""
			}
			switch value.Str {
			case "", "invalid_request", "invalid_request_error", "bad_request_error":
			default:
				return "", "", ""
			}
		}
	}
	value := root.Get("message")
	if value.Type != gjson.String || len(value.Str) > 4096 {
		return "", "", ""
	}
	text := strings.ToLower(strings.TrimSpace(value.Str))
	switch {
	case kimiMessageSizeError.MatchString(text):
		return "kimi_message_size_exceeded", "messages", "Kimi rejected the total message size; reduce conversation or tool-result size"
	case kimiTokenLimitError.MatchString(text):
		return "kimi_context_length_exceeded", "messages", "Kimi rejected the request token length; reduce the context to the model and account limit"
	case kimiMissingReasoningError.MatchString(text):
		return "kimi_thinking_history_missing", "messages[].reasoning_content", "Kimi requires original reasoning_content on assistant tool calls; preserve the returned thinking history"
	case kimiDuplicateToolError.MatchString(text):
		return "kimi_duplicate_tool_name", "tools[].function.name", "Kimi rejected duplicate function names; use unique tool names"
	case strings.HasPrefix(text, "your model id does not exist,"):
		return "kimi_model_id_invalid", "model", "Kimi rejected the upstream model ID; check the model mapping"
	case strings.HasPrefix(text, "k3.1 pro does not support disabled thinking mode."):
		return "kimi_thinking_required", "thinking", "Kimi K3.1 requires thinking; keep thinking enabled"
	case strings.HasPrefix(text, "invalid request: unsupported image url:"):
		return "kimi_image_url_unsupported", "messages[].content[].image_url", "Kimi rejected an image URL; use a supported URL or data URL"
	}
	if match := kimiParameterError.FindStringSubmatch(text); match != nil &&
		(strings.Contains(text, "invalid") || strings.Contains(text, "not supported") || strings.Contains(text, "unsupported") || strings.Contains(text, "must be") || strings.Contains(text, "out of range")) {
		return "kimi_request_parameter_invalid", match[1], "Kimi rejected a request parameter; check the reported field against the model capabilities"
	}
	return "", "", ""
}
