// Package claude provides authentication functionality for Anthropic's Claude API.
// This file implements a custom HTTP transport using utls to bypass TLS fingerprinting.
package claude

import (
	"net/http"
	"strings"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/transport/http2pool"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using utls with Chrome fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
type utlsRoundTripper struct {
	// dialer is used to create network connections, supporting proxies
	dialer proxy.Dialer
	pool   *http2pool.Pool
}

// newUtlsRoundTripper creates a new utls-based round tripper with optional proxy support
func newUtlsRoundTripper(cfg *config.SDKConfig) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if cfg != nil {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(cfg.ProxyURL)
		if errBuild != nil {
			log.Errorf("failed to configure proxy dialer for %q: %v", proxyutil.Redact(cfg.ProxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}

	rt := &utlsRoundTripper{dialer: dialer}
	rt.pool = http2pool.New(http2pool.DefaultMaxConnsPerHost, rt.createConnection)
	return rt
}

// createConnection creates a new HTTP/2 connection with Chrome TLS fingerprint.
// Chrome's TLS fingerprint is closer to Node.js/OpenSSL (which real Claude Code uses)
// than Firefox, reducing the mismatch between TLS layer and HTTP headers.
func (t *utlsRoundTripper) createConnection(host, addr string) (http2pool.ClientConn, error) {
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{ServerName: host}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloChrome_Auto)

	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}

	tr := &http2.Transport{}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	return h2Conn, nil
}

// RoundTrip implements http.RoundTripper
func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	addr := host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	// Get hostname without port for TLS ServerName
	hostname := req.URL.Hostname()

	h2Conn, err := t.pool.Get(hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		t.pool.Forget(hostname, h2Conn)
		return nil, err
	}

	return resp, nil
}

// NewAnthropicHttpClient creates an HTTP client that bypasses TLS fingerprinting
// for Anthropic domains by using utls with Chrome fingerprint.
// It accepts optional SDK configuration for proxy settings.
func NewAnthropicHttpClient(cfg *config.SDKConfig) *http.Client {
	return &http.Client{
		Transport: newUtlsRoundTripper(cfg),
	}
}
