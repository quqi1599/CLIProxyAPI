package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type openAICompatPolicyFixture struct {
	PolicyID   string                          `json:"policy_id"`
	CompatKind string                          `json:"compat_kind"`
	BaseURL    string                          `json:"base_url"`
	Cases      []openAICompatPolicyFixtureCase `json:"cases"`
}

type openAICompatPolicyFixtureCase struct {
	Name       string          `json:"name"`
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Expected   json.RawMessage `json:"expected"`
	Downgrades []string        `json:"downgrades"`
}

func TestOpenAICompatPolicyFixturesMatchLegacyBehavior(t *testing.T) {
	fixturePaths := []string{
		"testdata/compat/kimi_model_quirks.json",
		"testdata/compat/minimax_request_quirks.json",
		"testdata/compat/qwen38_thinking.json",
	}
	for _, fixturePath := range fixturePaths {
		fixture := readOpenAICompatPolicyFixture(t, fixturePath)
		t.Run(fixture.CompatKind, func(t *testing.T) {
			profile := openAICompatProfileForKind(fixture.CompatKind)
			for _, fixtureCase := range fixture.Cases {
				t.Run(fixtureCase.Name, func(t *testing.T) {
					legacy := scrubOpenAICompatPayloadForModel(fixtureCase.Input, profile, fixtureCase.Model, fixture.BaseURL)
					assertOpenAICompatJSONEqual(t, legacy, fixtureCase.Expected)

					ctx := internalpayload.WithAmplificationMode(
						internalpayload.WithTransformReport(context.Background(), int64(len(fixtureCase.Input))),
						internalpayload.AmplificationModeObserve,
					)
					actual, err := scrubOpenAICompatPayloadForModelWithPolicies(ctx, fixtureCase.Input, profile, fixtureCase.Model, fixture.BaseURL, compat.MatchContext{})
					if err != nil {
						t.Fatalf("scrubOpenAICompatPayloadForModelWithPolicies() error = %v", err)
					}
					assertOpenAICompatJSONEqual(t, actual, fixtureCase.Expected)
					assertOpenAICompatJSONEqual(t, actual, legacy)

					report, ok := internalpayload.TransformReportFromContext(ctx)
					if !ok || len(report.Stages) != 3 {
						t.Fatalf("transform report = %+v, ok=%v", report, ok)
					}
					preStage := report.Stages[0]
					stage := report.Stages[1]
					postStage := report.Stages[2]
					if preStage.Stage != openAICompatProviderPreQuirkStage || postStage.Stage != openAICompatProviderPostQuirkStage ||
						preStage.OutputBytes != stage.InputBytes || stage.OutputBytes != postStage.InputBytes {
						t.Fatalf("initial scrub stages are not disjoint: pre=%+v provider=%+v post=%+v", preStage, stage, postStage)
					}
					if stage.Stage != "compat/"+string(compat.ProviderQuirkPatch) || !slices.Equal(stage.AppliedPolicies, []string{fixture.PolicyID}) {
						t.Fatalf("transform stage = %+v", stage)
					}
					if !slices.Equal(stage.Downgrades, fixtureCase.Downgrades) {
						t.Fatalf("downgrades = %v, want %v", stage.Downgrades, fixtureCase.Downgrades)
					}
				})
			}
		})
	}
}

func TestOpenAICompatPostConfigPolicyFixture(t *testing.T) {
	fixture := readOpenAICompatPolicyFixture(t, "testdata/compat/post_config_revalidate.json")
	profile := openAICompatProfileForKind(fixture.CompatKind)
	for _, fixtureCase := range fixture.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			ctx := internalpayload.WithAmplificationMode(
				internalpayload.WithTransformReport(context.Background(), int64(len(fixtureCase.Input))),
				internalpayload.AmplificationModeObserve,
			)
			actual, err := revalidateOpenAICompatPayloadAfterConfig(
				ctx,
				fixtureCase.Input,
				profile,
				fixtureCase.Model,
				fixture.BaseURL,
				compat.MatchContext{Endpoint: "chat", Mode: "non-stream", SourceFormat: "openai", TargetFormat: "openai"},
			)
			if err != nil {
				t.Fatalf("revalidateOpenAICompatPayloadAfterConfig() error = %v", err)
			}
			assertOpenAICompatJSONEqual(t, actual, fixtureCase.Expected)
			report, ok := internalpayload.TransformReportFromContext(ctx)
			if !ok || len(report.Stages) != 1 {
				t.Fatalf("transform report = %+v, ok=%v", report, ok)
			}
			stage := report.Stages[0]
			if stage.Stage != openAICompatPostConfigRevalidateStage || !slices.Equal(stage.AppliedPolicies, []string{fixture.PolicyID}) {
				t.Fatalf("post-config stage = %+v", stage)
			}
			if !slices.Equal(stage.Downgrades, fixtureCase.Downgrades) {
				t.Fatalf("downgrades = %v, want %v", stage.Downgrades, fixtureCase.Downgrades)
			}
		})
	}
}

func TestOpenAICompatPostConfigSkipsPreCanonicalization(t *testing.T) {
	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object","properties":{"value":"object"}}}}]}`)
	profile := genericOpenAICompatProfile()
	postConfig := scrubOpenAICompatPostConfigPayload(payload, profile, "gpt-5", "https://api.openai.com/v1")
	if !bytes.Equal(postConfig, payload) {
		t.Fatalf("post-config revalidation reran pre-canonicalization: %s", postConfig)
	}
	full := scrubOpenAICompatPayloadForModel(payload, profile, "gpt-5", "https://api.openai.com/v1")
	if bytes.Equal(full, payload) {
		t.Fatal("fixture did not exercise the full scrub's schema canonicalization")
	}
}

func TestOpenAICompatPostConfigAmplificationModes(t *testing.T) {
	input := []byte(`{"model":"test"}`)
	expanded := make([]byte, len(input)+int(internalpayload.DefaultMaxExpansionBytes)+1)
	policy := openAICompatPostConfigRevalidatePolicy()
	policy.Apply = func(context.Context, []byte) (compat.TransformResult, error) {
		return compat.TransformResult{Payload: expanded}, nil
	}
	registry, err := compat.NewRegistry(policy)
	if err != nil {
		t.Fatalf("compat.NewRegistry() error = %v", err)
	}
	pipeline := compat.NewPipeline(registry)
	match := compat.MatchContext{ProviderFamily: "openai-compatibility"}

	observeCtx := internalpayload.WithAmplificationMode(
		internalpayload.WithTransformReport(context.Background(), int64(len(input))),
		internalpayload.AmplificationModeObserve,
	)
	result, err := pipeline.Apply(observeCtx, match, input)
	if err != nil || len(result.Payload) != len(expanded) {
		t.Fatalf("observe result bytes=%d error=%v", len(result.Payload), err)
	}
	assertOpenAICompatPostConfigAmplificationStage(t, observeCtx)

	enforceCtx := internalpayload.WithAmplificationMode(
		internalpayload.WithTransformReport(context.Background(), int64(len(input))),
		internalpayload.AmplificationModeEnforce,
	)
	result, err = pipeline.Apply(enforceCtx, match, input)
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InternalTransformError || typed.Scope != failurecontract.ScopeRequest || typed.ProviderCode != "compat_expansion_exceeded" {
		t.Fatalf("enforce failure = %#v, error=%v", typed, err)
	}
	if result.Payload != nil {
		t.Fatalf("enforce result retained %d payload bytes", len(result.Payload))
	}
	assertOpenAICompatPostConfigAmplificationStage(t, enforceCtx)
}

func assertOpenAICompatPostConfigAmplificationStage(t *testing.T, ctx context.Context) {
	t.Helper()
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || len(report.Stages) != 1 {
		t.Fatalf("post-config amplification report = %+v, ok=%v", report, ok)
	}
	stage := report.Stages[0]
	if stage.Stage != openAICompatPostConfigRevalidateStage || !stage.Amplification.Exceeded ||
		!slices.Equal(stage.AppliedPolicies, []string{openAICompatPostConfigRevalidatePolicyID}) {
		t.Fatalf("post-config amplification stage = %+v", stage)
	}
}

func TestOpenAICompatFirstPolicyScrubIsIdempotent(t *testing.T) {
	assertIdempotent := func(t *testing.T, payload []byte, profile openAICompatProfile, model, baseURL string) {
		t.Helper()
		first, err := scrubOpenAICompatPayloadForModelWithPolicies(context.Background(), payload, profile, model, baseURL, compat.MatchContext{})
		if err != nil {
			t.Fatalf("first scrub error: %v", err)
		}
		second, err := scrubOpenAICompatPayloadForModelWithPolicies(context.Background(), first, profile, model, baseURL, compat.MatchContext{})
		if err != nil {
			t.Fatalf("second scrub error: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("policy scrub is not byte-idempotent\nfirst:  %s\nsecond: %s", first, second)
		}
	}

	for _, fixturePath := range []string{
		"testdata/compat/kimi_model_quirks.json",
		"testdata/compat/minimax_request_quirks.json",
		"testdata/compat/qwen38_thinking.json",
	} {
		fixture := readOpenAICompatPolicyFixture(t, fixturePath)
		profile := openAICompatProfileForKind(fixture.CompatKind)
		for _, fixtureCase := range fixture.Cases {
			t.Run(fixture.CompatKind+"/"+fixtureCase.Name, func(t *testing.T) {
				assertIdempotent(t, fixtureCase.Input, profile, fixtureCase.Model, fixture.BaseURL)
			})
		}
	}

	additionalCases := []struct {
		name    string
		payload []byte
		profile openAICompatProfile
		model   string
		baseURL string
	}{
		{
			name:    "generic",
			payload: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"inspect","strict":true,"parameters":{"type":"object","properties":{"type":"object"},"additionalProperties":"object","required":null}}}]}`),
			profile: genericOpenAICompatProfile(), model: "gpt-5", baseURL: "https://api.openai.com/v1",
		},
		{
			name:    "deepseek",
			payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"thinking_budget":50,"thinking":{"type":"enabled","budget_tokens":99999},"reasoning_effort":"xhigh"}`),
			profile: genericOpenAICompatProfile(), model: "deepseek-v4-pro", baseURL: "https://api.deepseek.com/v1",
		},
		{
			name:    "doubao",
			payload: []byte(`{"model":"doubao-seed-2.0-pro","messages":[{"role":"user","content":[{"type":"input_text","text":"inspect"}]}],"temperature":1.8,"max_tokens":100,"max_completion_tokens":100000,"store":true,"metadata":{"tenant":"demo"},"parallel_tool_calls":true}`),
			profile: openAICompatProfileForKind("doubao"), model: "doubao-seed-2.0-pro", baseURL: "https://ark.cn-beijing.volces.com/api/v3",
		},
		{
			name:    "xiaomi",
			payload: []byte(`{"model":"mimo-v2.5-pro","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","temperature":0.2,"top_p":0.4}`),
			profile: openAICompatProfileForKind("xiaomi"), model: "mimo-v2.5-pro", baseURL: "https://api.xiaomimimo.com/v1",
		},
		{
			name:    "zhipu",
			payload: []byte(`{"model":"glm-4.5v","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"high"}},{"type":"text","text":"describe"}]}]}`),
			profile: openAICompatProfileForKind("zhipu"), model: "glm-4.5v", baseURL: "https://open.bigmodel.cn/api/paas/v4",
		},
	}
	for _, test := range additionalCases {
		t.Run(test.name, func(t *testing.T) {
			assertIdempotent(t, test.payload, test.profile, test.model, test.baseURL)
		})
	}
}

func TestOpenAICompatPolicyRegistryInventory(t *testing.T) {
	report, err := openAICompatPolicyInventory()
	if err != nil {
		t.Fatalf("openAICompatPolicyInventory() error = %v", err)
	}
	wantIDs := []string{
		openAICompatKimiPolicyID,
		openAICompatMiniMaxPolicyID,
		openAICompatQwen38PolicyID,
		openAICompatPostConfigRevalidatePolicyID,
	}
	if len(report.Policies) != len(wantIDs) {
		t.Fatalf("policy count = %d, want %d", len(report.Policies), len(wantIDs))
	}
	for index, policy := range report.Policies {
		if policy.ID != wantIDs[index] {
			t.Fatalf("policy %d ID = %q, want %q", index, policy.ID, wantIDs[index])
		}
		wantPhase := compat.ProviderQuirkPatch
		if policy.ID == openAICompatPostConfigRevalidatePolicyID {
			wantPhase = compat.PostConfigRevalidate
		}
		if policy.Owner != "runtime/executor" || policy.Phase != wantPhase || policy.Cost.Complexity != "O(bytes)" {
			t.Fatalf("policy metadata = %+v", policy)
		}
		if policy.RemovalCondition == "" || policy.Lifecycle.RetrySemantics == "" || policy.Lifecycle.ReviewDate == "" {
			t.Fatalf("policy lifecycle is incomplete: %+v", policy)
		}
		if policy.ID == openAICompatKimiPolicyID && !slices.Contains(policy.MutatedFields, "body.messages") {
			t.Fatalf("Kimi policy does not declare message mutation: %+v", policy.MutatedFields)
		}
		if policy.ID == openAICompatPostConfigRevalidatePolicyID {
			if !slices.Contains(policy.MutatedFields, "body.metadata") || !slices.Contains(policy.DowngradeIDs, openAICompatKimiToolChoiceDowngrade) {
				t.Fatalf("post-config policy inventory is incomplete: %+v", policy)
			}
		}
		fixturePath := filepath.Join("..", "..", "..", filepath.Clean(policy.Lifecycle.Fixture))
		if _, err := os.Stat(fixturePath); err != nil {
			t.Fatalf("policy fixture %q: %v", policy.Lifecycle.Fixture, err)
		}
	}
}

func TestOpenAICompatPlannerRevalidatesPoliciesAfterPayloadConfig(t *testing.T) {
	executor := NewOpenAICompatExecutor("kimi-provider", &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "kimi-k2.7", Protocol: "openai", NotExist: []string{"metadata"}}},
				Params: map[string]any{
					"model":       "configured-kimi-alias",
					"metadata":    map[string]any{"tenant": "reintroduced"},
					"temperature": 0.2,
				},
			}},
		},
	})
	payload := []byte(`{"model":"kimi-k2.7","messages":[{"role":"user","content":"hi"}],"temperature":0.3}`)
	ctx := internalpayload.WithAmplificationMode(
		internalpayload.WithTransformReport(context.Background(), int64(len(payload))),
		internalpayload.AmplificationModeObserve,
	)

	plan, err := executor.prepareOpenAICompatRequest(
		ctx,
		nil,
		cliproxyexecutor.Request{Model: "kimi-k2.7", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")},
		"https://api.moonshot.ai/v1",
		"kimi-k2.7",
		openAICompatProfileForKind("kimi"),
		false,
	)
	if err != nil {
		t.Fatalf("prepareOpenAICompatRequest() error = %v", err)
	}
	if got := gjson.GetBytes(plan.body, "model").String(); got != "configured-kimi-alias" {
		t.Fatalf("model = %q, want configured alias: %s", got, plan.body)
	}
	if got := gjson.GetBytes(plan.body, "temperature").Float(); got != kimiThinkingTemperature {
		t.Fatalf("temperature = %v, want post-config Kimi normalization %v: %s", got, kimiThinkingTemperature, plan.body)
	}
	if gjson.GetBytes(plan.body, "metadata").Exists() {
		t.Fatalf("post-config metadata was not removed: %s", plan.body)
	}

	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("transform report missing")
	}
	providerPolicyStages := 0
	postConfigStages := 0
	metadataDowngradeStages := 0
	stageByID := make(map[string]internalpayload.TransformStageReport)
	stageIndex := make(map[string]int)
	for index, stage := range report.Stages {
		stageByID[stage.Stage] = stage
		stageIndex[stage.Stage] = index
		if stage.Stage == "compat/"+string(compat.ProviderQuirkPatch) && slices.Contains(stage.AppliedPolicies, openAICompatKimiPolicyID) {
			providerPolicyStages++
		}
		if stage.Stage == openAICompatPostConfigRevalidateStage && slices.Contains(stage.AppliedPolicies, openAICompatPostConfigRevalidatePolicyID) {
			postConfigStages++
		}
		if slices.Contains(stage.Downgrades, openAICompatMetadataRemovedDowngrade) {
			metadataDowngradeStages++
			if stage.Stage != openAICompatPostConfigRevalidateStage {
				t.Fatalf("metadata revalidation attributed to %q, want %q; report=%+v", stage.Stage, openAICompatPostConfigRevalidateStage, report)
			}
		}
	}
	if providerPolicyStages != 1 || postConfigStages != 1 {
		t.Fatalf("policy stages = provider:%d post-config:%d, want 1 each; report=%+v", providerPolicyStages, postConfigStages, report)
	}
	if metadataDowngradeStages != 1 {
		t.Fatalf("metadata downgrade stages = %d, want 1; report=%+v", metadataDowngradeStages, report)
	}
	providerQuirkStageID := "compat/" + string(compat.ProviderQuirkPatch)
	preQuirkStage := stageByID[openAICompatProviderPreQuirkStage]
	providerQuirkStage := stageByID[providerQuirkStageID]
	postQuirkStage := stageByID[openAICompatProviderPostQuirkStage]
	payloadConfigStage := stageByID[openAICompatUserPayloadConfigStage]
	postConfigStage := stageByID[openAICompatPostConfigRevalidateStage]
	finalizationStage := stageByID[openAICompatProviderFinalizationStage]
	for _, stageID := range []string{
		openAICompatProviderPreQuirkStage,
		providerQuirkStageID,
		openAICompatProviderPostQuirkStage,
		openAICompatUserPayloadConfigStage,
		openAICompatPostConfigRevalidateStage,
		openAICompatProviderFinalizationStage,
	} {
		if _, ok := stageByID[stageID]; !ok {
			t.Fatalf("planner stage %q is missing; report=%+v", stageID, report)
		}
	}
	if preQuirkStage.OutputBytes != providerQuirkStage.InputBytes ||
		providerQuirkStage.OutputBytes != postQuirkStage.InputBytes ||
		postQuirkStage.OutputBytes != payloadConfigStage.InputBytes ||
		payloadConfigStage.OutputBytes != postConfigStage.InputBytes ||
		postConfigStage.OutputBytes != finalizationStage.InputBytes {
		t.Fatalf("planner stages do not form disjoint byte boundaries: pre=%+v provider=%+v post-quirk=%+v config=%+v post-config=%+v finalization=%+v", preQuirkStage, providerQuirkStage, postQuirkStage, payloadConfigStage, postConfigStage, finalizationStage)
	}
	if !(stageIndex[openAICompatProviderPreQuirkStage] < stageIndex[providerQuirkStageID] &&
		stageIndex[providerQuirkStageID] < stageIndex[openAICompatProviderPostQuirkStage] &&
		stageIndex[openAICompatProviderPostQuirkStage] < stageIndex[openAICompatUserPayloadConfigStage] &&
		stageIndex[openAICompatUserPayloadConfigStage] < stageIndex[openAICompatPostConfigRevalidateStage] &&
		stageIndex[openAICompatPostConfigRevalidateStage] < stageIndex[openAICompatProviderFinalizationStage]) {
		t.Fatalf("planner stage order is not monotonic: %+v", stageIndex)
	}
}

func TestOpenAICompatPlannerPayloadConfigSemantics(t *testing.T) {
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"existing":"before","drop":"remove","gate":{"match":"yes","not_match":"allowed","exist":true}}`)
	modelRule := func() config.PayloadModelRule {
		return config.PayloadModelRule{Name: "test-model", Protocol: "openai"}
	}
	override := func(rule config.PayloadModelRule, path string, value any) config.PayloadConfig {
		return config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{rule}, Params: map[string]any{path: value}}}}
	}
	tests := []struct {
		name       string
		config     config.PayloadConfig
		path       string
		wantRaw    string
		wantExists bool
		wantPost   bool
	}{
		{
			name: "default",
			config: config.PayloadConfig{Default: []config.PayloadRule{{
				Models: []config.PayloadModelRule{modelRule()},
				Params: map[string]any{"result.default": "set"},
			}}},
			path: "result.default", wantRaw: `"set"`, wantExists: true, wantPost: true,
		},
		{
			name:   "override",
			config: override(modelRule(), "existing", "after"),
			path:   "existing", wantRaw: `"after"`, wantExists: true, wantPost: true,
		},
		{
			name:   "override unchanged",
			config: override(modelRule(), "existing", "before"),
			path:   "existing", wantRaw: `"before"`, wantExists: true,
		},
		{
			name: "filter",
			config: config.PayloadConfig{Filter: []config.PayloadFilterRule{{
				Models: []config.PayloadModelRule{modelRule()},
				Params: []string{"drop"},
			}}},
			path: "drop", wantExists: false, wantPost: true,
		},
		{
			name:   "match applies",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", Match: []map[string]any{{"gate.match": "yes"}}}, "result.match", true),
			path:   "result.match", wantRaw: "true", wantExists: true, wantPost: true,
		},
		{
			name:   "match skips",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", Match: []map[string]any{{"gate.match": "no"}}}, "result.match", true),
			path:   "result.match", wantExists: false,
		},
		{
			name:   "not-match applies",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", NotMatch: []map[string]any{{"gate.not_match": "blocked"}}}, "result.not_match", true),
			path:   "result.not_match", wantRaw: "true", wantExists: true, wantPost: true,
		},
		{
			name:   "not-match skips",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", NotMatch: []map[string]any{{"gate.not_match": "allowed"}}}, "result.not_match", true),
			path:   "result.not_match", wantExists: false,
		},
		{
			name:   "exist applies",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", Exist: []string{"gate.exist"}}, "result.exist", true),
			path:   "result.exist", wantRaw: "true", wantExists: true, wantPost: true,
		},
		{
			name:   "exist skips",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", Exist: []string{"gate.missing"}}, "result.exist", true),
			path:   "result.exist", wantExists: false,
		},
		{
			name:   "not-exist applies",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", NotExist: []string{"gate.missing"}}, "result.not_exist", true),
			path:   "result.not_exist", wantRaw: "true", wantExists: true, wantPost: true,
		},
		{
			name:   "not-exist skips",
			config: override(config.PayloadModelRule{Name: "test-model", Protocol: "openai", NotExist: []string{"gate.exist"}}, "result.not_exist", true),
			path:   "result.not_exist", wantExists: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := internalpayload.WithAmplificationMode(
				internalpayload.WithTransformReport(context.Background(), int64(len(payload))),
				internalpayload.AmplificationModeObserve,
			)
			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{Payload: test.config})
			plan, err := executor.prepareOpenAICompatRequest(
				ctx,
				nil,
				cliproxyexecutor.Request{Model: "test-model", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")},
				"https://api.example.test/v1",
				"test-model",
				genericOpenAICompatProfile(),
				false,
			)
			if err != nil {
				t.Fatalf("prepareOpenAICompatRequest() error = %v", err)
			}
			result := gjson.GetBytes(plan.body, test.path)
			if result.Exists() != test.wantExists || test.wantExists && result.Raw != test.wantRaw {
				t.Fatalf("path %q = raw:%q exists:%t, want raw:%q exists:%t; body=%s", test.path, result.Raw, result.Exists(), test.wantRaw, test.wantExists, plan.body)
			}
			report, ok := internalpayload.TransformReportFromContext(ctx)
			if !ok {
				t.Fatal("transform report missing")
			}
			postConfigStages := 0
			for _, stage := range report.Stages {
				if stage.Stage == openAICompatPostConfigRevalidateStage {
					postConfigStages++
					if !slices.Contains(stage.AppliedPolicies, openAICompatPostConfigRevalidatePolicyID) {
						t.Fatalf("post-config stage policies = %v", stage.AppliedPolicies)
					}
				}
			}
			if got := postConfigStages == 1; got != test.wantPost {
				t.Fatalf("post-config stage present = %t, want %t; report=%+v", got, test.wantPost, report)
			}
		})
	}
}

func TestOpenAICompatPolicyRequestMatchIncludesPlannerDimensions(t *testing.T) {
	match := openAICompatPolicyRequestMatch(
		sdktranslator.FromString("claude"),
		sdktranslator.FromString("openai-response"),
		"/responses/compact",
		true,
	)
	match = openAICompatPolicyMatchContext(
		openAICompatProfileForKind("kimi"),
		[]byte(`{"model":"payload-alias"}`),
		"kimi-k2.7",
		match,
	)
	if match.ProviderFamily != "openai-compatibility" || match.CompatKind != "kimi" || match.Model != "kimi-k2.7" ||
		match.Endpoint != "compact" || match.Mode != "stream" || match.SourceFormat != "claude" || match.TargetFormat != "openai-response" {
		t.Fatalf("policy match context = %+v", match)
	}
}

func TestOpenAICompatQwenPolicyMatchAndApplyUseCanonicalModel(t *testing.T) {
	profile := openAICompatProfileForKind("qwen")
	tests := []struct {
		name         string
		model        string
		payloadModel string
		wantMatch    bool
	}{
		{name: "preview", model: "Qwen3.8-Max-Preview", payloadModel: "configured-preview-alias", wantMatch: true},
		{name: "namespaced suffix", model: "provider/Qwen3.8-Max(high)", payloadModel: "configured-namespaced-alias", wantMatch: true},
		{name: "payload model fallback", model: "configured-route-alias", payloadModel: "Qwen3.8-Max-Preview", wantMatch: true},
		{name: "earlier model", model: "qwen3.7-max", payloadModel: "qwen3.7-max", wantMatch: false},
		{name: "suffix lookalike", model: "qwen3.8-max-extra", payloadModel: "qwen3.8-max-extra", wantMatch: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"model":"` + test.payloadModel + `","messages":[{"role":"user","content":"hi"}],"enable_thinking":false,"reasoning_effort":"none"}`)
			match := openAICompatPolicyMatchContext(profile, payload, test.model, compat.MatchContext{})
			policies := openAICompatPolicyRegistry.PoliciesFor(match)
			matched := len(policies) == 1 && policies[0].ID == openAICompatQwen38PolicyID
			if matched != test.wantMatch {
				t.Fatalf("PoliciesFor(%+v) IDs = %v, want match=%t", match, policyIDsForTest(policies), test.wantMatch)
			}

			legacy := scrubOpenAICompatPayloadForModel(payload, profile, test.model, "https://example.test/v1")
			ctx := internalpayload.WithTransformReport(context.Background(), int64(len(payload)))
			actual, err := scrubOpenAICompatPayloadForModelWithPolicies(ctx, payload, profile, test.model, "https://example.test/v1", compat.MatchContext{})
			if err != nil {
				t.Fatalf("scrubOpenAICompatPayloadForModelWithPolicies() error = %v", err)
			}
			assertOpenAICompatJSONEqual(t, actual, legacy)
			report, ok := internalpayload.TransformReportFromContext(ctx)
			providerStages := 0
			for _, stage := range report.Stages {
				if stage.Stage == "compat/"+string(compat.ProviderQuirkPatch) && slices.Contains(stage.AppliedPolicies, openAICompatQwen38PolicyID) {
					providerStages++
				}
			}
			if !ok || (providerStages == 1) != test.wantMatch || len(report.Stages) != 2+providerStages {
				t.Fatalf("Apply stages = %+v, ok=%v, provider stages=%d, want match=%t", report.Stages, ok, providerStages, test.wantMatch)
			}
			if test.wantMatch {
				if !gjson.GetBytes(actual, "enable_thinking").Bool() || gjson.GetBytes(actual, "reasoning_effort").String() != "low" {
					t.Fatalf("matched Qwen payload was not normalized: %s", actual)
				}
			}
		})
	}
}

func policyIDsForTest(policies []compat.Policy) []string {
	ids := make([]string, len(policies))
	for index := range policies {
		ids[index] = policies[index].ID
	}
	return ids
}

func readOpenAICompatPolicyFixture(t *testing.T, path string) openAICompatPolicyFixture {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	var fixture openAICompatPolicyFixture
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", path, err)
	}
	if fixture.PolicyID == "" || fixture.CompatKind == "" || len(fixture.Cases) == 0 {
		t.Fatalf("fixture %q is incomplete: %+v", path, fixture)
	}
	return fixture
}

func assertOpenAICompatJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("actual JSON error = %v: %s", err, got)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("expected JSON error = %v: %s", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\nactual:   %s\nexpected: %s", got, want)
	}
}
