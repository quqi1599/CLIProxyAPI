package helps

import "bytes"

var (
	claudeToolUseMarker    = []byte(`"tool_use"`)
	claudeToolResultMarker = []byte(`"tool_result"`)
	claudeRoleMarker       = []byte(`"role"`)
	claudeSystemMarker     = []byte(`"system"`)
	claudeCacheMarker      = []byte(`"cache_control"`)
	claudeBetasMarker      = []byte(`"betas"`)
	claudeToolsMarker      = []byte(`"tools"`)
	claudeToolSearchMarker = []byte(`"tool_search"`)
	claudeToolSearchAlt    = []byte(`"tool-search"`)
)

// HasClaudeToolUseMarker reports whether a payload might contain a Claude
// tool_use block.
func HasClaudeToolUseMarker(body []byte) bool {
	return bytes.Contains(body, claudeToolUseMarker)
}

// HasClaudeToolUseOrResultMarkers reports whether a payload might contain Claude
// tool_use/tool_result blocks. It intentionally uses a cheap byte scan so
// callers can skip expensive JSON repair work on obviously unrelated requests.
func HasClaudeToolUseOrResultMarkers(body []byte) bool {
	return HasClaudeToolUseMarker(body) || bytes.Contains(body, claudeToolResultMarker)
}

// HasClaudeToolResultMarker reports whether a payload might contain a Claude
// tool_result block.
func HasClaudeToolResultMarker(body []byte) bool {
	return bytes.Contains(body, claudeToolResultMarker)
}

// HasClaudeSystemRoleMarker reports whether a payload might contain role=system
// messages that need Claude system-block normalization. It is intentionally a
// cheap preflight with acceptable false positives.
func HasClaudeSystemRoleMarker(body []byte) bool {
	return bytes.Contains(body, claudeRoleMarker) && bytes.Contains(body, claudeSystemMarker)
}

// HasClaudeCacheControlMarker reports whether a payload might contain cache_control.
func HasClaudeCacheControlMarker(body []byte) bool {
	return bytes.Contains(body, claudeCacheMarker)
}

// HasClaudeBetasMarker reports whether a payload might contain inline betas.
func HasClaudeBetasMarker(body []byte) bool {
	return bytes.Contains(body, claudeBetasMarker)
}

// HasClaudeToolsMarker reports whether a payload might contain top-level tools.
func HasClaudeToolsMarker(body []byte) bool {
	return bytes.Contains(body, claudeToolsMarker)
}

// HasClaudeToolSearchMarker reports whether a payload might contain Claude tool
// search/server-tool markers that require compatibility cleanup.
func HasClaudeToolSearchMarker(body []byte) bool {
	return bytes.Contains(body, claudeToolSearchMarker) || bytes.Contains(body, claudeToolSearchAlt)
}
