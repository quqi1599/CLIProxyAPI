package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexNativeResponsesManagementRoundTrip(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("codex-api-key: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(managementTestExecutor{id: "codex"})
	h := NewHandler(cfg, configPath, manager)
	authID, _ := synthesizer.NewStableIDGenerator().Next("codex:apikey", "test-key", "https://example.test/v1")

	request := func(method, query, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(method, "/v0/management/codex-api-key"+query, strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		switch method {
		case http.MethodGet:
			h.GetCodexKeys(ctx)
		case http.MethodPut:
			h.PutCodexKeys(ctx)
		case http.MethodPatch:
			h.PatchCodexKey(ctx)
		}
		return rec
	}
	for _, step := range []struct {
		name, method, query, body, want string
		status                          int
	}{
		{"create unspecified", http.MethodPut, "", `[{"api-key":"test-key","base-url":"https://example.test/v1"}]`, "", 200},
		{"patch true", http.MethodPatch, "", `{"index":0,"value":{"native-responses":true}}`, "true", 200},
		{"old client put preserves declaration", http.MethodPut, "", `[{"api-key":"test-key","base-url":"https://example.test/v1"}]`, "true", 200},
		{"patch false", http.MethodPatch, "", `{"index":0,"value":{"native-responses":false}}`, "false", 200},
		{"unrelated patch preserves false", http.MethodPatch, "", `{"index":0,"value":{"priority":2}}`, "false", 200},
		{"old client put preserves false", http.MethodPut, "", `[{"api-key":"test-key","base-url":"https://example.test/v1"}]`, "false", 200},
		{"invalid patch does not change capability", http.MethodPatch, "", `{"index":0,"value":{"native-responses":"true"}}`, "false", 400},
		{"clear declaration", http.MethodPatch, "", `{"index":0,"value":{"native-responses":null}}`, "", 200},
		{"put true", http.MethodPut, "", `[{"api-key":"test-key","base-url":"https://example.test/v1","native-responses":true}]`, "true", 200},
		{"put false", http.MethodPut, "", `[{"api-key":"test-key","base-url":"https://example.test/v1","native-responses":false}]`, "false", 200},
		{"full replace clears declaration", http.MethodPut, "?replace=true", `[{"api-key":"test-key","base-url":"https://example.test/v1"}]`, "", 200},
	} {
		t.Run(step.name, func(t *testing.T) {
			rec := request(step.method, step.query, step.body)
			if rec.Code != step.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, step.status, rec.Body.String())
			}
			auth, ok := manager.GetByID(authID)
			if !ok || auth == nil {
				t.Fatal("runtime auth missing")
			}
			if got, present := auth.Attributes["native_responses"]; got != step.want || present != (step.want != "") {
				t.Fatalf("runtime capability=%q present=%v, want=%q", got, present, step.want)
			}
			saved, err := config.LoadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			check := func(value *bool) {
				t.Helper()
				if step.want == "" {
					if value != nil {
						t.Fatal("expected unspecified capability")
					}
				} else if value == nil || *value != (step.want == "true") {
					t.Fatalf("capability=%v, want=%q", value, step.want)
				}
			}
			check(saved.CodexKey[0].NativeResponses)
			get := request(http.MethodGet, "", "")
			var response struct {
				Keys []config.CodexKey `json:"codex-api-key"`
			}
			if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil || len(response.Keys) != 1 {
				t.Fatalf("invalid GET response: %s error=%v", get.Body.String(), err)
			}
			check(response.Keys[0].NativeResponses)
		})
	}
}

func TestMergeCodexKeysPreservesNativeResponses(t *testing.T) {
	for _, merge := range []func([]config.CodexKey, []config.CodexKey) []config.CodexKey{
		mergeCodexKeysPreservingMissing, mergeCodexKeyFields,
	} {
		for _, capability := range []bool{true, false} {
			existing := []config.CodexKey{{APIKey: "test-key", BaseURL: "https://example.test/v1", NativeResponses: &capability}}
			incoming := []config.CodexKey{{APIKey: "test-key", BaseURL: "https://example.test/v1"}}
			merged := merge(existing, incoming)
			if len(merged) != 1 || merged[0].NativeResponses == nil || *merged[0].NativeResponses != capability {
				t.Fatal("omitted capability was not preserved")
			}
			if merged[0].NativeResponses == existing[0].NativeResponses {
				t.Fatal("merged capability shares mutable pointer with existing config")
			}
			inverse := !capability
			incoming[0].NativeResponses = &inverse
			merged = merge(existing, incoming)
			if merged[0].NativeResponses == nil || *merged[0].NativeResponses != inverse {
				t.Fatal("explicit capability was not applied")
			}
		}
	}
}
