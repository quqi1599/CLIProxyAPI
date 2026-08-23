package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type contentAuditReviewRouteExecutor struct {
	mu      sync.Mutex
	authIDs []string
	models  []string
}

func (e *contentAuditReviewRouteExecutor) Identifier() string { return "codex" }

func (e *contentAuditReviewRouteExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.models = append(e.models, req.Model)
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *contentAuditReviewRouteExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("unexpected streaming review")
}

func (e *contentAuditReviewRouteExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *contentAuditReviewRouteExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected count review")
}

func (e *contentAuditReviewRouteExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected raw review")
}

func (e *contentAuditReviewRouteExecutor) calls() ([]string, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...), append([]string(nil), e.models...)
}

func TestContentAuditReviewRouteBypassesOnlyRegistryExclusion(t *testing.T) {
	const publicModel = "gpt-5.4"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &contentAuditReviewRouteExecutor{}
	manager.RegisterExecutor(executor)

	registryRef := registry.GetGlobalRegistry()
	auths := []*Auth{
		{
			ID:       "content-review-disabled",
			Provider: "codex",
			Attributes: map[string]string{
				"auth_kind":       "oauth",
				"excluded_models": contentAuditReviewModel,
			},
			ModelStates: map[string]*ModelState{
				contentAuditReviewModel: {Status: StatusDisabled},
			},
		},
		{
			ID:       "content-review-healthy",
			Provider: "codex",
			Attributes: map[string]string{
				"auth_kind":       "oauth",
				"excluded_models": contentAuditReviewModel,
			},
		},
	}
	for _, auth := range auths {
		registryRef.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: publicModel}})
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			registryRef.UnregisterClient(auth.ID)
		}
	})

	request := cliproxyexecutor.Request{Model: contentAuditReviewModel, Payload: []byte(`{"model":"codex-auto-review"}`)}
	if _, errExecute := manager.Execute(t.Context(), []string{"codex"}, request, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("public Execute() error = nil, want excluded model rejection")
	}
	if _, errExecute := manager.Execute(t.Context(), []string{"codex"}, request, cliproxyexecutor.Options{
		Metadata:                        map[string]any{"internal_content_audit_review": true},
		InternalAuthSelectionCapability: &struct{}{},
	}); errExecute == nil {
		t.Fatal("forged public Execute() error = nil, want excluded model rejection")
	}
	if _, errPick := manager.scheduler.pickSingle(t.Context(), "codex", request.Model, cliproxyexecutor.Options{
		Metadata:                        map[string]any{"internal_content_audit_review": true},
		InternalAuthSelectionCapability: &struct{}{},
	}, nil); errPick == nil {
		t.Fatal("forged scheduler capability selected an excluded model")
	}

	if _, errExecute := manager.ExecuteContentAuditReview(t.Context(), request, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("ExecuteContentAuditReview() error = %v", errExecute)
	}
	authIDs, models := executor.calls()
	if len(authIDs) != 1 || authIDs[0] != "content-review-healthy" {
		t.Fatalf("executor auth IDs = %v, want only healthy auth", authIDs)
	}
	if len(models) != 1 || models[0] != contentAuditReviewModel {
		t.Fatalf("executor models = %v, want [%s]", models, contentAuditReviewModel)
	}

	// Building the internal scheduler shard must not expose the model to later public requests.
	if _, errExecute := manager.Execute(t.Context(), []string{"codex"}, request, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("public Execute() after internal route error = nil, internal shard leaked")
	}
}

func TestContentAuditReviewRouteConcurrentIsolation(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&contentAuditReviewRouteExecutor{})

	auth := &Auth{
		ID:       "content-review-concurrent",
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": contentAuditReviewModel,
		},
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { registryRef.UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: contentAuditReviewModel}
	const pairCount = 24
	var wg sync.WaitGroup
	errCh := make(chan error, pairCount*2)
	selectedIDs := make(chan string, pairCount)
	internalOpts := cliproxyexecutor.Options{InternalAuthSelectionCapability: contentAuditReviewAuthSelectionCapability}
	for range pairCount {
		wg.Add(2)
		go func() {
			defer wg.Done()
			selected, errPick := manager.scheduler.pickSingle(context.Background(), "codex", request.Model, internalOpts, nil)
			if errPick != nil {
				errCh <- errPick
				return
			}
			if selected == nil {
				errCh <- errors.New("internal scheduler returned no auth")
				return
			}
			selectedIDs <- selected.ID
		}()
		go func() {
			defer wg.Done()
			if _, errPick := manager.scheduler.pickSingle(context.Background(), "codex", request.Model, cliproxyexecutor.Options{}, nil); errPick == nil {
				errCh <- errors.New("public scheduler unexpectedly selected excluded model")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	close(selectedIDs)
	for err := range errCh {
		t.Error(err)
	}
	selectedCount := 0
	for authID := range selectedIDs {
		selectedCount++
		if authID != auth.ID {
			t.Fatalf("selected auth ID = %q, want %q", authID, auth.ID)
		}
	}
	if selectedCount != pairCount {
		t.Fatalf("internal selections = %d, want %d", selectedCount, pairCount)
	}
}

func TestContentAuditReviewRouteRejectsInvalidInternalRequests(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&contentAuditReviewRouteExecutor{})

	tests := []struct {
		name    string
		request cliproxyexecutor.Request
		opts    cliproxyexecutor.Options
	}{
		{name: "different model", request: cliproxyexecutor.Request{Model: "gpt-5.4"}},
		{name: "stream", request: cliproxyexecutor.Request{Model: contentAuditReviewModel}, opts: cliproxyexecutor.Options{Stream: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, errExecute := manager.ExecuteContentAuditReview(t.Context(), test.request, test.opts)
			var authErr *Error
			if !errors.As(errExecute, &authErr) || authErr.Code != "invalid_internal_request" || authErr.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("error = %#v, want invalid_internal_request", errExecute)
			}
		})
	}
}

func TestContentAuditReviewPublicRouteRemainsDeniedWhenRegistryAdvertisesModel(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &contentAuditReviewRouteExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{ID: "content-review-public-denied", Provider: "codex"}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: contentAuditReviewModel}})
	t.Cleanup(func() { registryRef.UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	request := cliproxyexecutor.Request{Model: contentAuditReviewModel}
	if _, errExecute := manager.Execute(t.Context(), []string{"codex"}, request, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("public Execute() error = nil")
	}
	if _, errCount := manager.ExecuteCount(t.Context(), []string{"codex"}, request, cliproxyexecutor.Options{}); errCount == nil {
		t.Fatal("public ExecuteCount() error = nil")
	}
	if _, errStream := manager.ExecuteStream(t.Context(), []string{"codex"}, request, cliproxyexecutor.Options{Stream: true}); errStream == nil {
		t.Fatal("public ExecuteStream() error = nil")
	}
	authIDs, _ := executor.calls()
	if len(authIDs) != 0 {
		t.Fatalf("executor auth IDs = %v, want no public execution", authIDs)
	}
}

func TestContentAuditReviewRouteHonorsUnavailableAuth(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &contentAuditReviewRouteExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "content-review-cooling",
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			contentAuditReviewModel: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(time.Hour),
			},
		},
	}
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { registryRef.UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if _, errExecute := manager.ExecuteContentAuditReview(t.Context(), cliproxyexecutor.Request{Model: contentAuditReviewModel}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("ExecuteContentAuditReview() error = nil, want unavailable auth rejection")
	}
	authIDs, _ := executor.calls()
	if len(authIDs) != 0 {
		t.Fatalf("executor auth IDs = %v, want no execution", authIDs)
	}
}
