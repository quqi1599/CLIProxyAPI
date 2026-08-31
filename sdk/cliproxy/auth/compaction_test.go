package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type compactionFailureExecutor struct {
	attempts atomic.Int32
}

type compactionFailoverExecutor struct {
	mu           sync.Mutex
	calls        []string
	failures     map[string]error
	streamChunks map[string][]cliproxyexecutor.StreamChunk
}

func (e *compactionFailureExecutor) Identifier() string { return "compaction-test" }
func (e *compactionFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.attempts.Add(1)
	return cliproxyexecutor.Response{}, e.failure()
}
func (e *compactionFailureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.attempts.Add(1)
	return nil, e.failure()
}
func (e *compactionFailureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *compactionFailureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *compactionFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, e.failure()
}
func (e *compactionFailureExecutor) failure() error {
	return &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusServiceUnavailable, OuterStatus: http.StatusServiceUnavailable,
		ProviderCode: "compaction_upstream_error", SemanticCode: "compaction_upstream_error", Retryable: true,
	}
}

func (e *compactionFailoverExecutor) Identifier() string { return "compaction-failover-test" }

func (e *compactionFailoverExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(auth)
	if err := e.failure(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"type":"response.completed","response":{"output":[{"type":"compaction","encrypted_content":"opaque"}]}}`)}, nil
}

func (e *compactionFailoverExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.record(auth)
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	configuredChunks, hasConfiguredChunks := e.streamChunks[authID]
	streamChunks := append([]cliproxyexecutor.StreamChunk(nil), configuredChunks...)
	e.mu.Unlock()
	if hasConfiguredChunks {
		chunks := make(chan cliproxyexecutor.StreamChunk, len(streamChunks))
		for _, chunk := range streamChunks {
			chunks <- chunk
		}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	if err := e.failure(auth); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"output":[{"type":"compaction","encrypted_content":"opaque"}]}}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *compactionFailoverExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *compactionFailoverExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *compactionFailoverExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *compactionFailoverExecutor) record(auth *Auth) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if auth != nil {
		e.calls = append(e.calls, auth.ID)
	}
}

func (e *compactionFailoverExecutor) failure(auth *Auth) error {
	if auth == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failures[auth.ID]
}

func (e *compactionFailoverExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func TestResolveResponsesCompactionCapability(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		auth        *Auth
		wantLegacy  string
		wantTrigger string
		wantContext bool
	}{
		{
			name:       "official codex oauth",
			auth:       &Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token"}},
			wantLegacy: ResponsesCompactionLegacyNative, wantTrigger: ResponsesCompactionTriggerNativeStream,
		},
		{
			name:       "custom defaults disabled",
			auth:       &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key", "base_url": "https://example.com/v1"}},
			wantLegacy: ResponsesCompactionUnsupported, wantTrigger: ResponsesCompactionUnsupported,
		},
		{
			name: "explicit bridge",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{
				"api_key": "key", "base_url": "https://example.com/v1",
				attributeResponsesCompactionLegacy:            ResponsesCompactionLegacyNative,
				attributeResponsesCompactionTrigger:           ResponsesCompactionTriggerBridgeLegacy,
				attributeResponsesCompactionContextManagement: "true",
			}},
			wantLegacy: ResponsesCompactionLegacyNative, wantTrigger: ResponsesCompactionTriggerBridgeLegacy, wantContext: true,
		},
		{
			name: "official openai explicit context disable",
			auth: &Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"base_url": "https://api.openai.com/v1", attributeResponsesCompactionContextManagement: "false",
			}},
			wantLegacy: ResponsesCompactionLegacyNative, wantTrigger: ResponsesCompactionTriggerNativeStream, wantContext: false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveResponsesCompactionCapability(test.auth)
			if got.LegacyEndpoint != test.wantLegacy || got.TriggerMode != test.wantTrigger || got.ContextManagement != test.wantContext {
				t.Fatalf("ResolveResponsesCompactionCapability() = %+v", got)
			}
		})
	}
}

func TestRemoteCompactionCandidateAllowed(t *testing.T) {
	t.Parallel()
	auth := &Auth{Provider: "codex", Attributes: map[string]string{
		"api_key": "key", "base_url": "https://example.com/v1",
		attributeResponsesCompactionLegacy:  ResponsesCompactionUnsupported,
		attributeResponsesCompactionTrigger: ResponsesCompactionTriggerNativeStream,
	}}
	if !remoteCompactionCandidateAllowed(auth, cliproxyexecutor.CompactionIntentV2Trigger) {
		t.Fatal("v2 capable auth was rejected")
	}
	if remoteCompactionCandidateAllowed(auth, cliproxyexecutor.CompactionIntentLegacyEndpoint) {
		t.Fatal("legacy-unsupported auth was accepted")
	}
}

func TestRemoteCompactionContextManagementUnsupportedIsRequestScoped(t *testing.T) {
	t.Parallel()
	failure, ok := failurecontract.As(remoteCompactionSelectionError(cliproxyexecutor.CompactionIntentContextManagement))
	if !ok || failure.Scope != failurecontract.ScopeRequest || failure.ErrorCode() != "remote_compaction_context_management_unsupported" {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestWithSelectedCompactionCapabilityDetectsRequestPayload(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	auth := &Auth{Attributes: map[string]string{
		attributeResponsesCompactionTrigger: ResponsesCompactionTriggerNativeStream,
		attributeResponsesCompactionGroup:   "gpt-agent-e2e",
	}}

	got := withSelectedCompactionCapability(cliproxyexecutor.Request{Payload: payload}, cliproxyexecutor.Options{}, auth)
	if intent := metadataString(got.Metadata, cliproxyexecutor.CompactionIntentMetadataKey); intent != string(cliproxyexecutor.CompactionIntentV2Trigger) {
		t.Fatalf("compaction intent = %q, want v2_trigger", intent)
	}
	if mode := metadataString(got.Metadata, cliproxyexecutor.CompactionTriggerModeMetadataKey); mode != ResponsesCompactionTriggerNativeStream {
		t.Fatalf("compaction trigger mode = %q, want native-stream", mode)
	}
	if group := metadataString(got.Metadata, cliproxyexecutor.CompactionCompatibilityGroupMetadataKey); group != "gpt-agent-e2e" {
		t.Fatalf("compaction compatibility group = %q, want gpt-agent-e2e", group)
	}
}

func TestRemoteCompactionUsesSingleAttemptAndIndependentBreaker(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(5, 0, 5)
	executor := &compactionFailureExecutor{}
	manager.RegisterExecutor(executor)
	_, err := manager.Register(context.Background(), &Auth{
		ID: "compaction-auth", Provider: executor.Identifier(), Status: StatusActive,
		Attributes: map[string]string{
			attributeResponsesCompactionTrigger: ResponsesCompactionTriggerNativeStream,
			attributeResponsesCompactionGroup:   "test-group",
		},
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	payload := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}
	opts := cliproxyexecutor.Options{
		Stream: true, OriginalRequest: payload,
		Metadata: map[string]any{cliproxyexecutor.CompactionIntentMetadataKey: string(cliproxyexecutor.CompactionIntentV2Trigger)},
	}

	_, err = manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
	if err == nil {
		t.Fatal("ExecuteStream() error = nil")
	}
	if got := executor.attempts.Load(); got != 1 {
		t.Fatalf("attempts after one request = %d, want 1", got)
	}
	manager.mu.RLock()
	stored := manager.auths["compaction-auth"].Clone()
	manager.mu.RUnlock()
	if stored.LastError != nil || stored.Unavailable {
		t.Fatalf("compaction failure polluted ordinary auth health: %+v", stored)
	}

	for i := 0; i < compactionBreakerFailureThreshold-1; i++ {
		_, _ = manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
	}
	_, err = manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
	failure, ok := failurecontract.As(err)
	if !ok || failure.ErrorCode() != "compaction_route_unavailable" || failure.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("breaker error = %T %v", err, err)
	}
	if got := executor.attempts.Load(); got != compactionBreakerFailureThreshold {
		t.Fatalf("attempts with open breaker = %d, want %d", got, compactionBreakerFailureThreshold)
	}
}

func TestRemoteCompactionHealthBlockedCompatibleRouteReturnsTypedUnavailable(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &compactionFailureExecutor{}
	manager.RegisterExecutor(executor)

	const model = "gpt-5.6-sol"
	retryAt := time.Now().Add(3 * time.Minute)
	for index := 0; index < 4; index++ {
		attributes := map[string]string{
			"base_url":      "https://route-" + string(rune('a'+index)) + ".example/v1",
			"routing_group": "route-" + string(rune('a'+index)),
		}
		auth := &Auth{
			ID:         "auth-" + string(rune('a'+index)),
			Provider:   executor.Identifier(),
			Status:     StatusActive,
			Attributes: attributes,
		}
		if index == 0 {
			auth.Attributes[attributeResponsesCompactionTrigger] = ResponsesCompactionTriggerNativeStream
			auth.Attributes[attributeResponsesCompactionGroup] = "compaction-group"
			auth.ModelStates = map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: retryAt,
					LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
					Health: HealthState{
						Observed:       true,
						BreakerState:   HealthBreakerOpen,
						OpenUntil:      retryAt,
						LastStatusCode: http.StatusUnauthorized,
					},
				},
			}
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}
	probeStartedAt := time.Now().Add(-10 * time.Second)
	if ok, _ := manager.reserveHalfOpenProbeWithWindow("auth-a", model, probeStartedAt, 20*time.Second, 5*time.Second); !ok {
		t.Fatal("failed to seed the cooling half-open probe window")
	}

	payload := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	req := cliproxyexecutor.Request{Model: model, Payload: payload}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.CompactionIntentMetadataKey: string(cliproxyexecutor.CompactionIntentV2Trigger),
		},
	}

	_, err := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
	failure, ok := failurecontract.As(err)
	if !ok || failure.ErrorCode() != "compaction_route_unavailable" || failure.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("ExecuteStream() error = %T %v, want typed compaction_route_unavailable 503", err, err)
	}
	if code := errorCodeFromError(err); code != "compaction_route_unavailable" {
		t.Fatalf("errorCodeFromError() = %q, want outer compaction_route_unavailable instead of wrapped auth code", code)
	}
	resultErr := resultErrorFromCause(err)
	if resultErr == nil || resultErr.Code != "compaction_route_unavailable" || resultErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("resultErrorFromCause() = %+v, want compaction_route_unavailable 503", resultErr)
	}
	if failure.RetryAfter == nil || *failure.RetryAfter < 9*time.Second || *failure.RetryAfter > 11*time.Second {
		t.Fatalf("RetryAfter = %v, want the next safe half-open probe window", failure.RetryAfter)
	}
	if got := executor.attempts.Load(); got != 0 {
		t.Fatalf("upstream attempts = %d, want 0 while the only compatible route is blocked", got)
	}

	fields := manager.authAvailabilityMetricFieldsForRequest([]string{executor.Identifier()}, model, opts, time.Now())
	for field, want := range map[string]any{
		"candidate_route_count":            1,
		"eligible_route_count":             0,
		"blocked_route_count":              1,
		"compaction_candidate_route_count": 1,
		"compaction_eligible_route_count":  0,
		"ordinary_candidate_route_count":   4,
		"ordinary_eligible_route_count":    3,
		"compaction_intent":                string(cliproxyexecutor.CompactionIntentV2Trigger),
	} {
		if got := fields[field]; got != want {
			t.Fatalf("%s = %#v, want %#v; fields=%+v", field, got, want, fields)
		}
	}
}

func TestRemoteCompactionFailsOverOnceWithinCompatibilityGroup(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			failure := &failurecontract.Failure{
				Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
				HTTPStatus: http.StatusServiceUnavailable, OuterStatus: http.StatusServiceUnavailable,
				ProviderCode: "compaction_upstream_error", SemanticCode: "compaction_upstream_error", Retryable: true,
			}
			manager, executor, req, opts := newCompactionFailoverFixture(t, "shared-group", "shared-group", failure)

			if stream {
				result, err := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream() error = %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("fallback stream error = %v", chunk.Err)
					}
				}
			} else {
				opts.Stream = false
				if _, err := manager.Execute(context.Background(), []string{executor.Identifier()}, req, opts); err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
			}

			assertCompactionCalls(t, executor.Calls(), []string{"auth-a", "auth-b"})
		})
	}
}

func TestRemoteCompactionDoesNotFailOverAcrossCompatibilityGroups(t *testing.T) {
	for _, test := range []struct {
		name        string
		firstGroup  string
		secondGroup string
	}{
		{name: "different groups", firstGroup: "group-a", secondGroup: "group-b"},
		{name: "missing group contract"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := &failurecontract.Failure{
				Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
				HTTPStatus: http.StatusServiceUnavailable, OuterStatus: http.StatusServiceUnavailable,
				ProviderCode: "compaction_upstream_error", SemanticCode: "compaction_upstream_error", Retryable: true,
			}
			manager, executor, req, opts := newCompactionFailoverFixture(t, test.firstGroup, test.secondGroup, failure)

			_, err := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
			if err == nil || errorCodeFromError(err) != "compaction_upstream_error" {
				t.Fatalf("ExecuteStream() error = %T %v, want the first route error", err, err)
			}
			assertCompactionCalls(t, executor.Calls(), []string{"auth-a"})
		})
	}
}

func TestRemoteCompactionDoesNotReplayRequestScopedFailure(t *testing.T) {
	failure := &failurecontract.Failure{
		Kind: failurecontract.InvalidRequest, Scope: failurecontract.ScopeRequest,
		HTTPStatus: http.StatusBadRequest, OuterStatus: http.StatusBadRequest,
		ProviderCode: "invalid_compaction_request", SemanticCode: "invalid_compaction_request", Retryable: true,
	}
	manager, executor, req, opts := newCompactionFailoverFixture(t, "shared-group", "shared-group", failure)

	_, err := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
	if err == nil || errorCodeFromError(err) != "invalid_compaction_request" {
		t.Fatalf("ExecuteStream() error = %T %v, want request-scoped failure", err, err)
	}
	assertCompactionCalls(t, executor.Calls(), []string{"auth-a"})
}

func TestRemoteCompactionDoesNotReplayAfterOutput(t *testing.T) {
	failure := &failurecontract.Failure{
		Kind: failurecontract.TransportError, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusBadGateway, OuterStatus: http.StatusBadGateway,
		ProviderCode: "stream_interrupted", SemanticCode: "stream_interrupted", Retryable: true,
	}
	manager, executor, req, opts := newCompactionFailoverFixture(t, "shared-group", "shared-group", failure)
	executor.streamChunks = map[string][]cliproxyexecutor.StreamChunk{
		"auth-a": {
			{Payload: []byte(`{"type":"response.completed","response":{"output":[{"type":"compaction","encrypted_content":"opaque"}]}}`)},
			{Err: failure},
		},
	}

	result, err := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	sawError := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("downstream stream did not preserve the post-output error")
	}
	assertCompactionCalls(t, executor.Calls(), []string{"auth-a"})
}

func TestRemoteCompactionFailoverIsLimitedToTwoAttempts(t *testing.T) {
	failure := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusServiceUnavailable, OuterStatus: http.StatusServiceUnavailable,
		ProviderCode: "compaction_upstream_error", SemanticCode: "compaction_upstream_error", Retryable: true,
	}
	manager, executor, req, opts := newCompactionFailoverFixture(t, "shared-group", "shared-group", failure)
	executor.failures["auth-b"] = failure
	registerCompactionFailoverAuth(t, manager, executor, "auth-c", "shared-group")
	executor.failures["auth-c"] = failure

	_, err := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, req, opts)
	if err == nil || errorCodeFromError(err) != "compaction_upstream_error" {
		t.Fatalf("ExecuteStream() error = %T %v, want second route failure", err, err)
	}
	calls := executor.Calls()
	if len(calls) != remoteCompactionMaxAttempts || calls[0] != "auth-a" {
		t.Fatalf("calls = %v, want exactly two attempts beginning with auth-a", calls)
	}
}

func newCompactionFailoverFixture(t *testing.T, firstGroup, secondGroup string, firstFailure error) (*Manager, *compactionFailoverExecutor, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	t.Helper()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &compactionFailoverExecutor{failures: map[string]error{"auth-a": firstFailure}}
	manager.RegisterExecutor(executor)

	for index, group := range []string{firstGroup, secondGroup} {
		authID := "auth-" + string(rune('a'+index))
		registerCompactionFailoverAuth(t, manager, executor, authID, group)
	}

	payload := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","content":[]},{"type":"compaction_trigger"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.CompactionIntentMetadataKey: string(cliproxyexecutor.CompactionIntentV2Trigger),
		},
	}
	return manager, executor, req, opts
}

func registerCompactionFailoverAuth(t *testing.T, manager *Manager, executor *compactionFailoverExecutor, authID, group string) {
	t.Helper()
	_, err := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: executor.Identifier(),
		Status:   StatusActive,
		Attributes: map[string]string{
			"base_url":                          "https://" + authID + ".example/v1",
			"routing_group":                     "route-" + authID,
			attributeResponsesCompactionTrigger: ResponsesCompactionTriggerNativeStream,
			attributeResponsesCompactionGroup:   group,
		},
	})
	if err != nil {
		t.Fatalf("Register(%s) error = %v", authID, err)
	}
}

func assertCompactionCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}
