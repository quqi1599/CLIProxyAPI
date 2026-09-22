package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentaudit"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func auditModeRequest(h *Handler, endpoint, body, version string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPatch, "/content-audit/"+endpoint, strings.NewReader(body))
	if version != "" {
		c.Request.Header.Set("If-Match", version)
	}
	if endpoint == "enabled" {
		h.PutContentAuditEnabled(c)
	} else {
		h.PutContentAuditMode(c)
	}
	return recorder
}

func TestContentAuditInvalidModesAndMissingConfiguration(t *testing.T) {
	for _, body := range []string{`{}`, `{"value":null}`, `{"value":true}`, `{"value":"invalid"}`, `{`} {
		h := &Handler{cfg: &config.Config{ContentAudit: config.ContentAuditConfig{Mode: "strict", Enabled: true}}}
		if response := auditModeRequest(h, "mode", body, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid mode %s returned %d", body, response.Code)
		}
		if h.cfg.ContentAudit.Mode != "strict" {
			t.Fatal("invalid input changed mode")
		}
	}
	for _, h := range []*Handler{nil, {}} {
		for _, endpoint := range []string{"mode", "enabled"} {
			body := `{"value":"off"}`
			if endpoint == "enabled" {
				body = `{"value":false}`
			}
			if response := auditModeRequest(h, endpoint, body, ""); response.Code != http.StatusServiceUnavailable {
				t.Fatalf("missing config %s returned %d", endpoint, response.Code)
			}
		}
	}
}

func TestContentAuditModeConflictKeepsDiskState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const disk = "content-audit:\n  enabled: true\n  mode: simple\n"
	if err := os.WriteFile(path, []byte(disk), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{ContentAudit: config.ContentAuditConfig{Mode: "strict", Enabled: true}}, configFilePath: path}
	if response := auditModeRequest(h, "mode", `{"value":"off"}`, "stale-version"); response.Code != http.StatusConflict {
		t.Fatalf("stale write = %d, want conflict", response.Code)
	}
	if h.cfg.ContentAudit.Mode != "simple" {
		t.Fatal("conflict rolled back the newer disk state")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != disk {
		t.Fatal("conflict changed configuration file")
	}
}

func TestContentAuditModeReadbackUsesEffectiveState(t *testing.T) {
	for _, cfg := range []config.ContentAuditConfig{
		{Mode: "strict", Enabled: false},
		{Mode: "off", Enabled: true},
		{Mode: " SIMPLE ", Enabled: true},
		{Enabled: true, AuditOnly: true},
	} {
		h := &Handler{cfg: &config.Config{ContentAudit: cfg}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		h.GetContentAuditMode(c)
		var got struct {
			Mode      string `json:"mode"`
			AuditOnly bool   `json:"audit_only"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || got.Mode != contentaudit.EffectiveMode(cfg) || got.AuditOnly != cfg.AuditOnly {
			t.Fatalf("inconsistent mode readback: %s", recorder.Body.String())
		}
	}
}

func TestContentAuditModeReloadAndConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("content-audit:\n  enabled: false\n  mode: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := &config.Config{ContentAudit: config.ContentAuditConfig{Mode: "off"}}
	h := &Handler{cfg: original, configFilePath: path}
	// The hook runs out of the handler lock and receives an independent snapshot.
	reloaded := make(chan *config.Config, 16)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloaded <- cfg })
	for _, mode := range []string{"simple", "strict", "off"} {
		if response := auditModeRequest(h, "mode", `{"value":"`+mode+`"}`, ""); response.Code != http.StatusOK {
			t.Fatalf("save mode %s: %d", mode, response.Code)
		}
		select {
		case snapshot := <-reloaded:
			if snapshot.ContentAudit.Mode != mode || snapshot.ContentAudit.Enabled != (mode != "off") || snapshot.ContentAudit.AuditOnly {
				t.Fatalf("bad runtime reload snapshot: %#v", snapshot.ContentAudit)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("successful save did not schedule reload")
		}
	}
	h.SetConfigReloadHook(nil)
	var wg sync.WaitGroup
	for _, endpoint := range []string{"enabled", "mode"} {
		wg.Go(func() {
			for range 5 {
				body := `{"value":"simple"}`
				if endpoint == "enabled" {
					body = `{"value":false}`
				}
				if response := auditModeRequest(h, endpoint, body, ""); response.Code != http.StatusOK {
					t.Errorf("concurrent save: %d", response.Code)
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				h.GetContentAuditMode(c)
			}
		})
	}
	wg.Wait()
	if response := auditModeRequest(h, "mode", `{"value":"strict"}`, ""); response.Code != http.StatusOK {
		t.Fatal("final mode save failed")
	}
	saved, err := config.LoadConfig(path)
	if err != nil || saved.ContentAudit.Mode != "strict" || !saved.ContentAudit.Enabled {
		t.Fatalf("disk and memory diverged: %v", err)
	}
	if original.ContentAudit.Mode != "off" || original.ContentAudit.Enabled {
		t.Fatal("handler mutated shared original configuration")
	}
}

func TestContentAuditWriteFailurePreservesMemory(t *testing.T) {
	for _, endpoint := range []string{"mode", "enabled"} {
		t.Run(endpoint, func(t *testing.T) {
			// A directory is not a writable configuration file, even as root.
			original := &config.Config{ContentAudit: config.ContentAuditConfig{Enabled: true, Mode: "strict"}}
			h := NewHandler(original, t.TempDir(), nil)
			body := `{"value":"off"}`
			if endpoint == "enabled" {
				body = `{"value":false}`
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPatch, "/content-audit/"+endpoint, strings.NewReader(body))
			if endpoint == "mode" {
				h.PutContentAuditMode(c)
			} else {
				h.PutContentAuditEnabled(c)
			}
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want failed save", recorder.Code)
			}
			if !h.cfg.ContentAudit.Enabled || h.cfg.ContentAudit.Mode != "strict" || !original.ContentAudit.Enabled {
				t.Fatal("failed write changed in-memory or shared configuration")
			}
		})
	}
}

func TestContentAuditSerializationFailurePreservesMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// The precondition read succeeds, but the existing document cannot be merged.
	const invalidDocument = "content-audit: [unterminated"
	if err := os.WriteFile(path, []byte(invalidDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	original := &config.Config{ContentAudit: config.ContentAuditConfig{Enabled: true, Mode: "strict"}}
	h := &Handler{cfg: original, configFilePath: path}
	if response := auditModeRequest(h, "mode", `{"value":"off"}`, ""); response.Code != http.StatusInternalServerError {
		t.Fatalf("save malformed document status = %d", response.Code)
	}
	if h.cfg != original || !h.cfg.ContentAudit.Enabled {
		t.Fatal("failed serialization did not restore original configuration")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != invalidDocument {
		t.Fatal("failed serialization modified the file")
	}
}

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
