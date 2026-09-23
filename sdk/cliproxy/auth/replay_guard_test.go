package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type replayBoundaryExecutor struct {
	authFallbackExecutor
	directStreamError bool
	payloadFirst      bool
	refreshCalls      int
}

func (e *replayBoundaryExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	e.mu.Unlock()
	return auth, nil
}

func (e *replayBoundaryExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if !e.directStreamError && !e.payloadFirst {
		return e.authFallbackExecutor.ExecuteStream(ctx, auth, req, opts)
	}
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	err := e.streamFirstErrors[auth.ID]
	e.mu.Unlock()
	if e.directStreamError {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")}
	chunks <- cliproxyexecutor.StreamChunk{Err: err}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestManagerCommittedFailureNeverReplays(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, operation := range []string{"execute", "count", "stream-bootstrap", "stream-direct"} {
			for _, status := range []int{http.StatusUnauthorized, http.StatusBadGateway} {
				for _, phaseOnly := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%d/phaseOnly=%t", provider, operation, status, phaseOnly), func(t *testing.T) {
						m := NewManager(nil, &FillFirstSelector{}, nil)
						m.SetRetryConfig(3, time.Second, 5)
						failure := &failurecontract.Failure{
							Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
							HTTPStatus: status, Retryable: true, OutputCommitted: !phaseOnly,
							StreamPhase: failurecontract.StreamPhaseAfterOutput, PublicMessage: "terminal after output",
						}
						executor := &replayBoundaryExecutor{authFallbackExecutor: authFallbackExecutor{
							id: provider, executeErrors: map[string]error{"committed-a": failure},
							countErrors: map[string]error{"committed-a": failure}, streamFirstErrors: map[string]error{"committed-a": failure},
						}, directStreamError: operation == "stream-direct"}
						m.RegisterExecutor(executor)
						model := "gpt-5.6-sol"
						if provider == "claude" {
							model = "claude-sonnet-4-6"
						}
						registerGPTChannelFailoverAuths(t, m, provider, model, []*Auth{
							{ID: "committed-a", Provider: provider, Attributes: map[string]string{"priority": "20"},
								Metadata: map[string]any{"access_token": "test-access", "refresh_token": "test-refresh"}},
							{ID: "committed-b", Provider: provider, Attributes: map[string]string{"priority": "10"}},
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
						if !hasCommittedOutput(err) || !reflect.DeepEqual(calls, []string{"committed-a"}) {
							t.Fatalf("err=%v calls=%v, want terminal failure without replay", err, calls)
						}
						if executor.refreshCalls != 0 {
							t.Fatalf("refreshCalls=%d, must not refresh/replay a committed 401", executor.refreshCalls)
						}
					})
				}
			}
		}
	}
}

func TestManagerDeliveredPayloadNeverReplays(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.SetRetryConfig(3, time.Second, 5)
			executor := &replayBoundaryExecutor{authFallbackExecutor: authFallbackExecutor{id: provider,
				streamFirstErrors: map[string]error{"payload-a": errors.New("upstream connection reset")}}, payloadFirst: true}
			m.RegisterExecutor(executor)
			model := "gpt-5.6-sol"
			if provider == "claude" {
				model = "claude-sonnet-4-6"
			}
			registerGPTChannelFailoverAuths(t, m, provider, model, []*Auth{
				{ID: "payload-a", Provider: provider, Attributes: map[string]string{"priority": "20"}},
				{ID: "payload-b", Provider: provider, Attributes: map[string]string{"priority": "10"}},
			})
			stream, err := m.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatal(err)
			}
			payloadSeen, failureSeen := false, false
			for chunk := range stream.Chunks {
				payloadSeen = payloadSeen || len(chunk.Payload) > 0
				failureSeen = failureSeen || chunk.Err != nil
			}
			if !payloadSeen || !failureSeen || !reflect.DeepEqual(executor.StreamCalls(), []string{"payload-a"}) {
				t.Fatalf("payload=%t failure=%t calls=%v, want delivered payload/error without replay", payloadSeen, failureSeen, executor.StreamCalls())
			}
		})
	}
}

func TestCommittedFailureStopsAttemptRunnerAndAlternateFallback(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(5, time.Second, 5)
	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.configureGPTRoute(true)
	err := &failurecontract.Failure{HTTPStatus: 502, Scope: failurecontract.ScopeProvider, Retryable: true, OutputCommitted: true}
	runs, fallbacks := 0, 0
	runner := managerAttemptRunner[int]{manager: m,
		runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (int, error) {
			runs++
			return 0, err
		},
		fallback: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, error) (int, bool, error) {
			fallbacks++
			return 1, true, nil
		},
	}
	outcome := runner.run(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{}, 5, time.Second)
	if runs != 1 || fallbacks != 0 || outcome.success || outcome.returnErr != err {
		t.Fatalf("runs=%d fallbacks=%d outcome=%+v, want preserved terminal error", runs, fallbacks, outcome)
	}
	if shouldFailoverGPTChannel(markGPTChannelFailoverError(err), []string{"codex"}, "gpt-5.6-sol") {
		t.Fatal("failover wrapper overrode the committed-output boundary")
	}
}

func TestCommittedFailureStopsStreamModelPool(t *testing.T) {
	for _, directError := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", directError), func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			errCommitted := &failurecontract.Failure{HTTPStatus: 502, Retryable: true, OutputCommitted: true, Scope: failurecontract.ScopeProvider}
			executor := &replayBoundaryExecutor{authFallbackExecutor: authFallbackExecutor{
				id: "codex", streamFirstErrors: map[string]error{"pool-a": errCommitted},
			}, directStreamError: directError}
			auth := &Auth{ID: "pool-a", Provider: "codex"}
			ctx, _ := ensureRequestAttemptTrace(context.Background())
			_, err := m.executeStreamWithModelPool(ctx, executor, auth, "codex", []string{"codex"},
				cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{}, "gpt-5.6-sol", []string{"upstream-a", "upstream-b"}, true)
			if !hasCommittedOutput(err) || !reflect.DeepEqual(executor.StreamCalls(), []string{"pool-a"}) {
				t.Fatalf("err=%v calls=%v, must not replay another pooled model", err, executor.StreamCalls())
			}
		})
	}
}
