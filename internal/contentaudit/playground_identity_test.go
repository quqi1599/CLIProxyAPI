package contentaudit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// This signature was computed independently with Python, and is also asserted
// by the NewAPI signing test. Do not recompute it with the implementation here.
func TestVerifyPlaygroundIdentityNewAPIGoldenSignature(t *testing.T) {
	r := newPlaygroundIdentityRequest(time.Unix(1_800_000_000, 0), nil)
	r.Header.Set(auditHeaderSignature, "d488ce87fc05402b1e47dee1eca3bc0850d859335bf912fe8f324ec393f74e78")
	id, err := VerifyIdentity(r, "shared-secret", true, time.Unix(1_800_000_000, 0), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Verified || id.ChannelTest || id.IdentityKind != "playground" || id.UserID != 42 || id.TokenID != 0 || id.TokenName != "__playground__" || id.RequestID != "req-pg" {
		t.Fatalf("unexpected identity: %#v", id)
	}
	if profile := profileForIdentity(config.ContentAuditConfig{EducationDocumentTokenIDs: []int64{42}}, id); profile != "" {
		t.Fatalf("playground must not receive token-based education exemption: %q", profile)
	}
}

func TestVerifyPlaygroundIdentityRejectsInvalidShapeAndTampering(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name   string
		change func(*http.Request)
		resign bool
	}{
		{"unknown version", func(r *http.Request) { r.Header.Set(auditHeaderVersion, "3") }, true},
		{"missing kind", func(r *http.Request) { r.Header.Del(auditHeaderIdentityKind) }, true},
		{"unknown kind", func(r *http.Request) { r.Header.Set(auditHeaderIdentityKind, "admin") }, true},
		{"signed v1 kind", func(r *http.Request) { r.Header.Set(auditHeaderVersion, "1") }, true},
		{"v1 zero token", func(r *http.Request) { r.Header.Set(auditHeaderVersion, "1"); r.Header.Del(auditHeaderIdentityKind) }, true},
		{"zero user", func(r *http.Request) { r.Header.Set(auditHeaderUserID, "0") }, true},
		{"negative user", func(r *http.Request) { r.Header.Set(auditHeaderUserID, "-42") }, true},
		{"bad user", func(r *http.Request) { r.Header.Set(auditHeaderUserID, "user") }, true},
		{"positive token", func(r *http.Request) { r.Header.Set(auditHeaderTokenID, "73") }, true},
		{"negative token", func(r *http.Request) { r.Header.Set(auditHeaderTokenID, "-1") }, true},
		{"wrong token name", func(r *http.Request) { r.Header.Set(auditHeaderTokenName, "production-token") }, true},
		{"channel test", func(r *http.Request) { r.Header.Set(auditHeaderChannelTest, "1") }, true},
		{"malformed channel test", func(r *http.Request) { r.Header.Set(auditHeaderChannelTest, "true") }, true},
		{"missing request id", func(r *http.Request) { r.Header.Del(auditHeaderRequestID) }, true},
		{"expired", func(r *http.Request) {
			r.Header.Set(auditHeaderTimestamp, strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10))
		}, true},
		{"future", func(r *http.Request) {
			r.Header.Set(auditHeaderTimestamp, strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10))
		}, true},
		{"missing signature", func(r *http.Request) { r.Header.Del(auditHeaderSignature) }, false},
		{"changed user", func(r *http.Request) { r.Header.Set(auditHeaderUserID, "43") }, false},
		{"changed request", func(r *http.Request) { r.Header.Set(auditHeaderRequestID, "other") }, false},
		{"changed model", func(r *http.Request) { r.Header.Set(auditHeaderModel, "other") }, false},
		{"changed method", func(r *http.Request) { r.Method = http.MethodGet }, false},
		{"changed path", func(r *http.Request) { r.URL.Path = "/v1/messages" }, false},
		{"stripped version and kind", func(r *http.Request) {
			r.Header.Set(auditHeaderVersion, "1")
			r.Header.Del(auditHeaderIdentityKind)
			r.Header.Set(auditHeaderTokenID, "73")
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newPlaygroundIdentityRequest(now, nil)
			tt.change(r)
			if tt.resign {
				signPlaygroundIdentityHeaders(r, "shared-secret")
			}
			if id, err := VerifyIdentity(r, "shared-secret", true, now, 5*time.Minute); err == nil || id.Verified {
				t.Fatalf("invalid envelope accepted: %#v err=%v", id, err)
			}
		})
	}
	for _, secret := range []string{"", "wrong-secret"} {
		if _, err := VerifyIdentity(newPlaygroundIdentityRequest(now, nil), secret, true, now, time.Minute); err == nil {
			t.Fatal("accepted invalid signing secret")
		}
	}
}

func TestPlaygroundReviewTenantScopeIsolation(t *testing.T) {
	base := Identity{UserID: 42, TokenID: 0, TokenName: "__playground__", Verified: true, IdentityKind: "playground"}
	want := "verified-playground:42"
	if got := base.reviewTenantScope(); got != want {
		t.Fatalf("scope=%q", got)
	}
	other := base
	other.UserID = 43
	if other.reviewTenantScope() == want {
		t.Fatal("two browser users share review scope")
	}
	api := Identity{UserID: 42, TokenID: 73, Verified: true}
	if api.reviewTenantScope() != "verified:42:73" || api.reviewTenantScope() == want {
		t.Fatal("API token scope changed or collides")
	}
	for _, mutate := range []func(*Identity){
		func(i *Identity) { i.Verified = false },
		func(i *Identity) { i.ChannelTest = true },
		func(i *Identity) { i.UserID = 0 },
		func(i *Identity) { i.TokenID = 73 },
		func(i *Identity) { i.IdentityKind = "" },
	} {
		id := base
		mutate(&id)
		if id.reviewTenantScope() != "" {
			t.Fatalf("invalid stable scope: %#v", id)
		}
	}
}

func TestPlaygroundMiddlewareAuthenticatesAuditsAndPreservesAttribution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(identitySecretEnv, "shared-secret")
	t.Setenv(evidenceKeyEnv, "0123456789abcdef0123456789abcdef")
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	policy := `version: playground-test
rules:
  - id: block-fixture
    category: synthetic
    severity: high
    action: block
    keywords: ["blocked fixture"]
  - id: observe-fixture
    category: synthetic
    severity: high
    action: observe
    keywords: ["observe fixture"]
`
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewService(config.ContentAuditConfig{Enabled: true, Mode: ModeStrict, PolicyFile: policyPath, DatabasePath: filepath.Join(dir, "audit.db"), RequireSignedIdentity: true, EvidenceKeyID: "test-key"}, filepath.Join(dir, "config.yaml"))
	state := svc.state.Load()
	if state == nil || state.initErr != nil {
		t.Fatalf("audit init failed: %#v", state)
	}
	t.Cleanup(func() { _ = state.store.Close() })
	nextCalls := 0
	router := gin.New()
	router.Use(svc.Middleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		nextCalls++
		if hasIdentityHeaders(c.Request.Header) {
			t.Error("audit identity reached downstream")
		}
		c.Status(http.StatusNoContent)
	})
	for _, tt := range []struct {
		name, input string
		mutate      func(*http.Request)
		code, calls int
	}{
		{"normal", "ordinary fixture", nil, 204, 1},
		{"observe", "observe fixture", nil, 204, 1},
		{"block", "blocked fixture", nil, 400, 0},
		{"changed user", "ordinary fixture", func(r *http.Request) { r.Header.Set(auditHeaderUserID, "43") }, 401, 0},
		{"unsigned", "ordinary fixture", func(r *http.Request) { r.Header = make(http.Header); r.Header.Set("Content-Type", "application/json") }, 401, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newPlaygroundIdentityRequest(time.Now(), strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"`+tt.input+`"}]}`))
			if tt.mutate != nil {
				tt.mutate(r)
			}
			before := nextCalls
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tt.code || nextCalls-before != tt.calls {
				t.Fatalf("status=%d next=%d body=%s", w.Code, nextCalls-before, w.Body.String())
			}
		})
	}
	list, err := svc.List(t.Context(), ListFilter{})
	if err != nil || list.Total != 2 {
		t.Fatalf("events=%#v error=%v", list, err)
	}
	for _, event := range list.Items {
		if event.UserID != 42 || event.TokenID != 0 || event.TokenName != "__playground__" || !event.IdentityVerified {
			t.Fatalf("wrong attribution: %#v", event)
		}
		if event.UpstreamSent != (event.RuleID == "observe-fixture") {
			t.Fatalf("wrong downstream result: %#v", event)
		}
	}
}

func TestStripIdentityHeadersRemovesNoncanonicalPlaygroundHeaders(t *testing.T) {
	h := http.Header{"x-cpa-audit-identity-kind": {"playground"}, "X-CPA-Audit-User-ID": {"42"}, "X-Unrelated": {"kept"}}
	StripIdentityHeaders(h)
	if hasIdentityHeaders(h) || h.Get("X-Unrelated") != "kept" {
		t.Fatalf("unexpected headers: %#v", h)
	}
}

func newPlaygroundIdentityRequest(now time.Time, body *strings.Reader) *http.Request {
	if body == nil {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range map[string]string{auditHeaderVersion: "2", auditHeaderIdentityKind: "playground", auditHeaderUserID: "42", auditHeaderTokenID: "0", auditHeaderTokenName: "__playground__", auditHeaderRequestID: "req-pg", auditHeaderTimestamp: strconv.FormatInt(now.Unix(), 10), auditHeaderModel: "claude-sonnet-4-6", auditHeaderChannelTest: "0"} {
		r.Header.Set(k, v)
	}
	signPlaygroundIdentityHeaders(r, "shared-secret")
	return r
}

func signPlaygroundIdentityHeaders(r *http.Request, secret string) {
	// Independent field table avoids verifying the canonical helper against itself.
	v := url.Values{}
	for key, header := range map[string]string{"version": auditHeaderVersion, "identity_kind": auditHeaderIdentityKind, "user_id": auditHeaderUserID, "token_id": auditHeaderTokenID, "token_name": auditHeaderTokenName, "request_id": auditHeaderRequestID, "timestamp": auditHeaderTimestamp, "model": auditHeaderModel} {
		if key == "identity_kind" && r.Header.Get(header) == "" {
			continue
		}
		v.Set(key, r.Header.Get(header))
	}
	v.Set("channel_test", "0")
	if r.Header.Get(auditHeaderChannelTest) == "1" {
		v.Set("channel_test", "1")
	}
	v.Set("method", r.Method)
	v.Set("path", r.URL.EscapedPath())
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(v.Encode()))
	r.Header.Set(auditHeaderSignature, hex.EncodeToString(mac.Sum(nil)))
}

func TestPlaygroundZeroHitTenantScopeMatchesVerifiedScope(t *testing.T) {
	browser := Identity{UserID: 42, TokenID: 0, TokenName: "__playground__", Verified: true, IdentityKind: "playground"}
	if got := reviewTenantScope(browser); got != "verified-playground:42" {
		t.Fatalf("zero-hit scope=%q", got)
	}
	browser.UserID = 43
	if got := reviewTenantScope(browser); got != "verified-playground:43" {
		t.Fatalf("other user's zero-hit scope=%q", got)
	}
	if got := reviewTenantScope(Identity{UserID: 42, TokenID: 73, Verified: true}); got != "verified:42:73" {
		t.Fatalf("API scope changed: %q", got)
	}
	if got := reviewTenantScope(Identity{}); got != "anonymous" {
		t.Fatalf("unverified scope changed: %q", got)
	}
}
