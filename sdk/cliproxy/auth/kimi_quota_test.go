package auth

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func kimiInsufficientQuotaTestFailure() error {
	return &failurecontract.Failure{
		Kind:          failurecontract.QuotaExceeded,
		Scope:         failurecontract.ScopeCredential,
		HTTPStatus:    http.StatusForbidden,
		OuterStatus:   http.StatusForbidden,
		ProviderCode:  "insufficient_quota",
		SemanticCode:  "insufficient_quota",
		SemanticType:  "insufficient_quota",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		Retryable:     true,
		PublicMessage: "quota is unavailable",
	}
}

func kimiQuotaTestAuth(id, provider, apiKey string) *Auth {
	return &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		Attributes: map[string]string{
			AttributeAPIKey: apiKey,
			"compat_kind":   "kimi",
		},
	}
}

func TestManagerMarkResult_KimiAccountQuotaCoolsProtocolAliases(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	claudeAlias := kimiQuotaTestAuth("kimi-claude-alias", "claude", "shared-kimi-key")
	openAIAlias := kimiQuotaTestAuth("kimi-openai-alias", "kimi", "shared-kimi-key")
	otherAccount := kimiQuotaTestAuth("kimi-other-account", "kimi", "other-kimi-key")
	nonKimi := &Auth{
		ID:       "non-kimi-same-secret",
		Provider: "claude",
		Status:   StatusActive,
		Attributes: map[string]string{
			AttributeAPIKey: "shared-kimi-key",
		},
	}
	for _, auth := range []*Auth{claudeAlias, openAIAlias, otherAccount, nonKimi} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   claudeAlias.ID,
		Provider: claudeAlias.Provider,
		Model:    "kimi-k3",
		Cause:    kimiInsufficientQuotaTestFailure(),
	})

	for _, authID := range []string{claudeAlias.ID, openAIAlias.ID} {
		updated, ok := manager.GetByID(authID)
		if !ok {
			t.Fatalf("auth %s not found", authID)
		}
		if !updated.Unavailable || !updated.Quota.Exceeded || updated.Quota.Reason != "billing_cycle_quota" {
			t.Fatalf("shared Kimi quota state for %s = unavailable:%v quota:%+v", authID, updated.Unavailable, updated.Quota)
		}
		if updated.LastError == nil || updated.LastError.Code != "insufficient_quota" {
			t.Fatalf("shared Kimi quota error for %s = %+v", authID, updated.LastError)
		}
	}
	for _, authID := range []string{otherAccount.ID, nonKimi.ID} {
		updated, ok := manager.GetByID(authID)
		if !ok {
			t.Fatalf("auth %s not found", authID)
		}
		if updated.Unavailable || updated.Quota.Exceeded {
			t.Fatalf("unrelated auth %s inherited Kimi quota state: %+v", authID, updated)
		}
	}
}

type kimiQuotaTestRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *kimiQuotaTestRecorder) record(authID string) {
	r.mu.Lock()
	r.calls = append(r.calls, authID)
	r.mu.Unlock()
}

func (r *kimiQuotaTestRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type kimiQuotaTestExecutor struct {
	id       string
	recorder *kimiQuotaTestRecorder
	failures map[string]error
}

func (e *kimiQuotaTestExecutor) Identifier() string { return e.id }

func (e *kimiQuotaTestExecutor) execute(auth *Auth) error {
	if auth == nil {
		return nil
	}
	e.recorder.record(auth.ID)
	return e.failures[auth.ID]
}

func (e *kimiQuotaTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.execute(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *kimiQuotaTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.execute(auth); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *kimiQuotaTestExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.execute(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func (e *kimiQuotaTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *kimiQuotaTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func TestManager_KimiInsufficientQuotaFallbackIsBoundedByPhysicalAccount(t *testing.T) {
	const model = "kimi-k3"
	tests := []struct {
		name   string
		invoke func(*Manager) error
	}{
		{
			name: "execute",
			invoke: func(manager *Manager) error {
				_, errExecute := manager.Execute(context.Background(), []string{"claude", "kimi"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "count",
			invoke: func(manager *Manager) error {
				_, errExecute := manager.ExecuteCount(context.Background(), []string{"claude", "kimi"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "stream",
			invoke: func(manager *Manager) error {
				_, errExecute := manager.ExecuteStream(context.Background(), []string{"claude", "kimi"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return errExecute
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.SetRetryConfig(3, 30*time.Second, 0)
			recorder := &kimiQuotaTestRecorder{}

			auths := []*Auth{
				kimiQuotaTestAuth(test.name+"-a-claude-alias", "claude", "shared-account-key"),
				kimiQuotaTestAuth(test.name+"-b-openai-alias", "kimi", "shared-account-key"),
				kimiQuotaTestAuth(test.name+"-c-second-account", "kimi", "second-account-key"),
				kimiQuotaTestAuth(test.name+"-d-third-account", "kimi", "third-account-key"),
			}
			failures := map[string]error{
				auths[0].ID: kimiInsufficientQuotaTestFailure(),
				auths[1].ID: kimiInsufficientQuotaTestFailure(),
				auths[2].ID: kimiInsufficientQuotaTestFailure(),
			}
			manager.RegisterExecutor(&kimiQuotaTestExecutor{id: "claude", recorder: recorder, failures: failures})
			manager.RegisterExecutor(&kimiQuotaTestExecutor{id: "kimi", recorder: recorder, failures: failures})

			reg := registry.GetGlobalRegistry()
			for _, auth := range auths {
				reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("register %s: %v", auth.ID, errRegister)
				}
			}
			t.Cleanup(func() {
				for _, auth := range auths {
					reg.UnregisterClient(auth.ID)
				}
			})

			errExecute := test.invoke(manager)
			if errExecute == nil {
				t.Fatal("expected insufficient_quota error")
			}
			if got := statusCodeFromError(errExecute); got != http.StatusForbidden {
				t.Fatalf("final status = %d, want upstream 403 instead of amplified 503", got)
			}
			if got := errorCodeFromError(errExecute); got != "insufficient_quota" {
				t.Fatalf("final error code = %q, want insufficient_quota", got)
			}
			wantCalls := []string{auths[0].ID, auths[2].ID}
			if gotCalls := recorder.snapshot(); !slices.Equal(gotCalls, wantCalls) {
				t.Fatalf("calls = %v, want %v; same-account alias and third account must not be attempted", gotCalls, wantCalls)
			}
			sharedAlias, _ := manager.GetByID(auths[1].ID)
			if sharedAlias == nil || !sharedAlias.Unavailable || !sharedAlias.Quota.Exceeded {
				t.Fatalf("same-account alias did not inherit shared quota breaker: %+v", sharedAlias)
			}
			thirdAccount, _ := manager.GetByID(auths[3].ID)
			if thirdAccount == nil || thirdAccount.Unavailable || thirdAccount.Quota.Exceeded {
				t.Fatalf("third account should remain untouched after bounded switch: %+v", thirdAccount)
			}
		})
	}
}
