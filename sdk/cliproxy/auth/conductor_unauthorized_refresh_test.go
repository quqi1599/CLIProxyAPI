package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type unauthorizedRefreshExecutor struct {
	id string

	mu            sync.Mutex
	executeCalls  []string
	streamCalls   []string
	refreshCalls  int
	tokenInvalid  map[string]struct{}
	invalidErr    error
	refreshFail   bool
	refreshErr    error
	refreshTokens map[string]string
	bootstrap401  bool

	bootstrap401OnlyStale    bool
	immediateErrorWithResult bool
	cancelCalls              int
	executeNotify            chan struct{}
	refreshStarted           chan struct{}
	refreshRelease           <-chan struct{}
	refreshStartOnce         sync.Once
}

func (e *unauthorizedRefreshExecutor) Identifier() string { return e.id }

func (e *unauthorizedRefreshExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	token := authAccessToken(auth)
	_, invalid := e.tokenInvalid[token]
	invalidErr := e.invalidErr
	executeNotify := e.executeNotify
	e.mu.Unlock()
	if invalid {
		if executeNotify != nil {
			executeNotify <- struct{}{}
		}
		if invalidErr == nil {
			invalidErr = &Error{
				HTTPStatus: http.StatusUnauthorized,
				Message:    "authentication token expired",
			}
		}
		return cliproxyexecutor.Response{}, invalidErr
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID + ":" + token)}, nil
}

func (e *unauthorizedRefreshExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	token := authAccessToken(auth)
	_, invalid := e.tokenInvalid[token]
	invalidErr := e.invalidErr
	bootstrap401 := e.bootstrap401
	bootstrap401OnlyStale := e.bootstrap401OnlyStale
	immediateErrorWithResult := e.immediateErrorWithResult
	e.mu.Unlock()
	if invalid {
		if invalidErr == nil {
			invalidErr = &Error{
				HTTPStatus: http.StatusUnauthorized,
				Message:    "authentication token expired",
			}
		}
		if bootstrap401 && (!bootstrap401OnlyStale || token == "stale-access-token") {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Err: invalidErr}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		}
		if immediateErrorWithResult {
			chunks := make(chan cliproxyexecutor.StreamChunk)
			close(chunks)
			return &cliproxyexecutor.StreamResult{
				Chunks: chunks,
				Cancel: func() {
					e.mu.Lock()
					e.cancelCalls++
					e.mu.Unlock()
				},
			}, invalidErr
		}
		return nil, invalidErr
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID + ":" + token)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Auth": {auth.ID}}, Chunks: chunks}, nil
}

func (e *unauthorizedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	refreshErr := e.refreshErr
	refreshFail := e.refreshFail
	refreshStarted := e.refreshStarted
	refreshRelease := e.refreshRelease
	e.mu.Unlock()
	if refreshStarted != nil {
		e.refreshStartOnce.Do(func() {
			close(refreshStarted)
		})
	}
	if refreshRelease != nil {
		<-refreshRelease
	}
	if refreshErr != nil {
		return nil, refreshErr
	}
	if refreshFail {
		return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "refresh token invalid"}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	next := e.refreshTokens[auth.ID]
	if next == "" {
		next = "refreshed-access-token"
	}
	auth.Metadata["access_token"] = next
	return auth, nil
}

func (e *unauthorizedRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *unauthorizedRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *unauthorizedRefreshExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeCalls))
	copy(out, e.executeCalls)
	return out
}

func (e *unauthorizedRefreshExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamCalls))
	copy(out, e.streamCalls)
	return out
}

func (e *unauthorizedRefreshExecutor) RefreshCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshCalls
}

func (e *unauthorizedRefreshExecutor) CancelCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelCalls
}

func newUnauthorizedRefreshFixture(t *testing.T, refreshFail bool) (*Manager, *unauthorizedRefreshExecutor, *Auth, *Auth, string) {
	t.Helper()

	model := "gpt-5.6-sol"
	primary := &Auth{
		ID:       "aa-primary",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "stale-access-token",
			"refresh_token": "primary-refresh-token",
		},
	}
	backup := &Auth{
		ID:       "bb-backup",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "backup-access-token",
			"refresh_token": "backup-refresh-token",
		},
	}

	executor := &unauthorizedRefreshExecutor{
		id: "codex",
		tokenInvalid: map[string]struct{}{
			"stale-access-token": {},
		},
		refreshFail: refreshFail,
		refreshTokens: map[string]string{
			primary.ID: "fresh-access-token",
		},
	}

	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(primary.ID, "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(backup.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(primary.ID)
		reg.UnregisterClient(backup.ID)
	})

	if _, errRegister := manager.Register(context.Background(), primary); errRegister != nil {
		t.Fatalf("register primary: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), backup); errRegister != nil {
		t.Fatalf("register backup: %v", errRegister)
	}

	return manager, executor, primary, backup, model
}

func TestManager_Execute_UnauthorizedRefreshesCurrentAuthBeforeFallback(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success on refreshed primary", errExecute)
	}
	if got := string(resp.Payload); got != primary.ID+":fresh-access-token" {
		t.Fatalf("payload = %q, want refreshed primary response", got)
	}
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 2 || got[0] != primary.ID || got[1] != primary.ID {
		t.Fatalf("Execute calls = %v, want [primary, primary]", got)
	}
	for _, id := range executor.ExecuteCalls() {
		if id == backup.ID {
			t.Fatal("backup auth should not be used when refresh recovers primary")
		}
	}

	updated, ok := manager.GetByID(primary.ID)
	if !ok || updated == nil {
		t.Fatal("primary auth missing after refresh")
	}
	if got := authAccessToken(updated); got != "fresh-access-token" {
		t.Fatalf("primary access_token = %q, want fresh-access-token", got)
	}
	if updated.Unavailable || updated.LastError != nil {
		t.Fatalf("primary auth should be active after refresh: %+v", updated)
	}
}

func TestManager_Execute_ModelSupportUnauthorizedDoesNotRefreshOrIsolate(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	executor.invalidErr = &Error{
		HTTPStatus: http.StatusUnauthorized,
		Message:    "unauthorized: The requested model is not supported for this account.",
	}

	for i := 0; i < 2; i++ {
		resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("Execute %d error = %v, want success via backup", i, errExecute)
		}
		if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
			t.Fatalf("Execute %d payload = %q, want backup response", i, got)
		}
	}
	if got := executor.RefreshCalls(); got != 0 {
		t.Fatalf("Refresh calls = %d, want 0 for a model-support 401", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 4 ||
		got[0] != primary.ID || got[1] != backup.ID ||
		got[2] != primary.ID || got[3] != backup.ID {
		t.Fatalf("Execute calls = %v, want [primary, backup, primary, backup]", got)
	}

	updated, ok := manager.GetByID(primary.ID)
	if !ok || updated == nil {
		t.Fatal("primary auth missing after model-support 401")
	}
	if hasUnauthorizedAuthFailure(updated) || updated.Unavailable {
		t.Fatalf("model-support 401 should not isolate the credential: %+v", updated)
	}
}

func TestManager_ExecuteStream_UnauthorizedRefreshesCurrentAuthBeforeFallback(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)

	stream, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("ExecuteStream error = %v, want success on refreshed primary", errStream)
	}
	if stream == nil || stream.Chunks == nil {
		t.Fatal("expected stream result")
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("expected stream chunk")
	}
	if chunk.Err != nil {
		t.Fatalf("stream chunk error = %v", chunk.Err)
	}
	if got := string(chunk.Payload); got != primary.ID+":fresh-access-token" {
		t.Fatalf("stream payload = %q, want refreshed primary response", got)
	}
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.StreamCalls(); len(got) != 2 || got[0] != primary.ID || got[1] != primary.ID {
		t.Fatalf("Stream calls = %v, want [primary, primary]", got)
	}
	for _, id := range executor.StreamCalls() {
		if id == backup.ID {
			t.Fatal("backup auth should not be used when refresh recovers primary")
		}
	}
}

func TestManager_ExecuteStream_BootstrapUnauthorizedRefreshesCurrentAuth(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	executor.bootstrap401 = true

	stream, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("ExecuteStream error = %v, want success on refreshed primary", errStream)
	}
	if stream == nil || stream.Chunks == nil {
		t.Fatal("expected stream result")
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("expected stream chunk")
	}
	if chunk.Err != nil {
		t.Fatalf("stream chunk error = %v", chunk.Err)
	}
	if got := string(chunk.Payload); got != primary.ID+":fresh-access-token" {
		t.Fatalf("stream payload = %q, want refreshed primary response", got)
	}
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.StreamCalls(); len(got) != 2 || got[0] != primary.ID || got[1] != primary.ID {
		t.Fatalf("Stream calls = %v, want [primary, primary]", got)
	}
	for _, id := range executor.StreamCalls() {
		if id == backup.ID {
			t.Fatal("backup auth should not be used when refresh recovers primary")
		}
	}
}

func TestManager_ExecuteStream_BootstrapUnauthorizedRetryErrorFallsBack(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	executor.bootstrap401 = true
	executor.bootstrap401OnlyStale = true
	executor.refreshTokens[primary.ID] = "still-invalid-token"
	executor.mu.Lock()
	executor.tokenInvalid["still-invalid-token"] = struct{}{}
	executor.mu.Unlock()

	stream, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("ExecuteStream error = %v, want success via backup", errStream)
	}
	if stream == nil || stream.Chunks == nil {
		t.Fatal("expected fallback stream result")
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("expected fallback stream chunk")
	}
	if chunk.Err != nil {
		t.Fatalf("fallback stream chunk error = %v", chunk.Err)
	}
	if got := string(chunk.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("stream payload = %q, want backup response", got)
	}
	stream.Close()
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.StreamCalls(); len(got) != 3 || got[0] != primary.ID || got[1] != primary.ID || got[2] != backup.ID {
		t.Fatalf("Stream calls = %v, want [primary, primary, backup]", got)
	}
}

func TestManager_ExecuteStream_ImmediateUnauthorizedClosesResultBeforeRefresh(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	executor.immediateErrorWithResult = true

	stream, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("ExecuteStream error = %v, want success on refreshed primary", errStream)
	}
	if stream == nil || stream.Chunks == nil {
		t.Fatal("expected stream result")
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("expected stream chunk")
	}
	if chunk.Err != nil {
		t.Fatalf("stream chunk error = %v", chunk.Err)
	}
	if got := string(chunk.Payload); got != primary.ID+":fresh-access-token" {
		t.Fatalf("stream payload = %q, want refreshed primary response", got)
	}
	stream.Close()
	if got := executor.CancelCalls(); got != 1 {
		t.Fatalf("canceled immediate error streams = %d, want 1", got)
	}
	for _, id := range executor.StreamCalls() {
		if id == backup.ID {
			t.Fatal("backup auth should not be used when refresh recovers primary")
		}
	}
}

func TestManager_Execute_UnauthorizedRefreshFailureFallsBackAndStaysIsolated(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, true)

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("payload = %q, want backup response", got)
	}
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}

	updated, ok := manager.GetByID(primary.ID)
	if !ok || updated == nil {
		t.Fatal("primary auth missing after failed refresh")
	}
	if !hasUnauthorizedAuthFailure(updated) {
		t.Fatalf("primary auth should retain unauthorized failure: %+v", updated)
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, model, time.Now()); !blocked {
		t.Fatal("primary auth should be blocked after refresh failure")
	}

	resp, errExecute = manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("second Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("second payload = %q, want backup response", got)
	}
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls after second request = %d, want 1", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 3 || got[0] != primary.ID || got[1] != backup.ID || got[2] != backup.ID {
		t.Fatalf("Execute calls = %v, want [primary, backup, backup]", got)
	}
}

func TestManager_Execute_ConcurrentUnauthorizedSharesFailedRefresh(t *testing.T) {
	tests := []struct {
		name       string
		refreshErr error
	}{
		{
			name: "terminal",
			refreshErr: &Error{
				HTTPStatus: http.StatusUnauthorized,
				Message:    "refresh token invalid",
			},
		},
		{
			name: "transient",
			refreshErr: &Error{
				HTTPStatus: http.StatusBadGateway,
				Message:    "refresh service unavailable",
				Retryable:  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
			manager.Remove(context.Background(), backup.ID)

			const requests = 8
			releaseRefresh := make(chan struct{})
			executor.refreshErr = tt.refreshErr
			executor.refreshStarted = make(chan struct{})
			executor.refreshRelease = releaseRefresh
			executor.executeNotify = make(chan struct{}, requests)

			var wg sync.WaitGroup
			errs := make(chan error, requests)
			run := func() {
				defer wg.Done()
				_, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				errs <- errExecute
			}

			wg.Add(1)
			go run()
			<-executor.refreshStarted

			wg.Add(requests - 1)
			for i := 1; i < requests; i++ {
				go run()
			}
			for i := 0; i < requests; i++ {
				<-executor.executeNotify
			}
			close(releaseRefresh)
			wg.Wait()
			close(errs)

			for errExecute := range errs {
				if errExecute == nil {
					t.Fatal("Execute error = nil, want unavailable credential failure")
				}
			}
			if got := executor.RefreshCalls(); got != 1 {
				t.Fatalf("Refresh calls = %d, want 1 shared failed refresh", got)
			}
			if got := executor.ExecuteCalls(); len(got) != requests {
				t.Fatalf("Execute calls = %d, want %d", len(got), requests)
			}

			executor.mu.Lock()
			executor.refreshErr = nil
			executor.refreshTokens[primary.ID] = "recovered-access-token"
			executor.mu.Unlock()
			refreshed, errRefresh := manager.refreshAuthForRequest(context.Background(), primary.ID, "")
			if errRefresh != nil {
				t.Fatalf("proactive refresh error = %v, want success", errRefresh)
			}
			if refreshed == nil || authAccessToken(refreshed) != "recovered-access-token" {
				t.Fatalf("proactive refresh auth = %+v, want recovered token", refreshed)
			}
			if got := executor.RefreshCalls(); got != 2 {
				t.Fatalf("Refresh calls after proactive recovery = %d, want 2", got)
			}

			executor.mu.Lock()
			executor.tokenInvalid["recovered-access-token"] = struct{}{}
			executor.refreshTokens[primary.ID] = "second-recovered-access-token"
			executor.mu.Unlock()
			resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if errExecute != nil {
				t.Fatalf("Execute with refreshed credential generation error = %v", errExecute)
			}
			if got := string(resp.Payload); got != primary.ID+":second-recovered-access-token" {
				t.Fatalf("payload after refreshed generation = %q", got)
			}
			if got := executor.RefreshCalls(); got != 3 {
				t.Fatalf("Refresh calls after new credential generation = %d, want 3", got)
			}
		})
	}
}

func TestManager_Execute_UnauthorizedWithoutRefreshTokenStaysIsolated(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	delete(primary.Metadata, "refresh_token")
	if _, errUpdate := manager.Update(context.Background(), primary); errUpdate != nil {
		t.Fatalf("update primary: %v", errUpdate)
	}

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("payload = %q, want backup response", got)
	}
	if got := executor.RefreshCalls(); got != 0 {
		t.Fatalf("Refresh calls = %d, want 0", got)
	}

	resp, errExecute = manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("second Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("second payload = %q, want backup response", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 3 || got[0] != primary.ID || got[1] != backup.ID || got[2] != backup.ID {
		t.Fatalf("Execute calls = %v, want [primary, backup, backup]", got)
	}
}

func TestManager_Execute_UnauthorizedRefreshThenRetryStillFailsFallsBackOnce(t *testing.T) {
	manager, executor, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	executor.refreshTokens[primary.ID] = "still-invalid-token"
	executor.mu.Lock()
	executor.tokenInvalid["still-invalid-token"] = struct{}{}
	executor.mu.Unlock()

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want success via backup", errExecute)
	}
	if got := string(resp.Payload); got != backup.ID+":backup-access-token" {
		t.Fatalf("payload = %q, want backup response", got)
	}
	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}
	if got := executor.ExecuteCalls(); len(got) != 3 || got[0] != primary.ID || got[1] != primary.ID || got[2] != backup.ID {
		t.Fatalf("Execute calls = %v, want [primary, primary, backup]", got)
	}
}

func TestManager_ShouldRefreshUnauthorizedAfterTransientRefreshBackoff(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	auth := &Auth{
		ID:               "codex-transient-refresh-failure",
		Provider:         "codex",
		NextRefreshAfter: now.Add(-time.Second),
		LastError: &Error{
			Code:       "unauthorized",
			HTTPStatus: http.StatusUnauthorized,
			Message:    "access token expired",
		},
		Metadata: map[string]any{
			"expired":                  now.Add(24 * time.Hour).Format(time.RFC3339),
			"refresh_token":            "refresh-token",
			"refresh_interval_seconds": 1,
		},
	}

	futureBackoff := now.Add(time.Minute)
	auth.NextRefreshAfter = futureBackoff
	if manager.shouldRefresh(auth, now) {
		t.Fatal("unauthorized auth should wait for its transient refresh backoff")
	}
	next, ok := nextRefreshCheckAt(now, auth, time.Hour)
	if !ok || !next.Equal(futureBackoff) {
		t.Fatalf("next refresh = %s, ok = %v, want %s, true", next, ok, futureBackoff)
	}

	auth.NextRefreshAfter = now.Add(-time.Second)
	if !manager.shouldRefresh(auth, now) {
		t.Fatal("unauthorized auth should retry refresh after a transient backoff expires")
	}
	next, ok = nextRefreshCheckAt(now, auth, time.Hour)
	if !ok {
		t.Fatal("unauthorized auth should remain scheduled after a transient refresh failure")
	}
	if next.After(now) {
		t.Fatalf("next refresh = %s, want due by %s", next, now)
	}

	auth.NextRefreshAfter = time.Time{}
	if manager.shouldRefresh(auth, now) {
		t.Fatal("terminal unauthorized refresh failure should remain unscheduled")
	}
	if _, ok := nextRefreshCheckAt(now, auth, time.Hour); ok {
		t.Fatal("terminal unauthorized refresh failure should be removed from the refresh schedule")
	}

	auth.NextRefreshAfter = futureBackoff
	delete(auth.Metadata, "refresh_token")
	if manager.shouldRefresh(auth, now) {
		t.Fatal("unauthorized auth without a refresh token should remain unscheduled")
	}
	if _, ok := nextRefreshCheckAt(now, auth, time.Hour); ok {
		t.Fatal("unauthorized auth without a refresh token should be removed from the refresh schedule")
	}
}

func TestIsTerminalAuthRefreshError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unauthorized",
			err:  &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
			want: true,
		},
		{
			name: "invalid_grant",
			err: &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    `{"error":"invalid_grant"}`,
			},
			want: true,
		},
		{
			name: "refresh_token_reused",
			err: &Error{
				HTTPStatus: http.StatusBadRequest,
				Message:    "token refresh failed reason=refresh_token_reused",
			},
			want: true,
		},
		{
			name: "transient",
			err: &Error{
				HTTPStatus: http.StatusBadGateway,
				Message:    "refresh service unavailable",
				Retryable:  true,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTerminalAuthRefreshError(tt.err); got != tt.want {
				t.Fatalf("isTerminalAuthRefreshError() = %v, want %v", got, tt.want)
			}
		})
	}
}
