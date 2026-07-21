package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	internalusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type codexAlphaSearchTestExecutor struct {
	body    []byte
	authIDs []string
}

func (e *codexAlphaSearchTestExecutor) Identifier() string { return "codex" }

func (e *codexAlphaSearchTestExecutor) Execute(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *codexAlphaSearchTestExecutor) ExecuteStream(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *codexAlphaSearchTestExecutor) Refresh(_ context.Context, candidate *auth.Auth) (*auth.Auth, error) {
	return candidate, nil
}

func (e *codexAlphaSearchTestExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *codexAlphaSearchTestExecutor) PrepareRequest(req *http.Request, candidate *auth.Auth) error {
	token, _ := candidate.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (e *codexAlphaSearchTestExecutor) HttpRequest(_ context.Context, candidate *auth.Auth, req *http.Request) (*http.Response, error) {
	e.authIDs = append(e.authIDs, candidate.ID)
	body, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		return nil, errRead
	}
	e.body = body
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"results":[]}`)),
	}, nil
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithOptions(t)
}

func newTestServerWithOptions(t *testing.T, opts ...ServerOption) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
		RemoteManagement:       proxyconfig.RemoteManagement{DisableControlPanel: true},
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath, opts...)
}

func TestCodexAlphaSearchUsesOAuthAndSanitizesBody(t *testing.T) {
	server := newTestServer(t)
	executor := &codexAlphaSearchTestExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	for _, candidate := range []*auth.Auth{
		{ID: "codex-api-key", Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{auth.AttributeAPIKey: "test-key"}},
		{ID: "codex-oauth", Provider: "codex", Status: auth.StatusActive, Metadata: map[string]any{"access_token": "oauth-token"}},
	} {
		if _, errRegister := server.handlers.AuthManager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("register %s: %v", candidate.ID, errRegister)
		}
	}

	payload := `{"id":"session-123","commands":{"search_query":[{"q":"golang"}]},"prompt_cache_key":"cache","prompt_cache_retention":"24h"}`
	for _, path := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		t.Run(path, func(t *testing.T) {
			executor.authIDs = nil
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
			req.Header.Set("Authorization", "Bearer test-key")
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
			}
			if len(executor.authIDs) != 1 || executor.authIDs[0] != "codex-oauth" {
				t.Fatalf("selected auth IDs = %v, want [codex-oauth]", executor.authIDs)
			}
			upstreamBody := string(executor.body)
			if strings.Contains(upstreamBody, "prompt_cache") || !strings.Contains(upstreamBody, "commands") {
				t.Fatalf("unexpected upstream body: %s", upstreamBody)
			}
		})
	}
}

func TestInjectManagementConfigVersionGuard(t *testing.T) {
	html := []byte("<html><body><main>ok</main></body></html>")

	out := injectManagementConfigVersionGuard(html, "sha256:test-version")

	if !strings.Contains(string(out), "cliproxy-config-version-guard") {
		t.Fatalf("expected injected guard script, got %s", string(out))
	}
	if !strings.Contains(string(out), `var latestConfigVersion = "sha256:test-version"`) {
		t.Fatalf("expected initial config version in guard script: %s", string(out))
	}
	if strings.Index(string(out), "cliproxy-config-version-guard") > strings.Index(string(out), "</body>") {
		t.Fatalf("guard script should be injected before closing body: %s", string(out))
	}
	again := injectManagementConfigVersionGuard(out, "sha256:test-version")
	if string(again) != string(out) {
		t.Fatalf("guard injection should be idempotent")
	}
}

func TestInjectManagementConfigVersionGuardReplacesOldGuard(t *testing.T) {
	html := []byte(`<html><body><script id="cliproxy-config-version-guard">old guard</script><main>ok</main></body></html>`)

	out := injectManagementConfigVersionGuard(html)
	body := string(out)

	if strings.Contains(body, "old guard") {
		t.Fatalf("expected old guard script to be replaced: %s", body)
	}
	if strings.Count(body, "cliproxy-config-version-guard") != 1 {
		t.Fatalf("expected exactly one guard script, got %s", body)
	}
	if !strings.Contains(body, "writeQueue") {
		t.Fatalf("expected serialized write queue in guard script: %s", body)
	}
	if !strings.Contains(body, "cliproxy:config-conflict") {
		t.Fatalf("expected conflict event publishing in guard script: %s", body)
	}
	if !strings.Contains(body, "response && response.status === 409") {
		t.Fatalf("expected guard to avoid promoting conflict versions to writable versions: %s", body)
	}
	if !strings.Contains(body, "detachAPICallAbortSignal") {
		t.Fatalf("expected api-call abort signal detachment in guard script: %s", body)
	}
	if !strings.Contains(body, `path === "/v0/management/api-call"`) {
		t.Fatalf("expected api-call queue detection in guard script: %s", body)
	}
	if !strings.Contains(body, "__cliproxySequentialMap") {
		t.Fatalf("expected sequential key test helper in guard script: %s", body)
	}
}

func TestInjectManagementConfigVersionGuardSerializesKeyTestBatches(t *testing.T) {
	html := []byte(`<html><body><script type="module">
let a=(await Promise.all(t.map(e=>I(e)))).filter(Boolean).length;
let b=(await Promise.all(t.map(e=>we(e)))).filter(Boolean).length;
let c=(await Promise.all(t.map(e=>P(e)))).filter(Boolean).length;
let d=(await Promise.all(t.map(e=>De(e)))).filter(Boolean).length;
let f=(await Promise.all(t.map(e=>N(e)))).filter(Boolean).length;
</script></body></html>`)

	out := injectManagementConfigVersionGuard(html)
	body := string(out)

	for _, fragment := range []string{
		`Promise.all(t.map(e=>I(e)))`,
		`Promise.all(t.map(e=>we(e)))`,
		`Promise.all(t.map(e=>P(e)))`,
		`Promise.all(t.map(e=>De(e)))`,
		`Promise.all(t.map(e=>N(e)))`,
	} {
		if strings.Contains(body, fragment) {
			t.Fatalf("expected parallel key test fragment %q to be patched: %s", fragment, body)
		}
	}
	for _, fragment := range []string{
		`window.__cliproxySequentialMap(t,e=>I(e))`,
		`window.__cliproxySequentialMap(t,e=>we(e))`,
		`window.__cliproxySequentialMap(t,e=>P(e))`,
		`window.__cliproxySequentialMap(t,e=>De(e))`,
		`window.__cliproxySequentialMap(t,e=>N(e))`,
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("expected sequential key test fragment %q in patched body: %s", fragment, body)
		}
	}
}

func TestInjectManagementConfigVersionGuardKeepsDisabledKeysInSameGroup(t *testing.T) {
	html := []byte(`<html><body><script type="module">
const jg=(e,t)=>JSON.stringify({provider:e,baseUrl:Tg(e,t.baseUrl),excludedModels:kg(t.excludedModels),cloak:e==="claude"?Ag(t.cloak):null});
</script></body></html>`)

	out := injectManagementConfigVersionGuard(html)
	body := string(out)

	if strings.Contains(body, `excludedModels:kg(t.excludedModels)`) {
		t.Fatalf("expected disabled-key grouping signature to be patched: %s", body)
	}
	if !strings.Contains(body, `excludedModels:kg(Lp(t.excludedModels))`) {
		t.Fatalf("expected grouping signature to ignore disable-all sentinel: %s", body)
	}

	again := injectManagementConfigVersionGuard(out)
	if strings.Count(string(again), `excludedModels:kg(Lp(t.excludedModels))`) != 1 {
		t.Fatalf("expected grouping patch to be idempotent: %s", string(again))
	}
}

func TestInjectManagementConfigVersionGuardPatchesAuthIndexStats(t *testing.T) {
	html := []byte(`<html><body><script type="module">` +
		string(managementPanelAPIKeyAuthIndexNeedle) +
		string(managementPanelOpenAIKeyAuthIndexNeedle) +
		string(managementPanelOpenAIProviderAuthIndexNeedle) +
		string(managementPanelAIProviderStatsNeedle) +
		string(managementPanelAIProviderStatusBlocksNeedle) +
		string(managementPanelGroupedProviderStatsNeedle) +
		string(managementPanelOpenAIProviderStatsNeedle) +
		string(managementPanelVertexProviderStatsNeedle) +
		`</script></body></html>`)

	out := injectManagementConfigVersionGuard(html)
	body := string(out)

	for _, oldFragment := range []string{
		string(managementPanelAPIKeyAuthIndexNeedle),
		string(managementPanelOpenAIKeyAuthIndexNeedle),
		string(managementPanelOpenAIProviderAuthIndexNeedle),
		string(managementPanelAIProviderStatsNeedle),
		string(managementPanelAIProviderStatusBlocksNeedle),
		string(managementPanelGroupedProviderStatsNeedle),
		string(managementPanelOpenAIProviderStatsNeedle),
		string(managementPanelVertexProviderStatsNeedle),
	} {
		if strings.Contains(body, oldFragment) {
			t.Fatalf("expected auth-index stats fragment to be patched, still found %q in %s", oldFragment, body)
		}
	}
	for _, expected := range []string{
		"i.authIndex=h",
		"authIndex:Ud",
		"t.byAuthIndex",
		"o.set(e,$p(t.blocks",
		"wm(n.apiKey,t,n.prefix,n.authIndex??n.auth_index)",
		"e?.authIndex??e?.auth_index",
		"wm(e.apiKey,t,e.prefix,e.authIndex??e.auth_index)",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected patched auth-index stats fragment %q in %s", expected, body)
		}
	}
}

func TestUsagePersistenceEnabledHotReload(t *testing.T) {
	t.Setenv("PGSTORE_DSN", "")
	t.Setenv("pgstore_dsn", "")
	t.Setenv("PGSTORE_SCHEMA", "")
	t.Setenv("pgstore_schema", "")

	internalusage.CloseDatabasePlugin()
	defer internalusage.CloseDatabasePlugin()

	server := newTestServer(t)

	disabled := *server.cfg
	disabled.UsagePersistenceEnabled = false
	server.UpdateClients(&disabled)
	if internalusage.GetDatabasePlugin() != nil {
		t.Fatalf("expected database plugin to be nil when disabled")
	}

	enabled := disabled
	enabled.UsagePersistenceEnabled = true
	server.UpdateClients(&enabled)
	firstPlugin := internalusage.GetDatabasePlugin()
	if firstPlugin == nil {
		t.Fatalf("expected database plugin to be initialized when enabled")
	}
	if _, err := os.Stat(filepath.Join(enabled.AuthDir, "usage.db")); err != nil {
		t.Fatalf("expected sqlite usage db to exist: %v", err)
	}

	disabledAgain := enabled
	disabledAgain.UsagePersistenceEnabled = false
	server.UpdateClients(&disabledAgain)
	if internalusage.GetDatabasePlugin() != nil {
		t.Fatalf("expected database plugin to be nil after disabling")
	}

	enabledAgain := disabledAgain
	enabledAgain.UsagePersistenceEnabled = true
	server.UpdateClients(&enabledAgain)
	secondPlugin := internalusage.GetDatabasePlugin()
	if secondPlugin == nil {
		t.Fatalf("expected database plugin to be initialized after re-enabling")
	}
	if secondPlugin == firstPlugin {
		t.Fatalf("expected database plugin to be re-initialized after re-enabling")
	}
}

func TestHealthz(t *testing.T) {
	server := newTestServer(t)

	t.Run("GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}

		var resp struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
		}
		if resp.Status != "ok" {
			t.Fatalf("unexpected response status: got %q want %q", resp.Status, "ok")
		}
	})

	t.Run("HEAD", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("expected empty body for HEAD request, got %q", rr.Body.String())
		}
	})
}

func TestHealthEndpointsTrackReadiness(t *testing.T) {
	server := newTestServer(t)

	assertHealthStatus := func(path string, wantCode int, wantStatus string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != wantCode {
			t.Fatalf("%s status code = %d, want %d; body=%s", path, rr.Code, wantCode, rr.Body.String())
		}
		var response struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("parse %s response: %v; body=%s", path, err, rr.Body.String())
		}
		if response.Status != wantStatus {
			t.Fatalf("%s response status = %q, want %q", path, response.Status, wantStatus)
		}
	}

	assertHealthStatus("/livez", http.StatusOK, "ok")
	assertHealthStatus("/healthz", http.StatusOK, "ok")
	assertHealthStatus("/readyz", http.StatusServiceUnavailable, "not_ready")

	startErr := make(chan error, 1)
	go func() {
		startErr <- server.Start()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !server.ready.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !server.ready.Load() {
		t.Fatal("server did not become ready")
	}
	assertHealthStatus("/readyz", http.StatusOK, "ready")

	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := <-startErr; err != nil {
		t.Fatalf("Start() error after Stop = %v", err)
	}
	assertHealthStatus("/readyz", http.StatusServiceUnavailable, "not_ready")
	assertHealthStatus("/livez", http.StatusOK, "ok")
}

func TestManagementResponseExposesPluginSupportHeaderForCORS(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	req.Header.Set("Origin", "http://127.0.0.1:5173")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if got := rr.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
		t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
	}

	exposedHeaders := make(map[string]struct{})
	for _, headerName := range strings.Split(rr.Header().Get("Access-Control-Expose-Headers"), ",") {
		headerName = strings.ToLower(strings.TrimSpace(headerName))
		if headerName != "" {
			exposedHeaders[headerName] = struct{}{}
		}
	}
	for _, headerName := range corsExposedResponseHeaders {
		if _, ok := exposedHeaders[strings.ToLower(headerName)]; !ok {
			t.Fatalf("Access-Control-Expose-Headers missing %s: %q", headerName, rr.Header().Get("Access-Control-Expose-Headers"))
		}
	}
}

func TestOAuthCallbackRouteSkipsManagementKeyMiddleware(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	state := "server-plugin-oauth-state"
	if errRegister := managementHandlers.RegisterPluginOAuthSession(state, "gemini-cli", nil); errRegister != nil {
		t.Fatalf("register plugin oauth session: %v", errRegister)
	}
	defer managementHandlers.CompleteOAuthSession(state)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback?state="+state+"&code=test-code", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	callbackPath := filepath.Join(server.cfg.AuthDir, ".oauth-gemini-cli-"+state+".oauth")
	if _, errRead := os.ReadFile(callbackPath); errRead != nil {
		t.Fatalf("expected callback file to be written without management key: %v", errRead)
	}
}

func TestNewServerWithPluginHostInjectsHandlerInterceptors(t *testing.T) {
	host := pluginhost.New()
	server := newTestServerWithOptions(t, WithPluginHost(host))

	if server.handlers == nil {
		t.Fatal("server handlers = nil")
	}
	got, ok := server.handlers.PluginHost.(*pluginhost.Host)
	if !ok || got != host {
		t.Fatalf("handler plugin host = %#v, want configured host", server.handlers.PluginHost)
	}
}

func TestNewServerWithoutPluginHostLeavesHandlerInterceptorsDisabled(t *testing.T) {
	server := newTestServer(t)

	if server.handlers == nil {
		t.Fatal("server handlers = nil")
	}
	if server.handlers.PluginHost != nil {
		t.Fatalf("handler plugin host = %#v, want nil", server.handlers.PluginHost)
	}
}

func TestManagementUsageRequiresManagementAuthAndPopsArray(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	prevQueueEnabled := redisqueue.Enabled()
	redisqueue.SetEnabled(false)
	t.Cleanup(func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
	})

	server := newTestServer(t)

	redisqueue.Enqueue([]byte(`{"id":1}`))
	redisqueue.Enqueue([]byte(`{"id":2}`))

	missingKeyReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)
	missingKeyRR := httptest.NewRecorder()
	server.engine.ServeHTTP(missingKeyRR, missingKeyReq)
	if missingKeyRR.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want %d body=%s", missingKeyRR.Code, http.StatusUnauthorized, missingKeyRR.Body.String())
	}

	// Fork-specific: /v0/management/usage is repurposed for GetUsageStatistics (not legacy 404).
	// The queue endpoint lives at /v0/management/usage-queue.

	authReq := httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)
	authReq.Header.Set("Authorization", "Bearer test-management-key")
	authRR := httptest.NewRecorder()
	server.engine.ServeHTTP(authRR, authReq)
	if authRR.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d body=%s", authRR.Code, http.StatusOK, authRR.Body.String())
	}

	var payload []json.RawMessage
	if errUnmarshal := json.Unmarshal(authRR.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v body=%s", errUnmarshal, authRR.Body.String())
	}
	if len(payload) != 2 {
		t.Fatalf("response records = %d, want 2", len(payload))
	}
	for i, raw := range payload {
		var record struct {
			ID int `json:"id"`
		}
		if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
			t.Fatalf("unmarshal record %d: %v", i, errUnmarshal)
		}
		if record.ID != i+1 {
			t.Fatalf("record %d id = %d, want %d", i, record.ID, i+1)
		}
	}

	if remaining := redisqueue.PopOldest(1); len(remaining) != 0 {
		t.Fatalf("remaining queue = %q, want empty", remaining)
	}
}

func TestManagementPluginsRouteRegistered(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	enabled := true
	server.cfg.Plugins.Configs = map[string]proxyconfig.PluginInstanceConfig{
		"sample": {Enabled: &enabled, Priority: 4},
	}
	if errWrite := os.WriteFile(server.configFilePath, []byte("{}\n"), 0o600); errWrite != nil {
		t.Fatalf("failed to write config file: %v", errWrite)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/plugins", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var payload struct {
		PluginsEnabled bool  `json:"plugins_enabled"`
		Plugins        []any `json:"plugins"`
	}
	if errUnmarshal := json.Unmarshal(rr.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v body=%s", errUnmarshal, rr.Body.String())
	}
	if payload.Plugins == nil {
		t.Fatalf("plugins field = nil, want array; body=%s", rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/plugins/sample/config", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("config status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var configPayload struct {
		Enabled  bool `json:"enabled"`
		Priority int  `json:"priority"`
	}
	if errUnmarshal := json.Unmarshal(rr.Body.Bytes(), &configPayload); errUnmarshal != nil {
		t.Fatalf("unmarshal config response: %v body=%s", errUnmarshal, rr.Body.String())
	}
	if !configPayload.Enabled || configPayload.Priority != 4 {
		t.Fatalf("plugin config = %#v, want enabled true priority 4", configPayload)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/sample", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestVideosRoutesKeepXAINativeAndExposeOpenAIPrefix(t *testing.T) {
	server := newTestServer(t)

	nativeReq := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"sora-2","prompt":"make a video"}`))
	nativeReq.Header.Set("Authorization", "Bearer test-key")
	nativeReq.Header.Set("Content-Type", "application/json")
	nativeRR := httptest.NewRecorder()
	server.engine.ServeHTTP(nativeRR, nativeReq)
	if nativeRR.Code != http.StatusBadRequest {
		t.Fatalf("native status = %d, want %d body=%s", nativeRR.Code, http.StatusBadRequest, nativeRR.Body.String())
	}
	if !strings.Contains(nativeRR.Body.String(), "/v1/videos/generations") {
		t.Fatalf("expected /v1/videos to keep xAI native validation, body=%s", nativeRR.Body.String())
	}

	openAIReq := httptest.NewRequest(http.MethodPost, "/openai/v1/videos", strings.NewReader(`{"model":`))
	openAIReq.Header.Set("Authorization", "Bearer test-key")
	openAIReq.Header.Set("Content-Type", "application/json")
	openAIRR := httptest.NewRecorder()
	server.engine.ServeHTTP(openAIRR, openAIReq)
	if openAIRR.Code != http.StatusBadRequest {
		t.Fatalf("openai create status = %d, want %d body=%s", openAIRR.Code, http.StatusBadRequest, openAIRR.Body.String())
	}
	if !strings.Contains(openAIRR.Body.String(), "body must be valid JSON") {
		t.Fatalf("expected /openai/v1/videos create handler, body=%s", openAIRR.Body.String())
	}

	contentReq := httptest.NewRequest(http.MethodGet, "/openai/v1/videos/video_123/content?variant=thumbnail", nil)
	contentReq.Header.Set("Authorization", "Bearer test-key")
	contentRR := httptest.NewRecorder()
	server.engine.ServeHTTP(contentRR, contentReq)
	if contentRR.Code != http.StatusBadRequest {
		t.Fatalf("content status = %d, want %d body=%s", contentRR.Code, http.StatusBadRequest, contentRR.Body.String())
	}
	if !strings.Contains(contentRR.Body.String(), "variant") {
		t.Fatalf("expected /openai/v1/videos content handler, body=%s", contentRR.Body.String())
	}
}

func TestHomeEnabledHidesManagementEndpointsAndControlPanel(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	server.cfg.Home.Enabled = true

	t.Run("management endpoints return 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.Header.Set("Authorization", "Bearer test-management-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
	})

	t.Run("management control panel returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
		}
	})
}

func TestModelsDispatchByAnthropicVersionHeader(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-anthropic-version-dispatch"
	modelRegistry.RegisterClient(clientID, "claude", []*registry.ModelInfo{
		{
			ID:                  "claude-sonnet-4-6",
			Object:              "model",
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude 4.6 Sonnet",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
		},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	server := newTestServer(t)

	// Anthropic API request (Anthropic-Version header, non-claude-cli User-Agent) -> Claude format.
	t.Run("anthropic version header routes to claude format", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("User-Agent", "Zed/1.0")
		req.Header.Set("Anthropic-Version", "2023-06-01")

		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}

		var resp struct {
			Object  string           `json:"object"`
			HasMore *bool            `json:"has_more"`
			Data    []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
		}
		if resp.Object == "list" {
			t.Fatalf("expected Claude format (no object=list), got OpenAI format: %s", rr.Body.String())
		}
		if resp.HasMore == nil {
			t.Fatalf("expected Claude envelope with has_more, got %s", rr.Body.String())
		}

		var claudeModel map[string]any
		for _, m := range resp.Data {
			if id, _ := m["id"].(string); id == "claude-sonnet-4-6" {
				claudeModel = m
			}
		}
		if claudeModel == nil {
			t.Fatalf("expected claude-sonnet-4-6 in response, got %s", rr.Body.String())
		}
		for _, field := range []string{"max_input_tokens", "max_tokens", "display_name"} {
			if _, ok := claudeModel[field]; !ok {
				t.Fatalf("expected Claude model to include %q, got %v", field, claudeModel)
			}
		}
	})

	// Plain request (no Anthropic-Version, non-claude-cli User-Agent) -> OpenAI format, unaffected.
	t.Run("plain request stays on openai format", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("User-Agent", "Mozilla/5.0")

		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}

		var resp struct {
			Object string           `json:"object"`
			Data   []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
		}
		if resp.Object != "list" {
			t.Fatalf("expected OpenAI format (object=list), got %s", rr.Body.String())
		}
		for _, m := range resp.Data {
			if _, ok := m["max_input_tokens"]; ok {
				t.Fatalf("did not expect max_input_tokens in OpenAI format, got %v", m)
			}
		}
	})
}

func TestModelsWithClientVersionReturnsCodexCatalog(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-client-version-catalog"
	modelRegistry.RegisterClient(clientID, "openai", []*registry.ModelInfo{
		{
			ID:            "gpt-5.5",
			Object:        "model",
			Created:       1776902400,
			OwnedBy:       "openai",
			Type:          "openai",
			DisplayName:   "GPT 5.5",
			Description:   "Frontier model for complex coding, research, and real-world work.",
			ContextLength: 272000,
			Thinking:      &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
		},
		{
			ID:            "custom-codex-model-test",
			Object:        "model",
			OwnedBy:       "test",
			Type:          "openai",
			DisplayName:   "Custom Codex Model",
			Description:   "Custom model from registry",
			ContextLength: 123456,
			Thinking:      &registry.ThinkingSupport{Levels: []string{"none", "minimal", "low", "medium", "unsupported", "high", "xhigh"}},
		},
		{ID: "grok-imagine-image-quality", Object: "model", OwnedBy: "xai", Type: "openai"},
		{ID: "gpt-image-2", Object: "model", OwnedBy: "openai", Type: "openai"},
		{ID: "grok-imagine-image", Object: "model", OwnedBy: "xai", Type: "openai"},
		{ID: "grok-imagine-video", Object: "model", OwnedBy: "xai", Type: "openai"},
		{ID: "grok-imagine-video-1.5-preview", Object: "model", OwnedBy: "xai", Type: "openai"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("User-Agent", "claude-cli/1.0")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp struct {
		Models []map[string]any `json:"models"`
		Object string           `json:"object"`
		Data   []any            `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v; body=%s", err, rr.Body.String())
	}
	if resp.Object != "" || resp.Data != nil {
		t.Fatalf("expected codex catalog format without object/data, got object=%q data=%v", resp.Object, resp.Data)
	}
	if len(resp.Models) == 0 {
		t.Fatal("expected codex catalog models")
	}

	var gpt55 map[string]any
	var custom map[string]any
	for _, model := range resp.Models {
		switch slug, _ := model["slug"].(string); slug {
		case "gpt-5.5":
			gpt55 = model
		case "custom-codex-model-test":
			custom = model
		}
	}
	if gpt55 == nil {
		t.Fatal("expected gpt-5.5 codex catalog entry")
	}
	if _, ok := gpt55["minimal_client_version"]; !ok {
		t.Fatal("expected minimal_client_version in codex catalog")
	}
	serviceTiers, ok := gpt55["service_tiers"].([]any)
	if !ok || len(serviceTiers) != 1 {
		t.Fatalf("expected gpt-5.5 priority service tier, got %#v", gpt55["service_tiers"])
	}
	if custom == nil {
		t.Fatal("expected custom model codex catalog entry")
	}
	if got, _ := custom["display_name"].(string); got != "Custom Codex Model" {
		t.Fatalf("custom display_name = %q, want Custom Codex Model", got)
	}
	if got := int(codexClientTestPriority(custom["priority"])); got != 129 {
		t.Fatalf("custom priority = %v, want 129", custom["priority"])
	}
	if got, _ := custom["description"].(string); got != "Custom model from registry" {
		t.Fatalf("custom description = %q, want Custom model from registry", got)
	}
	if got, _ := custom["context_window"].(float64); got != 123456 {
		t.Fatalf("custom context_window = %v, want 123456", custom["context_window"])
	}
	assertCodexSupportedReasoningLevels(t, custom, []string{"none", "low", "medium", "high", "xhigh"})
	if custom["base_instructions"] != gpt55["base_instructions"] {
		t.Fatal("expected custom model to use gpt-5.5 base_instructions fallback")
	}
	if _, ok := custom["available_in_plans"].([]any); !ok {
		t.Fatalf("expected custom model to use gpt-5.5 available_in_plans fallback, got %#v", custom["available_in_plans"])
	}
	if got, _ := custom["prefer_websockets"].(bool); got {
		t.Fatalf("custom prefer_websockets = %v, want false", custom["prefer_websockets"])
	}
	customServiceTiers, ok := custom["service_tiers"].([]any)
	if !ok || len(customServiceTiers) != 0 {
		t.Fatalf("expected custom model service_tiers = [], got %#v", custom["service_tiers"])
	}
	if _, ok := custom["apply_patch_tool_type"]; ok {
		t.Fatal("expected custom model to omit apply_patch_tool_type")
	}
	if _, ok := custom["upgrade"]; ok {
		t.Fatal("expected custom model to omit upgrade")
	}
	if _, ok := custom["availability_nux"]; ok {
		t.Fatal("expected custom model to omit availability_nux")
	}

	hiddenModels := map[string]bool{
		"grok-imagine-image-quality":     false,
		"gpt-image-2":                    false,
		"grok-imagine-image":             false,
		"grok-imagine-video":             false,
		"grok-imagine-video-1.5-preview": false,
	}
	for _, model := range resp.Models {
		slug, _ := model["slug"].(string)
		if _, ok := hiddenModels[slug]; !ok {
			continue
		}
		if visibility, _ := model["visibility"].(string); visibility != "hide" {
			t.Fatalf("%s visibility = %q, want hide", slug, visibility)
		}
		hiddenModels[slug] = true
	}
	for slug, found := range hiddenModels {
		if !found {
			t.Fatalf("expected hidden model %s in codex catalog", slug)
		}
	}
}

func codexClientTestPriority(raw any) int {
	switch value := raw.(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return -1
	}
}

func assertCodexSupportedReasoningLevels(t *testing.T, model map[string]any, want []string) {
	t.Helper()

	rawLevels, ok := model["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("expected supported_reasoning_levels, got %#v", model["supported_reasoning_levels"])
	}
	if len(rawLevels) != len(want) {
		t.Fatalf("supported_reasoning_levels length = %d, want %d: %#v", len(rawLevels), len(want), rawLevels)
	}
	for index, rawLevel := range rawLevels {
		levelEntry, ok := rawLevel.(map[string]any)
		if !ok {
			t.Fatalf("supported_reasoning_levels[%d] = %#v, want object", index, rawLevel)
		}
		if got, _ := levelEntry["effort"].(string); got != want[index] {
			t.Fatalf("supported_reasoning_levels[%d].effort = %q, want %q", index, got, want[index])
		}
	}
}

func TestDefaultRequestLoggerFactory_UsesResolvedLogDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	// Force ResolveLogDirectory to fallback to auth-dir/logs by making ./logs not a writable directory.
	if errWriteFile := os.WriteFile(filepath.Join(tmpDir, "logs"), []byte("not-a-directory"), 0o644); errWriteFile != nil {
		t.Fatalf("failed to create blocking logs file: %v", errWriteFile)
	}

	configDir := filepath.Join(tmpDir, "config")
	if errMkdirConfig := os.MkdirAll(configDir, 0o755); errMkdirConfig != nil {
		t.Fatalf("failed to create config dir: %v", errMkdirConfig)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	authDir := filepath.Join(tmpDir, "auth")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{
			RequestLog: false,
		},
		AuthDir:           authDir,
		ErrorLogsMaxFiles: 10,
	}

	logger := defaultRequestLoggerFactory(cfg, configPath)
	fileLogger, ok := logger.(*internallogging.FileRequestLogger)
	if !ok {
		t.Fatalf("expected *FileRequestLogger, got %T", logger)
	}

	errLog := fileLogger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"issue-1711",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("failed to write forced error request log: %v", errLog)
	}

	authLogsDir := filepath.Join(authDir, "logs")
	authEntries, errReadAuthDir := os.ReadDir(authLogsDir)
	if errReadAuthDir != nil {
		t.Fatalf("failed to read auth logs dir %s: %v", authLogsDir, errReadAuthDir)
	}
	foundErrorLogInAuthDir := false
	for _, entry := range authEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			foundErrorLogInAuthDir = true
			break
		}
	}
	if !foundErrorLogInAuthDir {
		t.Fatalf("expected forced error log in auth fallback dir %s, got entries: %+v", authLogsDir, authEntries)
	}

	configLogsDir := filepath.Join(configDir, "logs")
	configEntries, errReadConfigDir := os.ReadDir(configLogsDir)
	if errReadConfigDir != nil && !os.IsNotExist(errReadConfigDir) {
		t.Fatalf("failed to inspect config logs dir %s: %v", configLogsDir, errReadConfigDir)
	}
	for _, entry := range configEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			t.Fatalf("unexpected forced error log in config dir %s", configLogsDir)
		}
	}
}

func TestFormatHomeClaudeModelIncludesAnthropicSchemaFields(t *testing.T) {
	withMetadata := formatHomeClaudeModel(homeModelEntry{
		id:                  "claude-sonnet-4-6",
		created:             1771372800,
		ownedBy:             "anthropic",
		displayName:         "Claude 4.6 Sonnet",
		contextLength:       200000,
		maxCompletionTokens: 64000,
	})
	if got := withMetadata["created_at"]; got != "2026-02-18T00:00:00Z" {
		t.Fatalf("created_at = %v, want RFC3339 timestamp", got)
	}
	if got := withMetadata["type"]; got != "model" {
		t.Fatalf("type = %v, want model", got)
	}
	if got := withMetadata["display_name"]; got != "Claude 4.6 Sonnet" {
		t.Fatalf("display_name = %v, want Claude 4.6 Sonnet", got)
	}
	if got := withMetadata["max_input_tokens"]; got != 200000 {
		t.Fatalf("max_input_tokens = %v, want 200000", got)
	}
	if got := withMetadata["max_tokens"]; got != 64000 {
		t.Fatalf("max_tokens = %v, want 64000", got)
	}

	withDefaults := formatHomeClaudeModel(homeModelEntry{id: "claude-no-limits"})
	if got := withDefaults["display_name"]; got != "claude-no-limits" {
		t.Fatalf("display_name fallback = %v, want claude-no-limits", got)
	}
	if got := withDefaults["max_input_tokens"]; got != registry.DefaultClaudeMaxInputTokens {
		t.Fatalf("max_input_tokens fallback = %v, want %d", got, registry.DefaultClaudeMaxInputTokens)
	}
	if got := withDefaults["max_tokens"]; got != registry.DefaultClaudeMaxOutputTokens {
		t.Fatalf("max_tokens fallback = %v, want %d", got, registry.DefaultClaudeMaxOutputTokens)
	}
	if _, ok := withDefaults["created_at"]; ok {
		t.Fatalf("created_at should be omitted when source created is missing, got %v", withDefaults)
	}
}

func TestDecodeHomeModelsKeepsTokenMetadata(t *testing.T) {
	entries, errDecode := decodeHomeModels([]byte(`{
		"claude": [
			{
				"id": "claude-sonnet-4-6",
				"created": 1771372800,
				"owned_by": "anthropic",
				"context_length": 200000,
				"max_completion_tokens": 64000
			}
		],
		"gemini": [
			{
				"name": "models/gemini-3-pro",
				"inputTokenLimit": 1048576,
				"outputTokenLimit": 65536
			}
		]
	}`))
	if errDecode != nil {
		t.Fatalf("decodeHomeModels returned error: %v", errDecode)
	}

	byID := make(map[string]homeModelEntry, len(entries))
	for _, entry := range entries {
		byID[entry.id] = entry
	}
	claudeEntry, ok := byID["claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("expected claude-sonnet-4-6 entry, got %v", byID)
	}
	if claudeEntry.contextLength != 200000 || claudeEntry.maxCompletionTokens != 64000 {
		t.Fatalf("claude token metadata = %d/%d, want 200000/64000", claudeEntry.contextLength, claudeEntry.maxCompletionTokens)
	}
	geminiEntry, ok := byID["gemini-3-pro"]
	if !ok {
		t.Fatalf("expected gemini-3-pro entry, got %v", byID)
	}
	if geminiEntry.contextLength != 1048576 || geminiEntry.maxCompletionTokens != 65536 {
		t.Fatalf("gemini token metadata = %d/%d, want 1048576/65536", geminiEntry.contextLength, geminiEntry.maxCompletionTokens)
	}
}

func TestHomeModelsAuthStatus(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantStatus  int
		wantHandled bool
	}{
		{"no credentials", `{"error":{"type":"no_credentials","message":"Missing API key"}}`, http.StatusUnauthorized, true},
		{"invalid credential", `{"error":{"type":"invalid_credential","message":"Invalid API key"}}`, http.StatusUnauthorized, true},
		{"internal error maps to bad gateway", `{"error":{"type":"internal_error","message":"boom"}}`, http.StatusBadGateway, true},
		{"models payload not an error", `{"openai":[{"id":"gpt-5.5"}]}`, 0, false},
		{"empty payload not an error", `{}`, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, handled := homeModelsAuthStatus([]byte(tc.raw))
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v (status=%d)", handled, tc.wantHandled, status)
			}
			if handled && status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

func TestHomeModelsErrorMessage(t *testing.T) {
	if msg := homeModelsErrorMessage([]byte(`{"error":{"type":"invalid_credential","message":"Invalid API key"}}`)); msg != "Invalid API key" {
		t.Fatalf("message = %q, want %q", msg, "Invalid API key")
	}
	if msg := homeModelsErrorMessage([]byte(`{"openai":[]}`)); msg != "home models request failed" {
		t.Fatalf("default message = %q, want fallback", msg)
	}
}
