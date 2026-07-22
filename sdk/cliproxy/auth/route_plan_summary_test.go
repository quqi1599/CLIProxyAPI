package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/provideridentity"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type KimiExecutor struct {
	*authFallbackExecutor
}

func (e *KimiExecutor) Identifier() string { return "kimi" }

func TestManager_Execute_LogsRoutePlanWithFallback(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 0, 1)
	executor := &KimiExecutor{authFallbackExecutor: &authFallbackExecutor{
		id: "kimi",
		executeErrors: map[string]error{
			"aa-rate-limited-auth": &Error{
				Code:       "rate_limit_error",
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "upstream rate limited",
				Retryable:  true,
			},
		},
	}}
	m.RegisterExecutor(executor)

	model := "kimi-k2.6"
	blockedAuth := &Auth{ID: "aa-rate-limited-auth", Provider: "kimi", Attributes: map[string]string{"routing_group": "group-a"}}
	goodAuth := &Auth{ID: "bb-good-auth", Provider: "kimi", Attributes: map[string]string{"routing_group": "group-b"}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(blockedAuth.ID, "kimi", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(goodAuth.ID, "kimi", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(blockedAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	if _, errRegister := m.Register(context.Background(), blockedAuth); errRegister != nil {
		t.Fatalf("register blocked auth: %v", errRegister)
	}
	if _, errRegister := m.Register(context.Background(), goodAuth); errRegister != nil {
		t.Fatalf("register good auth: %v", errRegister)
	}

	ctx := logging.WithRequestID(context.Background(), "req-route-plan-1")
	_, errExecute := m.Execute(ctx, []string{"kimi"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions",
		},
	})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want success", errExecute)
	}

	plans := findRoutePlanEntries(hook.AllEntries())
	if len(plans) < 2 {
		t.Fatalf("route_plan entries = %d, want at least 2", len(plans))
	}

	first := plans[0]
	if first.RequestedModel != model {
		t.Fatalf("first requested_model = %q, want %q", first.RequestedModel, model)
	}
	if first.ResolvedModel != model {
		t.Fatalf("first resolved_model = %q, want %q", first.ResolvedModel, model)
	}
	if first.UpstreamModel != model {
		t.Fatalf("first upstream_model = %q, want %q", first.UpstreamModel, model)
	}
	if first.Provider != "kimi" {
		t.Fatalf("first provider = %q, want kimi", first.Provider)
	}
	if first.Protocol != "claude_messages" {
		t.Fatalf("first protocol = %q, want claude_messages", first.Protocol)
	}
	if first.Executor != "KimiExecutor" {
		t.Fatalf("first executor = %q, want KimiExecutor", first.Executor)
	}
	if first.UpstreamPath != "/v1/chat/completions" {
		t.Fatalf("first upstream_path = %q, want /v1/chat/completions", first.UpstreamPath)
	}
	if first.Translator != "ClaudeToKimiOpenAICompat" {
		t.Fatalf("first translator = %q, want ClaudeToKimiOpenAICompat", first.Translator)
	}
	if first.FallbackFrom != "" || first.FallbackReason != "" {
		t.Fatalf("first fallback fields should be empty: %+v", first)
	}

	second := plans[1]
	if second.Executor != "KimiExecutor" {
		t.Fatalf("second executor = %q, want KimiExecutor", second.Executor)
	}
	if second.FallbackReason != "rate_limit_error" {
		t.Fatalf("second fallback_reason = %q, want rate_limit_error", second.FallbackReason)
	}
	if second.FallbackFrom == "" {
		t.Fatalf("second fallback_from should be populated: %+v", second)
	}
	if second.RoutingGroup != "group-b" {
		t.Fatalf("second routing_group = %q, want group-b", second.RoutingGroup)
	}
}

func TestRoutePlanHelperMappings(t *testing.T) {
	cases := []struct {
		name         string
		protocol     string
		requestPath  string
		executorName string
		operation    string
		wantPath     string
		wantTrans    string
	}{
		{
			name:         "codex responses",
			protocol:     "openai_responses",
			requestPath:  "/v1/responses",
			executorName: "CodexAutoExecutor",
			operation:    "execute",
			wantPath:     "/responses",
			wantTrans:    "OpenAIResponsesToCodex",
		},
		{
			name:         "claude count",
			protocol:     "openai_chat",
			requestPath:  "/v1/chat/completions",
			executorName: "ClaudeExecutor",
			operation:    "count",
			wantPath:     "/v1/messages/count_tokens?beta=true",
			wantTrans:    "OpenAIToClaude",
		},
		{
			name:         "openai compat responses",
			protocol:     "openai_responses",
			requestPath:  "/v1/responses",
			executorName: "OpenAICompatExecutor",
			operation:    "execute",
			wantPath:     "/responses/compact",
			wantTrans:    "OpenAIResponsesToOpenAICompat",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routePlanUpstreamPath(tc.protocol, tc.requestPath, tc.executorName, tc.operation); got != tc.wantPath {
				t.Fatalf("routePlanUpstreamPath() = %q, want %q", got, tc.wantPath)
			}
			if got := routePlanTranslator(tc.protocol, tc.requestPath, tc.executorName); got != tc.wantTrans {
				t.Fatalf("routePlanTranslator() = %q, want %q", got, tc.wantTrans)
			}
		})
	}
}

func TestRoutePlanProviderIdentityUsesCanonicalPrecedence(t *testing.T) {
	auth := &Auth{
		Provider: "openai-compatible-configured-route",
		Attributes: map[string]string{
			"provider_key":                       "openai-compatible-configured-route",
			"provider_family":                    "openai-compatibility",
			"compat_name":                        "Configured Route",
			"compat_kind":                        "kimi",
			provideridentity.KindSourceAttribute: string(provideridentity.SourceCompatConfig),
			"base_url":                           "https://api.deepseek.com/v1",
		},
	}

	identity := routePlanProviderIdentity(auth, "openai-compatibility")
	if identity.CanonicalProvider != "kimi" || identity.Kind != "kimi" {
		t.Fatalf("identity provider/kind = %q/%q, want kimi/kimi", identity.CanonicalProvider, identity.Kind)
	}
	if identity.ExecutorKey != "openai-compatible-configured-route" {
		t.Fatalf("identity executor_key = %q", identity.ExecutorKey)
	}
	if identity.ProviderFamily != "openai-compatibility" || identity.CompatName != "Configured Route" {
		t.Fatalf("identity metadata = %+v", identity)
	}
	if identity.Source != provideridentity.SourceCompatConfig || identity.BaseHost != "api.deepseek.com" {
		t.Fatalf("identity source/base_host = %q/%q", identity.Source, identity.BaseHost)
	}

	kind, source := routePlanCompatKindWithSource(auth)
	if kind != identity.Kind || source != string(identity.Source) {
		t.Fatalf("route compat identity = %q/%q, want %q/%q", kind, source, identity.Kind, identity.Source)
	}
	if got := routePlanCompatBaseHost(auth); got != identity.BaseHost {
		t.Fatalf("route base host = %q, want %q", got, identity.BaseHost)
	}
	if got := routePlanThinkingProviderKey(auth, "openai-compatibility"); got != identity.ExecutorKey {
		t.Fatalf("thinking provider key = %q, want %q", got, identity.ExecutorKey)
	}
	if isDeepSeekOfficialRoute(auth, "deepseek-v4-pro") {
		t.Fatal("explicit Kimi identity must win over the DeepSeek URL")
	}

	plan := buildRoutePlanSummary(requestExecutionSummary{}, auth, "openai-compatibility", "kimi-k2.6", "kimi-k2.6", "kimi-k2.6", cliproxyexecutor.Options{}, nil, "execute", coreusage.RequestAttempt{})
	if plan.CompatKind != identity.Kind || plan.CompatKindSource != string(identity.Source) || plan.CompatBaseHost != identity.BaseHost {
		t.Fatalf("route plan identity = %+v, want %+v", plan, identity)
	}
}

func TestRoutePlanProviderIdentityPreservesNativeProvider(t *testing.T) {
	auth := &Auth{Provider: "vertex", Attributes: map[string]string{"provider_key": "vertex"}}
	identity := routePlanProviderIdentity(auth, "vertex")
	if identity.CanonicalProvider != "vertex" || identity.ExecutorKey != "vertex" || identity.Kind != "" || identity.Source != provideridentity.SourceDefault {
		t.Fatalf("identity = %+v", identity)
	}
	if kind, source := routePlanCompatKindWithSource(auth); kind != "" || source != "" {
		t.Fatalf("native compat identity = %q/%q, want empty", kind, source)
	}
	if got := routePlanThinkingProviderKey(auth, "vertex"); got != "vertex" {
		t.Fatalf("thinking provider key = %q, want vertex", got)
	}
}

func TestRoutePlanProviderIdentityKeepsCompatNameDiagnostic(t *testing.T) {
	auth := &Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"provider_key": "pool",
			"compat_name":  "pool",
			"base_url":     "https://example.com/v1",
		},
	}
	identity := routePlanProviderIdentity(auth, "")
	if identity.CanonicalProvider != "openai-compatibility" || identity.ExecutorKey != "pool" {
		t.Fatalf("identity provider/executor = %q/%q, want openai-compatibility/pool", identity.CanonicalProvider, identity.ExecutorKey)
	}
	if identity.CompatName != "pool" || identity.Kind != "" || identity.Source != provideridentity.SourceDefault {
		t.Fatalf("identity compatibility metadata = %+v", identity)
	}
}

func TestRoutePlanProviderIdentityUsesURLFallbackWithoutReplacingExecutorKey(t *testing.T) {
	auth := &Auth{
		Provider: "openai-compatible-deepseek-route",
		Attributes: map[string]string{
			"provider_key":                       "openai-compatible-deepseek-route",
			"provider_family":                    "openai-compatibility",
			provideridentity.KindSourceAttribute: string(provideridentity.SourceBaseURL),
			"base_url":                           "https://api.deepseek.com/v1",
		},
	}
	identity := routePlanProviderIdentity(auth, "openai-compatible-deepseek-route")
	if identity.Kind != "deepseek" || identity.Source != provideridentity.SourceBaseURL || identity.BaseHost != "api.deepseek.com" {
		t.Fatalf("identity = %+v", identity)
	}
	if got := routePlanThinkingProviderKey(auth, auth.Provider); got != "openai-compatible-deepseek-route" {
		t.Fatalf("thinking provider key = %q", got)
	}
	if kind, source := routePlanCompatKindWithSource(auth); kind != "deepseek" || source != string(provideridentity.SourceBaseURL) {
		t.Fatalf("route compat identity = %q/%q", kind, source)
	}
}

func TestRoutePlanProviderIdentityUsesSharedAttributeInput(t *testing.T) {
	tests := []struct {
		name       string
		attributes map[string]string
	}{
		{
			name: "legacy compat kind",
			attributes: map[string]string{
				"provider_key": "configured-route",
				"compat-kind":  "MiniMax",
				"base_url":     "https://api.deepseek.com/v1",
			},
		},
		{
			name: "stale URL-derived kind",
			attributes: map[string]string{
				"provider_key":                       "configured-route",
				"compat_kind":                        "minimax",
				provideridentity.KindSourceAttribute: string(provideridentity.SourceBaseURL),
				"base_url":                           "https://api.deepseek.com/v1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &Auth{Provider: "configured-route", Attributes: tt.attributes}
			got := routePlanProviderIdentity(auth, "openai-compatibility")
			want := provideridentity.Resolve(provideridentity.InputFromAttributes("openai-compatibility", tt.attributes))
			if got != want {
				t.Fatalf("routePlanProviderIdentity() = %+v, want %+v", got, want)
			}
			if got.ExecutorKey != "configured-route" {
				t.Fatalf("identity executor_key = %q, want configured-route", got.ExecutorKey)
			}
		})
	}
}

func TestRoutePlanNormalizedReasoningEffort_OfficialDeepSeekUsesOfficialSemantics(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("route-plan-deepseek-official", "deepseek", []*registry.ModelInfo{{
		ID:       "deepseek-v4-pro",
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}},
	}})
	t.Cleanup(func() {
		reg.UnregisterClient("route-plan-deepseek-official")
	})

	auth := &Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     "https://api.deepseek.com/v1",
			"provider_key": "deepseek",
			"compat_kind":  "deepseek",
		},
	}

	got := routePlanNormalizedReasoningEffort(auth, "openai-compatibility", "deepseek-v4-pro[1m]", "claude_code", "deepseek-v4-pro", "low")
	if got != "high" {
		t.Fatalf("routePlanNormalizedReasoningEffort() = %q, want high", got)
	}
}

func TestRoutePlanNormalizedReasoningEffort_RemappedDeepSeekIntentUsesFinalSupport(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("route-plan-remap", "openai-compatibility", []*registry.ModelInfo{{
		ID:       "generic-openai-model",
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
	}})
	t.Cleanup(func() {
		reg.UnregisterClient("route-plan-remap")
	})

	auth := &Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": "https://example.com/v1",
		},
	}

	got := routePlanNormalizedReasoningEffort(auth, "openai-compatibility", "deepseek-v4-pro[1m]", "claude_code", "generic-openai-model", "max")
	if got != "high" {
		t.Fatalf("routePlanNormalizedReasoningEffort() = %q, want high", got)
	}
}

func TestBuildRoutePlanSummaryMarksDeepSeekV4ViaDoubaoMapping(t *testing.T) {
	auth := &Auth{
		ID:       "route-plan-doubao-deepseek",
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":    "https://ark.cn-beijing.volces.com/api/v3",
			"compat_kind": "doubao",
		},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "deepseek-v4-pro[1m]",
			cliproxyexecutor.RequestPathMetadataKey:    "/v1/messages",
		},
	}

	plan := buildRoutePlanSummary(requestExecutionSummary{}, auth, "openai-compatibility", "deepseek-v4-pro[1m]", "deepseek-v4-pro", "deepseek-v4-pro", opts, nil, "execute", coreusage.RequestAttempt{})
	if plan.CompatKind != "doubao" {
		t.Fatalf("compat_kind = %q, want doubao", plan.CompatKind)
	}
	if plan.CompatKindSource != "auth_attribute:compat_kind" {
		t.Fatalf("compat_kind_source = %q, want auth_attribute:compat_kind", plan.CompatKindSource)
	}
	if plan.CompatMapping != "deepseek_v4_via_doubao_volcengine" {
		t.Fatalf("compat_mapping = %q, want deepseek_v4_via_doubao_volcengine", plan.CompatMapping)
	}
}

func findRoutePlanEntries(entries []*log.Entry) []routePlanSummary {
	out := make([]routePlanSummary, 0)
	for _, entry := range entries {
		if entry == nil || entry.Data["event"] != "route_plan" {
			continue
		}
		plan, ok := entry.Data["route_plan"].(routePlanSummary)
		if !ok {
			continue
		}
		out = append(out, plan)
	}
	return out
}
