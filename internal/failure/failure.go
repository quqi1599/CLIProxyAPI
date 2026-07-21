// Package failure defines the provider-agnostic execution failure contract.
package failure

import (
	"errors"
	"strings"
	"time"
)

// Kind identifies the stable failure family. The zero value is unclassified.
type Kind string

const (
	InvalidRequest         Kind = "invalid_request"
	RequestTooLarge        Kind = "request_too_large"
	UnsupportedFeature     Kind = "unsupported_feature"
	ContextLengthExceeded  Kind = "context_length_exceeded"
	ContentSafetyBlocked   Kind = "content_safety_blocked"
	InvalidThinkingHistory Kind = "invalid_thinking_history"
	InvalidSignature       Kind = "invalid_signature"
	AuthenticationFailed   Kind = "authentication_failed"
	QuotaExceeded          Kind = "quota_exceeded"
	RateLimited            Kind = "rate_limited"
	ModelUnavailable       Kind = "model_unavailable"
	ProviderUnavailable    Kind = "provider_unavailable"
	UpstreamProtocolError  Kind = "upstream_protocol_error"
	TransportError         Kind = "transport_error"
	Cancelled              Kind = "cancelled"
	InternalTransformError Kind = "internal_transform_error"
)

// Scope identifies which routing object a failure applies to. The zero value
// leaves legacy failures unscoped until an executor classifies them.
type Scope string

const (
	ScopeRequest    Scope = "request"
	ScopeModel      Scope = "model"
	ScopeCredential Scope = "credential"
	ScopeProvider   Scope = "provider"
)

// Failure is the canonical execution failure contract.
//
// Cause is retained for diagnostics and legacy metadata fallback. PublicMessage
// is the only field intended for a client response.
type Failure struct {
	Kind         Kind
	Scope        Scope
	HTTPStatus   int
	ProviderCode string
	// RetryAfter is nil when no delay was supplied; a non-nil zero requests an immediate retry.
	RetryAfter    *time.Duration
	Retryable     bool
	Cause         error
	PublicMessage string
}

// Error returns the client-safe message when one is available.
func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	if message := strings.TrimSpace(f.PublicMessage); message != "" {
		return message
	}
	if f.Kind != "" {
		return string(f.Kind)
	}
	if f.Cause != nil {
		return f.Cause.Error()
	}
	return "failure"
}

// Unwrap exposes the diagnostic cause to errors.Is and errors.As.
func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

// StatusCode preserves the status accessor used by existing API and auth code.
func (f *Failure) StatusCode() int {
	if f == nil {
		return 0
	}
	if f.HTTPStatus > 0 {
		return f.HTTPStatus
	}
	return legacyHTTPStatus(f.Cause)
}

// ErrorCode preserves the provider-code accessor used by existing auth code.
func (f *Failure) ErrorCode() string {
	if f == nil {
		return ""
	}
	if code := strings.TrimSpace(f.ProviderCode); code != "" {
		return code
	}
	return legacyProviderCode(f.Cause)
}

// As returns the first typed Failure in err's unwrap chain.
func As(err error) (*Failure, bool) {
	if err == nil {
		return nil, false
	}
	var typed *Failure
	if !errors.As(err, &typed) || typed == nil {
		return nil, false
	}
	return typed, true
}

// Classify returns a detached typed view of err. Unknown legacy errors remain
// unclassified (zero Kind and Scope) while their established status, code,
// retry hint, cause, and message are preserved.
func Classify(err error) *Failure {
	if err == nil {
		return nil
	}
	if typed, ok := As(err); ok {
		classified := *typed
		if classified.HTTPStatus <= 0 {
			classified.HTTPStatus = legacyHTTPStatus(err)
		}
		if strings.TrimSpace(classified.ProviderCode) == "" {
			classified.ProviderCode = legacyProviderCode(err)
		}
		if classified.RetryAfter == nil {
			if retryAfter, okRetry := legacyRetryAfter(err); okRetry {
				classified.RetryAfter = durationPointer(retryAfter)
			}
		} else {
			classified.RetryAfter = durationPointer(*classified.RetryAfter)
		}
		if strings.TrimSpace(classified.PublicMessage) == "" {
			classified.PublicMessage = typed.Error()
		}
		return &classified
	}

	classified := &Failure{
		HTTPStatus:    legacyHTTPStatus(err),
		ProviderCode:  legacyProviderCode(err),
		Cause:         err,
		PublicMessage: err.Error(),
	}
	if retryAfter, ok := legacyRetryAfter(err); ok {
		classified.RetryAfter = durationPointer(retryAfter)
	}
	return classified
}

// HTTPStatusOf returns the typed status or the legacy StatusCode fallback.
func HTTPStatusOf(err error) int {
	classified := Classify(err)
	if classified == nil {
		return 0
	}
	return classified.HTTPStatus
}

// ProviderCodeOf returns the typed provider code or the legacy ErrorCode fallback.
func ProviderCodeOf(err error) string {
	classified := Classify(err)
	if classified == nil {
		return ""
	}
	return classified.ProviderCode
}

// RetryAfterOf returns the typed retry delay or the legacy RetryAfter fallback.
func RetryAfterOf(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if typed, ok := As(err); ok {
		if typed.RetryAfter != nil {
			return *typed.RetryAfter, true
		}
		return legacyRetryAfter(err)
	}
	return legacyRetryAfter(err)
}

func durationPointer(value time.Duration) *time.Duration {
	return &value
}

func legacyHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var provider statusCoder
	if errors.As(err, &provider) && provider != nil {
		if status := provider.StatusCode(); status > 0 {
			return status
		}
	}
	return 0
}

func legacyProviderCode(err error) string {
	if err == nil {
		return ""
	}
	type errorCoder interface {
		ErrorCode() string
	}
	var provider errorCoder
	if errors.As(err, &provider) && provider != nil {
		return strings.TrimSpace(provider.ErrorCode())
	}
	return ""
}

func legacyRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	var provider retryAfterProvider
	if !errors.As(err, &provider) || provider == nil {
		return 0, false
	}
	retryAfter := provider.RetryAfter()
	if retryAfter == nil {
		return 0, false
	}
	return *retryAfter, true
}
