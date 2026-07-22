package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
)

func TestOAuthCallbackRoutesShareBehavior(t *testing.T) {
	server := newTestServer(t)
	tests := []struct {
		name       string
		path       string
		storageKey string
		query      string
		wantCode   string
		wantError  string
	}{
		{
			name:       "anthropic authorization code",
			path:       "/anthropic/callback",
			storageKey: "anthropic",
			query:      "code=anthropic-code",
			wantCode:   "anthropic-code",
		},
		{
			name:       "codex error",
			path:       "/codex/callback",
			storageKey: "codex",
			query:      "error=access_denied",
			wantError:  "access_denied",
		},
		{
			name:       "antigravity error description fallback",
			path:       "/antigravity/callback",
			storageKey: "antigravity",
			query:      "error_description=consent+required",
			wantError:  "consent required",
		},
		{
			name:       "xai error takes precedence",
			path:       "/xai/callback",
			storageKey: "xai",
			query:      "error=primary&error_description=secondary",
			wantError:  "primary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := tt.storageKey + "-generic-callback-state"
			managementHandlers.RegisterOAuthSession(state, tt.storageKey)
			defer managementHandlers.CompleteOAuthSession(state)

			req := httptest.NewRequest(http.MethodGet, tt.path+"?state="+state+"&"+tt.query, nil)
			recorder := httptest.NewRecorder()
			server.engine.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
			}
			if got := recorder.Body.String(); got != oauthCallbackSuccessHTML {
				t.Fatalf("body = %q, want success HTML", got)
			}

			callbackPath := filepath.Join(server.authDirSnapshot(), ".oauth-"+tt.storageKey+"-"+state+".oauth")
			data, errRead := os.ReadFile(callbackPath)
			if errRead != nil {
				t.Fatalf("read callback file: %v", errRead)
			}
			var payload struct {
				Code  string `json:"code"`
				State string `json:"state"`
				Error string `json:"error"`
			}
			if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
				t.Fatalf("decode callback payload: %v", errUnmarshal)
			}
			if payload.Code != tt.wantCode || payload.State != state || payload.Error != tt.wantError {
				t.Fatalf("payload = %+v, want code=%q state=%q error=%q", payload, tt.wantCode, state, tt.wantError)
			}
		})
	}
}

func TestOAuthCallbackWithoutStateDoesNotPersist(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/codex/callback?code=unused", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	entries, errRead := os.ReadDir(server.authDirSnapshot())
	if errRead != nil {
		t.Fatalf("read auth dir: %v", errRead)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".oauth-") {
			t.Fatalf("unexpected callback file %q", entry.Name())
		}
	}
}
