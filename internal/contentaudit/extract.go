package contentaudit

import (
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxEvidenceStringRunes = 200_000

// ExtractedRequest contains the text scanned by policy and a redacted evidence payload.
type ExtractedRequest struct {
	Text              string
	Model             string
	Stream            bool
	Evidence          []byte
	ExtractedFields   []string
	EvidenceSanitized bool
}

var promptBearingKeys = map[string]struct{}{
	"content":  {},
	"contents": {},
	"input":    {},
	"messages": {},
	"output":   {},
	"prompt":   {},
	"query":    {},
	"text":     {},
}

var skippedContentKeys = map[string]struct{}{
	"api_key":            {},
	"authorization":      {},
	"cookie":             {},
	"credentials":        {},
	"id":                 {},
	"image_url":          {},
	"instructions":       {},
	"metadata":           {},
	"model":              {},
	"name":               {},
	"password":           {},
	"role":               {},
	"secret":             {},
	"system":             {},
	"system_instruction": {},
	"tools":              {},
	"type":               {},
	"url":                {},
}

var evidenceSecretKeys = map[string]struct{}{
	"api_key":       {},
	"apikey":        {},
	"authorization": {},
	"cookie":        {},
	"password":      {},
	"secret":        {},
	"token":         {},
}

// ExtractJSONRequest extracts prompt-bearing strings without scanning model names,
// tool schemas, URLs, credentials, or opaque media payloads.
func ExtractJSONRequest(body []byte) ExtractedRequest {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		text := strings.TrimSpace(string(body))
		if utf8.RuneCountInString(text) > maxEvidenceStringRunes {
			text = string([]rune(text)[:maxEvidenceStringRunes])
		}
		evidence, _ := json.Marshal(map[string]any{
			"invalid_json": true,
			"raw_text":     text,
		})
		return ExtractedRequest{Text: text, Evidence: evidence, EvidenceSanitized: true}
	}

	model := ""
	stream := false
	if object, ok := root.(map[string]any); ok {
		model, _ = object["model"].(string)
		stream, _ = object["stream"].(bool)
	}

	parts := make([]string, 0, 16)
	fields := make([]string, 0, 16)
	collectPromptFields(root, "$", false, &parts, &fields)
	sanitized := sanitizeEvidenceValue(root, "")
	evidence, err := json.Marshal(map[string]any{
		"request_body":   sanitized,
		"extracted_text": strings.Join(parts, "\n"),
		"fields":         fields,
	})
	if err != nil {
		evidence = []byte(`{"evidence_error":"marshal_failed"}`)
	}
	return ExtractedRequest{
		Text:              strings.Join(parts, "\n"),
		Model:             strings.TrimSpace(model),
		Stream:            stream,
		Evidence:          evidence,
		ExtractedFields:   fields,
		EvidenceSanitized: true,
	}
}

func collectPromptFields(value any, path string, active bool, parts, fields *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if role, exists := typed["role"].(string); exists {
			if isTrustedPromptRole(role) {
				if isAssistantPromptRole(role) {
					for _, key := range []string{"tool_calls", "function_call", "functionCall"} {
						if dynamicCall, ok := typed[key]; ok {
							collectPromptFields(dynamicCall, path+"."+key, true, parts, fields)
						}
					}
				}
				return
			}
			active = true
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if _, skipped := skippedContentKeys[lowerKey]; skipped {
				continue
			}
			_, promptBearing := promptBearingKeys[lowerKey]
			collectPromptFields(typed[key], path+"."+key, active || promptBearing, parts, fields)
		}
	case []any:
		for index, item := range typed {
			collectPromptFields(item, fmt.Sprintf("%s[%d]", path, index), active, parts, fields)
		}
	case string:
		if !active {
			return
		}
		text := strings.TrimSpace(typed)
		if text == "" || isURLOrData(text) {
			return
		}
		if utf8.RuneCountInString(text) > maxEvidenceStringRunes {
			text = string([]rune(text)[:maxEvidenceStringRunes])
		}
		*parts = append(*parts, text)
		*fields = append(*fields, path)
	}
}

func isAssistantPromptRole(role string) bool {
	role = strings.TrimSpace(role)
	return strings.EqualFold(role, "assistant") || strings.EqualFold(role, "model")
}

func isTrustedPromptRole(role string) bool {
	role = strings.TrimSpace(role)
	return strings.EqualFold(role, "system") ||
		strings.EqualFold(role, "developer") ||
		strings.EqualFold(role, "assistant") ||
		strings.EqualFold(role, "model")
}

func sanitizeEvidenceValue(value any, key string) any {
	lowerKey := strings.ToLower(strings.TrimSpace(key))
	if _, sensitive := evidenceSecretKeys[lowerKey]; sensitive {
		return "[REDACTED_SECRET]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			out[childKey] = sanitizeEvidenceValue(childValue, childKey)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index := range typed {
			out[index] = sanitizeEvidenceValue(typed[index], key)
		}
		return out
	case string:
		if isURLOrData(typed) || looksLikeBase64(typed) {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(typed)), "data:") {
				return fmt.Sprintf("[REDACTED_DATA_URI bytes=%d]", len(typed))
			}
			if looksLikeBase64(typed) {
				return fmt.Sprintf("[REDACTED_OPAQUE_DATA bytes=%d]", len(typed))
			}
		}
		return typed
	default:
		return value
	}
}

func isURLOrData(value string) bool {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "data:") {
		return true
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return true
	}
	return false
}

func looksLikeBase64(value string) bool {
	if len(value) < 2048 {
		return false
	}
	valid := 0
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			valid++
		case r >= 'A' && r <= 'Z':
			valid++
		case r >= '0' && r <= '9':
			valid++
		case r == '+', r == '/', r == '=', r == '-', r == '_', r == '\r', r == '\n':
			valid++
		}
	}
	return valid*100/len(value) >= 98
}

// ExtractMultipartRequest scans text form fields and stores only file metadata.
func ExtractMultipartRequest(form *multipart.Form) ExtractedRequest {
	if form == nil {
		return ExtractedRequest{}
	}
	parts := make([]string, 0, len(form.Value))
	fields := make([]string, 0, len(form.Value))
	values := make(map[string][]string, len(form.Value))
	for key, entries := range form.Value {
		values[key] = append([]string(nil), entries...)
		if _, promptBearing := promptBearingKeys[strings.ToLower(strings.TrimSpace(key))]; !promptBearing {
			continue
		}
		for _, entry := range entries {
			entry = strings.TrimSpace(entry)
			if entry == "" || isURLOrData(entry) {
				continue
			}
			parts = append(parts, entry)
			fields = append(fields, "form."+key)
		}
	}
	files := make(map[string][]map[string]any, len(form.File))
	for key, entries := range form.File {
		for _, file := range entries {
			if file == nil {
				continue
			}
			files[key] = append(files[key], map[string]any{
				"filename":     file.Filename,
				"size":         file.Size,
				"content_type": file.Header.Get("Content-Type"),
			})
		}
	}
	model := ""
	if models := form.Value["model"]; len(models) > 0 {
		model = strings.TrimSpace(models[0])
	}
	evidence, _ := json.Marshal(map[string]any{
		"form_fields":    sanitizeEvidenceValue(values, ""),
		"files":          files,
		"extracted_text": strings.Join(parts, "\n"),
		"fields":         fields,
	})
	return ExtractedRequest{
		Text:              strings.Join(parts, "\n"),
		Model:             model,
		Evidence:          evidence,
		ExtractedFields:   fields,
		EvidenceSanitized: true,
	}
}
