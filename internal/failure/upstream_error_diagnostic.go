package failure

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const maxReportedStringLength int64 = 1_000_000_000

var reportedStringLengthPattern = regexp.MustCompile(`^Expected a string with maximum length ([0-9]+), but got a string with length ([0-9]+) instead\.$`)

// UpstreamErrorDiagnostic retains only reviewed protocol field names and
// upstream-reported lengths. It never contains parameter values or error prose.
type UpstreamErrorDiagnostic struct {
	Param                string
	ReportedMaxLength    *int64
	ReportedActualLength *int64
	LengthUnit           string
}

// Normalized also detaches numeric pointers so later caller mutations cannot
// change a diagnostic already handed to an asynchronous usage sink.
func (d UpstreamErrorDiagnostic) Normalized() UpstreamErrorDiagnostic {
	clean := UpstreamErrorDiagnostic{Param: upstreamErrorParameter(d.Param)}
	for _, length := range []*int64{d.ReportedMaxLength, d.ReportedActualLength} {
		if length != nil && (*length < 0 || *length > maxReportedStringLength) {
			return UpstreamErrorDiagnostic{}
		}
	}
	if d.ReportedMaxLength != nil {
		value := *d.ReportedMaxLength
		clean.ReportedMaxLength = &value
	}
	if d.ReportedActualLength != nil {
		value := *d.ReportedActualLength
		clean.ReportedActualLength = &value
	}
	if clean.ReportedMaxLength != nil || clean.ReportedActualLength != nil {
		switch d.LengthUnit {
		case "bytes", "characters":
			clean.LengthUnit = d.LengthUnit
		}
	}
	return clean
}

// ParseUpstreamErrorDiagnostic examines one selected error object. Lengths are
// accepted only for the exact string-length code and a complete fixed template;
// the template does not establish whether a length counts bytes or characters.
func ParseUpstreamErrorDiagnostic(errorObject []byte) UpstreamErrorDiagnostic {
	return ParseUpstreamErrorDiagnosticAt(errorObject, "")
}

// ParseUpstreamErrorDiagnosticAt rejects ambiguity in the enclosing JSON before
// selecting an error object. Only the executor's existing envelope paths apply.
func ParseUpstreamErrorDiagnosticAt(body []byte, path string) UpstreamErrorDiagnostic {
	switch path {
	case "", "error", "body.error", "response.error", "data.error", "detail.error":
	default:
		return UpstreamErrorDiagnostic{}
	}
	if !unambiguousDiagnosticJSON(body) {
		return UpstreamErrorDiagnostic{}
	}
	root := gjson.ParseBytes(body)
	if path != "" {
		root = root.Get(path)
	}
	if !root.IsObject() {
		return UpstreamErrorDiagnostic{}
	}
	rawParam := root.Get("param")
	diagnostic := UpstreamErrorDiagnostic{}
	if rawParam.Type == gjson.String {
		diagnostic.Param = upstreamErrorParameter(rawParam.Str)
	}
	code := root.Get("code")
	if code.Type != gjson.String || code.Str != "string_above_max_length" {
		return diagnostic
	}
	message := root.Get("message")
	if message.Type != gjson.String || len(message.Str) > 512 {
		return diagnostic
	}
	text := message.Str
	if strings.HasPrefix(text, "Invalid '") {
		if rawParam.Type != gjson.String || diagnostic.Param == "" {
			return diagnostic
		}
		prefix := "Invalid '" + rawParam.Str + "': string too long. "
		if !strings.HasPrefix(text, prefix) {
			return diagnostic
		}
		text = strings.TrimPrefix(text, prefix)
	}
	matches := reportedStringLengthPattern.FindStringSubmatch(text)
	if matches == nil {
		return diagnostic
	}
	limit, errLimit := strconv.ParseInt(matches[1], 10, 64)
	actual, errActual := strconv.ParseInt(matches[2], 10, 64)
	if errLimit != nil || errActual != nil || limit > maxReportedStringLength || actual > maxReportedStringLength {
		return UpstreamErrorDiagnostic{}
	}
	if actual <= limit {
		return diagnostic
	}
	diagnostic.ReportedMaxLength, diagnostic.ReportedActualLength = &limit, &actual
	return diagnostic
}

// All limits apply only to diagnostic extraction, never to request acceptance.
func unambiguousDiagnosticJSON(body []byte) bool {
	if len(body) > 16<<10 || !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return false
	}
	nodes := 0
	var check func(gjson.Result, int) bool
	check = func(value gjson.Result, depth int) bool {
		nodes++
		if nodes > 256 || depth > 8 {
			return false
		}
		if value.IsObject() {
			seen, valid := map[string]bool{}, true
			value.ForEach(func(key, entry gjson.Result) bool {
				if seen[key.Str] {
					valid = false
					return false
				}
				seen[key.Str] = true
				valid = check(entry, depth+1)
				return valid
			})
			return valid
		}
		if value.IsArray() {
			valid := true
			value.ForEach(func(_, entry gjson.Result) bool {
				valid = check(entry, depth+1)
				return valid
			})
			return valid
		}
		return true
	}
	return check(root, 0)
}

func upstreamErrorParameter(param string) string {
	if len(param) > 256 {
		return ""
	}
	for _, char := range param {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '.' || char == '[' || char == ']') {
			return ""
		}
	}
	parts := strings.Split(param, ".")
	if len(parts) > 16 {
		return ""
	}
	result := make([]string, 0, len(parts))
	for i, part := range parts {
		if i > 0 && (result[0] == "metadata" || result[0] == "logit_bias") {
			return result[0]
		}
		name, index, hasIndex := strings.Cut(part, "[")
		if hasIndex {
			if len(index) > 7 || !strings.HasSuffix(index, "]") {
				return ""
			}
			for _, char := range index[:len(index)-1] {
				if char < '0' || char > '9' {
					return ""
				}
			}
		}
		if i == 0 {
			switch name {
			case "model", "input", "messages", "instructions", "tools", "tool_choice", "parallel_tool_calls",
				"temperature", "top_p", "max_tokens", "max_completion_tokens", "max_output_tokens", "reasoning",
				"reasoning_effort", "thinking", "text", "response_format", "output_config", "stream", "stream_options",
				"store", "metadata", "user", "service_tier", "include", "previous_response_id", "conversation",
				"truncation", "prompt_cache_key", "prompt_cache_retention", "stop", "n", "seed", "frequency_penalty",
				"presence_penalty", "logprobs", "top_logprobs", "logit_bias", "modalities", "audio":
			default:
				return ""
			}
		} else {
			switch name {
			case "content", "role", "type", "text", "image_url", "url", "detail", "input_audio", "audio", "data",
				"mime_type", "function", "name", "parameters", "input_schema", "strict", "description", "arguments",
				"id", "call_id", "tool_call_id", "tool_use_id", "tool_calls", "reasoning", "effort", "summary", "enabled",
				"format", "json_schema", "schema", "max_tokens", "budget_tokens":
			default:
				return strings.Join(result, ".")
			}
		}
		if hasIndex {
			name += "[]"
		}
		result = append(result, name)
	}
	return strings.Join(result, ".")
}
