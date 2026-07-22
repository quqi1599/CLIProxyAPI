package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexRawAlphaSearchOwnsProviderExchange(t *testing.T) {
	var requestBody string
	var requestHeaders http.Header
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestPath = req.URL.Path
		requestHeaders = req.Header.Clone()
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
			return
		}
		requestBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(server.Close)

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "codex-oauth",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey: "oauth-token",
			"base_url":                   server.URL,
		},
		Metadata: map[string]any{"account_id": "account-1"},
	}
	response, err := executor.ExecuteRawEndpoint(context.Background(), auth, cliproxyexecutor.RawEndpointRequest{
		Endpoint: cliproxyexecutor.CodexAlphaSearchEndpoint,
		Body:     []byte(`{"commands":{},"prompt_cache_key":"cache","prompt_cache_retention":"24h"}`),
		Headers: http.Header{
			"Version":             []string{"1.2.3"},
			"User-Agent":          []string{"codex-test"},
			"Session_id":          []string{"session-1"},
			"X-Client-Request-Id": []string{"request-1"},
		},
	})
	if err != nil {
		t.Fatalf("execute raw endpoint: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"results":[]}` {
		t.Fatalf("response = status %d body %s", response.StatusCode, response.Body)
	}
	if requestPath != "/alpha/search" {
		t.Fatalf("path = %q, want /alpha/search", requestPath)
	}
	if strings.Contains(requestBody, "prompt_cache") || !strings.Contains(requestBody, "commands") {
		t.Fatalf("unexpected upstream body: %s", requestBody)
	}
	for name, want := range map[string]string{
		"Authorization":       "Bearer oauth-token",
		"Originator":          "codex_cli_rs",
		"Version":             "1.2.3",
		"User-Agent":          "codex-test",
		"Session_id":          "session-1",
		"X-Client-Request-Id": "request-1",
		"Chatgpt-Account-Id":  "account-1",
	} {
		if got := requestHeaders.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}
