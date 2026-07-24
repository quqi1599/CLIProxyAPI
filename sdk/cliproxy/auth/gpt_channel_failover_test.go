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

func TestManagerGPTChannelFailoverStopsWithinModelPool(t *testing.T) {
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
			want := []string{firstModel}
			if !stringSlicesEqual(got, want) {
				t.Fatalf("%s models = %v, want %v", test.name, got, want)
			}
		})
	}
}

func TestManagerGPTChannelBreakerStopsModelPoolAfterThree5xx(t *testing.T) {
	const alias = "gpt-breaker-pool"
	models := []internalconfig.CodexModel{
		{Name: "gpt-breaker-a", Alias: alias},
		{Name: "gpt-breaker-b", Alias: alias},
		{Name: "gpt-breaker-c", Alias: alias},
		{Name: "gpt-breaker-d", Alias: alias},
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
				t.Fatalf("%s unexpectedly reached the fourth model", test.name)
			}
			want := []string{models[0].Name, models[1].Name, models[2].Name}
			if got := test.calls(test.executor); !stringSlicesEqual(got, want) {
				t.Fatalf("%s models = %v, want %v", test.name, got, want)
			}
		})
	}
}

func TestManagerGPTChannelFailoverCapsFiveDistinctChannelsWithoutOuterRetry(t *testing.T) {
	const (
		model    = "gpt-5.5"
		provider = "gpt-channel-cap"
	)
	for _, operation := range gptChannelFailoverOperations() {
		t.Run(operation.name, func(t *testing.T) {
			failure := retryableGPTChannelFailure(http.StatusServiceUnavailable)
			executor := &authFallbackExecutor{
				id:                provider,
				executeErrors:     make(map[string]error),
				countErrors:       make(map[string]error),
				streamFirstErrors: make(map[string]error),
			}
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(10, 30*time.Second, 10)
			manager.RegisterExecutor(executor)

			auths := make([]*Auth, 0, 6)
			for i := 1; i <= 6; i++ {
				authID := fmt.Sprintf("channel-%d-%s", i, operation.name)
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
			if len(got) != 5 {
				t.Fatalf("%s calls = %v, want five distinct channels", operation.name, got)
			}
			seen := make(map[string]struct{}, len(got))
			for _, authID := range got {
				if _, duplicate := seen[authID]; duplicate {
					t.Fatalf("%s retried an outer channel round: %v", operation.name, got)
				}
				seen[authID] = struct{}{}
			}
		})
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
	manager.gptChannelBreakers[routingChannelBaseKey(oauth)] = &codexChannelBreakerState{
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
		manager.gptChannelBreakers[routingChannelBaseKey(auth)] = &codexChannelBreakerState{
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
