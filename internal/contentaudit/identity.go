package contentaudit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	auditIdentityVersion = "1"
	auditHeaderPrefix    = "x-cpa-audit-"

	auditHeaderVersion     = "X-CPA-Audit-Version"
	auditHeaderUserID      = "X-CPA-Audit-User-ID"
	auditHeaderTokenID     = "X-CPA-Audit-Token-ID"
	auditHeaderTokenName   = "X-CPA-Audit-Token-Name"
	auditHeaderRequestID   = "X-CPA-Audit-Request-ID"
	auditHeaderTimestamp   = "X-CPA-Audit-Timestamp"
	auditHeaderModel       = "X-CPA-Audit-Model"
	auditHeaderChannelTest = "X-CPA-Audit-Channel-Test"
	auditHeaderSignature   = "X-CPA-Audit-Signature"
)

// Identity is the verified NewAPI customer reference attached to a CPA request.
type Identity struct {
	UserID      int64  `json:"user_id,omitempty"`
	TokenID     int64  `json:"token_id,omitempty"`
	TokenName   string `json:"token_name,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	Model       string `json:"model,omitempty"`
	Verified    bool   `json:"verified"`
	ChannelTest bool   `json:"channel_test,omitempty"`
}

func identityCanonicalValues(version, timestamp, requestID, userID, tokenID, tokenName, method, path, model, channelTest string) string {
	values := url.Values{}
	values.Set("channel_test", channelTest)
	values.Set("method", strings.ToUpper(strings.TrimSpace(method)))
	values.Set("model", model)
	values.Set("path", path)
	values.Set("request_id", requestID)
	values.Set("timestamp", timestamp)
	values.Set("token_id", tokenID)
	values.Set("token_name", tokenName)
	values.Set("user_id", userID)
	values.Set("version", version)
	return values.Encode()
}

func hasIdentityHeaders(header http.Header) bool {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), auditHeaderPrefix) {
			return true
		}
	}
	return false
}

// StripIdentityHeaders prevents internal customer metadata from reaching model providers.
func StripIdentityHeaders(header http.Header) {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), auditHeaderPrefix) {
			header.Del(name)
		}
	}
}

// VerifyIdentity validates the NewAPI HMAC envelope against method and actual CPA path.
func VerifyIdentity(request *http.Request, secret string, required bool, now time.Time, allowedSkew time.Duration) (Identity, error) {
	if request == nil {
		return Identity{}, fmt.Errorf("request is unavailable")
	}
	present := hasIdentityHeaders(request.Header)
	if !present {
		if required {
			return Identity{}, fmt.Errorf("signed CPA audit identity is required")
		}
		return Identity{}, nil
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Identity{}, fmt.Errorf("CPA audit identity secret is not configured")
	}

	version := strings.TrimSpace(request.Header.Get(auditHeaderVersion))
	userIDText := strings.TrimSpace(request.Header.Get(auditHeaderUserID))
	tokenIDText := strings.TrimSpace(request.Header.Get(auditHeaderTokenID))
	tokenName := sanitizeIdentityText(request.Header.Get(auditHeaderTokenName), 128)
	requestID := sanitizeIdentityText(request.Header.Get(auditHeaderRequestID), 128)
	timestampText := strings.TrimSpace(request.Header.Get(auditHeaderTimestamp))
	model := sanitizeIdentityText(request.Header.Get(auditHeaderModel), 256)
	channelTest := strings.TrimSpace(request.Header.Get(auditHeaderChannelTest)) == "1"
	channelTestValue := "0"
	if channelTest {
		channelTestValue = "1"
	}
	signatureText := strings.TrimSpace(request.Header.Get(auditHeaderSignature))
	if version != auditIdentityVersion {
		return Identity{}, fmt.Errorf("unsupported CPA audit identity version")
	}
	userID, err := strconv.ParseInt(userIDText, 10, 64)
	if err != nil || userID < 0 || !channelTest && userID == 0 {
		return Identity{}, fmt.Errorf("invalid CPA audit user id")
	}
	tokenID, err := strconv.ParseInt(tokenIDText, 10, 64)
	if err != nil || tokenID < 0 || !channelTest && tokenID == 0 {
		return Identity{}, fmt.Errorf("invalid CPA audit token id")
	}
	if requestID == "" {
		return Identity{}, fmt.Errorf("invalid CPA audit request id")
	}
	if channelTest && (userID != 0 || tokenID != 0 || tokenName != "__channel_test__") {
		return Identity{}, fmt.Errorf("invalid CPA audit channel test identity")
	}
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid CPA audit timestamp")
	}
	if allowedSkew <= 0 {
		allowedSkew = 5 * time.Minute
	}
	delta := now.Sub(time.Unix(timestamp, 0))
	if delta < -allowedSkew || delta > allowedSkew {
		return Identity{}, fmt.Errorf("expired CPA audit identity timestamp")
	}
	providedSignature, err := hex.DecodeString(signatureText)
	if err != nil || len(providedSignature) != sha256.Size {
		return Identity{}, fmt.Errorf("invalid CPA audit signature")
	}
	path := request.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := identityCanonicalValues(
		version,
		timestampText,
		requestID,
		userIDText,
		tokenIDText,
		tokenName,
		request.Method,
		path,
		model,
		channelTestValue,
	)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	if !hmac.Equal(providedSignature, mac.Sum(nil)) {
		return Identity{}, fmt.Errorf("invalid CPA audit signature")
	}
	return Identity{
		UserID:      userID,
		TokenID:     tokenID,
		TokenName:   tokenName,
		RequestID:   requestID,
		Model:       model,
		Verified:    true,
		ChannelTest: channelTest,
	}, nil
}

func sanitizeIdentityText(value string, maxRunes int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', 0:
			return -1
		default:
			return r
		}
	}, value))
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}
