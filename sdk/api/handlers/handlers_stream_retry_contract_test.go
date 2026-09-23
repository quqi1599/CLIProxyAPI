package handlers

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type retryContractStreamExecutor struct {
	failOnceStreamExecutor
	provider   string
	failure    error
	alwaysFail bool
}

func (e *retryContractStreamExecutor) Identifier() string { return e.provider }

func (e *retryContractStreamExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()
	ch := make(chan coreexecutor.StreamChunk, 1)
	if call == 1 || e.alwaysFail {
		ch <- coreexecutor.StreamChunk{Err: e.failure}
	} else {
		ch <- coreexecutor.StreamChunk{Payload: []byte("ok")}
	}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func registerRetryContractAuth(t *testing.T, m *coreauth.Manager, provider, model string, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("%s-%d", t.Name(), index)
		a := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive, Attributes: map[string]string{
			"api_key": "test-only", "base_url": fmt.Sprintf("https://retry-contract-%d.example/v1", index),
		}}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
}

func runRetryContractHandler(t *testing.T, m *coreauth.Manager, model string, bootstrapRetries int) ([]byte, error) {
	t.Helper()
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: bootstrapRetries}}, m)
	data, _, errs := h.ExecuteStreamWithAuthManager(context.Background(), "openai", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
	var payload []byte
	if data != nil {
		for chunk := range data {
			payload = append(payload, chunk...)
		}
	}
	var terminal error
	if errs != nil {
		for err := range errs {
			if err != nil {
				terminal = err.Error
			}
		}
	}
	return payload, terminal
}

func TestHandlerPreservesGPTManagerRetryBudget(t *testing.T) {
	var baseline int
	for _, bootstrapRetries := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(bootstrapRetries), func(t *testing.T) {
			e := &retryContractStreamExecutor{provider: "codex", alwaysFail: true, failure: &coreauth.Error{Code: "gpt_first_event_timeout", Message: "local first-event deadline exceeded", Retryable: true, HTTPStatus: http.StatusGatewayTimeout}}
			m := coreauth.NewManager(nil, nil, nil)
			m.RegisterExecutor(e)
			const model = "gpt-5.6-sol"
			registerRetryContractAuth(t, m, e.provider, model, 1)
			payload, err := runRetryContractHandler(t, m, model, bootstrapRetries)
			if err == nil || len(payload) != 0 {
				t.Fatalf("want terminal failure without output, got payload=%q error=%v", payload, err)
			}
			if bootstrapRetries == 0 {
				baseline = e.Calls()
				if baseline < 2 {
					t.Fatalf("manager retry policy was not exercised: calls=%d", baseline)
				}
			}
			if n := e.Calls(); n != baseline {
				t.Fatalf("handler reset per-request GPT budget: calls=%d want=%d bootstrapRetries=%d", n, baseline, bootstrapRetries)
			}
		})
	}
}

func TestHandlerDoesNotReplayTerminalFailure(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, tc := range []struct {
			name string
			f    *failurecontract.Failure
		}{
			{"committed", &failurecontract.Failure{Kind: failurecontract.TransportError, Scope: failurecontract.ScopeProvider, HTTPStatus: 502, Retryable: true, OutputCommitted: true}},
			{"after_output_phase", &failurecontract.Failure{Kind: failurecontract.TransportError, Scope: failurecontract.ScopeProvider, HTTPStatus: 502, Retryable: true, StreamPhase: failurecontract.StreamPhaseAfterOutput}},
			{"request_scope", &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeRequest, HTTPStatus: 503, Retryable: true}},
			{"nonretryable", &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeRequest, HTTPStatus: 503, Retryable: false}},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				e := &retryContractStreamExecutor{provider: provider, failure: fmt.Errorf("wrapped failure: %w", tc.f)}
				m := coreauth.NewManager(nil, nil, nil)
				m.RegisterExecutor(e)
				const model = "test-retry-contract"
				registerRetryContractAuth(t, m, provider, model, 2)
				payload, err := runRetryContractHandler(t, m, model, 2)
				if n := e.Calls(); n != 1 {
					t.Fatalf("terminal failure replayed: calls=%d want=1", n)
				}
				if err == nil || len(payload) != 0 {
					t.Fatalf("want original terminal failure without output, got payload=%q error=%v", payload, err)
				}
				if got, ok := failurecontract.As(err); !ok || got != tc.f {
					t.Fatalf("original failure contract lost: %v", err)
				}
			})
		}
	}
}

func TestHandlerManagerRetainsIndependentChannelFailover(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
			t.Run(fmt.Sprintf("%s/%d", provider, status), func(t *testing.T) {
				e := &retryContractStreamExecutor{provider: provider, failure: &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider, HTTPStatus: status, Retryable: true, StreamPhase: failurecontract.StreamPhaseBeforeOutput}}
				m := coreauth.NewManager(nil, nil, nil)
				m.RegisterExecutor(e)
				model := "claude-test-retry-contract"
				if provider == "codex" {
					model = "gpt-test-retry-contract"
				}
				registerRetryContractAuth(t, m, provider, model, 2)
				payload, err := runRetryContractHandler(t, m, model, 0)
				if n := e.Calls(); n != 2 || err != nil || string(payload) != "ok" {
					t.Fatalf("safe manager failover lost: calls=%d payload=%q error=%v", n, payload, err)
				}
			})
		}
	}
}

func TestHandlerNonRetryableFailureDoesNotRestartManager(t *testing.T) {
	failure := &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider, HTTPStatus: 503, Retryable: false}
	e := &retryContractStreamExecutor{provider: "claude", failure: failure, alwaysFail: true}
	m := coreauth.NewManager(nil, nil, nil)
	m.RegisterExecutor(e)
	const model = "claude-bootstrap-contract"
	registerRetryContractAuth(t, m, e.provider, model, 2)
	payload, err := runRetryContractHandler(t, m, model, 2)
	// The manager may try a genuinely independent route. Once it finishes, the
	// handler must preserve its terminal failure instead of starting selection
	// again and replacing the failure with a local exhausted-pool error.
	if got, ok := failurecontract.As(err); !ok || got != failure || len(payload) != 0 || e.Calls() != 2 {
		t.Fatalf("manager terminal failure replaced/replayed: calls=%d payload=%q error=%v", e.Calls(), payload, err)
	}
}
