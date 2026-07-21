package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

type legacyStatusFailure struct {
	status int
	text   string
}

func (e legacyStatusFailure) Error() string   { return e.text }
func (e legacyStatusFailure) StatusCode() int { return e.status }

func TestStatusFromErrorTypedAndLegacyParity(t *testing.T) {
	legacy := legacyStatusFailure{status: http.StatusTooManyRequests, text: "rate limited"}
	typed := &failurecontract.Failure{
		Kind:          failurecontract.RateLimited,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusTooManyRequests,
		Retryable:     true,
		PublicMessage: "rate limited",
	}

	if got := statusFromError(legacy); got != legacy.status {
		t.Fatalf("legacy status = %d, want %d", got, legacy.status)
	}
	if got := statusFromError(typed); got != legacy.status {
		t.Fatalf("typed status = %d, want legacy %d", got, legacy.status)
	}
	if got := executionErrorMessage(typed); got.StatusCode != legacy.status || got.Error.Error() != legacy.Error() {
		t.Fatalf("typed message = %#v, want status/message parity", got)
	}
}

func TestStatusFromErrorUsesErrorsAsForLegacyFallback(t *testing.T) {
	legacy := legacyStatusFailure{status: http.StatusBadGateway, text: "upstream failed"}
	wrapped := fmt.Errorf("executor: %w", legacy)
	if got := statusFromError(wrapped); got != legacy.status {
		t.Fatalf("wrapped legacy status = %d, want %d", got, legacy.status)
	}
	if got := executionErrorMessage(wrapped); got.StatusCode != legacy.status {
		t.Fatalf("wrapped legacy message status = %d, want %d", got.StatusCode, legacy.status)
	}
	if !errors.Is(wrapped, legacy) {
		t.Fatal("test wrapper does not preserve legacy error")
	}
}

func TestStatusFromErrorUsesErrorsAsForTypedFailure(t *testing.T) {
	typed := &failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusServiceUnavailable,
		Retryable:     true,
		PublicMessage: "provider unavailable",
	}
	wrapped := fmt.Errorf("executor: %w", typed)
	if got := statusFromError(wrapped); got != typed.HTTPStatus {
		t.Fatalf("wrapped typed status = %d, want %d", got, typed.HTTPStatus)
	}
	if got := executionErrorMessage(wrapped); got.StatusCode != typed.HTTPStatus {
		t.Fatalf("wrapped typed message status = %d, want %d", got.StatusCode, typed.HTTPStatus)
	}
}
