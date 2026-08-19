package contentaudit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifyIdentityMatchesNewAPICanonicalContract(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	request := httptest.NewRequest(http.MethodPost, "https://cpa.example/v1/responses?beta=true", nil)
	request.Header.Set(auditHeaderVersion, "1")
	request.Header.Set(auditHeaderUserID, "42")
	request.Header.Set(auditHeaderTokenID, "73")
	request.Header.Set(auditHeaderTokenName, "production-token")
	request.Header.Set(auditHeaderRequestID, "req-abc")
	request.Header.Set(auditHeaderTimestamp, "1800000000")
	request.Header.Set(auditHeaderModel, "gpt-5.4-cpa")
	canonical := identityCanonicalValues("1", "1800000000", "req-abc", "42", "73", "production-token", http.MethodPost, "/v1/responses", "gpt-5.4-cpa", "0")
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	_, _ = mac.Write([]byte(canonical))
	request.Header.Set(auditHeaderSignature, hex.EncodeToString(mac.Sum(nil)))

	identity, err := VerifyIdentity(request, "shared-secret", true, now, 5*time.Minute)
	if err != nil {
		t.Fatalf("VerifyIdentity() error = %v", err)
	}
	if !identity.Verified || identity.UserID != 42 || identity.TokenID != 73 || identity.TokenName != "production-token" || identity.RequestID != "req-abc" {
		t.Fatalf("VerifyIdentity() = %#v", identity)
	}

	StripIdentityHeaders(request.Header)
	if hasIdentityHeaders(request.Header) {
		t.Fatal("StripIdentityHeaders() left internal headers")
	}
}

func TestVerifyIdentityRejectsChangedPath(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	request := httptest.NewRequest(http.MethodPost, "https://cpa.example/v1/chat/completions", nil)
	request.Header.Set(auditHeaderVersion, "1")
	request.Header.Set(auditHeaderUserID, "42")
	request.Header.Set(auditHeaderTokenID, "73")
	request.Header.Set(auditHeaderRequestID, "req-abc")
	request.Header.Set(auditHeaderTimestamp, "1800000000")
	canonical := identityCanonicalValues("1", "1800000000", "req-abc", "42", "73", "", http.MethodPost, "/v1/responses", "", "0")
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	_, _ = mac.Write([]byte(canonical))
	request.Header.Set(auditHeaderSignature, hex.EncodeToString(mac.Sum(nil)))

	if _, err := VerifyIdentity(request, "shared-secret", true, now, 5*time.Minute); err == nil {
		t.Fatal("VerifyIdentity() error = nil, want changed path rejection")
	}
}

func TestVerifyIdentityAcceptsSignedNewAPIChannelTest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	request := httptest.NewRequest(http.MethodPost, "https://cpa.example/v1/chat/completions", nil)
	request.Header.Set(auditHeaderVersion, "1")
	request.Header.Set(auditHeaderUserID, "0")
	request.Header.Set(auditHeaderTokenID, "0")
	request.Header.Set(auditHeaderTokenName, "__channel_test__")
	request.Header.Set(auditHeaderRequestID, "channel-test")
	request.Header.Set(auditHeaderTimestamp, "1800000000")
	request.Header.Set(auditHeaderChannelTest, "1")
	canonical := identityCanonicalValues("1", "1800000000", "channel-test", "0", "0", "__channel_test__", http.MethodPost, "/v1/chat/completions", "", "1")
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	_, _ = mac.Write([]byte(canonical))
	request.Header.Set(auditHeaderSignature, hex.EncodeToString(mac.Sum(nil)))

	identity, err := VerifyIdentity(request, "shared-secret", true, now, 5*time.Minute)
	if err != nil || !identity.Verified || !identity.ChannelTest {
		t.Fatalf("VerifyIdentity() = %#v, err=%v", identity, err)
	}
}
