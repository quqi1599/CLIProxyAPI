package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type failureScopeMatrixExecutor struct {
	failure error
	calls   atomic.Int32
}

func (executor *failureScopeMatrixExecutor) Identifier() string { return "failure-matrix" }

func (executor *failureScopeMatrixExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	executor.calls.Add(1)
	return cliproxyexecutor.Response{}, executor.failure
}

func (executor *failureScopeMatrixExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	executor.calls.Add(1)
	return nil, executor.failure
}

func (executor *failureScopeMatrixExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (executor *failureScopeMatrixExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	executor.calls.Add(1)
	return cliproxyexecutor.Response{}, executor.failure
}

func (executor *failureScopeMatrixExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerTypedFailureRetryAndCooldownMatrix(t *testing.T) {
	operations := []struct {
		name   string
		invoke func(context.Context, *Manager, cliproxyexecutor.Request) error
	}{
		{
			name: "execute",
			invoke: func(ctx context.Context, manager *Manager, request cliproxyexecutor.Request) error {
				_, err := manager.Execute(ctx, []string{"failure-matrix"}, request, cliproxyexecutor.Options{})
				return err
			},
		},
		{
			name: "count",
			invoke: func(ctx context.Context, manager *Manager, request cliproxyexecutor.Request) error {
				_, err := manager.ExecuteCount(ctx, []string{"failure-matrix"}, request, cliproxyexecutor.Options{})
				return err
			},
		},
		{
			name: "stream",
			invoke: func(ctx context.Context, manager *Manager, request cliproxyexecutor.Request) error {
				_, err := manager.ExecuteStream(ctx, []string{"failure-matrix"}, request, cliproxyexecutor.Options{})
				return err
			},
		},
	}
	retryNow := time.Duration(0)
	scopes := []struct {
		name           string
		failure        *failurecontract.Failure
		wantCalls      int32
		wantModel      bool
		wantCredential bool
	}{
		{
			name: "request",
			failure: &failurecontract.Failure{
				Kind: failurecontract.InvalidRequest, Scope: failurecontract.ScopeRequest,
				HTTPStatus: http.StatusBadRequest, PublicMessage: "invalid request",
			},
			wantCalls: 1,
		},
		{
			name: "model",
			failure: &failurecontract.Failure{
				Kind: failurecontract.ModelUnavailable, Scope: failurecontract.ScopeModel,
				HTTPStatus: http.StatusNotFound, PublicMessage: "model unavailable",
			},
			wantCalls: 1,
			wantModel: true,
		},
		{
			name: "credential",
			failure: &failurecontract.Failure{
				Kind: failurecontract.RateLimited, Scope: failurecontract.ScopeCredential,
				HTTPStatus: http.StatusTooManyRequests, Retryable: true, RetryAfter: &retryNow,
				PublicMessage: "credential rate limited",
			},
			wantCalls:      2,
			wantCredential: true,
		},
		{
			name: "provider",
			failure: &failurecontract.Failure{
				Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
				HTTPStatus: http.StatusServiceUnavailable, Retryable: true, RetryAfter: &retryNow,
				PublicMessage: "provider unavailable",
			},
			wantCalls: 2,
		},
	}

	for _, operation := range operations {
		for _, scope := range scopes {
			t.Run(operation.name+"/"+scope.name, func(t *testing.T) {
				const model = "failure-matrix-model"
				authID := "failure-matrix-" + operation.name + "-" + scope.name
				executor := &failureScopeMatrixExecutor{failure: scope.failure}
				manager := NewManager(nil, nil, nil)
				manager.SetRetryConfig(1, time.Second, 0)
				manager.SetRetryQueueDelay(time.Hour)
				manager.RegisterExecutor(executor)

				modelRegistry := registry.GetGlobalRegistry()
				modelRegistry.RegisterClient(authID, executor.Identifier(), []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
				if _, err := manager.Register(context.Background(), &Auth{ID: authID, Provider: executor.Identifier()}); err != nil {
					t.Fatalf("register auth: %v", err)
				}

				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				defer cancel()
				if err := operation.invoke(ctx, manager, cliproxyexecutor.Request{Model: model}); err == nil {
					t.Fatal("operation unexpectedly succeeded")
				}
				if got := executor.calls.Load(); got != scope.wantCalls {
					t.Fatalf("executor calls = %d, want %d", got, scope.wantCalls)
				}

				updated, ok := manager.GetByID(authID)
				if !ok {
					t.Fatal("registered auth disappeared")
				}
				modelState := updated.ModelStates[model]
				if got := modelState != nil; got != scope.wantModel {
					t.Fatalf("model state present = %t, want %t; state=%+v", got, scope.wantModel, modelState)
				}
				if updated.Quota.Exceeded != scope.wantCredential {
					t.Fatalf("credential quota state = %t, want %t; auth=%+v", updated.Quota.Exceeded, scope.wantCredential, updated)
				}
				if scope.wantModel {
					if modelState.Status != StatusDisabled || !modelState.Unavailable || modelState.LastError == nil {
						t.Fatalf("model failure state = %+v", modelState)
					}
					if modelState.LastError.Kind != string(scope.failure.Kind) || modelState.LastError.Scope != string(scope.failure.Scope) {
						t.Fatalf("model typed metadata = %+v", modelState.LastError)
					}
				}
				if scope.wantCredential {
					if updated.LastError == nil || updated.LastError.Kind != string(scope.failure.Kind) || updated.LastError.Scope != string(scope.failure.Scope) {
						t.Fatalf("credential typed metadata = %+v", updated.LastError)
					}
				}
			})
		}
	}
}

func TestTypedFailureUnauthorizedEvictionMatrix(t *testing.T) {
	tests := []struct {
		name  string
		kind  failurecontract.Kind
		scope failurecontract.Scope
		want  bool
	}{
		{name: "request", kind: failurecontract.InvalidRequest, scope: failurecontract.ScopeRequest},
		{name: "model", kind: failurecontract.ModelUnavailable, scope: failurecontract.ScopeModel},
		{name: "credential", kind: failurecontract.AuthenticationFailed, scope: failurecontract.ScopeCredential, want: true},
		{name: "provider", kind: failurecontract.ProviderUnavailable, scope: failurecontract.ScopeProvider},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := &failurecontract.Failure{
				Kind: test.kind, Scope: test.scope, HTTPStatus: http.StatusUnauthorized,
				PublicMessage: "typed unauthorized failure",
			}
			if got := shouldEvictUnauthorizedError(err); got != test.want {
				t.Fatalf("shouldEvictUnauthorizedError() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAuthResultLogIncludesTypedFailureMetadata(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetLevel(previousLevel) })

	manager := NewManager(nil, nil, nil)
	typed := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusServiceUnavailable, Retryable: true,
		PublicMessage: "provider unavailable",
	}
	manager.logAuthResultMetric(context.Background(), &Auth{ID: "typed-log", Provider: "failure-matrix"}, Result{
		Provider: "failure-matrix",
		Model:    "failure-matrix-model",
		Error:    resultErrorFromCause(typed),
		Cause:    typed,
	})

	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	if got := entries[0].Data["failure_kind"]; got != string(failurecontract.ProviderUnavailable) {
		t.Fatalf("failure_kind = %#v", got)
	}
	if got := entries[0].Data["failure_scope"]; got != string(failurecontract.ScopeProvider) {
		t.Fatalf("failure_scope = %#v", got)
	}
}
