package compat

import (
	"net/url"
	"strings"
)

// DeepSeekCodingResponsesUnsupported is shared by selection and execution so a
// known protocol-incompatible Coding endpoint need not consume a retry attempt.
// Standard Ark inference endpoints and unrelated hosts are deliberately excluded.
func DeepSeekCodingResponsesUnsupported(baseURL, endpoint string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".volces.com") {
		return false
	}
	p := strings.TrimRight(u.Path, "/")
	return (p == "/api/coding" || p == "/api/coding/v1") && (endpoint == "/responses" || endpoint == "/responses/compact")
}
