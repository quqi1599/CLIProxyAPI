package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type compactionFailureExecutor struct {
	attempts atomic.Int32
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
