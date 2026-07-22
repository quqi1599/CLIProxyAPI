package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
					actual, err := scrubOpenAICompatPayloadForModelWithPolicies(ctx, fixtureCase.Input, profile, fixtureCase.Model, fixture.BaseURL)
					if err != nil {
						t.Fatalf("scrubOpenAICompatPayloadForModelWithPolicies() error = %v", err)
					}
					assertOpenAICompatJSONEqual(t, actual, fixtureCase.Expected)
					assertOpenAICompatJSONEqual(t, actual, legacy)

					report, ok := internalpayload.TransformReportFromContext(ctx)
					if !ok || len(report.Stages) != 1 {
						t.Fatalf("transform report = %+v, ok=%v", report, ok)
					}
					stage := report.Stages[0]
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

func TestOpenAICompatPolicyRegistryInventory(t *testing.T) {
	if openAICompatPolicyRegistryErr != nil {
		t.Fatalf("openAICompatPolicyRegistry error = %v", openAICompatPolicyRegistryErr)
	}
	report := openAICompatPolicyRegistry.Report()
	wantIDs := []string{
		openAICompatKimiPolicyID,
		openAICompatMiniMaxPolicyID,
		openAICompatQwen38PolicyID,
	}
	if len(report.Policies) != len(wantIDs) {
		t.Fatalf("policy count = %d, want %d", len(report.Policies), len(wantIDs))
	}
	for index, policy := range report.Policies {
		if policy.ID != wantIDs[index] {
			t.Fatalf("policy %d ID = %q, want %q", index, policy.ID, wantIDs[index])
		}
		if policy.Owner != "runtime/executor" || policy.Phase != compat.ProviderQuirkPatch || policy.Cost.Complexity != "O(bytes)" {
			t.Fatalf("policy metadata = %+v", policy)
		}
		if policy.RemovalCondition == "" || policy.Lifecycle.RetrySemantics == "" || policy.Lifecycle.ReviewDate == "" {
			t.Fatalf("policy lifecycle is incomplete: %+v", policy)
		}
		if policy.ID == openAICompatKimiPolicyID && !slices.Contains(policy.MutatedFields, "body.messages") {
			t.Fatalf("Kimi policy does not declare message mutation: %+v", policy.MutatedFields)
		}
		fixturePath := filepath.Join("..", "..", "..", filepath.Clean(policy.Lifecycle.Fixture))
		if _, err := os.Stat(fixturePath); err != nil {
			t.Fatalf("policy fixture %q: %v", policy.Lifecycle.Fixture, err)
		}
	}
}

func TestOpenAICompatPlannerReappliesPoliciesAfterPayloadConfig(t *testing.T) {
	executor := NewOpenAICompatExecutor("kimi-provider", &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "kimi-k2.7", Protocol: "openai"}},
				Params: map[string]any{
					"model":       "configured-kimi-alias",
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

	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok {
		t.Fatal("transform report missing")
	}
	policyStages := 0
	for _, stage := range report.Stages {
		if stage.Stage == "compat/"+string(compat.ProviderQuirkPatch) && slices.Contains(stage.AppliedPolicies, openAICompatKimiPolicyID) {
			policyStages++
		}
	}
	if policyStages != 2 {
		t.Fatalf("Kimi policy stages = %d, want pre-config and post-config stages; report=%+v", policyStages, report)
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"model":"` + test.payloadModel + `","messages":[{"role":"user","content":"hi"}],"enable_thinking":false,"reasoning_effort":"none"}`)
			match := openAICompatPolicyMatchContext(profile, payload, test.model)
			policies := openAICompatPolicyRegistry.PoliciesFor(match)
			matched := len(policies) == 1 && policies[0].ID == openAICompatQwen38PolicyID
			if matched != test.wantMatch {
				t.Fatalf("PoliciesFor(%+v) IDs = %v, want match=%t", match, policyIDsForTest(policies), test.wantMatch)
			}

			legacy := scrubOpenAICompatPayloadForModel(payload, profile, test.model, "https://example.test/v1")
			ctx := internalpayload.WithTransformReport(context.Background(), int64(len(payload)))
			actual, err := scrubOpenAICompatPayloadForModelWithPolicies(ctx, payload, profile, test.model, "https://example.test/v1")
			if err != nil {
				t.Fatalf("scrubOpenAICompatPayloadForModelWithPolicies() error = %v", err)
			}
			assertOpenAICompatJSONEqual(t, actual, legacy)
			report, ok := internalpayload.TransformReportFromContext(ctx)
			if !ok || (len(report.Stages) == 1) != test.wantMatch {
				t.Fatalf("Apply stages = %+v, ok=%v, want match=%t", report.Stages, ok, test.wantMatch)
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
