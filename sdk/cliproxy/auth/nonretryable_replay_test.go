package auth

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerRequestScopedFailureNeverReplays(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, operation := range []string{"execute", "count", "stream-bootstrap", "stream-direct"} {
			for _, status := range []int{401, 502, 503} {
				t.Run(fmt.Sprintf("%s/%s/%d", provider, operation, status), func(t *testing.T) {
					m := NewManager(nil, &FillFirstSelector{}, nil)
					m.SetRetryConfig(3, time.Second, 5)
					failure := &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeRequest, HTTPStatus: status, Retryable: true}
					wrapped := fmt.Errorf("wrapped terminal: %w", failure)
					executor := &replayBoundaryExecutor{authFallbackExecutor: authFallbackExecutor{
						id: provider, executeErrors: map[string]error{"terminal-a": wrapped},
						countErrors: map[string]error{"terminal-a": wrapped}, streamFirstErrors: map[string]error{"terminal-a": wrapped},
					}, directStreamError: operation == "stream-direct"}
					m.RegisterExecutor(executor)
					model := "gpt-5.6-sol"
					if provider == "claude" {
						model = "claude-sonnet-4-6"
					}
					registerGPTChannelFailoverAuths(t, m, provider, model, []*Auth{
						{ID: "terminal-a", Provider: provider, Attributes: map[string]string{"priority": "20"}, Metadata: map[string]any{"access_token": "test-only", "refresh_token": "test-only"}},
						{ID: "terminal-b", Provider: provider, Attributes: map[string]string{"priority": "10"}},
					})
					request := cliproxyexecutor.Request{Model: model}
					var err error
					var calls []string
					switch operation {
					case "execute":
						_, err = m.Execute(context.Background(), []string{provider}, request, cliproxyexecutor.Options{})
						calls = executor.ExecuteCalls()
					case "count":
						_, err = m.ExecuteCount(context.Background(), []string{provider}, request, cliproxyexecutor.Options{})
						calls = executor.CountCalls()
					default:
						var stream *cliproxyexecutor.StreamResult
						stream, err = m.ExecuteStream(context.Background(), []string{provider}, request, cliproxyexecutor.Options{})
						if err == nil {
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
						calls = executor.StreamCalls()
					}
					if !errors.Is(err, failure) || !reflect.DeepEqual(calls, []string{"terminal-a"}) || executor.refreshCalls != 0 {
						t.Fatalf("err=%v calls=%v refresh=%d, want preserved terminal failure without replay", err, calls, executor.refreshCalls)
					}
				})
			}
		}
	}
}

func TestNonRequestFailureRetainsIndependentChannelFailover(t *testing.T) {
	for _, operation := range []string{"execute", "count", "stream"} {
		for _, scope := range []failurecontract.Scope{failurecontract.ScopeCredential, failurecontract.ScopeModel, failurecontract.ScopeProvider} {
			t.Run(fmt.Sprintf("%s/%s", operation, scope), func(t *testing.T) {
				m := NewManager(nil, &FillFirstSelector{}, nil)
				failure := &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: scope, HTTPStatus: 503, Retryable: false}
				if scope == failurecontract.ScopeCredential {
					failure.Kind, failure.HTTPStatus = failurecontract.AuthenticationFailed, 401
				}
				executor := &replayBoundaryExecutor{authFallbackExecutor: authFallbackExecutor{id: "codex",
					executeErrors: map[string]error{"credential-a": failure}, countErrors: map[string]error{"credential-a": failure}, streamFirstErrors: map[string]error{"credential-a": failure},
				}}
				m.RegisterExecutor(executor)
				registerGPTChannelFailoverAuths(t, m, "codex", "gpt-5.6-sol", []*Auth{
					{ID: "credential-a", Provider: "codex", Attributes: map[string]string{"priority": "20", "api_key": "test-only"}},
					{ID: "credential-b", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
				})
				request := cliproxyexecutor.Request{Model: "gpt-5.6-sol"}
				var err error
				var calls []string
				switch operation {
				case "execute":
					_, err = m.Execute(context.Background(), []string{"codex"}, request, cliproxyexecutor.Options{})
					calls = executor.ExecuteCalls()
				case "count":
					_, err = m.ExecuteCount(context.Background(), []string{"codex"}, request, cliproxyexecutor.Options{})
					calls = executor.CountCalls()
				case "stream":
					var stream *cliproxyexecutor.StreamResult
					stream, err = m.ExecuteStream(context.Background(), []string{"codex"}, request, cliproxyexecutor.Options{})
					if err == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
					calls = executor.StreamCalls()
				}
				if err != nil || !reflect.DeepEqual(calls, []string{"credential-a", "credential-b"}) || executor.refreshCalls != 0 {
					t.Fatalf("err=%v calls=%v refresh=%d, want a distinct healthy credential without replaying the failed credential", err, calls, executor.refreshCalls)
				}
			})
		}
	}
}

func TestRequestScopedContractOverridesGPTFailoverMarker(t *testing.T) {
	err := &failurecontract.Failure{HTTPStatus: 503, Scope: failurecontract.ScopeRequest, Retryable: false}
	if shouldFailoverGPTChannel(markGPTChannelFailoverError(err), []string{"codex"}, "gpt-5.6-sol") {
		t.Fatal("failover marker overrode the request-scoped contract")
	}
}

func TestManagerOwnsStreamRetryBudget(t *testing.T) {
	m := NewManager(nil, nil, nil)
	for _, tc := range []struct {
		name      string
		providers []string
		model     string
		payload   string
		want      bool
	}{
		{"gpt model", []string{"openai"}, "gpt-5.6-sol", "", true},
		{"gpt suffix", []string{"openai"}, "gpt-5.6-sol(high)", "", true},
		{"codex alias", []string{" CODEX ", "codex"}, "custom-alias", "", true},
		{"non gpt", []string{"claude"}, "claude-sonnet-4-6", "", false},
		{"mixed non gpt", []string{"codex", "claude"}, "custom-alias", "", false},
		{"remote compaction", []string{"claude"}, "custom-alias", `{"input":[{"type":"compaction_trigger"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := m.OwnsStreamRetryBudget(tc.providers, cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.payload)}, cliproxyexecutor.Options{})
			if got != tc.want {
				t.Fatalf("OwnsStreamRetryBudget=%t want=%t", got, tc.want)
			}
		})
	}
}
