package handlers

import (
	"net/http"
	"testing"
)

func TestFilterUpstreamHeaders_RemovesConnectionScopedHeaders(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "keep-alive, x-hop-a, x-hop-b")
	src.Add("Connection", "x-hop-c")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("X-Hop-A", "a")
	src.Set("X-Hop-B", "b")
	src.Set("X-Hop-C", "c")
	src.Set("X-Request-Id", "req-1")
	src.Set("Set-Cookie", "session=secret")

	filtered := FilterUpstreamHeaders(src)
	if filtered == nil {
		t.Fatalf("expected filtered headers, got nil")
	}

	requestID := filtered.Get("X-Request-Id")
	if requestID != "req-1" {
		t.Fatalf("expected X-Request-Id to be preserved, got %q", requestID)
	}

	blockedHeaderKeys := []string{
		"Connection",
		"Keep-Alive",
		"X-Hop-A",
		"X-Hop-B",
		"X-Hop-C",
		"Set-Cookie",
	}
	for _, key := range blockedHeaderKeys {
		value := filtered.Get(key)
		if value != "" {
			t.Fatalf("expected %s to be removed, got %q", key, value)
		}
	}
}

func TestFilterUpstreamHeaders_ReturnsNilWhenAllHeadersBlocked(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "x-hop-a")
	src.Set("X-Hop-A", "a")
	src.Set("Set-Cookie", "session=secret")

	filtered := FilterUpstreamHeaders(src)
	if filtered != nil {
		t.Fatalf("expected nil when all headers are filtered, got %#v", filtered)
	}
}

func TestWriteErrorAddonHeadersFiltersSensitiveHeadersWithPassthrough(t *testing.T) {
	dst := http.Header{}
	addon := http.Header{
		"Connection":        {"keep-alive, X-Hop"},
		"X-Hop":             {"private"},
		"Transfer-Encoding": {"chunked"},
		"Content-Length":    {"42"},
		"Set-Cookie":        {"session=secret"},
		"Retry-After":       {"2"},
		"X-Request-Id":      {"req-1"},
	}

	WriteErrorAddonHeaders(dst, addon, true)
	if got := dst.Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if got := dst.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("X-Request-Id = %q, want req-1", got)
	}
	for _, key := range []string{"Connection", "X-Hop", "Transfer-Encoding", "Content-Length", "Set-Cookie"} {
		if got := dst.Get(key); got != "" {
			t.Fatalf("%s leaked through error addon filtering: %q", key, got)
		}
	}
}

func TestFilterErrorAddonHeadersAppliesPassthroughPolicyAfterSafetyFilter(t *testing.T) {
	addon := http.Header{
		"Retry-After":  {"2"},
		"X-Request-Id": {"req-1"},
		"Set-Cookie":   {"session=secret"},
	}

	defaultHeaders := FilterErrorAddonHeaders(addon, false)
	if got := defaultHeaders.Get("Retry-After"); got != "2" {
		t.Fatalf("default Retry-After = %q, want 2", got)
	}
	if got := defaultHeaders.Get("X-Request-Id"); got != "" {
		t.Fatalf("default X-Request-Id = %q, want empty", got)
	}

	passthroughHeaders := FilterErrorAddonHeaders(addon, true)
	if got := passthroughHeaders.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("passthrough X-Request-Id = %q, want req-1", got)
	}
	if got := passthroughHeaders.Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie leaked through safety filter: %q", got)
	}
	if got := addon.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("source headers were mutated: X-Request-Id = %q", got)
	}
}
