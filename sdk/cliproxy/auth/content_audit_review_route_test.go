package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type contentAuditReviewRouteExecutor struct {
	mu       sync.Mutex
	authIDs  []string
	models   []string
	response func(*http.Request) (*http.Response, error)
}

func (e *contentAuditReviewRouteExecutor) Identifier() string { return "codex" }

func (e *contentAuditReviewRouteExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected translated review execution")
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

func (e *contentAuditReviewRouteExecutor) HttpRequest(_ context.Context, auth *Auth, request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Stream {
		return nil, errors.New("internal review unexpectedly enabled streaming")
	}
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.models = append(e.models, payload.Model)
	e.mu.Unlock()
	if e.response != nil {
		return e.response(request)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, nil
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
				"auth_kind":                     "oauth",
				"base_url":                      "https://review.test/v1",
				"excluded_models":               contentAuditReviewModel,
				contentAuditReviewAuthAttribute: "true",
			},
			ModelStates: map[string]*ModelState{
				contentAuditReviewModel: {Status: StatusDisabled},
			},
		},
		{
			ID:       "content-review-healthy",
			Provider: "codex",
			Attributes: map[string]string{
				"auth_kind":                     "oauth",
				"base_url":                      "https://review.test/v1",
				"excluded_models":               contentAuditReviewModel,
				contentAuditReviewAuthAttribute: "true",
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

func TestContentAuditReviewRouteSelectsOnlyDedicatedCredential(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &contentAuditReviewRouteExecutor{}
	manager.RegisterExecutor(executor)

	registryRef := registry.GetGlobalRegistry()
	auths := []*Auth{
		{
			ID:       "unrelated-high-priority",
			Provider: "codex",
			Attributes: map[string]string{
				"excluded_models": contentAuditReviewModel,
				"priority":        "100",
			},
		},
		{
			ID:       "dedicated-review",
			Provider: "codex",
			Attributes: map[string]string{
				"base_url":                      "https://review.test/v1",
				"excluded_models":               contentAuditReviewModel,
				"priority":                      "1",
				contentAuditReviewAuthAttribute: "true",
			},
		},
	}
	for _, auth := range auths {
		registryRef.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
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
	if _, errExecute := manager.ExecuteContentAuditReview(t.Context(), request, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("ExecuteContentAuditReview() error = %v", errExecute)
	}
	authIDs, _ := executor.calls()
	if len(authIDs) != 1 || authIDs[0] != "dedicated-review" {
		t.Fatalf("executor auth IDs = %v, want only dedicated-review", authIDs)
	}
}

func TestContentAuditReviewRouteConcurrentIsolation(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&contentAuditReviewRouteExecutor{})

	auth := &Auth{
		ID:       "content-review-concurrent",
		Provider: "codex",
		Attributes: map[string]string{
			"auth_kind":                     "oauth",
			"excluded_models":               contentAuditReviewModel,
			contentAuditReviewAuthAttribute: "true",
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
		{name: "array payload", request: cliproxyexecutor.Request{Model: contentAuditReviewModel, Payload: []byte(`[]`)}},
		{name: "invalid payload", request: cliproxyexecutor.Request{Model: contentAuditReviewModel, Payload: []byte(`{`)}},
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

type auditReviewErrorReader struct{ err error }

func (r auditReviewErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (r auditReviewErrorReader) Close() error             { return nil }

func newContentAuditReviewFixtureManager(t *testing.T, baseURL string, response func(*http.Request) (*http.Response, error)) *Manager {
	t.Helper()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&contentAuditReviewRouteExecutor{response: response})
	auth := &Auth{ID: "review-diagnostics-" + t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": baseURL, contentAuditReviewAuthAttribute: "true"}}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestContentAuditReviewRouteClassifiesSanitizedFailures(t *testing.T) {
	tests := []struct {
		name, code string
		response   func(*http.Request) (*http.Response, error)
	}{
		{"transport", "review_transport_error", func(*http.Request) (*http.Response, error) { return nil, errors.New("sensitive-transport-url") }},
		{"empty response", "review_response_empty", func(*http.Request) (*http.Response, error) { return nil, nil }},
		{"empty body", "review_response_empty", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(" "))}, nil
		}},
		{"rate limit", "review_upstream_http_429", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: auditReviewErrorReader{err: errors.New("sensitive-body-must-not-read")}}, nil
		}},
		{"redirect rejected", "review_upstream_http_3xx", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: auditReviewErrorReader{err: errors.New("sensitive-redirect-body-must-not-read")}}, nil
		}},
		{"schema rejected", "review_upstream_http_400", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader("sensitive-provider-error"))}, nil
		}},
		{"server failure", "review_upstream_http_5xx", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("sensitive-provider-error"))}, nil
		}},
		{"read failure", "review_response_read_error", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: auditReviewErrorReader{err: errors.New("sensitive-read-error")}}, nil
		}},
		{"bounded body", "review_response_too_large", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (2<<20)+1)))}, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newContentAuditReviewFixtureManager(t, "https://review.invalid/v1", test.response)
			_, err := manager.ExecuteContentAuditReview(t.Context(), cliproxyexecutor.Request{Model: contentAuditReviewModel, Payload: []byte(`{}`)}, cliproxyexecutor.Options{})
			var classified interface {
				AuditReviewFailureCode() string
				AuditReviewStageLatenciesMS() map[string]int64
			}
			if !errors.As(err, &classified) || classified.AuditReviewFailureCode() != test.code {
				t.Fatalf("error=%v want=%s", err, test.code)
			}
			if strings.Contains(err.Error(), "sensitive-") {
				t.Fatal("failure leaked provider data")
			}
			if _, ok := classified.AuditReviewStageLatenciesMS()["auth_select"]; !ok {
				t.Fatal("missing auth stage")
			}
			if _, ok := classified.AuditReviewStageLatenciesMS()["transport"]; !ok {
				t.Fatal("missing transport stage")
			}
		})
	}
}

func TestContentAuditReviewRouteRecordsHTTPPhasesWithoutChangingDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed","output":[]}`)
	}))
	defer server.Close()
	manager := newContentAuditReviewFixtureManager(t, server.URL+"/v1", func(request *http.Request) (*http.Response, error) {
		// The fixture reader consumed the request once to inspect model isolation.
		request.Body, _ = request.GetBody()
		if _, hasDeadline := request.Context().Deadline(); hasDeadline {
			t.Error("internal route invented a deadline")
		}
		return server.Client().Do(request)
	})
	ctx, trace := cliproxyexecutor.WithContentAuditReviewTrace(t.Context())
	_, err := manager.ExecuteContentAuditReview(ctx, cliproxyexecutor.Request{Model: contentAuditReviewModel, Payload: []byte(`{}`)}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"auth_select", "connect", "request_write", "ttfb", "transport", "read"} {
		if elapsed, ok := trace.Snapshot()[stage]; !ok || elapsed < 0 {
			t.Fatalf("phase %s missing or invalid: %v", stage, trace.Snapshot())
		}
	}
}

func TestContentAuditReviewPublicRouteRemainsDeniedWhenRegistryAdvertisesModel(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &contentAuditReviewRouteExecutor{}
	manager.RegisterExecutor(executor)
	auth := &Auth{ID: "content-review-public-denied", Provider: "codex", Attributes: map[string]string{contentAuditReviewAuthAttribute: "true"}}
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
		Attributes: map[string]string{
			contentAuditReviewAuthAttribute: "true",
		},
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
