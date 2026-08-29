package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const kimiInsufficientQuotaMaxAccountsPerRequest = 2

func isKimiCredentialAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "kimi") ||
		strings.EqualFold(strings.TrimSpace(executorKeyFromAuth(auth)), "kimi") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	for _, key := range []string{"compat_kind", "compat_name"} {
		if strings.EqualFold(strings.TrimSpace(auth.Attributes[key]), "kimi") {
			return true
		}
	}
	return false
}

func kimiCredentialSecret(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if apiKey := strings.TrimSpace(auth.Attributes[AttributeAPIKey]); apiKey != "" {
			return apiKey
		}
	}
	if auth.Metadata != nil {
		if accessToken, ok := auth.Metadata["access_token"].(string); ok {
			return strings.TrimSpace(accessToken)
		}
	}
	return ""
}

// kimiCredentialFingerprint identifies one physical Kimi credential across
// protocol-specific auth records without retaining or logging the raw secret.
func kimiCredentialFingerprint(auth *Auth) string {
	if !isKimiCredentialAuth(auth) {
		return ""
	}
	secret := kimiCredentialSecret(auth)
	if secret == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

func kimiCredentialAttemptKey(auth *Auth) string {
	if fingerprint := kimiCredentialFingerprint(auth); fingerprint != "" {
		return "credential:" + fingerprint
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return ""
	}
	return "auth:" + strings.TrimSpace(auth.ID)
}

func isKimiInsufficientQuotaError(auth *Auth, err error) bool {
	if !isKimiCredentialAuth(auth) || err == nil {
		return false
	}
	if failure := failurecontract.Classify(err); failure != nil {
		if failure.Kind == failurecontract.QuotaExceeded && failure.Scope == failurecontract.ScopeCredential &&
			typedFailureHasSemanticIdentifier(failure, "insufficient_quota") {
			return true
		}
	}
	if strings.EqualFold(strings.TrimSpace(errorCodeFromError(err)), "insufficient_quota") {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "insufficient_quota")
}

func shouldShareKimiAccountQuota(auth *Auth, result Result) bool {
	if !isKimiCredentialAuth(auth) || result.Success {
		return false
	}
	if failure, ok := typedFailureFromResult(result); ok {
		return isTypedAccountQuotaExhaustedFailure(failure)
	}
	if isAccountQuotaExhaustedResultError(result.Error) {
		return true
	}
	return result.Error != nil && strings.EqualFold(strings.TrimSpace(result.Error.Code), "insufficient_quota")
}

// applyKimiSharedAccountQuotaLocked mirrors an account-wide quota cooldown to
// protocol aliases backed by the same Kimi secret. The caller must hold m.mu.
func (m *Manager) applyKimiSharedAccountQuotaLocked(ctx context.Context, selected *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) []*Auth {
	if m == nil || selected == nil {
		return nil
	}
	fingerprint := kimiCredentialFingerprint(selected)
	if fingerprint == "" {
		return nil
	}

	snapshots := make([]*Auth, 0, 1)
	for _, peer := range m.auths {
		if peer == nil || peer.ID == selected.ID || peer.Disabled || peer.Status == StatusDisabled ||
			kimiCredentialFingerprint(peer) != fingerprint {
			continue
		}
		applyAccountQuotaFailureState(peer, nil, resultErr, retryAfter, now)
		if errPersist := m.persist(ctx, peer); errPersist != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", peer.ID).Warnf("failed to persist shared Kimi quota state: %v", errPersist)
		}
		snapshots = append(snapshots, peer.Clone())
	}
	return snapshots
}

func (m *Manager) markKimiCredentialAliasesTried(tried map[string]struct{}, selected *Auth) {
	if m == nil || tried == nil || selected == nil {
		return
	}
	fingerprint := kimiCredentialFingerprint(selected)
	if fingerprint == "" {
		return
	}
	m.mu.RLock()
	for _, peer := range m.auths {
		if peer != nil && kimiCredentialFingerprint(peer) == fingerprint {
			tried[peer.ID] = struct{}{}
		}
	}
	m.mu.RUnlock()
}

func (t *requestAttemptTrace) allowKimiInsufficientQuotaFallback(accountKey string) bool {
	if t == nil || strings.TrimSpace(accountKey) == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.kimiQuotaAccounts == nil {
		t.kimiQuotaAccounts = make(map[string]struct{}, kimiInsufficientQuotaMaxAccountsPerRequest)
	}
	if _, alreadyTried := t.kimiQuotaAccounts[accountKey]; alreadyTried {
		return false
	}
	t.kimiQuotaAccounts[accountKey] = struct{}{}
	return len(t.kimiQuotaAccounts) < kimiInsufficientQuotaMaxAccountsPerRequest
}

func (m *Manager) prepareKimiInsufficientQuotaFallback(ctx context.Context, tried map[string]struct{}, localAccounts map[string]struct{}, auth *Auth, err error) bool {
	if !isKimiInsufficientQuotaError(auth, err) {
		return false
	}
	m.markKimiCredentialAliasesTried(tried, auth)
	accountKey := kimiCredentialAttemptKey(auth)
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		return trace.allowKimiInsufficientQuotaFallback(accountKey)
	}
	if accountKey == "" || localAccounts == nil {
		return false
	}
	if _, alreadyTried := localAccounts[accountKey]; alreadyTried {
		return false
	}
	localAccounts[accountKey] = struct{}{}
	return len(localAccounts) < kimiInsufficientQuotaMaxAccountsPerRequest
}
