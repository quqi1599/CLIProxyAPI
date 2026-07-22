package helps

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyPayloadConfigWithRoot behaves like applyPayloadConfig but treats all parameter
// paths as relative to the provided root path and restricts matches to the given
// protocol when supplied. Defaults are checked
// against the original payload when provided. requestedModel carries the client-visible
// model name before alias resolution so payload rules can target aliases precisely.
// requestPath is the inbound HTTP request path (when available) used for endpoint-scoped gates.
func ApplyPayloadConfigWithRoot(cfg *config.Config, model, protocol, root string, payload, original []byte, requestedModel string, requestPath string) []byte {
	return ApplyPayloadConfigWithRequest(cfg, model, protocol, "", root, payload, original, requestedModel, requestPath, nil)
}

// ApplyPayloadConfigWithRequest applies payload config using source protocol and request header gates.
func ApplyPayloadConfigWithRequest(cfg *config.Config, model, protocol, fromProtocol, root string, payload, original []byte, requestedModel string, requestPath string, headers http.Header) []byte {
	if cfg == nil || len(payload) == 0 {
		return payload
	}
	out := payload

	// Apply disable-image-generation filtering before payload rules so config payload
	// overrides can explicitly re-enable image_generation when desired.
	if shouldStripImageGeneration(cfg.DisableImageGeneration, requestPath) {
		out = removeToolTypeFromPayloadWithRoot(out, root, "image_generation")
		out = removeToolChoiceFromPayloadWithRoot(out, root, "image_generation")
	}

	rules := cfg.Payload
	hasPayloadRules := len(rules.Default) != 0 || len(rules.DefaultRaw) != 0 || len(rules.Override) != 0 || len(rules.OverrideRaw) != 0 || len(rules.Filter) != 0
	model = strings.TrimSpace(model)
	requestedModel = strings.TrimSpace(requestedModel)
	if !hasPayloadRules || model == "" && requestedModel == "" {
		return out
	}

	document, ok := newPayloadJSONDocument(out)
	if !ok {
		return out
	}
	source := original
	if len(source) == 0 {
		source = payload
	}
	sourceDocument, _ := newPayloadJSONDocument(source)
	candidates := payloadModelCandidates(model, requestedModel)
	appliedDefaults := make(map[string]struct{})

	// Defaults are first-write-wins and check the unmodified source payload.
	for i := range rules.Default {
		rule := &rules.Default[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, document, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			for _, resolvedPath := range resolvePayloadRulePaths(document, fullPath) {
				if sourceDocument.valueAtPath(resolvedPath) != nil {
					continue
				}
				if _, exists := appliedDefaults[resolvedPath]; exists {
					continue
				}
				rawValue, errMarshal := json.Marshal(value)
				if errMarshal == nil && document.setRaw(resolvedPath, rawValue) {
					appliedDefaults[resolvedPath] = struct{}{}
				}
			}
		}
	}
	for i := range rules.DefaultRaw {
		rule := &rules.DefaultRaw[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, document, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			for _, resolvedPath := range resolvePayloadRulePaths(document, fullPath) {
				if sourceDocument.valueAtPath(resolvedPath) != nil {
					continue
				}
				if _, exists := appliedDefaults[resolvedPath]; exists {
					continue
				}
				rawValue, valid := payloadRawValue(value)
				if valid && document.setRaw(resolvedPath, rawValue) {
					appliedDefaults[resolvedPath] = struct{}{}
				}
			}
		}
	}

	// Overrides are last-write-wins.
	for i := range rules.Override {
		rule := &rules.Override[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, document, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			rawValue, errMarshal := json.Marshal(value)
			if errMarshal != nil {
				continue
			}
			for _, resolvedPath := range resolvePayloadRulePaths(document, buildPayloadPath(root, path)) {
				document.setRaw(resolvedPath, rawValue)
			}
		}
	}
	for i := range rules.OverrideRaw {
		rule := &rules.OverrideRaw[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, document, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			rawValue, valid := payloadRawValue(value)
			if !valid {
				continue
			}
			for _, resolvedPath := range resolvePayloadRulePaths(document, buildPayloadPath(root, path)) {
				document.setRaw(resolvedPath, rawValue)
			}
		}
	}

	// Filters resolve all matching array indexes before deleting them in reverse.
	for i := range rules.Filter {
		rule := &rules.Filter[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, document, root, candidates) {
			continue
		}
		for _, path := range rule.Params {
			resolvedPaths := resolvePayloadRulePaths(document, buildPayloadPath(root, path))
			document.deleteAll(resolvedPaths)
		}
	}
	return document.bytes()
}

func isImagesEndpointRequestPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if path == "/v1/images/generations" || path == "/v1/images/edits" {
		return true
	}
	// Be tolerant of prefix routers that may report a longer matched route.
	if strings.HasSuffix(path, "/v1/images/generations") || strings.HasSuffix(path, "/v1/images/edits") {
		return true
	}
	if strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits") {
		return true
	}
	return false
}

// shouldStripImageGeneration reports whether the built-in image_generation tool must be
// removed from the outbound payload for the given mode and request path.
//   - All: strip on every endpoint.
//   - Chat: strip only on non-images endpoints; keep it on /v1/images/* endpoints.
//   - Off / Passthrough: never strip. Off injects the tool elsewhere; Passthrough forwards
//     the client payload untouched.
func shouldStripImageGeneration(mode config.DisableImageGenerationMode, requestPath string) bool {
	switch mode {
	case config.DisableImageGenerationAll:
		return true
	case config.DisableImageGenerationChat:
		return !isImagesEndpointRequestPath(requestPath)
	default:
		return false
	}
}

func payloadModelRulesMatch(rules []config.PayloadModelRule, protocol string, fromProtocol string, headers http.Header, payload *payloadJSONDocument, root string, models []string) bool {
	if len(rules) == 0 || len(models) == 0 {
		return false
	}
	for _, model := range models {
		for _, entry := range rules {
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				continue
			}
			if ep := strings.TrimSpace(entry.Protocol); ep != "" && protocol != "" && !strings.EqualFold(ep, protocol) {
				continue
			}
			if !payloadFromProtocolMatches(entry.FromProtocol, fromProtocol) {
				continue
			}
			if !payloadHeadersMatch(headers, entry.Headers) {
				continue
			}
			if !matchModelPattern(name, model) {
				continue
			}
			if payloadModelRuleConditionsMatch(payload, root, entry) {
				return true
			}
		}
	}
	return false
}

func payloadModelRuleConditionsMatch(payload *payloadJSONDocument, root string, rule config.PayloadModelRule) bool {
	if !payloadMatchConditionsMatch(payload, root, rule.Match) {
		return false
	}
	if !payloadNotMatchConditionsMatch(payload, root, rule.NotMatch) {
		return false
	}
	if !payloadExistConditionsMatch(payload, root, rule.Exist) {
		return false
	}
	if !payloadNotExistConditionsMatch(payload, root, rule.NotExist) {
		return false
	}
	return true
}

func payloadMatchConditionsMatch(payload *payloadJSONDocument, root string, conditions []map[string]any) bool {
	for _, condition := range conditions {
		for path, value := range condition {
			if strings.TrimSpace(path) == "" {
				continue
			}
			if !payloadPathMatchesValue(payload, buildPayloadPath(root, path), value) {
				return false
			}
		}
	}
	return true
}

func payloadNotMatchConditionsMatch(payload *payloadJSONDocument, root string, conditions []map[string]any) bool {
	for _, condition := range conditions {
		for path, value := range condition {
			if strings.TrimSpace(path) == "" {
				continue
			}
			if payloadPathMatchesValue(payload, buildPayloadPath(root, path), value) {
				return false
			}
		}
	}
	return true
}

func payloadExistConditionsMatch(payload *payloadJSONDocument, root string, paths []string) bool {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if !payloadPathExists(payload, buildPayloadPath(root, path)) {
			return false
		}
	}
	return true
}

func payloadNotExistConditionsMatch(payload *payloadJSONDocument, root string, paths []string) bool {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if payloadPathExists(payload, buildPayloadPath(root, path)) {
			return false
		}
	}
	return true
}

func payloadPathMatchesValue(payload *payloadJSONDocument, path string, value any) bool {
	for _, resolvedPath := range resolvePayloadRulePaths(payload, path) {
		result := payload.valueAtPath(resolvedPath)
		if result == nil {
			continue
		}
		if payloadResultEquals(result, value) {
			return true
		}
	}
	return false
}

func payloadPathExists(payload *payloadJSONDocument, path string) bool {
	for _, resolvedPath := range resolvePayloadRulePaths(payload, path) {
		result := payload.valueAtPath(resolvedPath)
		if result != nil && !result.isNull() {
			return true
		}
	}
	return false
}

func payloadResultEquals(result *payloadJSONValue, value any) bool {
	actual, ok := normalizedPayloadJSON(result.appendJSON(nil))
	if !ok {
		return false
	}
	expected, ok := normalizedPayloadValue(value)
	if !ok {
		return false
	}
	return reflect.DeepEqual(actual, expected)
}

func normalizedPayloadValue(value any) (any, bool) {
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, false
	}
	return normalizedPayloadJSON(encoded)
}

func normalizedPayloadJSON(data []byte) (any, bool) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, false
	}
	var out any
	if errUnmarshal := json.Unmarshal(data, &out); errUnmarshal != nil {
		return nil, false
	}
	return out, true
}

func payloadFromProtocolMatches(pattern, fromProtocol string) bool {
	pattern = normalizePayloadFromProtocol(pattern)
	if pattern == "" {
		return true
	}
	fromProtocol = normalizePayloadFromProtocol(fromProtocol)
	if fromProtocol == "" {
		return false
	}
	return strings.EqualFold(pattern, fromProtocol)
}

func normalizePayloadFromProtocol(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "openai-response", "openai-responses", "response":
		return "responses"
	default:
		return protocol
	}
}

func payloadHeadersMatch(headers http.Header, rules map[string]string) bool {
	if len(rules) == 0 {
		return true
	}
	for key, pattern := range rules {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values := payloadHeaderValues(headers, key)
		if len(values) == 0 {
			return false
		}
		matched := false
		for _, value := range values {
			if matchModelPattern(pattern, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func payloadHeaderValues(headers http.Header, key string) []string {
	if headers == nil {
		return nil
	}
	var values []string
	for headerKey, headerValues := range headers {
		if strings.EqualFold(headerKey, key) {
			values = append(values, headerValues...)
		}
	}
	return values
}

func payloadModelCandidates(model, requestedModel string) []string {
	model = strings.TrimSpace(model)
	requestedModel = strings.TrimSpace(requestedModel)
	if model == "" && requestedModel == "" {
		return nil
	}
	candidates := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	addCandidate := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, value)
	}
	if model != "" {
		addCandidate(model)
	}
	if requestedModel != "" {
		parsed := thinking.ParseSuffix(requestedModel)
		base := strings.TrimSpace(parsed.ModelName)
		if base != "" {
			addCandidate(base)
		}
		if parsed.HasSuffix {
			addCandidate(requestedModel)
		}
	}
	return candidates
}

// buildPayloadPath combines an optional root path with a relative parameter path.
// When root is empty, the parameter path is used as-is. When root is non-empty,
// the parameter path is treated as relative to root.
func buildPayloadPath(root, path string) string {
	r := strings.TrimSpace(root)
	p := strings.TrimSpace(path)
	if r == "" {
		return p
	}
	if p == "" {
		return r
	}
	if strings.HasPrefix(p, ".") {
		p = p[1:]
	}
	return r + "." + p
}

// payloadJSONDocument keeps object field order while payload rules mutate a parsed tree.
// It avoids serializing the full request after every configured field update.
type payloadJSONDocument struct {
	root     *payloadJSONValue
	capacity int
	original []byte
	changed  bool
}

type payloadJSONValue struct {
	kind        byte
	raw         []byte
	object      []payloadJSONField
	objectIndex map[string]int
	array       []*payloadJSONValue
}

type payloadJSONField struct {
	name    string
	rawName []byte
	value   *payloadJSONValue
}

type payloadJSONPathPart struct {
	name    string
	index   int
	numeric bool
	append  bool
}

type payloadJSONDeleteTarget struct {
	parent   *payloadJSONValue
	position int
	object   bool
}

func newPayloadJSONDocument(data []byte) (*payloadJSONDocument, bool) {
	if !gjson.ValidBytes(data) {
		return nil, false
	}
	return &payloadJSONDocument{
		root:     newPayloadJSONValue(gjson.ParseBytes(data)),
		capacity: len(data),
		original: data,
	}, true
}

func newPayloadJSONValue(result gjson.Result) *payloadJSONValue {
	raw := []byte(strings.TrimSpace(result.Raw))
	if result.Type != gjson.JSON || len(raw) == 0 {
		return &payloadJSONValue{raw: raw}
	}
	value := &payloadJSONValue{kind: raw[0]}
	switch value.kind {
	case '{':
		value.objectIndex = make(map[string]int)
		result.ForEach(func(key, child gjson.Result) bool {
			rawName := []byte(key.Raw)
			if len(rawName) == 0 {
				rawName, _ = json.Marshal(key.String())
			}
			field := payloadJSONField{name: key.String(), rawName: rawName, value: newPayloadJSONValue(child)}
			if _, exists := value.objectIndex[field.name]; !exists {
				value.objectIndex[field.name] = len(value.object)
			}
			value.object = append(value.object, field)
			return true
		})
	case '[':
		items := result.Array()
		value.array = make([]*payloadJSONValue, 0, len(items))
		for _, item := range items {
			value.array = append(value.array, newPayloadJSONValue(item))
		}
	default:
		value.kind = 0
		value.raw = raw
	}
	return value
}

func newPayloadJSONRawValue(raw []byte) *payloadJSONValue {
	if gjson.ValidBytes(raw) {
		return newPayloadJSONValue(gjson.ParseBytes(raw))
	}
	return &payloadJSONValue{raw: raw}
}

func (document *payloadJSONDocument) bytes() []byte {
	if document == nil || document.root == nil {
		return nil
	}
	if !document.changed {
		return document.original
	}
	return document.root.appendJSON(make([]byte, 0, document.capacity))
}

func (value *payloadJSONValue) appendJSON(out []byte) []byte {
	if value == nil {
		return append(out, "null"...)
	}
	switch value.kind {
	case '{':
		out = append(out, '{')
		written := false
		for _, field := range value.object {
			if field.value == nil {
				continue
			}
			if written {
				out = append(out, ',')
			}
			written = true
			out = append(out, field.rawName...)
			out = append(out, ':')
			out = field.value.appendJSON(out)
		}
		return append(out, '}')
	case '[':
		out = append(out, '[')
		written := false
		for _, item := range value.array {
			if item == nil {
				continue
			}
			if written {
				out = append(out, ',')
			}
			written = true
			out = item.appendJSON(out)
		}
		return append(out, ']')
	default:
		return append(out, value.raw...)
	}
}

func (value *payloadJSONValue) isNull() bool {
	return value != nil && value.kind == 0 && strings.TrimSpace(string(value.raw)) == "null"
}

func (document *payloadJSONDocument) valueAtPath(path string) *payloadJSONValue {
	if document == nil || document.root == nil {
		return nil
	}
	if path == "" {
		return document.root
	}
	parts, ok := parsePayloadJSONPath(path)
	if !ok {
		return nil
	}
	value := document.root
	for _, part := range parts {
		switch value.kind {
		case '{':
			position, exists := value.objectIndex[part.name]
			if !exists || position >= len(value.object) || value.object[position].value == nil {
				return nil
			}
			value = value.object[position].value
		case '[':
			if !part.numeric {
				return nil
			}
			position, exists := value.arrayPosition(part.index)
			if !exists {
				return nil
			}
			value = value.array[position]
		default:
			return nil
		}
	}
	return value
}

func (document *payloadJSONDocument) setRaw(path string, raw []byte) bool {
	parts, ok := parsePayloadJSONPath(path)
	if !ok || len(parts) == 0 || document == nil || document.root == nil {
		return false
	}
	if !document.root.set(parts, newPayloadJSONRawValue(raw)) {
		return false
	}
	document.changed = true
	return true
}

func (value *payloadJSONValue) set(parts []payloadJSONPathPart, replacement *payloadJSONValue) bool {
	part := parts[0]
	if value.kind != '{' && value.kind != '[' {
		if part.numeric || part.append {
			value.reset('[')
		} else {
			value.reset('{')
		}
	}
	if value.kind == '{' {
		position, exists := value.objectIndex[part.name]
		if len(parts) == 1 {
			if exists && value.object[position].value != nil {
				value.object[position].value = replacement
				return true
			}
			value.appendObjectField(part.name, replacement)
			return true
		}
		if !exists || value.object[position].value == nil {
			child := &payloadJSONValue{}
			value.appendObjectField(part.name, child)
			return child.set(parts[1:], replacement)
		}
		return value.object[position].value.set(parts[1:], replacement)
	}
	if !part.numeric && !part.append {
		return false
	}
	if part.append {
		if len(parts) == 1 {
			value.array = append(value.array, replacement)
			return true
		}
		child := &payloadJSONValue{}
		value.array = append(value.array, child)
		return child.set(parts[1:], replacement)
	}
	position, exists := value.arrayPosition(part.index)
	if !exists {
		for value.arrayLength() < part.index {
			value.array = append(value.array, &payloadJSONValue{raw: []byte("null")})
		}
		child := replacement
		if len(parts) > 1 {
			child = &payloadJSONValue{}
		}
		value.array = append(value.array, child)
		if len(parts) == 1 {
			return true
		}
		return child.set(parts[1:], replacement)
	}
	if len(parts) == 1 {
		value.array[position] = replacement
		return true
	}
	return value.array[position].set(parts[1:], replacement)
}

func (document *payloadJSONDocument) deleteAll(paths []string) {
	if document == nil || document.root == nil || len(paths) == 0 {
		return
	}
	targets := make([]payloadJSONDeleteTarget, 0, len(paths))
	for _, path := range paths {
		if target, ok := document.deleteTarget(path); ok {
			targets = append(targets, target)
		}
	}
	touched := make(map[*payloadJSONValue]struct{})
	for i := len(targets) - 1; i >= 0; i-- {
		target := targets[i]
		if target.object {
			if target.position >= len(target.parent.object) || target.parent.object[target.position].value == nil {
				continue
			}
			target.parent.object[target.position].value = nil
		} else {
			if target.position >= len(target.parent.array) || target.parent.array[target.position] == nil {
				continue
			}
			target.parent.array[target.position] = nil
		}
		document.changed = true
		touched[target.parent] = struct{}{}
	}
	for parent := range touched {
		parent.compact()
	}
}

func (document *payloadJSONDocument) deleteTarget(path string) (payloadJSONDeleteTarget, bool) {
	parts, ok := parsePayloadJSONPath(path)
	if !ok || len(parts) == 0 {
		return payloadJSONDeleteTarget{}, false
	}
	parent := document.root
	for _, part := range parts[:len(parts)-1] {
		switch parent.kind {
		case '{':
			position, exists := parent.objectIndex[part.name]
			if !exists || parent.object[position].value == nil {
				return payloadJSONDeleteTarget{}, false
			}
			parent = parent.object[position].value
		case '[':
			if !part.numeric {
				return payloadJSONDeleteTarget{}, false
			}
			position, exists := parent.arrayPosition(part.index)
			if !exists {
				return payloadJSONDeleteTarget{}, false
			}
			parent = parent.array[position]
		default:
			return payloadJSONDeleteTarget{}, false
		}
	}
	part := parts[len(parts)-1]
	if parent.kind == '{' {
		position, exists := parent.objectIndex[part.name]
		return payloadJSONDeleteTarget{parent: parent, position: position, object: true}, exists && parent.object[position].value != nil
	}
	if parent.kind != '[' {
		return payloadJSONDeleteTarget{}, false
	}
	if part.append {
		part.index = parent.arrayLength() - 1
		part.numeric = true
	}
	if !part.numeric {
		return payloadJSONDeleteTarget{}, false
	}
	position, exists := parent.arrayPosition(part.index)
	return payloadJSONDeleteTarget{parent: parent, position: position}, exists
}

func (value *payloadJSONValue) compact() {
	if value.kind == '[' {
		kept := value.array[:0]
		for _, item := range value.array {
			if item != nil {
				kept = append(kept, item)
			}
		}
		value.array = kept
		return
	}
	kept := value.object[:0]
	value.objectIndex = make(map[string]int, len(value.object))
	for _, field := range value.object {
		if field.value == nil {
			continue
		}
		if _, exists := value.objectIndex[field.name]; !exists {
			value.objectIndex[field.name] = len(kept)
		}
		kept = append(kept, field)
	}
	value.object = kept
}

func (value *payloadJSONValue) reset(kind byte) {
	*value = payloadJSONValue{kind: kind}
	if kind == '{' {
		value.objectIndex = make(map[string]int)
	}
}

func (value *payloadJSONValue) appendObjectField(name string, child *payloadJSONValue) {
	rawName, _ := json.Marshal(name)
	value.objectIndex[name] = len(value.object)
	value.object = append(value.object, payloadJSONField{name: name, rawName: rawName, value: child})
}

func (value *payloadJSONValue) arrayLength() int {
	length := 0
	for _, item := range value.array {
		if item != nil {
			length++
		}
	}
	return length
}

func (value *payloadJSONValue) arrayPosition(index int) (int, bool) {
	if index < 0 {
		return 0, false
	}
	if index < len(value.array) && value.array[index] != nil {
		return index, true
	}
	logical := 0
	for position, item := range value.array {
		if item == nil {
			continue
		}
		if logical == index {
			return position, true
		}
		logical++
	}
	return 0, false
}

func parsePayloadJSONPath(path string) ([]payloadJSONPathPart, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	rawParts := splitPayloadRulePath(path)
	parts := make([]payloadJSONPathPart, 0, len(rawParts))
	for _, rawPart := range rawParts {
		forceString := strings.HasPrefix(rawPart, ":")
		if forceString {
			rawPart = rawPart[1:]
		}
		var name strings.Builder
		for i := 0; i < len(rawPart); i++ {
			if rawPart[i] == '\\' {
				i++
				if i >= len(rawPart) {
					return nil, false
				}
				name.WriteByte(rawPart[i])
				continue
			}
			switch rawPart[i] {
			case '|', '#', '@', '*', '?':
				return nil, false
			default:
				name.WriteByte(rawPart[i])
			}
		}
		part := payloadJSONPathPart{name: name.String()}
		if !forceString && part.name == "-1" {
			part.append = true
		} else if !forceString && part.name != "" {
			part.index, part.numeric = parsePayloadArrayIndex(part.name)
		}
		parts = append(parts, part)
	}
	return parts, true
}

func parsePayloadArrayIndex(value string) (int, bool) {
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	index, errAtoi := strconv.Atoi(value)
	return index, errAtoi == nil
}

func resolvePayloadRulePaths(payload *payloadJSONDocument, path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if !strings.Contains(path, "#(") {
		return []string{path}
	}
	parts := splitPayloadRulePath(path)
	if len(parts) == 0 {
		return nil
	}
	paths := []string{""}
	for _, part := range parts {
		query, allMatches, ok := parsePayloadQueryPathPart(part)
		if !ok {
			for i := range paths {
				paths[i] = appendPayloadPathPart(paths[i], part)
			}
			continue
		}
		nextPaths := make([]string, 0, len(paths))
		for _, basePath := range paths {
			array := payload.valueAtPath(basePath)
			if array == nil || array.kind != '[' {
				continue
			}
			index := 0
			for _, item := range array.array {
				if item == nil {
					continue
				}
				if !payloadQueryMatches(item, query) {
					index++
					continue
				}
				nextPaths = append(nextPaths, appendPayloadPathPart(basePath, strconv.Itoa(index)))
				index++
				if !allMatches {
					break
				}
			}
		}
		paths = nextPaths
		if len(paths) == 0 {
			return nil
		}
	}
	return paths
}

func splitPayloadRulePath(path string) []string {
	var parts []string
	start := 0
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(path); i++ {
		ch := path[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '(' {
			depth++
			continue
		}
		if ch == ')' {
			if depth > 0 {
				depth--
			}
			continue
		}
		if ch == '.' && depth == 0 {
			parts = append(parts, path[start:i])
			start = i + 1
		}
	}
	parts = append(parts, path[start:])
	return parts
}

func parsePayloadQueryPathPart(part string) (string, bool, bool) {
	if !strings.HasPrefix(part, "#(") {
		return "", false, false
	}
	closeIndex := findPayloadQueryClose(part)
	if closeIndex < 0 {
		return "", false, false
	}
	suffix := part[closeIndex+1:]
	if suffix != "" && suffix != "#" {
		return "", false, false
	}
	return strings.TrimSpace(part[2:closeIndex]), suffix == "#", true
}

func findPayloadQueryClose(part string) int {
	var quote byte
	escaped := false
	depth := 1
	for i := 2; i < len(part); i++ {
		ch := part[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '(' {
			depth++
			continue
		}
		if ch == ')' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func appendPayloadPathPart(path, part string) string {
	if path == "" {
		return part
	}
	if part == "" {
		return path
	}
	return path + "." + part
}

func payloadQueryMatches(item *payloadJSONValue, query string) bool {
	for _, orPart := range splitPayloadLogical(query, "||") {
		if payloadQueryAndMatches(item, orPart) {
			return true
		}
	}
	return false
}

func payloadQueryAndMatches(item *payloadJSONValue, query string) bool {
	parts := splitPayloadLogical(query, "&&")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !payloadQueryTermMatches(item, part) {
			return false
		}
	}
	return true
}

func splitPayloadLogical(query, operator string) []string {
	var parts []string
	start := 0
	var quote byte
	escaped := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if strings.HasPrefix(query[i:], operator) {
			parts = append(parts, strings.TrimSpace(query[start:i]))
			i += len(operator) - 1
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(query[start:]))
	return parts
}

func payloadQueryTermMatches(item *payloadJSONValue, term string) bool {
	term = strings.TrimSpace(term)
	if term == "" || item == nil {
		return false
	}
	raw := item.appendJSON(nil)
	wrapped := make([]byte, 0, len(raw)+2)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, raw...)
	wrapped = append(wrapped, ']')
	return gjson.GetBytes(wrapped, "#("+term+")").Exists()
}

func removeToolTypeFromPayloadWithRoot(payload []byte, root string, toolType string) []byte {
	if len(payload) == 0 {
		return payload
	}
	toolType = strings.TrimSpace(toolType)
	if toolType == "" {
		return payload
	}
	toolsPath := buildPayloadPath(root, "tools")
	return removeToolTypeFromToolsArray(payload, toolsPath, toolType)
}

func removeToolChoiceFromPayloadWithRoot(payload []byte, root string, toolType string) []byte {
	if len(payload) == 0 {
		return payload
	}
	toolType = strings.TrimSpace(toolType)
	if toolType == "" {
		return payload
	}
	toolChoicePath := buildPayloadPath(root, "tool_choice")
	return removeToolChoiceFromPayload(payload, toolChoicePath, toolType)
}

func removeToolChoiceFromPayload(payload []byte, toolChoicePath string, toolType string) []byte {
	choice := gjson.GetBytes(payload, toolChoicePath)
	if !choice.Exists() {
		return payload
	}
	if choice.Type == gjson.String {
		if strings.EqualFold(strings.TrimSpace(choice.String()), toolType) {
			updated, errDel := sjson.DeleteBytes(payload, toolChoicePath)
			if errDel == nil {
				return updated
			}
		}
		return payload
	}
	if choice.Type != gjson.JSON {
		return payload
	}
	choiceType := strings.TrimSpace(choice.Get("type").String())
	if strings.EqualFold(choiceType, toolType) {
		updated, errDel := sjson.DeleteBytes(payload, toolChoicePath)
		if errDel == nil {
			return updated
		}
		return payload
	}
	if strings.EqualFold(choiceType, "tool") {
		name := strings.TrimSpace(choice.Get("name").String())
		if strings.EqualFold(name, toolType) {
			updated, errDel := sjson.DeleteBytes(payload, toolChoicePath)
			if errDel == nil {
				return updated
			}
		}
	}
	return payload
}

func removeToolTypeFromToolsArray(payload []byte, toolsPath string, toolType string) []byte {
	tools := gjson.GetBytes(payload, toolsPath)
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}
	removed := false
	filtered := make([]json.RawMessage, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == toolType {
			removed = true
			continue
		}
		filtered = append(filtered, json.RawMessage(tool.Raw))
	}
	if !removed {
		return payload
	}
	updated, errSet := sjson.SetRawBytes(payload, toolsPath, internalpayload.BuildRaw(filtered))
	if errSet != nil {
		return payload
	}
	return updated
}

func payloadRawValue(value any) ([]byte, bool) {
	if value == nil {
		return nil, false
	}
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		raw, errMarshal := json.Marshal(typed)
		if errMarshal != nil {
			return nil, false
		}
		return raw, true
	}
}

func PayloadRequestedModel(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return strings.TrimSpace(v)
	case []byte:
		if len(v) == 0 {
			return fallback
		}
		trimmed := strings.TrimSpace(string(v))
		if trimmed == "" {
			return fallback
		}
		return trimmed
	default:
		return fallback
	}
}

func PayloadRequestPath(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

// matchModelPattern performs simple wildcard matching where '*' matches zero or more characters.
// Examples:
//
//	"*-5" matches "gpt-5"
//	"gpt-*" matches "gpt-5" and "gpt-4"
//	"gemini-*-pro" matches "gemini-2.5-pro" and "gemini-3-pro".
func matchModelPattern(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	model = strings.TrimSpace(model)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	// Iterative glob-style matcher supporting only '*' wildcard.
	pi, si := 0, 0
	starIdx := -1
	matchIdx := 0
	for si < len(model) {
		if pi < len(pattern) && (pattern[pi] == model[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = si
			pi++
			continue
		}
		if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
