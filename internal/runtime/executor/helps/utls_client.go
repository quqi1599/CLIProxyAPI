package helps

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/transport/http2pool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using utls with Chrome fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
type utlsRoundTripper struct {
	dialer proxy.Dialer
	pool   *http2pool.Pool
}

func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	rt := &utlsRoundTripper{dialer: dialer}
	rt.pool = http2pool.New(http2pool.DefaultMaxConnsPerHost, rt.createConnection)
	return rt
}

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

	tr := &http2.Transport{DisableCompression: true}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	return h2Conn, nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

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

// utlsProtectedHosts contains the hosts that should use utls Chrome TLS fingerprint
// to bypass Cloudflare's TLS fingerprinting.
var utlsProtectedHosts = map[string]struct{}{
	"api.anthropic.com": {},
	"chatgpt.com":       {},
}

// fallbackRoundTripper uses utls for protected HTTPS hosts and falls back to
// standard transport for all other requests.
type fallbackRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		if _, ok := utlsProtectedHosts[strings.ToLower(req.URL.Hostname())]; ok {
			return f.utls.RoundTrip(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

// utlsClientCache reuses utls-backed *http.Client instances across requests,
// keyed by proxyURL. A fresh client per request would force a new TLS handshake
// and HTTP/2 connection setup every time; the underlying utlsRoundTripper is
// already concurrency-safe and designed for connection reuse, so sharing the
// client process-wide removes the per-request handshake cost. Only timeout==0
// clients without context-injected transports are cached (see NewUtlsHTTPClient).
var utlsClientCache sync.Map

// buildUtlsHTTPClient assembles a utls-backed HTTP client for the given proxyURL.
// The dialer, fallback transport, and fallbackRoundTripper are wired here so that
// both the cached and uncached paths share identical construction logic.
func buildUtlsHTTPClient(proxyURL string, ctxRoundTripper http.RoundTripper) *http.Client {
	var utlsRT http.RoundTripper = newUtlsRoundTripper(proxyURL)

	var standardTransport http.RoundTripper = &http.Transport{
		DisableCompression: true,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			standardTransport = transport
		}
	} else if ctxRoundTripper != nil {
		utlsRT = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}

	return &http.Client{
		Transport: &fallbackRoundTripper{
			utls:     utlsRT,
			fallback: standardTransport,
		},
	}
}

// NewUtlsHTTPClient creates an HTTP client using utls Chrome TLS fingerprint.
// Use this for Claude API requests to match real Claude Code's TLS behavior.
// Falls back to standard transport for non-HTTPS requests.
//
// When timeout == 0 the client is reused process-wide (keyed by proxyURL) unless
// a context-injected RoundTripper is present. When timeout > 0 a dedicated client
// is built and not cached, preserving the per-request timeout semantics.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var ctxRoundTripper http.RoundTripper
	if proxyURL == "" && ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	if timeout > 0 {
		client := buildUtlsHTTPClient(proxyURL, ctxRoundTripper)
		client.Timeout = timeout
		return client
	}
	if ctxRoundTripper != nil {
		return buildUtlsHTTPClient(proxyURL, ctxRoundTripper)
	}

	if cached, ok := utlsClientCache.Load(proxyURL); ok {
		if client, okCast := cached.(*http.Client); okCast && client != nil {
			return client
		}
	}
	client := buildUtlsHTTPClient(proxyURL, nil)
	actual, _ := utlsClientCache.LoadOrStore(proxyURL, client)
	if cached, ok := actual.(*http.Client); ok && cached != nil {
		return cached
	}
	return client
}
