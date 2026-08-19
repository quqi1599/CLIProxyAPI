package contentaudit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestMiddlewareBlocksBeforeNextAndPersistsCustomerEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(identitySecretEnv, "identity-shared-secret")
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	tempDir := t.TempDir()
	policyPath := filepath.Join(tempDir, "policy.yaml")
	policy := `version: test-v1
rules:
  - id: synthetic-rule
    category: cyber_abuse
    severity: high
    keywords: ["sensitive synthetic phrase"]
`
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	service := NewService(config.ContentAuditConfig{
		Enabled:               true,
		PolicyFile:            policyPath,
		DatabasePath:          filepath.Join(tempDir, "audit.db"),
		RequireSignedIdentity: true,
		EvidenceKeyID:         "test-key",
	}, filepath.Join(tempDir, "config.yaml"))
	state := service.state.Load()
	if state == nil || state.initErr != nil {
		t.Fatalf("service state error = %v", state.initErr)
	}
	defer func() { _ = state.store.Close() }()

	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusNoContent)
	})

	body := `{"model":"gpt-test","stream":true,"input":"sensitive synthetic phrase"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	signAuditTestRequest(request, time.Now(), "42", "73", "production-token", "req-1", "gpt-test", "identity-shared-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if nextCalls != 0 {
		t.Fatalf("next handler calls = %d, want 0", nextCalls)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("cpa_content_audit_blocked")) {
		t.Fatalf("response body = %s", response.Body.String())
	}
	list, err := service.List(t.Context(), ListFilter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.Total != 1 || list.Items[0].UserID != 42 || list.Items[0].TokenID != 73 || list.Items[0].TokenName != "production-token" || list.Items[0].MatchedTerm != "sensitive synthetic phrase" || list.Items[0].UpstreamSent {
		t.Fatalf("stored event = %#v", list)
	}
	matchedList, err := service.List(t.Context(), ListFilter{Search: "synthetic phrase"})
	if err != nil || matchedList.Total != 1 {
		t.Fatalf("List() matched term search = %#v err=%v", matchedList, err)
	}
	evidence, err := service.Reveal(t.Context(), list.Items[0].ID, "127.0.0.1")
	if err != nil || !bytes.Contains(evidence, []byte("sensitive synthetic phrase")) {
		t.Fatalf("Reveal() evidence=%s err=%v", evidence, err)
	}
	detail, err := service.Get(t.Context(), list.Items[0].ID)
	if err != nil || len(detail.AccessHistory) != 1 || detail.AccessHistory[0].Action != "reveal" || detail.AccessHistory[0].Reason != "management console evidence view" {
		t.Fatalf("Get() detail=%#v err=%v", detail, err)
	}
}

func TestMiddlewareAllowsNonMatchWithoutDatabaseWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(identitySecretEnv, "identity-shared-secret")
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	tempDir := t.TempDir()
	policyPath := filepath.Join(tempDir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("version: test-v1\nrules:\n  - id: synthetic-rule\n    category: synthetic\n    severity: high\n    keywords: [\"blocked fixture\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.ContentAuditConfig{Enabled: true, PolicyFile: policyPath, DatabasePath: filepath.Join(tempDir, "audit.db"), EvidenceKeyID: "test-key"}, filepath.Join(tempDir, "config.yaml"))
	state := service.state.Load()
	defer func() { _ = state.store.Close() }()

	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || nextCalls != 1 {
		t.Fatalf("status=%d next=%d body=%s", response.Code, nextCalls, response.Body.String())
	}
	list, err := service.List(t.Context(), ListFilter{})
	if err != nil || list.Total != 0 {
		t.Fatalf("List() = %#v err=%v", list, err)
	}
}

func TestMiddlewareAuditOnlyRecordsAndContinues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(identitySecretEnv, "identity-shared-secret")
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	tempDir := t.TempDir()
	policyPath := filepath.Join(tempDir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("version: test-v1\nrules:\n  - id: synthetic-rule\n    category: synthetic\n    severity: high\n    keywords: [\"blocked fixture\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.ContentAuditConfig{
		Enabled:               true,
		AuditOnly:             true,
		PolicyFile:            policyPath,
		DatabasePath:          filepath.Join(tempDir, "audit.db"),
		RequireSignedIdentity: true,
		EvidenceKeyID:         "test-key",
	}, filepath.Join(tempDir, "config.yaml"))
	state := service.state.Load()
	defer func() { _ = state.store.Close() }()

	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"blocked fixture"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || nextCalls != 1 {
		t.Fatalf("status=%d next=%d body=%s", response.Code, nextCalls, response.Body.String())
	}
	list, err := service.List(t.Context(), ListFilter{})
	if err != nil || list.Total != 1 || !list.Items[0].UpstreamSent || list.Items[0].IdentityVerified {
		t.Fatalf("List() = %#v err=%v", list, err)
	}
}

func TestMiddlewareMixedPolicyBlocksOnlyBlockRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	tempDir := t.TempDir()
	policyPath := filepath.Join(tempDir, "policy.yaml")
	policy := `version: mixed-v1
rules:
  - id: observe-rule
    category: academic
    severity: critical
    action: observe
    keywords: ["academic sensitive term"]
  - id: block-rule
    category: jailbreak
    severity: high
    action: block
    keywords: ["high confidence jailbreak"]
`
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.ContentAuditConfig{
		Enabled:       true,
		PolicyFile:    policyPath,
		DatabasePath:  filepath.Join(tempDir, "audit.db"),
		EvidenceKeyID: "test-key",
	}, filepath.Join(tempDir, "config.yaml"))
	state := service.state.Load()
	defer func() { _ = state.store.Close() }()

	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusNoContent)
	})

	observeRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"academic sensitive term"}`))
	observeRequest.Header.Set("Content-Type", "application/json")
	observeResponse := httptest.NewRecorder()
	router.ServeHTTP(observeResponse, observeRequest)
	if observeResponse.Code != http.StatusNoContent || nextCalls != 1 {
		t.Fatalf("observe status=%d next=%d body=%s", observeResponse.Code, nextCalls, observeResponse.Body.String())
	}

	blockRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"high confidence jailbreak"}`))
	blockRequest.Header.Set("Content-Type", "application/json")
	blockResponse := httptest.NewRecorder()
	router.ServeHTTP(blockResponse, blockRequest)
	if blockResponse.Code != http.StatusBadRequest || nextCalls != 1 || !bytes.Contains(blockResponse.Body.Bytes(), []byte("cpa_content_audit_blocked")) {
		t.Fatalf("block status=%d next=%d body=%s", blockResponse.Code, nextCalls, blockResponse.Body.String())
	}

	list, err := service.List(t.Context(), ListFilter{})
	if err != nil || list.Total != 2 {
		t.Fatalf("List() = %#v err=%v", list, err)
	}
	for _, item := range list.Items {
		switch item.RuleID {
		case "block-rule":
			if item.UpstreamSent {
				t.Fatalf("block event was sent upstream: %#v", item)
			}
		case "observe-rule":
			if !item.UpstreamSent {
				t.Fatalf("observe event was not sent upstream: %#v", item)
			}
		default:
			t.Fatalf("unexpected event: %#v", item)
		}
	}
	status := service.Status()
	if status.BlockRuleCount != 1 || status.ObserveRuleCount != 1 || status.AuditOnly {
		t.Fatalf("Status() = %#v", status)
	}
}

func TestMiddlewareFailsClosedForUnauditedWebSocketFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(identitySecretEnv, "identity-shared-secret")
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	tempDir := t.TempDir()
	policyPath := filepath.Join(tempDir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("version: test-v1\nrules:\n  - id: synthetic-rule\n    category: synthetic\n    severity: high\n    keywords: [\"blocked fixture\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(config.ContentAuditConfig{Enabled: true, PolicyFile: policyPath, DatabasePath: filepath.Join(tempDir, "audit.db"), EvidenceKeyID: "test-key"}, filepath.Join(tempDir, "config.yaml"))
	state := service.state.Load()
	defer func() { _ = state.store.Close() }()

	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.GET("/v1/responses", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusSwitchingProtocols)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || nextCalls != 0 {
		t.Fatalf("status=%d next=%d body=%s", response.Code, nextCalls, response.Body.String())
	}
}

func TestMiddlewareAuditOnlyAllowsUnauditedWebSocketFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tempDir := t.TempDir()
	service := NewService(config.ContentAuditConfig{
		Enabled:      true,
		AuditOnly:    true,
		PolicyFile:   filepath.Join(tempDir, "missing-policy.yaml"),
		DatabasePath: filepath.Join(tempDir, "audit.db"),
	}, filepath.Join(tempDir, "config.yaml"))

	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.GET("/v1/responses", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusSwitchingProtocols)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusSwitchingProtocols || nextCalls != 1 {
		t.Fatalf("status=%d next=%d body=%s", response.Code, nextCalls, response.Body.String())
	}
}

func TestMiddlewareMixedPolicyAllowsConfiguredUnauditedWebSocketFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tempDir := t.TempDir()
	service := NewService(config.ContentAuditConfig{
		Enabled:                 true,
		AllowUnauditedWebsocket: true,
		PolicyFile:              filepath.Join(tempDir, "missing-policy.yaml"),
		DatabasePath:            filepath.Join(tempDir, "audit.db"),
	}, filepath.Join(tempDir, "config.yaml"))
	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.GET("/v1/responses", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusSwitchingProtocols)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusSwitchingProtocols || nextCalls != 1 {
		t.Fatalf("status=%d next=%d body=%s", response.Code, nextCalls, response.Body.String())
	}
}

func TestMiddlewareAuditOnlyDoesNotFailTrafficWhenAuditUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tempDir := t.TempDir()
	service := NewService(config.ContentAuditConfig{
		Enabled:      true,
		AuditOnly:    true,
		PolicyFile:   filepath.Join(tempDir, "missing-policy.yaml"),
		DatabasePath: filepath.Join(tempDir, "audit.db"),
	}, filepath.Join(tempDir, "config.yaml"))

	nextCalls := 0
	router := gin.New()
	router.Use(service.Middleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		nextCalls++
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"ordinary request"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || nextCalls != 1 {
		t.Fatalf("status=%d next=%d body=%s", response.Code, nextCalls, response.Body.String())
	}
	status := service.Status()
	if !status.Enabled || !status.AuditOnly || status.Ready {
		t.Fatalf("Status() = %#v", status)
	}
}

func signAuditTestRequest(request *http.Request, now time.Time, userID, tokenID, tokenName, requestID, model, secret string) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	request.Header.Set(auditHeaderVersion, "1")
	request.Header.Set(auditHeaderUserID, userID)
	request.Header.Set(auditHeaderTokenID, tokenID)
	request.Header.Set(auditHeaderTokenName, tokenName)
	request.Header.Set(auditHeaderRequestID, requestID)
	request.Header.Set(auditHeaderTimestamp, timestamp)
	request.Header.Set(auditHeaderModel, model)
	canonical := identityCanonicalValues("1", timestamp, requestID, userID, tokenID, tokenName, request.Method, request.URL.EscapedPath(), model, "0")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	request.Header.Set(auditHeaderSignature, hex.EncodeToString(mac.Sum(nil)))
}
