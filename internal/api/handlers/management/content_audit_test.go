package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestContentAuditEnabledSwitchPersists(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("content-audit:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	h := NewHandler(&config.Config{ContentAudit: config.ContentAuditConfig{Enabled: true}}, configPath, coreauth.NewManager(nil, nil, nil))

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	getContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/content-audit/enabled", nil)
	h.GetContentAuditEnabled(getContext)
	if getRecorder.Code != http.StatusOK || !strings.Contains(getRecorder.Body.String(), `"enabled":true`) {
		t.Fatalf("GET switch status/body = %d/%s", getRecorder.Code, getRecorder.Body.String())
	}

	patchRecorder := httptest.NewRecorder()
	patchContext, _ := gin.CreateTestContext(patchRecorder)
	patchContext.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/content-audit/enabled", strings.NewReader(`{"value":false}`))
	patchContext.Request.Header.Set("Content-Type", "application/json")
	h.PutContentAuditEnabled(patchContext)
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("PATCH switch status = %d, body = %s", patchRecorder.Code, patchRecorder.Body.String())
	}
	if h.cfg.ContentAudit.Enabled {
		t.Fatal("in-memory content audit switch remains enabled")
	}

	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}
	if !strings.Contains(string(saved), "enabled: false") {
		t.Fatalf("saved config = %q, want content-audit enabled false", string(saved))
	}
}

func TestContentAuditModeSwitchPersists(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("content-audit:\n  enabled: true\n  mode: strict\n"), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	h := NewHandler(&config.Config{ContentAudit: config.ContentAuditConfig{Enabled: true, Mode: "strict"}}, configPath, coreauth.NewManager(nil, nil, nil))

	patchRecorder := httptest.NewRecorder()
	patchContext, _ := gin.CreateTestContext(patchRecorder)
	patchContext.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/content-audit/mode", strings.NewReader(`{"value":"simple"}`))
	patchContext.Request.Header.Set("Content-Type", "application/json")
	h.PutContentAuditMode(patchContext)
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("PATCH mode status = %d, body = %s", patchRecorder.Code, patchRecorder.Body.String())
	}
	if got := h.cfg.ContentAudit.Mode; got != "simple" || !h.cfg.ContentAudit.Enabled || h.cfg.ContentAudit.AuditOnly {
		t.Fatalf("in-memory mode state = %#v, want simple/enabled/non-observe", h.cfg.ContentAudit)
	}

	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}
	if !strings.Contains(string(saved), "mode: simple") || !strings.Contains(string(saved), "enabled: true") {
		t.Fatalf("saved config = %q, want simple mode enabled", string(saved))
	}
}
