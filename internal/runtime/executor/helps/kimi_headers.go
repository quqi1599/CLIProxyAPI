package helps

import (
	"net/http"
	"net/url"
	"strings"
)

// IsKimiCodeEndpoint excludes Moonshot's separately authenticated open platform
// and third-party gateways, even if they advertise Kimi models.
func IsKimiCodeEndpoint(endpoint *url.URL) bool {
	if endpoint == nil || endpoint.Scheme != "https" {
		return false
	}
	host := strings.ToLower(endpoint.Hostname())
	if host != "api.kimi.com" && host != "api.kimi.ai" {
		return false
	}
	return endpoint.Path == "/coding" || strings.HasPrefix(endpoint.Path, "/coding/")
}

// ApplyKimiCodeClientIdentity forwards the caller's real software identity. It
// does not synthesize an official CLI version, device ID or OAuth fingerprint.
// Explicit administrator header overrides are applied by the caller afterward.
func ApplyKimiCodeClientIdentity(req *http.Request, userAgent string) {
	if req == nil || !IsKimiCodeEndpoint(req.URL) {
		return
	}
	if strings.ContainsAny(userAgent, "\r\n\x00") || len(userAgent) > 512 {
		userAgent = ""
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = "CLIProxyAPI"
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("User-Agent", userAgent)
}
