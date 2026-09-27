package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

// This is the pre-projection sequence, retained only as a behavioral oracle.
func legacyDeepSeekPolicy(input []byte, model, baseURL string, endpoint compat.EndpointKind) compat.TransformResult {
	output := input
	if endpoint == "chat" {
		output = normalizeDeepSeekChatAliases(output)
	}
	output = scrubDeepSeekThinkingBudgetForCompat(output, model, baseURL, "deepseek")
	output = scrubDeepSeekThinkingToolChoice(output, model, baseURL, "deepseek")
	if endpoint == "responses" || endpoint == "compact" {
		output = helps.NormalizeDeepSeekResponsesThinking(output)
	} else if requiresDeepSeekToolSchemaCompatibility(model) {
		output = scrubDeepSeekToolPayload(output, baseURL)
	}
	return compat.TransformResult{Payload: output, Downgrades: openAICompatDeepSeekPolicyDowngrades(input, output)}
}

func TestDeepSeekToolFieldViewPreservesMessages(t *testing.T) {
	body := []byte(`{"messages":[ {"role":"assistant","content":"\u4e2d","unknown":9007199254740993,"tool_calls":[{"id":"call1","type":"function","function":{"name":"echo","arguments":"{}"}}]} ],"tools":[{"type":"function","function":{"name":"echo","strict":true,"parameters":{"type":"object","required":null}}}]}`)
	got := scrubDeepSeekToolPayloadFields(body, "https://api.deepseek.com/v1")
	if gjson.GetBytes(got, "messages").Raw != gjson.GetBytes(body, "messages").Raw {
		t.Fatal("schema-only change rewrote message contents")
	}
	if gjson.GetBytes(got, "tools.0.function.strict").Exists() || gjson.GetBytes(got, "tools.0.function.parameters.required").Exists() {
		t.Fatal("schema normalization was skipped")
	}
}

func TestDeepSeekFieldViewReportsCompleteBodySizes(t *testing.T) {
	body := deepSeekPreparationFixture(8192, true)
	ctx := internalpayload.WithTransformReport(context.Background(), int64(len(body)))
	out, err := scrubOpenAICompatPayloadForModelWithPolicies(ctx, body,
		openAICompatProfileForKind("deepseek"), "deepseek-flash", "https://api.deepseek.com/v1",
		compat.MatchContext{Endpoint: "responses"})
	if err != nil {
		t.Fatal(err)
	}
	report, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || len(out) < 8192 {
		t.Fatal("missing full output/report")
	}
	seen := map[string]bool{}
	for _, stage := range report.Stages {
		seen[stage.Stage] = true
		if stage.InputBytes < 8192 || stage.OutputBytes < 8192 {
			t.Fatalf("small view leaked into admission/metrics: %+v", stage)
		}
	}
	for _, stage := range []string{openAICompatProviderPreQuirkStage, "compat/ProviderQuirkPatch", openAICompatProviderPostQuirkStage} {
		if !seen[stage] {
			t.Fatalf("lost stage %s", stage)
		}
	}
}

func assertDeepSeekJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()
	decode := func(body []byte) any {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return string(body)
		}
		return value
	}
	if !reflect.DeepEqual(decode(want), decode(got)) {
		t.Fatalf("payload changed\nwant %s\n got %s", want, got)
	}
}

func TestDeepSeekFieldViewMatchesLegacyPolicy(t *testing.T) {
	controls := []string{
		``, `,"thinking":{"type":"disabled","budget_tokens":500},"reasoning":{"effort":"high","summary":"auto"}`,
		`,"reasoning":{"effort":"none","summary":"auto"}`, `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`,
		`,"enable_thinking":false,"max_completion_tokens":"42"`, `,"enable_thinking":"bogus","max_completion_tokens":1.5`,
		`,"reasoning_effort":"xhigh","thinking_budget":999999`, `,"thinking":{"reasoning_effort":"off"}`,
		`,"thinking":{"type":"enabled","budget_tokens":"bad"},"thinking_budget":0`,
	}
	tools := []string{
		`[]`, `[{"type":"function","name":"echo","parameters":{"type":"object","required":null},"strict":true}]`,
		`[{"type":"function","function":{"name":"echo.dot","strict":true,"parameters":{"type":"object","properties":{"q":"string"}}}}]`,
	}
	for _, endpoint := range []compat.EndpointKind{"chat", "responses", "compact"} {
		for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-pro", "deepseek-reasoner"} {
			for _, baseURL := range []string{"https://api.deepseek.com/v1", "https://api.deepseek.com/beta"} {
				for _, control := range controls {
					for _, tool := range tools {
						for _, choice := range []string{`"auto"`, `"required"`, `{"type":"function","function":{"name":"echo.dot"}}`} {
							body := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"test"}],"messages":[{"role":"assistant","content":"text","reasoning_content":"real","tool_calls":[{"id":"call1","type":"function","function":{"name":"echo.dot","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"ok"}],"tools":%s,"tool_choice":%s%s}`, model, tool, choice, control))
							ctx := context.WithValue(context.Background(), openAICompatPolicyContextKey{}, openAICompatPolicyContext{model: model, baseURL: baseURL, endpoint: endpoint})
							got, err := applyOpenAICompatDeepSeekPolicy(ctx, body)
							if err != nil {
								t.Fatal(err)
							}
							want := legacyDeepSeekPolicy(body, model, baseURL, endpoint)
							assertDeepSeekJSONEqual(t, want.Payload, got.Payload)
							if !reflect.DeepEqual(want.Downgrades, got.Downgrades) {
								t.Fatalf("%s/%s: downgrades %v != %v; input %s", model, endpoint, want.Downgrades, got.Downgrades, body)
							}
						}
					}
				}
			}
		}
	}
}

func TestDeepSeekFieldViewScrubMatchesLegacy(t *testing.T) {
	profile := openAICompatProfileForKind("deepseek")
	model, baseURL := "deepseek-flash", "https://api.deepseek.com/v1"
	for _, endpoint := range []compat.EndpointKind{"chat", "responses", "compact"} {
		for _, body := range [][]byte{
			deepSeekPreparationFixture(8192, true),
			deepSeekPreparationFixture(8192, false),
			[]byte(`{"model":"deepseek-flash","input":"bad\v escape","store":true,"tools":[{"type":"function","name":"echo","parameters":{"required":null}}]}`),
			[]byte(`{"input":"text","store":false,"store":true,"thinking":{"type":"disabled"}}`),
			[]byte(`{"input":"text","messages":[{"role":"assistant","content":""}],"store":true,"stream_options":{"include_usage":true}}`),
		} {
			want := legacyDeepSeekPreScrub(body, profile, model)
			want = legacyDeepSeekPolicy(want, model, baseURL, endpoint).Payload
			want = scrubOpenAICompatPayloadAfterRegisteredProviderQuirks(want, profile, model, baseURL)
			got, err := scrubOpenAICompatPayloadForModelWithPolicies(context.Background(), body, profile, model, baseURL, compat.MatchContext{Endpoint: endpoint})
			if err != nil {
				t.Fatal(err)
			}
			assertDeepSeekJSONEqual(t, want, got)
			assertDeepSeekJSONEqual(t,
				scrubOpenAICompatPostConfigFields(body, profile, model, baseURL, endpoint),
				scrubOpenAICompatPostConfigPayload(body, profile, model, baseURL, endpoint))
		}
	}
}

func legacyDeepSeekPreScrub(body []byte, profile openAICompatProfile, model string) []byte {
	if repaired, ok := helps.RepairInvalidJSONStringEscapes(body); ok {
		body = repaired
	}
	body = scrubOpenAICompatPayload(body, openAICompatCapabilityProfileForModel(profile, model))
	body = repairOpenAICompatToolCallHistory(body)
	body = sanitizeOpenAICompatToolSchemas(body)
	if profile.NormalizeToolHistory {
		if normalized, err := normalizeOpenAICompatToolMessageLinksWithReasoningRepair(body, "openai compat executor", !requiresReturnedThinkingHistory(model)); err == nil {
			body = normalized
		}
	}
	return body
}

func TestDeepSeekFieldViewCapabilityProfiles(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":"answer","reasoning_content":"real reasoning"}],"store":true,"metadata":{"keep":"value"},"reasoning_effort":"high","reasoning":{"effort":"high"}}`)
	for _, supports := range []bool{false, true} {
		for _, preserves := range []bool{false, true} {
			profile := openAICompatProfileForKind("deepseek")
			profile.SupportsReasoning = supports
			profile.PreserveReasoningContent = preserves
			assertDeepSeekJSONEqual(t, legacyDeepSeekPreScrub(body, profile, "deepseek-flash"),
				scrubOpenAICompatPayloadBeforeRegisteredProviderQuirks(body, profile, "deepseek-flash", "https://api.deepseek.com/v1"))
		}
	}
}

func TestDeepSeekFieldViewConcurrentIsolation(t *testing.T) {
	body := deepSeekPreparationFixture(8192, true)
	original := bytes.Clone(body)
	for _, endpoint := range []compat.EndpointKind{"chat", "responses"} {
		t.Run(string(endpoint), func(t *testing.T) {
			t.Parallel()
			state := openAICompatPolicyContext{model: "deepseek-flash", baseURL: "https://api.deepseek.com/v1", endpoint: endpoint}
			ctx := context.WithValue(context.Background(), openAICompatPolicyContextKey{}, state)
			want := legacyDeepSeekPolicy(original, state.model, state.baseURL, state.endpoint)
			for i := 0; i < 8; i++ {
				got, err := applyOpenAICompatDeepSeekPolicy(ctx, body)
				if err != nil {
					t.Fatal(err)
				}
				assertDeepSeekJSONEqual(t, want.Payload, got.Payload)
				if !bytes.Equal(body, original) {
					t.Fatal("parallel requests mutated shared source bytes")
				}
			}
		})
	}
}
