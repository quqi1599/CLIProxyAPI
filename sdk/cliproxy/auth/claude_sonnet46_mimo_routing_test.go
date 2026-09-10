package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const claudeSonnet46MiMoTestAlias = "claude-sonnet-4-6"

func TestClaudeSonnet46MiMoHistoryIncompatibleIsAliasAndIntentScoped(t *testing.T) {
	missingOpenAI := []byte(`{
		"reasoning_effort":"high",
		"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]
	}`)
	completeOpenAI := []byte(`{
		"reasoning_effort":"high",
		"messages":[{"role":"assistant","reasoning_content":"real plan","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]
	}`)
	disabledOpenAI := []byte(`{
		"reasoning_effort":"none",
		"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]
	}`)
	missingClaude := []byte(`{
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]}]
	}`)

	tests := []struct {
		name         string
		routeModel   string
		payload      []byte
		format       sdktranslator.Format
		requested    string
		incompatible bool
	}{
		{name: "OpenAI missing history", routeModel: claudeSonnet46MiMoTestAlias, payload: missingOpenAI, format: sdktranslator.FormatOpenAI, incompatible: true},
		{name: "Claude missing history", routeModel: claudeSonnet46MiMoTestAlias, payload: missingClaude, format: sdktranslator.FormatClaude, incompatible: true},
		{name: "complete history", routeModel: claudeSonnet46MiMoTestAlias, payload: completeOpenAI, format: sdktranslator.FormatOpenAI},
		{name: "thinking disabled", routeModel: claudeSonnet46MiMoTestAlias, payload: disabledOpenAI, format: sdktranslator.FormatOpenAI},
		{name: "thinking default", routeModel: claudeSonnet46MiMoTestAlias, payload: []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1"}]}]}`), format: sdktranslator.FormatOpenAI},
		{name: "other aggregate alias", routeModel: "claude-opus-4-6", payload: missingOpenAI, format: sdktranslator.FormatOpenAI},
		{name: "direct MiMo request", routeModel: "mimo-v2.5", payload: missingOpenAI, format: sdktranslator.FormatOpenAI},
		{name: "requested alias metadata", routeModel: "mimo-v2.5", requested: claudeSonnet46MiMoTestAlias, payload: missingOpenAI, format: sdktranslator.FormatOpenAI, incompatible: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := map[string]any{}
			if test.requested != "" {
				metadata[cliproxyexecutor.RequestedModelMetadataKey] = test.requested
			}
			opts := cliproxyexecutor.Options{
				SourceFormat:    test.format,
				OriginalRequest: test.payload,
				Metadata:        metadata,
			}
			got := claudeSonnet46MiMoHistoryIncompatible(test.routeModel, cliproxyexecutor.Request{Model: test.routeModel, Payload: test.payload}, opts)
			if got != test.incompatible {
				t.Fatalf("incompatible = %t, want %t", got, test.incompatible)
			}
		})
	}
}

func TestClaudeSonnet46MiMoExecutionModelFilterKeepsSafeFallback(t *testing.T) {
	payload := []byte(`{
		"reasoning_effort":"high",
		"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]
	}`)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: payload}
	auth := &Auth{Attributes: map[string]string{"compat_kind": "xiaomi"}}
	req := cliproxyexecutor.Request{Model: claudeSonnet46MiMoTestAlias, Payload: payload}

	got := filterClaudeSonnet46MiMoExecutionModels(auth, req.Model, req, opts, []string{"mimo-v2.5", "safe-model"})
	if len(got) != 1 || got[0] != "safe-model" {
		t.Fatalf("filtered models = %v, want [safe-model]", got)
	}

	got = filterClaudeSonnet46MiMoExecutionModels(auth, req.Model, req, opts, []string{"mimo-v2.5"})
	if len(got) != 1 || got[0] != "mimo-v2.5" {
		t.Fatalf("sole MiMo model = %v, want preserved for actionable validation error", got)
	}
}

func TestClaudeSonnet46MiMoCompatibilityFilterUpdatesCandidateTelemetry(t *testing.T) {
	trace := &requestAttemptTrace{}
	trace.recordSelectionAvailability(selectionAvailabilitySummary{total: 1, ready: 1})
	trace.recordSelectionCompatibilityPrefiltered(1)
	fields := trace.selectionLogFields()
	if fields["candidate_count"] != 2 || fields["candidate_ready_count"] != 1 || fields["candidate_skipped_compatibility"] != 1 {
		t.Fatalf("selection fields = %v", fields)
	}
}

func TestManagerClaudeSonnet46AffinityReselectsBeforeIncompatibleMiMoExecution(t *testing.T) {
	const (
		xiaomiProvider  = "xiaomi-route"
		minimaxProvider = "minimax-route"
		xiaomiAuthID    = "aa-xiaomi-auth"
		minimaxAuthID   = "zz-minimax-auth"
	)

	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	manager := NewManager(nil, selector, nil)
	manager.SetConfig(&internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{
		{
			Name:    xiaomiProvider,
			Kind:    "xiaomi",
			BaseURL: "https://token-plan-cn.xiaomimimo.com/v1",
			Models:  []internalconfig.OpenAICompatibilityModel{{Name: "mimo-v2.5", Alias: claudeSonnet46MiMoTestAlias}},
		},
		{
			Name:    minimaxProvider,
			Kind:    "minimax",
			BaseURL: "https://api.minimaxi.com/v1",
			Models:  []internalconfig.OpenAICompatibilityModel{{Name: "MiniMax-M3", Alias: claudeSonnet46MiMoTestAlias}},
		},
	}})

	xiaomiExecutor := &authScopedOpenAICompatPoolExecutor{id: xiaomiProvider}
	minimaxExecutor := &authScopedOpenAICompatPoolExecutor{id: minimaxProvider}
	manager.RegisterExecutor(xiaomiExecutor)
	manager.RegisterExecutor(minimaxExecutor)

	auths := []*Auth{
		{
			ID:       xiaomiAuthID,
			Provider: xiaomiProvider,
			Status:   StatusActive,
			Attributes: map[string]string{
				"api_key":      "xiaomi-key",
				"base_url":     "https://token-plan-cn.xiaomimimo.com/v1",
				"compat_name":  xiaomiProvider,
				"compat_kind":  "xiaomi",
				"priority":     "100",
				"provider_key": xiaomiProvider,
			},
		},
		{
			ID:       minimaxAuthID,
			Provider: minimaxProvider,
			Status:   StatusActive,
			Attributes: map[string]string{
				"api_key":      "minimax-key",
				"base_url":     "https://api.minimaxi.com/v1",
				"compat_name":  minimaxProvider,
				"compat_kind":  "minimax",
				"priority":     "1",
				"provider_key": minimaxProvider,
			},
		},
	}
	modelRegistry := registry.GetGlobalRegistry()
	for _, auth := range auths {
		modelRegistry.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: claudeSonnet46MiMoTestAlias}})
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			modelRegistry.UnregisterClient(auth.ID)
		}
	})

	headers := make(http.Header)
	headers.Set("X-Session-ID", "workbuddy-session")
	complete := []byte(`{
		"model":"claude-sonnet-4-6",
		"reasoning_effort":"high",
		"messages":[{"role":"assistant","reasoning_content":"real plan","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]
	}`)
	missing := []byte(`{
		"model":"claude-sonnet-4-6",
		"reasoning_effort":"high",
		"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]
	}`)
	providers := []string{xiaomiProvider, minimaxProvider}

	first, err := manager.Execute(context.Background(), providers, cliproxyexecutor.Request{Model: claudeSonnet46MiMoTestAlias, Payload: complete}, cliproxyexecutor.Options{
		Headers:         headers,
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: complete,
	})
	if err != nil {
		t.Fatalf("complete history execute: %v", err)
	}
	if string(first.Payload) != xiaomiAuthID+"|mimo-v2.5" {
		t.Fatalf("first payload = %q, want Xiaomi affinity", first.Payload)
	}
	affinityKey := "mixed::header:workbuddy-session::claude-sonnet-4-6"
	if boundAuth, _, ok := selector.cache.GetBinding(affinityKey); !ok || boundAuth != xiaomiAuthID {
		t.Fatalf("initial affinity = (%q, %t), want %q", boundAuth, ok, xiaomiAuthID)
	}

	second, err := manager.Execute(context.Background(), providers, cliproxyexecutor.Request{Model: claudeSonnet46MiMoTestAlias, Payload: missing}, cliproxyexecutor.Options{
		Headers:         headers,
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: missing,
	})
	if err != nil {
		t.Fatalf("incomplete history execute: %v", err)
	}
	if string(second.Payload) != minimaxAuthID+"|MiniMax-M3" {
		t.Fatalf("second payload = %q, want compatible MiniMax route", second.Payload)
	}
	if boundAuth, _, ok := selector.cache.GetBinding(affinityKey); !ok || boundAuth != minimaxAuthID {
		t.Fatalf("reselected affinity = (%q, %t), want %q", boundAuth, ok, minimaxAuthID)
	}
	if calls := xiaomiExecutor.ExecuteCalls(); len(calls) != 1 {
		t.Fatalf("Xiaomi execute calls = %v, want only initial compatible request", calls)
	}
	if calls := minimaxExecutor.ExecuteCalls(); len(calls) != 1 {
		t.Fatalf("MiniMax execute calls = %v, want one compatibility reselect", calls)
	}
}
