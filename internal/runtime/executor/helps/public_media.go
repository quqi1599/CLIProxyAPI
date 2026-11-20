package helps

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// NewPublicMediaTransport connects directly to a validated public address.
// The TLS handshake still verifies the original host name.
func NewPublicMediaTransport() *http.Transport {
	return proxyutil.ApplyHTTPTransportLimits(&http.Transport{
		DialContext: dialPublicMedia, ForceAttemptHTTP2: true, DisableCompression: true,
	})
}

var globalIPv6MediaPrefix = netip.MustParsePrefix("2000::/3")

var blockedMediaPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func PublicMediaURLAllowed(parsed *url.URL) bool {
	if parsed == nil {
		return false
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" {
		return false
	}
	if parsed.User != nil {
		return false
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return false
	}
	lowerHost := strings.ToLower(strings.TrimSuffix(host, "."))
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") || strings.HasSuffix(lowerHost, ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return publicMediaIPAllowed(ip)
	}
	return true
}

func dialPublicMedia(ctx context.Context, network string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("media host resolved to no addresses")
	}
	for _, candidate := range ips {
		if !publicMediaIPAllowed(candidate.IP) {
			return nil, fmt.Errorf("blocked media host address")
		}
	}

	var dialer net.Dialer
	var lastErr error
	for _, candidate := range ips {
		conn, errDial := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if errDial == nil {
			return conn, nil
		}
		lastErr = errDial
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("media host dial failed")
}

func publicMediaIPAllowed(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	// Public IPv6 media hosts use global unicast space. Exclude translation
	// prefixes that could reach a different address family after validation.
	if addr.Is6() && !globalIPv6MediaPrefix.Contains(addr) {
		return false
	}
	for _, prefix := range blockedMediaPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
