package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const kimiInsufficientQuotaMaxAccountsPerRequest = 2

func sameKimiCredential(a, b *Auth) bool {
	fingerprint := kimiCredentialFingerprint(a)
	return fingerprint != "" && fingerprint == kimiCredentialFingerprint(b)
}

// preserveKimiRuntimeState keeps runtime observations across configuration
// reloads of the same secret. A changed secret or explicit disable is a new
// lifecycle; neither inherits the previous credential's cooldown.
func preserveKimiRuntimeState(next, current *Auth, now time.Time) {
	if next == nil || current == nil || next.Disabled || current.Disabled ||
		next.Status == StatusDisabled || current.Status == StatusDisabled || !sameKimiCredential(next, current) {
		return
	}
	next.Health = current.Health
	next.Success, next.Failed, next.recentRequests = current.Success, current.Failed, current.recentRequests
	if !current.Unavailable || !current.NextRetryAfter.After(now) {
		return
	}
	next.Unavailable = current.Unavailable
	next.NextRetryAfter = current.NextRetryAfter
	next.Quota = current.Quota
	next.LastError = cloneError(current.LastError)
	next.Status, next.StatusMessage = current.Status, current.StatusMessage
}

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

func isKimiPermissionRoutingError(auth *Auth, err error) bool {
	if !isKimiCredentialAuth(auth) || hasCommittedOutput(err) {
		return false
	}
	f, ok := failurecontract.As(err)
	if !ok || f.StatusCode() != http.StatusForbidden ||
		(f.Scope != failurecontract.ScopeModel && f.Scope != failurecontract.ScopeCredential) {
		return false
	}
	switch f.SemanticCode {
	case failurecontract.KimiServiceAccessDenied, failurecontract.KimiModelAccessDenied, failurecontract.KimiPermissionUnverified:
		return true
	default:
		return false
	}
}

func isKimiCredentialFallbackError(auth *Auth, err error) bool {
	// A configured model pool can still try a different model on this key.
	return isKimiInsufficientQuotaError(auth, err) ||
		(isKimiPermissionRoutingError(auth, err) && errorCodeFromError(err) == failurecontract.KimiServiceAccessDenied)
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

// applyKimiSharedCredentialCooldownLocked mirrors confirmed account-wide
// failures to protocol aliases for the same secret. The caller holds m.mu.
func (m *Manager) applyKimiSharedCredentialCooldownLocked(ctx context.Context, selected *Auth, result Result, failure *failurecontract.Failure, now time.Time) []*Auth {
	if !isKimiCredentialAuth(selected) || result.Success {
		return nil
	}
	quota := shouldShareKimiAccountQuota(selected, result)
	if !quota && (failure == nil || failure.Scope != failurecontract.ScopeCredential ||
		failure.SemanticCode != failurecontract.KimiServiceAccessDenied) {
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
		if quota {
			applyAccountQuotaFailureState(peer, nil, result.Error, result.RetryAfter, now)
		} else {
			applyTypedCredentialFailureState(peer, failure, result.Error, now, m.cooldownDisabledForAuth(peer))
		}
		if errPersist := m.persist(ctx, peer); errPersist != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", peer.ID).Warnf("failed to persist shared Kimi credential state: %v", errPersist)
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
