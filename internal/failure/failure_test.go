package failure

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type legacyFailure struct {
	status     int
	code       string
	retryAfter *time.Duration
}

func (e *legacyFailure) Error() string              { return "legacy failure" }
func (e *legacyFailure) StatusCode() int            { return e.status }
func (e *legacyFailure) ErrorCode() string          { return e.code }
func (e *legacyFailure) RetryAfter() *time.Duration { return e.retryAfter }

type outerLegacyFailure struct {
	cause      error
	status     int
	code       string
	retryAfter *time.Duration
}

func (e *outerLegacyFailure) Error() string              { return "outer legacy failure" }
func (e *outerLegacyFailure) Unwrap() error              { return e.cause }
func (e *outerLegacyFailure) StatusCode() int            { return e.status }
func (e *outerLegacyFailure) ErrorCode() string          { return e.code }
func (e *outerLegacyFailure) RetryAfter() *time.Duration { return e.retryAfter }

func TestClassifyTypedFailure(t *testing.T) {
	cause := errors.New("private upstream detail")
	retryAfter := 30 * time.Second
	err := fmt.Errorf("executor: %w", &Failure{
		Kind:          QuotaExceeded,
		Scope:         ScopeCredential,
		HTTPStatus:    http.StatusTooManyRequests,
		ProviderCode:  "insufficient_quota",
		RetryAfter:    &retryAfter,
		Retryable:     true,
		Cause:         cause,
		PublicMessage: "quota exceeded",
	})

	classified := Classify(err)
	if classified == nil {
		t.Fatal("Classify() = nil")
	}
	if classified.Kind != QuotaExceeded || classified.Scope != ScopeCredential {
		t.Fatalf("classification = %q/%q", classified.Kind, classified.Scope)
	}
	if classified.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d", classified.HTTPStatus)
	}
	if classified.ProviderCode != "insufficient_quota" {
		t.Fatalf("ProviderCode = %q", classified.ProviderCode)
	}
	if classified.RetryAfter == nil || *classified.RetryAfter != retryAfter || !classified.Retryable {
		t.Fatalf("retry metadata = %v/%t", classified.RetryAfter, classified.Retryable)
	}
	if classified.PublicMessage != "quota exceeded" {
		t.Fatalf("PublicMessage = %q", classified.PublicMessage)
	}
	if !errors.Is(classified, cause) {
		t.Fatal("classified failure does not preserve Cause")
	}
}

func TestClassifyLegacyFallbackParity(t *testing.T) {
	retryAfter := 12 * time.Second
	legacy := &legacyFailure{
		status:     http.StatusTooManyRequests,
		code:       "rate_limit",
		retryAfter: &retryAfter,
	}
	err := fmt.Errorf("wrapped: %w", legacy)

	classified := Classify(err)
	if classified == nil {
		t.Fatal("Classify() = nil")
	}
	if classified.Kind != "" || classified.Scope != "" {
		t.Fatalf("legacy failure was guessed as %q/%q", classified.Kind, classified.Scope)
	}
	if classified.HTTPStatus != legacy.StatusCode() {
		t.Fatalf("HTTPStatus = %d, want %d", classified.HTTPStatus, legacy.StatusCode())
	}
	if classified.ProviderCode != legacy.ErrorCode() {
		t.Fatalf("ProviderCode = %q, want %q", classified.ProviderCode, legacy.ErrorCode())
	}
	if classified.RetryAfter == nil || *classified.RetryAfter != retryAfter {
		t.Fatalf("RetryAfter = %v, want %v", classified.RetryAfter, retryAfter)
	}
	if classified.PublicMessage != err.Error() || !errors.Is(classified, legacy) {
		t.Fatalf("legacy cause/message not preserved: %#v", classified)
	}
	if got := HTTPStatusOf(err); got != legacy.StatusCode() {
		t.Fatalf("HTTPStatusOf() = %d, want %d", got, legacy.StatusCode())
	}
	if got := ProviderCodeOf(err); got != legacy.ErrorCode() {
		t.Fatalf("ProviderCodeOf() = %q, want %q", got, legacy.ErrorCode())
	}
	if got, ok := RetryAfterOf(err); !ok || got != retryAfter {
		t.Fatalf("RetryAfterOf() = %v/%t, want %v/true", got, ok, retryAfter)
	}
}

func TestTypedFailureFallsBackToLegacyCauseMetadata(t *testing.T) {
	retryAfter := 5 * time.Second
	cause := &legacyFailure{
		status:     http.StatusServiceUnavailable,
		code:       "overloaded",
		retryAfter: &retryAfter,
	}
	typed := &Failure{
		Kind:          ProviderUnavailable,
		Scope:         ScopeProvider,
		Retryable:     true,
		Cause:         cause,
		PublicMessage: "provider unavailable",
	}

	classified := Classify(typed)
	if classified.HTTPStatus != cause.StatusCode() || classified.ProviderCode != cause.ErrorCode() {
		t.Fatalf("legacy cause metadata = %d/%q", classified.HTTPStatus, classified.ProviderCode)
	}
	if classified.RetryAfter == nil || *classified.RetryAfter != retryAfter {
		t.Fatalf("RetryAfter = %v, want %v", classified.RetryAfter, retryAfter)
	}
	if typed.StatusCode() != cause.StatusCode() || typed.ErrorCode() != cause.ErrorCode() {
		t.Fatalf("legacy accessors = %d/%q", typed.StatusCode(), typed.ErrorCode())
	}
}

func TestTypedFailureFallsBackToOuterLegacyMetadata(t *testing.T) {
	retryAfter := 9 * time.Second
	typed := &Failure{
		Kind:          ProviderUnavailable,
		Scope:         ScopeProvider,
		Retryable:     true,
		PublicMessage: "provider unavailable",
	}
	err := &outerLegacyFailure{
		cause:      typed,
		status:     http.StatusServiceUnavailable,
		code:       "outer_overloaded",
		retryAfter: &retryAfter,
	}

	classified := Classify(err)
	if classified.HTTPStatus != err.StatusCode() || classified.ProviderCode != err.ErrorCode() {
		t.Fatalf("outer legacy metadata = %d/%q", classified.HTTPStatus, classified.ProviderCode)
	}
	if classified.RetryAfter == nil || *classified.RetryAfter != retryAfter {
		t.Fatalf("RetryAfter = %v, want %v", classified.RetryAfter, retryAfter)
	}
	if got, ok := RetryAfterOf(err); !ok || got != retryAfter {
		t.Fatalf("RetryAfterOf() = %v/%t, want %v/true", got, ok, retryAfter)
	}
}

func TestTypedFailurePreservesExplicitZeroRetryAfter(t *testing.T) {
	zero := time.Duration(0)
	typed := &Failure{RetryAfter: &zero}
	classified := Classify(typed)
	if classified.RetryAfter == nil || *classified.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want explicit zero", classified.RetryAfter)
	}
	if classified.RetryAfter == typed.RetryAfter {
		t.Fatal("Classify returned an aliased RetryAfter pointer")
	}
	if got, ok := RetryAfterOf(typed); !ok || got != 0 {
		t.Fatalf("RetryAfterOf() = %v/%t, want 0/true", got, ok)
	}
}

func TestFailureErrorDoesNotExposeCauseWhenKindIsKnown(t *testing.T) {
	err := &Failure{
		Kind:  InternalTransformError,
		Scope: ScopeRequest,
		Cause: errors.New("sensitive transform detail"),
	}
	if got := err.Error(); got != string(InternalTransformError) {
		t.Fatalf("Error() = %q, want %q", got, InternalTransformError)
	}
	if !errors.Is(err, err.Cause) {
		t.Fatal("Cause is not available through errors.Is")
	}
}

func TestLegacyNonPositiveStatusRemainsUnset(t *testing.T) {
	for _, status := range []int{0, -1} {
		legacy := &legacyFailure{status: status}
		if got := HTTPStatusOf(legacy); got != 0 {
			t.Fatalf("HTTPStatusOf(status=%d) = %d, want 0", status, got)
		}
	}
}

func TestClassifyNil(t *testing.T) {
	if got := Classify(nil); got != nil {
		t.Fatalf("Classify(nil) = %#v", got)
	}
	if got := HTTPStatusOf(nil); got != 0 {
		t.Fatalf("HTTPStatusOf(nil) = %d", got)
	}
	if _, ok := RetryAfterOf(nil); ok {
		t.Fatal("RetryAfterOf(nil) unexpectedly found a delay")
	}
}
