package bootstrap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testConfigPersister struct {
	calls int
}

func (p *testConfigPersister) PersistConfig(context.Context) error {
	p.calls++
	return nil
}

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (f testRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestResolveObjectEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantSSL bool
		wantErr bool
	}{
		{name: "bare endpoint", raw: "s3.example.com/", want: "s3.example.com", wantSSL: true},
		{name: "http endpoint", raw: "http://s3.example.com/root/", want: "s3.example.com/root", wantSSL: false},
		{name: "https endpoint", raw: "https://s3.example.com", want: "s3.example.com", wantSSL: true},
		{name: "unsupported scheme", raw: "ftp://s3.example.com", wantErr: true},
		{name: "missing host", raw: "https:///root", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint, useSSL, errResolve := resolveObjectEndpoint(tt.raw)
			if (errResolve != nil) != tt.wantErr {
				t.Fatalf("resolveObjectEndpoint() error = %v, wantErr %t", errResolve, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if endpoint != tt.want || useSSL != tt.wantSSL {
				t.Fatalf("resolveObjectEndpoint() = (%q, %t), want (%q, %t)", endpoint, useSSL, tt.want, tt.wantSSL)
			}
		})
	}
}

func TestBootstrapGitBackedConfigUsesRemoteTemplateWhenLocalTemplateMissing(t *testing.T) {
	const fallbackURL = "https://raw.githubusercontent.com/caidaoli/CLIProxyAPI/refs/heads/main/config.example.yaml"
	const remoteConfig = "port: 8317\n"

	oldTransport := http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
	})

	requests := 0
	http.DefaultTransport = testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.String() != fallbackURL {
			return nil, fmt.Errorf("unexpected fallback URL: %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(remoteConfig)),
			Request:    req,
		}, nil
	})

	tmpDir := t.TempDir()
	persister := &testConfigPersister{}
	configPath := filepath.Join(tmpDir, "gitstore", "config", "config.yaml")
	examplePath := filepath.Join(tmpDir, "config.example.yaml")

	if errBootstrap := bootstrapGitBackedConfig(context.Background(), examplePath, configPath, persister); errBootstrap != nil {
		t.Fatalf("bootstrapGitBackedConfig() error = %v", errBootstrap)
	}
	if requests != 1 {
		t.Fatalf("fallback request count = %d, want 1", requests)
	}
	if persister.calls != 1 {
		t.Fatalf("PersistConfig calls = %d, want 1", persister.calls)
	}
	got, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read config: %v", errRead)
	}
	if string(got) != remoteConfig {
		t.Fatalf("config content = %q, want %q", string(got), remoteConfig)
	}
}

func TestResolveLegacyAuthDirUsesConfiguredDirectory(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "custom-auth")
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("auth-dir: \""+authDir+"\"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	if got := resolveLegacyAuthDir(configPath); got != authDir {
		t.Fatalf("resolveLegacyAuthDir() = %q, want %q", got, authDir)
	}
}
