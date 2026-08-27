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
	EnforcementText   string
	Model             string
	Stream            bool
	Evidence          []byte
	ExtractedFields   []string
	EnforcementFields []string
	Continuation      bool
	EvidenceSanitized bool
	promptSegments    []promptSegment
}

type promptSegment struct {
	text string
	role string
}

type enforcementSegment struct {
	text   string
	fields []string
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

var enforcementPromptBearingKeys = map[string]struct{}{
	"content":  {},
	"contents": {},
	"input":    {},
	"messages": {},
	"prompt":   {},
	"query":    {},
}

var skippedContentKeys = map[string]struct{}{
	"api_key":            {},
	"authorization":      {},
	"call_id":            {},
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
	"request_id":         {},
	"tool_call_id":       {},
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
		return ExtractedRequest{
			Text:              text,
			EnforcementText:   text,
			Evidence:          evidence,
			EvidenceSanitized: true,
			promptSegments:    []promptSegment{{text: text, role: "unknown"}},
		}
	}

	model := ""
	stream := false
	if object, ok := root.(map[string]any); ok {
		model, _ = object["model"].(string)
		stream, _ = object["stream"].(bool)
	}

	parts := make([]string, 0, 16)
	fields := make([]string, 0, 16)
	promptSegments := make([]promptSegment, 0, 16)
	collectPromptFieldsWithRoles(root, "$", false, "unknown", &parts, &fields, &promptSegments)
	enforcementSegments := make([]enforcementSegment, 0, 8)
	collectEnforcementSegments(root, "$", false, &enforcementSegments)
	if len(enforcementSegments) == 0 {
		if object, ok := root.(map[string]any); ok {
			if text, exists := object["text"].(string); exists {
				appendEnforcementSegment(text, []string{"$.text"}, &enforcementSegments)
			}
		}
	}
	enforcementText, enforcementFields, continuation := enforcementScope(enforcementSegments)
	sanitized := sanitizeEvidenceValue(root, "")
	evidence, err := json.Marshal(map[string]any{
		"request_body":       sanitized,
		"extracted_text":     strings.Join(parts, "\n"),
		"fields":             fields,
		"enforcement_fields": enforcementFields,
		"continuation":       continuation,
	})
	if err != nil {
		evidence = []byte(`{"evidence_error":"marshal_failed"}`)
	}
	return ExtractedRequest{
		Text:              strings.Join(parts, "\n"),
		EnforcementText:   enforcementText,
		Model:             strings.TrimSpace(model),
		Stream:            stream,
		Evidence:          evidence,
		ExtractedFields:   fields,
		EnforcementFields: enforcementFields,
		Continuation:      continuation,
		EvidenceSanitized: true,
		promptSegments:    promptSegments,
	}
}

// MatchedRoles returns stable role categories for prompt segments containing a matched term.
func (r ExtractedRequest) MatchedRoles(term string) []string {
	needle := moderationCandidateText(term)
	if needle == "" {
		return nil
	}
	seen := make(map[string]struct{})
	for _, segment := range r.promptSegments {
		if strings.Contains(moderationCandidateText(segment.text), needle) {
			seen[segment.role] = struct{}{}
		}
	}
	if len(seen) == 0 && strings.Contains(moderationCandidateText(r.Text), needle) {
		seen["unknown"] = struct{}{}
	}
	return sortedRoles(seen)
}

// EnforcementMatchedRoles reports only roles that can contribute to a scoped
// audit decision. Tool, system, developer, and assistant content remains in
// encrypted evidence but cannot be attributed as the reason for a match.
func (r ExtractedRequest) EnforcementMatchedRoles(term string) []string {
	needle := moderationCandidateText(term)
	if needle == "" {
		return nil
	}
	seen := make(map[string]struct{})
	for _, segment := range r.promptSegments {
		if segment.role != "user" && segment.role != "unknown" {
			continue
		}
		if strings.Contains(moderationCandidateText(segment.text), needle) {
			seen[segment.role] = struct{}{}
		}
	}
	if len(seen) == 0 && strings.Contains(moderationCandidateText(r.EnforcementText), needle) {
		seen["unknown"] = struct{}{}
	}
	return sortedRoles(seen)
}

func sortedRoles(seen map[string]struct{}) []string {
	roles := make([]string, 0, len(seen))
	for role := range seen {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// FingerprintMaterial returns normalized role-bound prompt text for keyed deduplication.
func (r ExtractedRequest) FingerprintMaterial() string {
	segments := r.promptSegments
	if len(segments) == 0 && strings.TrimSpace(r.Text) != "" {
		segments = []promptSegment{{text: r.Text, role: "unknown"}}
	}
	var output strings.Builder
	for _, segment := range segments {
		text := moderationCandidateText(segment.text)
		if text == "" {
			continue
		}
		if output.Len() > 0 {
			output.WriteByte('\n')
		}
		output.WriteString(segment.role)
		output.WriteByte(':')
		output.WriteString(text)
	}
	return output.String()
}

func collectEnforcementSegments(value any, path string, active bool, segments *[]enforcementSegment) {
	switch typed := value.(type) {
	case map[string]any:
		if role, exists := typed["role"].(string); exists {
			if !strings.EqualFold(strings.TrimSpace(role), "user") {
				return
			}
			parts := make([]string, 0, 4)
			fields := make([]string, 0, 4)
			collectPromptFields(typed, path, true, &parts, &fields)
			appendEnforcementSegment(strings.Join(parts, "\n"), fields, segments)
			return
		}
		if rawType, exists := typed["type"].(string); exists && isToolPromptType(rawType) {
			return
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
			_, promptBearing := enforcementPromptBearingKeys[lowerKey]
			collectEnforcementSegments(typed[key], path+"."+key, active || promptBearing, segments)
		}
	case []any:
		for index, item := range typed {
			collectEnforcementSegments(item, fmt.Sprintf("%s[%d]", path, index), active, segments)
		}
	case string:
		if active {
			appendEnforcementSegment(typed, []string{path}, segments)
		}
	}
}

func appendEnforcementSegment(text string, fields []string, segments *[]enforcementSegment) {
	text = strings.TrimSpace(text)
	if text == "" || isURLOrData(text) {
		return
	}
	if utf8.RuneCountInString(text) > maxEvidenceStringRunes {
		text = string([]rune(text)[:maxEvidenceStringRunes])
	}
	*segments = append(*segments, enforcementSegment{text: text, fields: append([]string(nil), fields...)})
}

func enforcementScope(segments []enforcementSegment) (string, []string, bool) {
	if len(segments) == 0 {
		return "", nil, false
	}
	current := segments[len(segments)-1]
	if len(segments) == 1 || !isContinuationPrompt(current.text) {
		return current.text, append([]string(nil), current.fields...), false
	}
	previous := segments[len(segments)-2]
	fields := append([]string(nil), previous.fields...)
	fields = append(fields, current.fields...)
	return previous.text + "\n" + current.text, fields, true
}

func isContinuationPrompt(text string) bool {
	normalized := moderationCandidateText(text)
	if normalized == "" {
		return false
	}
	shortCues := []string{
		"继续", "继续写", "继续上面", "继续刚才", "继续前文", "继续内容", "继续剧情", "继续故事",
		"接着", "接着写", "接着上面", "接着前文", "续写", "往下写", "不要停",
		"continue", "continueabove", "continueprevious", "continuethestory", "continuewriting",
		"keepgoing", "carryon", "keepwriting", "sameasbefore",
	}
	if utf8.RuneCountInString(normalized) <= 64 {
		for _, cue := range shortCues {
			if normalized == cue {
				return true
			}
		}
	}
	referencesPrevious := containsAny(normalized, []string{"前文", "上文", "上面内容", "上面的内容", "之前内容", "之前的内容", "刚才内容", "刚才的内容", "previous", "above", "earlier"})
	continues := containsAny(normalized, []string{"继续", "接着", "续写", "延续", "保持", "continue", "keepgoing", "carryon", "keepwriting"})
	return referencesPrevious && continues
}

func containsAny(text string, values []string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func collectPromptFields(value any, path string, active bool, parts, fields *[]string) {
	collectPromptFieldsWithRoles(value, path, active, "unknown", parts, fields, nil)
}

func collectPromptFieldsWithRoles(value any, path string, active bool, sourceRole string, parts, fields *[]string, segments *[]promptSegment) {
	switch typed := value.(type) {
	case map[string]any:
		if role, exists := typed["role"].(string); exists {
			if isTrustedPromptRole(role) {
				if isAssistantPromptRole(role) {
					for _, key := range []string{"tool_calls", "function_call", "functionCall"} {
						if dynamicCall, ok := typed[key]; ok {
							collectPromptFieldsWithRoles(dynamicCall, path+"."+key, true, "assistant_tool_call", parts, fields, segments)
						}
					}
				}
				return
			}
			sourceRole = normalizePromptSourceRole(role)
			active = true
		}
		if rawType, exists := typed["type"].(string); exists && isToolPromptType(rawType) {
			sourceRole = normalizePromptSourceRole(rawType)
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
			childRole := sourceRole
			if childRole == "unknown" && (lowerKey == "input" || lowerKey == "prompt" || lowerKey == "query") {
				childRole = "user"
			}
			collectPromptFieldsWithRoles(typed[key], path+"."+key, active || promptBearing, childRole, parts, fields, segments)
		}
	case []any:
		for index, item := range typed {
			collectPromptFieldsWithRoles(item, fmt.Sprintf("%s[%d]", path, index), active, sourceRole, parts, fields, segments)
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
		if segments != nil {
			*segments = append(*segments, promptSegment{text: text, role: sourceRole})
		}
	}
}

func normalizePromptSourceRole(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "user", "tool", "function", "custom_tool_call_output", "function_call_output", "tool_result", "mcp_result", "computer_result", "web_search_result", "assistant_tool_call":
		return value
	case "assistant", "model":
		return "assistant"
	case "system", "developer":
		return value
	default:
		if strings.Contains(value, "tool") {
			return "tool"
		}
		if strings.Contains(value, "function") {
			return "function_call_output"
		}
		if strings.Contains(value, "mcp") {
			return "mcp_result"
		}
		if strings.Contains(value, "computer") {
			return "computer_result"
		}
		if strings.Contains(value, "web_search") {
			return "web_search_result"
		}
		return "unknown"
	}
}

func isAssistantPromptRole(role string) bool {
	role = strings.TrimSpace(role)
	return strings.EqualFold(role, "assistant") || strings.EqualFold(role, "model")
}

func isToolPromptType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(value, "tool") ||
		strings.Contains(value, "function_call") ||
		strings.Contains(value, "mcp_call") ||
		strings.Contains(value, "computer_") ||
		strings.Contains(value, "web_search_call") ||
		value == "reasoning"
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
	promptSegments := make([]promptSegment, 0, len(form.Value))
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
			promptSegments = append(promptSegments, promptSegment{text: entry, role: "user"})
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
		EnforcementText:   strings.Join(parts, "\n"),
		Model:             model,
		Evidence:          evidence,
		ExtractedFields:   fields,
		EnforcementFields: append([]string(nil), fields...),
		EvidenceSanitized: true,
		promptSegments:    promptSegments,
	}
}
