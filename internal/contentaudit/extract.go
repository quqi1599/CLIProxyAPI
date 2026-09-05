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
	CurrentUserText   string
	CurrentTruncated  bool
	ReferenceText     string
	ReferenceFields   []string
	ContextIncomplete bool
	Model             string
	Stream            bool
	Evidence          []byte
	ExtractedFields   []string
	EnforcementFields []string
	Continuation      bool
	EvidenceSanitized bool
	promptSegments    []promptSegment
	enforcementParts  []promptSegment
	referenceParts    []promptSegment
}

type promptSegment struct {
	text string
	role string
}

type enforcementSegment struct {
	text      string
	fields    []string
	role      string
	parts     []promptSegment
	truncated bool
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
	return extractJSONRequest(body, "")
}

// ExtractJSONRequestForPath selects the protocol's authoritative prompt field.
// A decoy messages array must not replace Responses input (or vice versa).
func ExtractJSONRequestForPath(body []byte, requestPath string) ExtractedRequest {
	return extractJSONRequest(body, requestPath)
}

func extractJSONRequest(body []byte, requestPath string) ExtractedRequest {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		text := strings.TrimSpace(string(body))
		truncated := utf8.RuneCountInString(text) > maxEvidenceStringRunes
		if truncated {
			text = string([]rune(text)[:maxEvidenceStringRunes])
		}
		evidence, _ := json.Marshal(map[string]any{
			"invalid_json":      true,
			"raw_text":          text,
			"current_truncated": truncated,
		})
		return ExtractedRequest{
			Text:              text,
			EnforcementText:   text,
			CurrentUserText:   text,
			CurrentTruncated:  truncated,
			ContextIncomplete: true,
			Evidence:          evidence,
			EvidenceSanitized: true,
			promptSegments:    []promptSegment{{text: text, role: "unknown"}},
			enforcementParts:  []promptSegment{{text: text, role: "unknown"}},
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
	enforcementRoot, missingProtocolInput := protocolEnforcementRoot(root, requestPath)
	enforcementSegments := make([]enforcementSegment, 0, 8)
	collectEnforcementSegments(enforcementRoot, "$", false, &enforcementSegments)
	if len(enforcementSegments) == 0 {
		if object, ok := enforcementRoot.(map[string]any); ok {
			if text, exists := object["text"].(string); exists {
				appendEnforcementSegment(text, []string{"$.text"}, &enforcementSegments)
			}
		}
	}
	current, references, continuation := enforcementScope(enforcementSegments)
	referenceParts := make([]promptSegment, 0, 4)
	referenceFields := make([]string, 0, 4)
	referenceTexts := make([]string, 0, len(references))
	contextIncomplete := missingProtocolInput || current.truncated || current.role != "" && current.text == ""
	const maxReferenceRunes = 4096
	for _, reference := range references {
		referenceText := reference.text
		if utf8.RuneCountInString(referenceText) > maxReferenceRunes {
			referenceText = string([]rune(referenceText)[utf8.RuneCountInString(referenceText)-maxReferenceRunes:])
			contextIncomplete = true
		}
		referenceTexts = append(referenceTexts, reference.role+":\n"+referenceText)
		referenceFields = append(referenceFields, reference.fields...)
		referenceParts = append(referenceParts, promptSegment{text: referenceText, role: reference.role})
		contextIncomplete = contextIncomplete || reference.truncated
	}
	if object, ok := root.(map[string]any); ok {
		if previousID, _ := object["previous_response_id"].(string); strings.TrimSpace(previousID) != "" {
			contextIncomplete = true
		}
	}
	if continuation && len(references) == 0 {
		contextIncomplete = true
	}
	sanitized := sanitizeEvidenceValue(root, "")
	evidence, err := json.Marshal(map[string]any{
		"request_body":       sanitized,
		"extracted_text":     strings.Join(parts, "\n"),
		"fields":             fields,
		"enforcement_fields": current.fields,
		"reference_fields":   referenceFields,
		"context_incomplete": contextIncomplete,
		"current_truncated":  current.truncated,
		"continuation":       continuation,
	})
	if err != nil {
		evidence = []byte(`{"evidence_error":"marshal_failed"}`)
	}
	return ExtractedRequest{
		Text:              strings.Join(parts, "\n"),
		EnforcementText:   current.text,
		CurrentUserText:   current.text,
		CurrentTruncated:  current.truncated,
		ReferenceText:     strings.Join(referenceTexts, "\n\n"),
		ReferenceFields:   referenceFields,
		ContextIncomplete: contextIncomplete,
		Model:             strings.TrimSpace(model),
		Stream:            stream,
		Evidence:          evidence,
		ExtractedFields:   fields,
		EnforcementFields: current.fields,
		Continuation:      continuation,
		EvidenceSanitized: true,
		promptSegments:    promptSegments,
		enforcementParts:  current.parts,
		referenceParts:    referenceParts,
	}
}

func protocolEnforcementRoot(root any, requestPath string) (any, bool) {
	object, ok := root.(map[string]any)
	if !ok || requestPath == "" {
		return root, false
	}
	var field string
	switch {
	case strings.HasSuffix(requestPath, "/chat/completions"), strings.HasSuffix(requestPath, "/messages"):
		field = "messages"
	case strings.HasSuffix(requestPath, "/responses"), strings.HasSuffix(requestPath, "/responses/compact"), strings.HasSuffix(requestPath, "/embeddings"):
		field = "input"
	case strings.HasSuffix(requestPath, ":generateContent"), strings.HasSuffix(requestPath, ":streamGenerateContent"):
		field = "contents"
	case strings.HasSuffix(requestPath, "/completions"), strings.HasSuffix(requestPath, "/images/generations"), strings.HasSuffix(requestPath, "/images/edits"):
		field = "prompt"
	default:
		return root, false
	}
	value, exists := object[field]
	return map[string]any{field: value}, !exists || value == nil
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
	for _, segment := range r.enforcementParts {
		if strings.Contains(moderationCandidateText(segment.text), needle) {
			seen[segment.role] = struct{}{}
		}
	}
	if len(seen) == 0 && strings.Contains(moderationCandidateText(r.EnforcementText), needle) {
		seen["unknown"] = struct{}{}
	}
	return sortedRoles(seen)
}

// DecisionMatchedRoles reports the source actually evaluated for this decision.
func (r ExtractedRequest) DecisionMatchedRoles(decision Decision) []string {
	if decision.MatchSource != "reference" {
		return r.EnforcementMatchedRoles(decision.MatchedTerm)
	}
	needle := moderationCandidateText(decision.MatchedTerm)
	seen := make(map[string]struct{})
	for _, part := range r.referenceParts {
		if needle != "" && strings.Contains(moderationCandidateText(part.text), needle) {
			seen[part.role] = struct{}{}
		}
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

// FingerprintMaterial preserves exact source-bound text for keyed evidence
// deduplication. A punctuation variant must never reuse a different original.
func (r ExtractedRequest) FingerprintMaterial() string {
	segments := r.promptSegments
	if len(segments) == 0 && strings.TrimSpace(r.Text) != "" {
		segments = []promptSegment{{text: r.Text, role: "unknown"}}
	}
	var output strings.Builder
	output.WriteString("exact-source-v2\n")
	for _, segment := range segments {
		text := segment.text
		if text == "" {
			continue
		}
		_, _ = fmt.Fprintf(&output, "%d:%s:%d:", len(segment.role), segment.role, len(text))
		output.WriteString(text)
		output.WriteByte('\n')
	}
	for _, segment := range r.referenceParts {
		_, _ = fmt.Fprintf(&output, "reference:%d:%s:%d:", len(segment.role), segment.role, len(segment.text))
		output.WriteString(segment.text)
		output.WriteByte('\n')
	}
	return output.String()
}

func collectEnforcementSegments(value any, path string, active bool, segments *[]enforcementSegment) {
	switch typed := value.(type) {
	case map[string]any:
		if path == "$" {
			for _, key := range []string{"messages", "contents", "input", "prompt", "query", "content"} {
				if child, exists := typed[key]; exists {
					before := len(*segments)
					collectEnforcementSegments(child, path+"."+key, true, segments)
					for _, segment := range (*segments)[before:] {
						if segment.role == "user" || segment.role == "unknown" {
							return
						}
					}
				}
			}
			return
		}
		if rawType, exists := typed["type"].(string); exists && isToolPromptType(rawType) {
			return
		}
		role := "unknown"
		if path == "$.input" || path == "$.prompt" || path == "$.query" {
			role = "user"
		}
		if isProtocolMessagePath(path) {
			if rawRole, exists := typed["role"].(string); exists {
				role = normalizePromptSourceRole(rawRole)
			}
		}
		if role != "user" && role != "unknown" && role != "assistant" {
			return
		}
		appendMessageSegment(typed, path, role, segments)
	case []any:
		// Responses may represent one current message as several input_text blocks.
		// Keep all of those blocks in one task instead of selecting the last block.
		textBlocks := len(typed) > 0
		for _, item := range typed {
			object, ok := item.(map[string]any)
			if !ok {
				textBlocks = false
				break
			}
			typeName, _ := object["type"].(string)
			_, hasRole := object["role"]
			if hasRole || typeName != "input_text" && typeName != "text" {
				textBlocks = false
				break
			}
		}
		if textBlocks {
			appendMessageSegment(typed, path, "user", segments)
			return
		}
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
	truncated := utf8.RuneCountInString(text) > maxEvidenceStringRunes
	if truncated {
		text = string([]rune(text)[utf8.RuneCountInString(text)-maxEvidenceStringRunes:])
	}
	*segments = append(*segments, enforcementSegment{text: text, fields: append([]string(nil), fields...), role: "user", parts: []promptSegment{{text: text, role: "user"}}, truncated: truncated})
}

func appendMessageSegment(value any, path, role string, segments *[]enforcementSegment) {
	parts := make([]string, 0, 4)
	fields := make([]string, 0, 4)
	collectMessageText(value, path, &parts, &fields)
	if len(parts) == 0 {
		if role == "user" || role == "unknown" {
			*segments = append(*segments, enforcementSegment{role: role})
		}
		return
	}
	segment := enforcementSegment{role: role, fields: fields}
	for index, text := range parts {
		if utf8.RuneCountInString(text) > maxEvidenceStringRunes {
			text = string([]rune(text)[utf8.RuneCountInString(text)-maxEvidenceStringRunes:])
			parts[index] = text
			segment.truncated = true
		}
		segment.parts = append(segment.parts, promptSegment{text: text, role: role})
	}
	segment.text = strings.Join(parts, "\n")
	*segments = append(*segments, segment)
}

// Only protocol-level message objects establish a source role. Role-shaped
// objects pasted inside user content are untrusted material, not new messages.
func isProtocolMessagePath(path string) bool {
	for _, prefix := range []string{"$.messages[", "$.contents[", "$.input["} {
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "]") {
			continue
		}
		index := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "]")
		if index == "" {
			return false
		}
		for _, character := range index {
			if character < '0' || character > '9' {
				return false
			}
		}
		return true
	}
	return false
}

func collectMessageText(value any, path string, parts, fields *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if typeName, _ := typed["type"].(string); isToolPromptType(typeName) {
			return
		}
		for _, key := range []string{"content", "parts", "text", "input_text"} {
			if child, exists := typed[key]; exists {
				collectMessageText(child, path+"."+key, parts, fields)
			}
		}
	case []any:
		for index, item := range typed {
			collectMessageText(item, fmt.Sprintf("%s[%d]", path, index), parts, fields)
		}
	case string:
		text := strings.TrimSpace(typed)
		if text != "" && !isURLOrData(text) {
			*parts = append(*parts, text)
			*fields = append(*fields, path)
		}
	}
}

func enforcementScope(segments []enforcementSegment) (enforcementSegment, []enforcementSegment, bool) {
	currentIndex := -1
	for index := len(segments) - 1; index >= 0; index-- {
		if segments[index].role == "user" || segments[index].role == "unknown" {
			currentIndex = index
			break
		}
	}
	if currentIndex < 0 {
		return enforcementSegment{}, nil, false
	}
	current := segments[currentIndex]
	if !isContinuationPrompt(current.text) {
		return current, nil, false
	}
	previousUser, previousAssistant := -1, -1
	for index := currentIndex - 1; index >= 0; index-- {
		if segments[index].role == "assistant" && previousAssistant < 0 {
			previousAssistant = index
		}
		if segments[index].role == "user" || segments[index].role == "unknown" {
			previousUser = index
			break
		}
	}
	references := make([]enforcementSegment, 0, 2)
	if previousUser >= 0 {
		references = append(references, segments[previousUser])
	}
	if previousAssistant >= 0 {
		references = append(references, segments[previousAssistant])
	}
	return current, references, true
}

func isContinuationPrompt(text string) bool {
	normalized := moderationCandidateText(text)
	if normalized == "" {
		return false
	}
	for _, cancellation := range []string{"不再继续", "不要继续", "忽略前文", "donotcontinue", "stopcontinuing", "ignoreprevious"} {
		if index := strings.Index(normalized, cancellation); index >= 0 && !negatedContinuationCue(normalized[:index]) {
			return false
		}
	}
	for _, topicChange := range []string{"换个话题", "换一个话题", "newtopic"} {
		if index := strings.Index(normalized, topicChange); index >= 0 {
			prefix := normalized[:index]
			if !negatedContinuationCue(prefix) && !strings.HasSuffix(prefix, "donotstarta") {
				return false
			}
		}
	}
	shortCues := []string{
		"继续", "继续写", "继续上面", "继续刚才", "继续前文", "继续内容", "继续剧情", "继续故事",
		"接着", "接着写", "接着上面", "接着前文", "续写", "往下写", "不要停", "请继续", "请续写", "继续吧",
		"continue", "continueabove", "continueprevious", "continuethestory", "continuewriting",
		"keepgoing", "carryon", "keepwriting", "sameasbefore", "pleasecontinue", "goon",
	}
	if utf8.RuneCountInString(normalized) <= 64 {
		for _, cue := range shortCues {
			if normalized == cue {
				return true
			}
		}
	}
	referencesPrevious := containsAny(normalized, []string{"前文", "上文", "上一段", "前一段", "上面内容", "上面的内容", "之前内容", "之前的内容", "刚才内容", "刚才的内容", "刚才格式", "刚才的格式", "previous", "above", "earlier"})
	continues := containsAny(normalized, []string{"继续", "接着", "续写", "延续", "保持", "细化", "扩展", "再写", "补充", "continue", "keepgoing", "carryon", "keepwriting"})
	return referencesPrevious && continues
}

func negatedContinuationCue(prefix string) bool {
	for _, negative := range []string{"不要", "不能", "请勿", "别", "不应", "不允许", "donot", "cannot"} {
		if strings.HasSuffix(prefix, negative) {
			return true
		}
	}
	return false
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
		if role, exists := typed["role"].(string); exists && isProtocolMessagePath(path) {
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
		CurrentUserText:   strings.Join(parts, "\n"),
		Model:             model,
		Evidence:          evidence,
		ExtractedFields:   fields,
		EnforcementFields: append([]string(nil), fields...),
		EvidenceSanitized: true,
		promptSegments:    promptSegments,
		enforcementParts:  promptSegments,
	}
}
