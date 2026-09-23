package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func longResponsesToolOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RequestPathMetadataKey:          "/v1/responses",
		cliproxyexecutor.ClientProfileMetadataKey:        "codex",
		cliproxyexecutor.MessageCountMetadataKey:         272,
		cliproxyexecutor.ToolInteractionCountMetadataKey: 12,
	}}
}

func TestLongToolHistoryDoesNotReplayCommittedFailure(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.SetRetryConfig(3, time.Second, 5)
	committed := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusBadGateway, Retryable: true,
		OutputCommitted: true, StreamPhase: failurecontract.StreamPhaseAfterOutput,
		PublicMessage: "failure after output",
	}
	exec := &authFallbackExecutor{id: "codex", streamFirstErrors: map[string]error{"verified-a": committed}}
	m.RegisterExecutor(exec)
	const model = "gpt-5.6-sol"
	registerGPTChannelFailoverAuths(t, m, "codex", model, []*Auth{
		{ID: "verified-a", Provider: "codex", Attributes: map[string]string{"base_url": "https://verified-a.example/v1", "native_responses": "true", "priority": "20"}},
		{ID: "verified-b", Provider: "codex", Attributes: map[string]string{"base_url": "https://verified-b.example/v1", "native_responses": "true", "priority": "10"}},
	})
	result, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, longResponsesToolOptions())
	if err == nil {
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if err == nil {
		t.Fatal("expected committed upstream failure")
	}
	if got := exec.StreamCalls(); !reflect.DeepEqual(got, []string{"verified-a"}) {
		t.Fatalf("unsafe stream replay: %v", got)
	}
}

func TestLongToolHistoryHealthAndFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{0, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("stream=%t/first_status=%d", stream, status), func(t *testing.T) {
				m := NewManager(nil, &FillFirstSelector{}, nil)
				m.SetRetryConfig(0, 0, 5)
				exec := &authFallbackExecutor{id: "codex", executeErrors: map[string]error{}, streamFirstErrors: map[string]error{}}
				if status > 0 {
					var failure error = retryableGPTChannelFailure(status)
					if status == http.StatusBadRequest {
						failure = &Error{HTTPStatus: status, Code: "request_feature_unsupported", Message: "invalid tool result"}
					}
					exec.executeErrors["verified-a"] = failure
					exec.streamFirstErrors["verified-a"] = failure
				}
				m.RegisterExecutor(exec)
				const model = "gpt-5.6-sol"
				auths := []*Auth{
					{ID: "bad-oauth", Provider: "codex", Attributes: map[string]string{"priority": "100"}, ModelStates: map[string]*ModelState{model: {
						Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(5 * time.Minute),
						LastError: &Error{HTTPStatus: http.StatusUnauthorized},
						Health:    HealthState{Observed: true, LastStatusCode: http.StatusUnauthorized, BreakerState: HealthBreakerOpen, OpenUntil: time.Now().Add(5 * time.Minute)},
					}}},
					{ID: "unknown", Provider: "codex", Attributes: map[string]string{"base_url": "https://unknown.example/v1", "priority": "90"}},
					{ID: "verified-a", Provider: "codex", Attributes: map[string]string{"base_url": "https://verified-a.example/v1", "native_responses": "true", "priority": "20"}},
					{ID: "verified-b", Provider: "codex", Attributes: map[string]string{"base_url": "https://verified-b.example/v1", "native_responses": "true", "priority": "10"}},
				}
				registerGPTChannelFailoverAuths(t, m, "codex", model, auths)
				req := cliproxyexecutor.Request{Model: model}
				var err error
				if stream {
					var result *cliproxyexecutor.StreamResult
					result, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, longResponsesToolOptions())
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = m.Execute(context.Background(), []string{"codex"}, req, longResponsesToolOptions())
				}
				want := []string{"verified-a"}
				if status == http.StatusBadRequest {
					if statusCodeFromError(err) != http.StatusBadRequest {
						t.Fatalf("error=%v, want deterministic 400", err)
					}
				} else {
					if err != nil {
						t.Fatalf("compatible route should succeed: %v", err)
					}
					if status > 0 {
						want = append(want, "verified-b")
					}
				}
				calls := exec.ExecuteCalls()
				if stream {
					calls = exec.StreamCalls()
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("calls=%v, want %v", calls, want)
				}
			})
		}
	}
}

func TestGPTLongPlainTextDoesNotRequireNativeToolCapability(t *testing.T) {
	opts := longResponsesToolOptions()
	opts.Metadata[cliproxyexecutor.MessageCountMetadataKey] = 320
	opts.Metadata[cliproxyexecutor.ToolInteractionCountMetadataKey] = 0
	custom := &Auth{ID: "custom", Provider: "codex", Attributes: map[string]string{"base_url": "https://custom.example/v1"}}
	native := &Auth{ID: "native", Provider: "codex"}
	got, excluded := preferGPTNativeResponsesAuths([]*Auth{custom, native}, []string{"codex"}, "gpt-5.6-sol", opts)
	if len(got) != 2 || excluded != 0 {
		t.Fatalf("plain text candidates=%d excluded=%d, want 2/0", len(got), excluded)
	}
}

func TestGPTLongToolHistoryRejectsUnknownBeforeDispatch(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 5)
	exec := &authFallbackExecutor{id: "codex"}
	m.RegisterExecutor(exec)
	const model = "gpt-5.6-sol"
	registerGPTChannelFailoverAuths(t, m, "codex", model, []*Auth{
		{ID: "unverified", Provider: "codex", Attributes: map[string]string{"base_url": "https://unverified.example/v1"}},
	})
	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, longResponsesToolOptions())
	if statusCodeFromError(err) != http.StatusBadRequest || errorCodeFromError(err) != "request_feature_unsupported" {
		t.Fatalf("error=%v, want explicit nonretryable capability error", err)
	}
	if calls := exec.ExecuteCalls(); len(calls) != 0 {
		t.Fatalf("unverified route was dispatched: %v", calls)
	}
}

func TestLongToolHistoryExhaustedCompatiblePoolIsNotRateLimit(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.SetRetryConfig(0, 0, 5)
	exec := &authFallbackExecutor{id: "codex"}
	m.RegisterExecutor(exec)
	const model = "gpt-5.6-sol"
	// The pool has already consumed its bounded half-open probe. Observe the
	// terminal no-attempt response while that probe lease is still cooling down.
	m.halfOpenProbeNext[halfOpenProbeKey("bad-native", model)] = time.Now().Add(5 * time.Minute)
	registerGPTChannelFailoverAuths(t, m, "codex", model, []*Auth{
		{ID: "bad-native", Provider: "codex", ModelStates: map[string]*ModelState{model: {
			Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(5 * time.Minute), LastError: &Error{HTTPStatus: http.StatusUnauthorized},
			Health: HealthState{Observed: true, LastStatusCode: http.StatusUnauthorized, BreakerState: HealthBreakerOpen, OpenUntil: time.Now().Add(5 * time.Minute)},
		}}},
		{ID: "unknown", Provider: "codex", Attributes: map[string]string{"base_url": "https://unknown.example/v1"}},
	})
	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, longResponsesToolOptions())
	if statusCodeFromError(err) != http.StatusServiceUnavailable || errorCodeFromError(err) != "auth_unavailable" {
		t.Fatalf("error=%v status=%d code=%s, want 503/auth_unavailable", err, statusCodeFromError(err), errorCodeFromError(err))
	}
	if calls := exec.ExecuteCalls(); len(calls) != 0 {
		t.Fatalf("unavailable pool executed upstream: %v", calls)
	}
}
