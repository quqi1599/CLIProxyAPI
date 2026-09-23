package auth

import (
	"net/http"
	"strings"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

// routeUnavailableError marks a local selection failure, not an upstream 503.
// Keeping this distinction prevents terminal normalization from hiding the
// auth_unavailable contract used by downstream gateways to avoid empty retries.
type routeUnavailableError struct {
	*failurecontract.Failure
}

func (e *routeUnavailableError) Unwrap() error { return e.Failure }

func newRouteUnavailableError(resetIn *time.Duration) *routeUnavailableError {
	var retryAfter *time.Duration
	if resetIn != nil {
		wait := *resetIn
		if wait < 0 {
			wait = 0
		}
		retryAfter = &wait
	}
	return &routeUnavailableError{Failure: &failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusServiceUnavailable,
		OuterStatus:   http.StatusServiceUnavailable,
		ProviderCode:  "auth_unavailable",
		SemanticCode:  "auth_unavailable",
		SemanticType:  "server_error",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		RetryAfter:    retryAfter,
		Retryable:     true,
		PublicMessage: "auth_unavailable: no auth available",
		Cause: &Error{
			Code:       "auth_unavailable",
			Message:    "no auth available",
			HTTPStatus: http.StatusServiceUnavailable,
			Retryable:  true,
		},
	}}
}

// blockedRouteSelectionError is shared by the incremental scheduler and direct
// selectors. An unavailable route is not rate limited unless every candidate
// was blocked by a verified quota/rate cause.
func blockedRouteSelectionError(model, provider string, total, rateLimitedCount int, earliest, now time.Time) error {
	if total == 0 {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	var retryAfter *time.Duration
	if !earliest.IsZero() {
		wait := earliest.Sub(now)
		if wait < 0 {
			wait = 0
		}
		retryAfter = &wait
	}
	if rateLimitedCount == total && retryAfter != nil {
		if provider == "mixed" {
			provider = ""
		}
		return newModelCooldownError(model, provider, *retryAfter)
	}
	return newRouteUnavailableError(retryAfter)
}

// routeTemporalBlockIsRateLimited requires every active blocker for this
// candidate to be quota/rate related. Authentication or transport failures must
// not become 429 merely because their recovery is time based, even if a stale
// quota flag or a second rate-limited blocker is also present.
func routeTemporalBlockIsRateLimited(auth *Auth, model string, now time.Time, healthBlocked bool) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	rateLimited := false
	if healthBlocked {
		if resolveHealthState(auth, model).LastStatusCode != http.StatusTooManyRequests {
			return false
		}
		rateLimited = true
	}
	if blocked, reason, _ := authLevelBlockState(auth, now); blocked {
		if reason != blockReasonCooldown || !rateLimitCompatibleLastError(auth.LastError) {
			return false
		}
		rateLimited = true
	}
	state := auth.ModelStates[model]
	if state == nil {
		state = auth.ModelStates[canonicalModelKey(model)]
	}
	if state != nil && state.Status == StatusDisabled {
		return false
	}
	if state != nil && state.Unavailable && state.NextRetryAfter.After(now) {
		if !state.Quota.Exceeded || !rateLimitCompatibleLastError(state.LastError) {
			return false
		}
		rateLimited = true
	}
	return rateLimited
}

func rateLimitCompatibleLastError(err *Error) bool {
	if err == nil {
		return true
	}
	if err.HTTPStatus > 0 && err.HTTPStatus != http.StatusTooManyRequests {
		return false
	}
	if err.Kind != "" {
		return err.Kind == string(failurecontract.RateLimited) || err.Kind == string(failurecontract.QuotaExceeded)
	}
	if err.HTTPStatus == http.StatusTooManyRequests {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(err.Code)) {
	case "", "rate_limit", "rate_limit_error", "rate_limit_exceeded", "quota_exceeded", "insufficient_quota", "insufficient_balance", "model_cooldown":
		return true
	default:
		return false
	}
}
