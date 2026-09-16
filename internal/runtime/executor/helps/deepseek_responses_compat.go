package helps

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DeepSeekCodingResponsesUnsupported identifies the Coding endpoint observed to
// reject Responses. Standard Ark inference endpoints are deliberately excluded.
func DeepSeekCodingResponsesUnsupported(baseURL, endpoint string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".volces.com") {
		return false
	}
	p := strings.TrimRight(u.Path, "/")
	return (p == "/api/coding" || p == "/api/coding/v1") &&
		DeepSeekIsResponsesEndpoint(endpoint)
}

// DeepSeekIsResponsesEndpoint reports whether endpoint is a Responses API path
// whose request body may carry namespace tools or namespaced function-call
// history. Both the standard and legacy compaction endpoints are included.
func DeepSeekIsResponsesEndpoint(endpoint string) bool {
	return endpoint == "/responses" || endpoint == "/responses/compact"
}

type deepSeekToolName struct{ namespace, name string }

// DeepSeekNamespaceMap is request-local and restores only function-call names.
type DeepSeekNamespaceMap map[string]deepSeekToolName

var deepSeekFunctionName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// FlattenDeepSeekNamespaces preserves function schemas, choice, and history.
// Unsupported namespace children fail closed; no tool is silently dropped.
func FlattenDeepSeekNamespaces(body []byte) ([]byte, DeepSeekNamespaceMap, error) {
	root := gjson.ParseBytes(body)
	names := DeepSeekNamespaceMap{}
	reserved := map[string]bool{}
	for _, tool := range root.Get("tools").Array() {
		if tool.Get("type").String() != "namespace" {
			reserved[tool.Get("name").String()] = true
		}
	}
	register := func(namespace, name string) (string, error) {
		if !deepSeekFunctionName.MatchString(namespace) || !deepSeekFunctionName.MatchString(name) {
			return "", fmt.Errorf("invalid namespace function name")
		}
		hash := sha256.Sum256([]byte(namespace + "\x00" + name))
		wire := fmt.Sprintf("ns_%x", hash[:16])
		pair := deepSeekToolName{namespace, name}
		if reserved[wire] || (names[wire] != (deepSeekToolName{}) && names[wire] != pair) {
			return "", fmt.Errorf("namespace function name collision")
		}
		names[wire] = pair
		return wire, nil
	}
	out := body
	var tools []json.RawMessage
	declared := map[string]bool{}
	changedTools := false
	for _, tool := range root.Get("tools").Array() {
		if tool.Get("type").String() != "namespace" {
			tools = append(tools, json.RawMessage(tool.Raw))
			continue
		}
		children := tool.Get("tools")
		if !children.IsArray() || len(children.Array()) == 0 {
			return body, nil, fmt.Errorf("empty or invalid namespace")
		}
		for _, child := range children.Array() {
			if child.Get("type").String() != "function" {
				return body, nil, fmt.Errorf("unsupported namespace child")
			}
			wire, err := register(tool.Get("name").String(), child.Get("name").String())
			if err != nil || declared[wire] {
				return body, nil, fmt.Errorf("invalid or duplicate namespace function")
			}
			declared[wire] = true
			flat, _ := sjson.Set(child.Raw, "name", wire)
			// Preserve the original name as model-visible meaning, even when the
			// wire name must be hashed to avoid collisions and the 64-byte limit.
			description := tool.Get("name").String() + "." + child.Get("name").String()
			if parent := tool.Get("description").String(); parent != "" {
				description += "\n" + parent
			}
			description += "\n" + child.Get("description").String()
			flat, _ = sjson.Set(flat, "description", description)
			tools = append(tools, json.RawMessage(flat))
		}
		changedTools = true
	}
	if changedTools {
		out, _ = sjson.SetBytes(out, "tools", tools)
	}
	for i, item := range root.Get("input").Array() {
		if item.Get("type").String() != "function_call" || item.Get("namespace").String() == "" {
			continue
		}
		wire, err := register(item.Get("namespace").String(), item.Get("name").String())
		if err != nil {
			return body, nil, err
		}
		out, _ = sjson.SetBytes(out, fmt.Sprintf("input.%d.name", i), wire)
		out, _ = sjson.DeleteBytes(out, fmt.Sprintf("input.%d.namespace", i))
	}
	choice := root.Get("tool_choice")
	if choice.Get("namespace").String() != "" {
		if choice.Get("type").String() != "function" {
			return body, nil, fmt.Errorf("unsupported namespaced tool choice")
		}
		wire, err := register(choice.Get("namespace").String(), choice.Get("name").String())
		if err != nil {
			return body, nil, err
		}
		out, _ = sjson.SetBytes(out, "tool_choice.name", wire)
		out, _ = sjson.DeleteBytes(out, "tool_choice.namespace")
	}
	return out, names, nil
}

// Restore accepts either a Responses JSON object or one SSE data line.
func (names DeepSeekNamespaceMap) Restore(body []byte) []byte {
	if len(names) == 0 {
		return body
	}
	prefix := []byte(nil)
	raw := body
	if bytes.HasPrefix(raw, []byte("data:")) {
		prefix = []byte("data: ")
		raw = bytes.TrimSpace(raw[5:])
	}
	if !gjson.ValidBytes(raw) {
		return body
	}
	root := gjson.ParseBytes(raw)
	out := raw
	restore := func(path string, item gjson.Result) {
		pair, ok := names[item.Get("name").String()]
		if !ok {
			return
		}
		out, _ = sjson.SetBytes(out, path+"name", pair.name)
		out, _ = sjson.SetBytes(out, path+"namespace", pair.namespace)
	}
	for _, base := range []string{"", "response."} {
		for i, item := range root.Get(base + "output").Array() {
			if item.Get("type").String() == "function_call" {
				restore(fmt.Sprintf("%soutput.%d.", base, i), item)
			}
		}
	}
	if item := root.Get("item"); item.Get("type").String() == "function_call" {
		restore("item.", item)
	}
	if strings.HasPrefix(root.Get("type").String(), "response.function_call_arguments.") {
		restore("", root)
	}
	if bytes.Equal(out, raw) {
		return body
	}
	return append(prefix, out...)
}

var deepSeekErrorField = regexp.MustCompile(`\b(messages|input|tools)\[[0-9]{1,6}\](\.(content|tool_calls|role|reasoning_content|parameters)(\[[0-9]{1,6}\])?)*`)

// DeepSeekErrorDiagnostic returns only a fixed category and structural path.
// Provider messages can echo prompts, tool arguments, or secrets; never log them.
func DeepSeekErrorDiagnostic(body []byte) (reason, field string) {
	message := gjson.GetBytes(body, "error.message").String()
	if message == "" {
		message = gjson.GetBytes(body, "message").String()
	}
	if len(message) > 8192 {
		message = message[:8192]
	}
	lower := strings.ToLower(message)
	field = deepSeekErrorField.FindString(message)
	switch {
	case strings.Contains(lower, "reasoning_content") || strings.Contains(lower, "thinking"):
		reason = "thinking_history_or_parameter"
	case strings.Contains(lower, "tool_call_id") || strings.Contains(lower, "tool_result") || strings.Contains(lower, "tool_calls"):
		reason = "tool_history_pairing"
	case strings.Contains(lower, "schema"):
		reason = "tool_or_output_schema"
	case strings.Contains(lower, "context") || strings.Contains(lower, "maximum context"):
		reason = "context_limit"
	case strings.Contains(lower, "image"):
		reason = "image_input"
	case strings.Contains(lower, "content") || strings.Contains(lower, "role"):
		reason = "message_shape"
	default:
		reason = "unclassified"
	}
	return reason, field
}
