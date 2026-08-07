package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type gptChannelFailoverOperation struct {
	name   string
	invoke func(context.Context, *Manager, string, cliproxyexecutor.Request) error
	calls  func(*authFallbackExecutor) []string
}

func gptChannelFailoverOperations() []gptChannelFailoverOperation {
	return []gptChannelFailoverOperation{
		{
			name: "execute",
			invoke: func(ctx context.Context, manager *Manager, provider string, request cliproxyexecutor.Request) error {
				_, err := manager.Execute(ctx, []string{provider}, request, cliproxyexecutor.Options{})
				return err
			},
			calls: (*authFallbackExecutor).ExecuteCalls,
		},
		{
			name: "count",
			invoke: func(ctx context.Context, manager *Manager, provider string, request cliproxyexecutor.Request) error {
				_, err := manager.ExecuteCount(ctx, []string{provider}, request, cliproxyexecutor.Options{})
				return err
			},
			calls: (*authFallbackExecutor).CountCalls,
		},
		{
			name: "stream",
			invoke: func(ctx context.Context, manager *Manager, provider string, request cliproxyexecutor.Request) error {
				result, err := manager.ExecuteStream(ctx, []string{provider}, request, cliproxyexecutor.Options{})
				if err != nil {
					return err
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			},
			calls: (*authFallbackExecutor).StreamCalls,
		},
	}
}

func retryableGPTChannelFailure(status int) error {
	retryNow := time.Duration(0)
	kind := failurecontract.ProviderUnavailable
	scope := failurecontract.ScopeProvider
	message := http.StatusText(status)
	if status == http.StatusTooManyRequests {
		kind = failurecontract.RateLimited
		scope = failurecontract.ScopeCredential
		message = "rate limit"
	}
	return &failurecontract.Failure{
		Kind:          kind,
		Scope:         scope,
		HTTPStatus:    status,
		RetryAfter:    &retryNow,
		Retryable:     true,
		PublicMessage: message,
	}
}

func registerGPTChannelFailoverAuths(t *testing.T, manager *Manager, provider, model string, auths []*Auth) {
	t.Helper()
	modelRegistry := registry.GetGlobalRegistry()
	for _, auth := range auths {
		modelRegistry.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			modelRegistry.UnregisterClient(auth.ID)
		}
	})
}

func TestManagerGPTChannelFailoverSwitchesBaseURLImmediately(t *testing.T) {
	const (
		model    = "gpt-5.5"
		provider = "gpt-channel-failover"
	)
	statuses := []int{
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	}

	for _, operation := range gptChannelFailoverOperations() {
		for _, status := range statuses {
			t.Run(fmt.Sprintf("%s/%d", operation.name, status), func(t *testing.T) {
				failure := retryableGPTChannelFailure(status)
				executor := &authFallbackExecutor{
					id: provider,
					executeErrors: map[string]error{
						"aa-bad-primary": failure,
						"ab-bad-peer":    failure,
					},
					countErrors: map[string]error{
						"aa-bad-primary": failure,
						"ab-bad-peer":    failure,
					},
					streamFirstErrors: map[string]error{
						"aa-bad-primary": failure,
						"ab-bad-peer":    failure,
					},
				}
				manager := NewManager(nil, nil, nil)
				manager.SetRetryConfig(10, 30*time.Second, 10)
				manager.RegisterExecutor(executor)

				auths := []*Auth{
					openAICompatChannelBreakerAuth("aa-bad-primary", provider, "https://bad.example/v1", 10),
					openAICompatChannelBreakerAuth("ab-bad-peer", provider, "https://bad.example/v1", 10),
					openAICompatChannelBreakerAuth("ba-good-backup", provider, "https://good.example/v1", 1),
				}
				registerGPTChannelFailoverAuths(t, manager, provider, model, auths)

				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				err := operation.invoke(ctx, manager, provider, cliproxyexecutor.Request{Model: model})
				if err != nil {
					t.Fatalf("%s returned error: %v", operation.name, err)
				}
				got := operation.calls(executor)
				want := []string{"aa-bad-primary", "ba-good-backup"}
				if !stringSlicesEqual(got, want) {
					t.Fatalf("%s calls = %v, want %v", operation.name, got, want)
				}
			})
		}
	}
}

func TestManagerGPTChannelFailoverDoesNotSwitchModelsAcrossRounds(t *testing.T) {
	const (
		alias      = "gpt-pool"
		firstModel = "gpt-pool-a"
	)
	failure := retryableGPTChannelFailure(http.StatusServiceUnavailable)

	tests := []struct {
		name     string
		executor *apiKeyPoolExecutor
		invoke   func(context.Context, *Manager) error
		calls    func(*apiKeyPoolExecutor) []string
	}{
		{
			name: "execute",
			executor: &apiKeyPoolExecutor{
				id:            "codex",
				executeErrors: map[string]error{firstModel: failure},
			},
			invoke: func(ctx context.Context, manager *Manager) error {
				_, err := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
				return err
			},
			calls: (*apiKeyPoolExecutor).ExecuteModels,
		},
		{
			name: "count",
			executor: &apiKeyPoolExecutor{
				id:          "codex",
				countErrors: map[string]error{firstModel: failure},
			},
			invoke: func(ctx context.Context, manager *Manager) error {
				_, err := manager.ExecuteCount(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
				return err
			},
			calls: (*apiKeyPoolExecutor).CountModels,
		},
		{
			name: "stream",
			executor: &apiKeyPoolExecutor{
				id:                "codex",
				streamFirstErrors: map[string]error{firstModel: failure},
			},
			invoke: func(ctx context.Context, manager *Manager) error {
				result, err := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
				if err != nil {
					return err
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			},
			calls: (*apiKeyPoolExecutor).StreamModels,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newCodexAPIKeyPoolTestManager(t, alias, []internalconfig.CodexModel{
				{Name: firstModel, Alias: alias},
				{Name: "gpt-pool-b", Alias: alias},
			}, test.executor)
			manager.SetRetryConfig(10, 30*time.Second, 10)

			err := test.invoke(context.Background(), manager)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded via the same channel model pool", test.name)
			}
			got := test.calls(test.executor)
			want := []string{firstModel, firstModel, firstModel}
			if !stringSlicesEqual(got, want) {
				t.Fatalf("%s models = %v, want %v", test.name, got, want)
			}
		})
	}
}

func TestManagerGPTChannelBreakerStopsModelPoolAfterFive5xx(t *testing.T) {
	const alias = "gpt-breaker-pool"
	models := []internalconfig.CodexModel{
		{Name: "gpt-breaker-a", Alias: alias},
		{Name: "gpt-breaker-b", Alias: alias},
		{Name: "gpt-breaker-c", Alias: alias},
		{Name: "gpt-breaker-d", Alias: alias},
		{Name: "gpt-breaker-e", Alias: alias},
		{Name: "gpt-breaker-f", Alias: alias},
	}
	failure := &Error{
		HTTPStatus: http.StatusInternalServerError,
		Code:       "upstream_internal_error",
		Message:    "upstream internal error",
		Retryable:  true,
	}
	failures := map[string]error{
		models[0].Name: failure,
		models[1].Name: failure,
		models[2].Name: failure,
		models[3].Name: failure,
		models[4].Name: failure,
	}
	tests := []struct {
		name     string
		executor *apiKeyPoolExecutor
		invoke   func(context.Context, *Manager) error
		calls    func(*apiKeyPoolExecutor) []string
	}{
		{
			name:     "execute",
			executor: &apiKeyPoolExecutor{id: "codex", executeErrors: failures},
			invoke: func(ctx context.Context, manager *Manager) error {
				_, err := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
				return err
			},
			calls: (*apiKeyPoolExecutor).ExecuteModels,
		},
		{
			name:     "count",
			executor: &apiKeyPoolExecutor{id: "codex", countErrors: failures},
			invoke: func(ctx context.Context, manager *Manager) error {
				_, err := manager.ExecuteCount(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
				return err
			},
			calls: (*apiKeyPoolExecutor).CountModels,
		},
		{
			name:     "stream",
			executor: &apiKeyPoolExecutor{id: "codex", streamFirstErrors: failures},
			invoke: func(ctx context.Context, manager *Manager) error {
				result, err := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
				if err != nil {
					return err
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			},
			calls: (*apiKeyPoolExecutor).StreamModels,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newCodexAPIKeyPoolTestManager(t, alias, models, test.executor)
			manager.SetRetryConfig(10, 30*time.Second, 10)
			if err := test.invoke(context.Background(), manager); err == nil {
				t.Fatalf("%s unexpectedly reached the sixth model", test.name)
			}
			want := []string{models[0].Name, models[1].Name, models[2].Name, models[3].Name, models[4].Name}
			if got := test.calls(test.executor); !stringSlicesEqual(got, want) {
				t.Fatalf("%s models = %v, want %v", test.name, got, want)
			}
		})
	}
}

func TestManagerGPTChannelFailoverUsesBoundedRounds(t *testing.T) {
	const (
		model    = "gpt-5.5"
		provider = "gpt-channel-cap"
	)
	tests := []struct {
		status int
		rounds int
	}{
		{status: http.StatusTooManyRequests, rounds: 2},
		{status: http.StatusBadGateway, rounds: 2},
		{status: http.StatusServiceUnavailable, rounds: 3},
	}
	for _, test := range tests {
		for _, channelCount := range []int{6, gptImmediateFailoverMaxChannels} {
			for _, operation := range gptChannelFailoverOperations() {
				t.Run(fmt.Sprintf("%s/%d/%d-channels", operation.name, test.status, channelCount), func(t *testing.T) {
					failure := retryableGPTChannelFailure(test.status)
					executor := &authFallbackExecutor{
						id:                provider,
						executeErrors:     make(map[string]error),
						countErrors:       make(map[string]error),
						streamFirstErrors: make(map[string]error),
					}
					manager := NewManager(nil, nil, nil)
					manager.SetRetryConfig(10, 30*time.Second, 10)
					manager.RegisterExecutor(executor)

					auths := make([]*Auth, 0, channelCount)
					for i := 1; i <= channelCount; i++ {
						authID := fmt.Sprintf("channel-%d-%s-%d", i, operation.name, test.status)
						executor.executeErrors[authID] = failure
						executor.countErrors[authID] = failure
						executor.streamFirstErrors[authID] = failure
						auths = append(auths, openAICompatChannelBreakerAuth(
							authID,
							provider,
							fmt.Sprintf("https://channel-%d.example/v1", i),
							10,
						))
					}
					registerGPTChannelFailoverAuths(t, manager, provider, model, auths)

					err := operation.invoke(context.Background(), manager, provider, cliproxyexecutor.Request{Model: model})
					if err == nil {
						t.Fatalf("%s unexpectedly succeeded", operation.name)
					}
					got := operation.calls(executor)
					if want := channelCount * test.rounds; len(got) != want {
						t.Fatalf("%s calls = %d, want %d: %v", operation.name, len(got), want, got)
					}
					for round := 0; round < test.rounds; round++ {
						seen := make(map[string]struct{}, channelCount)
						for _, authID := range got[round*channelCount : (round+1)*channelCount] {
							seen[authID] = struct{}{}
						}
						if len(seen) != channelCount {
							t.Fatalf("%s round %d did not try %d distinct channels: %v", operation.name, round+1, channelCount, got)
						}
					}
				})
			}
		}
	}
}

func TestManagerGPTChannelFailoverThirdRoundOnlyRetriesEligibleChannels(t *testing.T) {
	const (
		model    = "gpt-5.5"
		provider = "gpt-third-round"
	)
	executor := &authFallbackExecutor{
		id: provider,
		executeErrors: map[string]error{
			"channel-429": retryableGPTChannelFailure(http.StatusTooManyRequests),
			"channel-502": retryableGPTChannelFailure(http.StatusBadGateway),
			"channel-503": retryableGPTChannelFailure(http.StatusServiceUnavailable),
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(10, 30*time.Second, 10)
	manager.RegisterExecutor(executor)
	registerGPTChannelFailoverAuths(t, manager, provider, model, []*Auth{
		openAICompatChannelBreakerAuth("channel-429", provider, "https://channel-429.example/v1", 10),
		openAICompatChannelBreakerAuth("channel-502", provider, "https://channel-502.example/v1", 10),
		openAICompatChannelBreakerAuth("channel-503", provider, "https://channel-503.example/v1", 10),
	})

	if _, err := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("execute unexpectedly succeeded")
	}
	counts := make(map[string]int)
	for _, authID := range executor.ExecuteCalls() {
		counts[authID]++
	}
	if counts["channel-429"] != 2 || counts["channel-502"] != 2 || counts["channel-503"] != 3 {
		t.Fatalf("channel attempt counts = %v, want 429=2, 502=2, 503=3", counts)
	}
}

func TestShouldRetryGPTRoundPolicy(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		round int
		want  bool
	}{
		{name: "first 429", err: retryableGPTChannelFailure(http.StatusTooManyRequests), round: 0, want: true},
		{name: "third 429 blocked", err: retryableGPTChannelFailure(http.StatusTooManyRequests), round: 1, want: false},
		{name: "third 502 blocked", err: retryableGPTChannelFailure(http.StatusBadGateway), round: 1, want: false},
		{name: "third 503", err: retryableGPTChannelFailure(http.StatusServiceUnavailable), round: 1, want: true},
		{name: "third network", err: &Error{Code: "upstream_network_error", Message: "connection reset by peer", Retryable: true}, round: 1, want: true},
		{name: "third auth unavailable", err: &Error{Code: "auth_unavailable", Message: "no auth available"}, round: 1, want: true},
		{name: "400 blocked", err: &Error{HTTPStatus: http.StatusBadRequest, Code: "invalid_request_error", Message: "bad request"}, round: 0, want: false},
		{name: "fourth blocked", err: retryableGPTChannelFailure(http.StatusServiceUnavailable), round: 2, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := shouldRetryGPTRound(test.err, test.round, []string{"codex"}, "gpt-5.5", nil)
			if got != test.want {
				t.Fatalf("shouldRetryGPTRound() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestManagerAttemptRunnerGPTPreservesShortModelCooldownWait(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(1, 50*time.Millisecond, 0)
	auth := &Auth{ID: "cooldown-auth", Provider: "codex", Status: StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, trace := ensureRequestAttemptTrace(context.Background())
	trace.configureGPTRoute(true)
	calls := 0
	runner := managerAttemptRunner[cliproxyexecutor.Response]{
		manager: manager,
		runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (cliproxyexecutor.Response, error) {
			calls++
			if calls == 1 {
				return cliproxyexecutor.Response{}, newModelCooldownError("gpt-5.5", "codex", time.Millisecond)
			}
			return cliproxyexecutor.Response{}, nil
		},
	}

	outcome := runner.run(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.5"}, cliproxyexecutor.Options{}, 0, 50*time.Millisecond)
	if !outcome.success || calls != 2 {
		t.Fatalf("outcome success/calls = %t/%d, want true/2", outcome.success, calls)
	}
}

func TestManagerGPTChannelFailoverDoesNotGateCodexOAuth(t *testing.T) {
	const (
		model    = "gpt-5.5"
		provider = "codex"
	)
	executor := &authFallbackExecutor{id: provider}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	oauth := &Auth{
		ID:       "codex-oauth",
		Provider: provider,
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "test-token"},
	}
	registerGPTChannelFailoverAuths(t, manager, provider, model, []*Auth{oauth})

	now := time.Now()
	manager.gptChannelBreakers[gptChannelBreakerKey(oauth, model)] = &codexChannelBreakerState{
		Health: HealthState{
			BreakerState: HealthBreakerOpen,
			OpenUntil:    now.Add(time.Minute),
		},
	}
	if blocked, _ := manager.healthSelectionBlocked(oauth, model, now); blocked {
		t.Fatal("Codex OAuth was blocked by GPT channel health")
	}

	_, err := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if got, want := executor.ExecuteCalls(), []string{oauth.ID}; !stringSlicesEqual(got, want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
}

func TestManagerGPTChannelFailoverSkipsOpenChannelsWithoutSpendingBudget(t *testing.T) {
	const (
		model    = "gpt-5.5"
		provider = "codex"
	)
	executor := &authFallbackExecutor{id: provider}
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(10, 30*time.Second, 10)
	manager.RegisterExecutor(executor)

	auths := make([]*Auth, 0, 6)
	for i := 1; i <= 6; i++ {
		auths = append(auths, &Auth{
			ID:       fmt.Sprintf("channel-%d", i),
			Provider: provider,
			Status:   StatusActive,
			Attributes: map[string]string{
				AttributeAPIKey: fmt.Sprintf("test-key-%d", i),
				"base_url":      fmt.Sprintf("https://channel-%d.example/v1", i),
				"priority":      "10",
			},
		})
	}
	registerGPTChannelFailoverAuths(t, manager, provider, model, auths)

	now := time.Now()
	for _, auth := range auths[:5] {
		manager.gptChannelBreakers[gptChannelBreakerKey(auth, model)] = &codexChannelBreakerState{
			Health: HealthState{
				BreakerState: HealthBreakerOpen,
				OpenUntil:    now.Add(time.Minute),
			},
		}
	}

	_, err := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if got, want := executor.ExecuteCalls(), []string{"channel-6"}; !stringSlicesEqual(got, want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
}

func TestManagerMixedProviderNonGPTRouteDoesNotUseGPTChannelFailover(t *testing.T) {
	const model = "claude-sonnet-mixed"
	failure := retryableGPTChannelFailure(http.StatusServiceUnavailable)
	codexExecutor := &authFallbackExecutor{
		id:            "codex",
		executeErrors: map[string]error{"aa-codex-primary": failure},
	}
	claudeExecutor := &authFallbackExecutor{id: "claude"}
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(10, 30*time.Second, 10)
	manager.RegisterExecutor(codexExecutor)
	manager.RegisterExecutor(claudeExecutor)

	codexAuths := []*Auth{
		{
			ID:       "aa-codex-primary",
			Provider: "codex",
			Status:   StatusActive,
			Attributes: map[string]string{
				AttributeAPIKey: "test-key-a",
				"base_url":      "https://codex.example/v1",
				"priority":      "10",
			},
		},
		{
			ID:       "ab-codex-peer",
			Provider: "codex",
			Status:   StatusActive,
			Attributes: map[string]string{
				AttributeAPIKey: "test-key-b",
				"base_url":      "https://codex.example/v1",
				"priority":      "10",
			},
		},
	}
	claudeAuth := &Auth{
		ID:       "ba-claude-backup",
		Provider: "claude",
		Status:   StatusActive,
		Attributes: map[string]string{
			"priority": "1",
		},
	}
	registerGPTChannelFailoverAuths(t, manager, "codex", model, codexAuths)
	registerGPTChannelFailoverAuths(t, manager, "claude", model, []*Auth{claudeAuth})

	_, err := manager.Execute(context.Background(), []string{"codex", "claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if got, want := codexExecutor.ExecuteCalls(), []string{"aa-codex-primary", "ab-codex-peer"}; !stringSlicesEqual(got, want) {
		t.Fatalf("Codex execute calls = %v, want %v", got, want)
	}
	if got := claudeExecutor.ExecuteCalls(); len(got) != 0 {
		t.Fatalf("Claude fallback calls = %v, want none", got)
	}
}

func TestManagerMixedProviderNonGPTRouteIgnoresGPTChannelBreaker(t *testing.T) {
	const model = "claude-sonnet-mixed"
	codexExecutor := &authFallbackExecutor{id: "codex"}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexExecutor)
	manager.RegisterExecutor(&authFallbackExecutor{id: "claude"})

	codexAuth := &Auth{
		ID:       "mixed-non-gpt-codex",
		Provider: "codex",
		Status:   StatusActive,
		Attributes: map[string]string{
			AttributeAPIKey: "test-key",
			"base_url":      "https://mixed-non-gpt.example/v1",
		},
		Health: HealthState{
			Observed:     true,
			Score:        10,
			BreakerState: HealthBreakerOpen,
			OpenUntil:    time.Now().Add(time.Minute),
		},
	}
	registerGPTChannelFailoverAuths(t, manager, "codex", model, []*Auth{codexAuth})

	if _, err := manager.Execute(context.Background(), []string{"codex", "claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("execute mixed non-GPT route: %v", err)
	}
	if got, want := codexExecutor.ExecuteCalls(), []string{codexAuth.ID}; !stringSlicesEqual(got, want) {
		t.Fatalf("Codex execute calls = %v, want %v", got, want)
	}
}

func TestManagerMixedProviderNonGPTRouteIgnoresGPTCooldownWait(t *testing.T) {
	const model = "claude-sonnet-mixed"
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 30*time.Second, 3)

	codexAuth := &Auth{
		ID:       "mixed-cooldown-codex",
		Provider: "codex",
		Status:   StatusActive,
		Attributes: map[string]string{
			AttributeAPIKey: "test-key",
			"base_url":      "https://mixed-cooldown.example/v1",
		},
		Health: HealthState{
			Observed:     true,
			Score:        10,
			BreakerState: HealthBreakerOpen,
			OpenUntil:    time.Now().Add(time.Minute),
		},
	}
	claudeAuth := &Auth{ID: "mixed-cooldown-claude", Provider: "claude", Status: StatusActive}
	for _, auth := range []*Auth{codexAuth, claudeAuth} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %q: %v", auth.ID, errRegister)
		}
	}

	if wait, found := manager.closestCooldownWait([]string{"codex", "claude"}, model, 0); found {
		t.Fatalf("mixed non-GPT cooldown wait = %v, want no GPT-channel wait", wait)
	}
}
