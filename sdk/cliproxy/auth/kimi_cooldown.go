package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// kimiHardCooldownUntil protects an active permission or quota backoff from
// early probes. The latest applicable deadline governs this credential.
func kimiHardCooldownUntil(a *Auth, model string, now time.Time) time.Time {
	if !isKimiCredentialAuth(a) {
		return time.Time{}
	}
	var until time.Time
	if blocked, _, next := authLevelBlockState(a, now); blocked && (a.Quota.Exceeded || isForbiddenStatus(a.LastError)) {
		until = next
	}
	s := a.ModelStates[model]
	if s == nil {
		s = a.ModelStates[canonicalModelKey(model)]
	}
	if s != nil && s.Unavailable && s.NextRetryAfter.After(now) && (s.Quota.Exceeded || isForbiddenStatus(s.LastError)) {
		next := s.NextRetryAfter
		if s.Quota.NextRecoverAt.After(next) {
			next = s.Quota.NextRecoverAt
		}
		if next.After(until) {
			until = next
		}
	}
	h := resolveHealthState(a, model)
	if h.BreakerState == HealthBreakerOpen && h.OpenUntil.After(now) && h.OpenUntil.After(until) &&
		(h.LastStatusCode == http.StatusForbidden || h.LastStatusCode == http.StatusOK && h.lastFailureStatus == http.StatusForbidden) {
		until = h.OpenUntil
	}
	return until
}

func isForbiddenStatus(e *Error) bool { return e != nil && e.HTTPStatus == http.StatusForbidden }

// A private local selection error is the only source of the cross-protocol
// exhaustion signal. Check every registered entrance, even if this request
// pinned a provider or filtered some credentials. An unrelated ready route
// must remain reachable by the downstream gateway.
func (m *Manager) markKimiPoolExhausted(err error, model string, now time.Time) error {
	switch e := err.(type) {
	case *routeUnavailableError:
	case *Error:
		// The scheduler can omit suspended model registrations from its
		// shard. Reclassify only after independently verifying the whole pool.
		if e.Code != "auth_not_found" {
			return err
		}
	default:
		return err
	}
	if !strings.HasPrefix(strings.ToLower(model), "kimi-") || m.HomeEnabled() || m.hasPluginScheduler() {
		return err
	}
	reg := registry.GetGlobalRegistry()
	m.mu.RLock()
	defer m.mu.RUnlock()
	var earliest time.Time
	for _, a := range m.auths {
		if a == nil || a.Disabled || a.Status == StatusDisabled || !m.authSupportsRouteModel(reg, a, model) {
			continue
		}
		until := kimiHardCooldownUntil(a, m.selectionModelForAuth(a, model), now)
		if !until.After(now) {
			return err
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	if earliest.IsZero() {
		return err
	}
	wait := earliest.Sub(now)
	local := newRouteUnavailableError(&wait)
	local.poolExhausted = true
	return local
}

// KimiPoolExhausted reports only verified gateway-wide local selection failures.
func KimiPoolExhausted(err error) bool {
	var local *routeUnavailableError
	return errors.As(err, &local) && local.poolExhausted
}
