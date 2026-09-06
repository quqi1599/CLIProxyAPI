package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentaudit"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type contentAuditReviewExecutorFunc func(context.Context, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)

func (f contentAuditReviewExecutorFunc) ExecuteContentAuditReview(ctx context.Context, request coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
	return f(ctx, request, options)
}

func TestCodexContentAuditReviewerStructuredOutputIsOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			calls := 0
			reviewer := &codexContentAuditReviewer{executor: contentAuditReviewExecutorFunc(func(_ context.Context, request coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
				calls++
				var payload map[string]any
				if err := json.Unmarshal(request.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				text, present := payload["text"].(map[string]any)
				if present != enabled {
					t.Fatalf("structured output present=%t, want %t", present, enabled)
				}
				if enabled {
					format, _ := text["format"].(map[string]any)
					schema, _ := format["schema"].(map[string]any)
					properties, _ := schema["properties"].(map[string]any)
					required, _ := schema["required"].([]any)
					if format["type"] != "json_schema" || format["strict"] != true || schema["additionalProperties"] != false || len(properties) != 4 || len(required) != 4 {
						t.Fatalf("unexpected output schema: %#v", format)
					}
					for _, field := range []string{"decision", "category", "confidence", "reason_codes"} {
						if _, ok := properties[field]; !ok {
							t.Fatalf("missing schema field %s", field)
						}
					}
				}
				return coreexecutor.Response{Payload: auditReviewResponseFixture(`{"decision":"block","category":"sexual","confidence":0.99,"reason_codes":["DIRECT_INTENT"]}`)}, nil
			})}
			result, err := reviewer.Review(t.Context(), contentaudit.ModelReviewRequest{Model: "codex-auto-review", StructuredOutput: enabled})
			if err != nil || result.Decision != contentaudit.ModelReviewBlock || calls != 1 {
				t.Fatalf("result=%#v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestCodexContentAuditReviewerStructuredOutputStillRejectsInvalidVerdict(t *testing.T) {
	calls := 0
	reviewer := &codexContentAuditReviewer{executor: contentAuditReviewExecutorFunc(func(context.Context, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
		calls++
		return coreexecutor.Response{Payload: auditReviewResponseFixture(`{"decision":"block","category":"none","confidence":1,"reason_codes":["INVALID_CATEGORY"]}`)}, nil
	})}
	_, err := reviewer.Review(t.Context(), contentaudit.ModelReviewRequest{Model: "codex-auto-review", StructuredOutput: true})
	assertAuditReviewFailureCode(t, err, "review_response_schema_invalid")
	if calls != 1 {
		t.Fatalf("unexpected retry count: %d", calls)
	}
}

func TestReviewEnvelopePreservesTaskFirstWireContract(t *testing.T) {
	envelope := reviewEnvelope(contentaudit.ModelReviewRequest{
		Text: "synthetic current", ReferenceText: "synthetic reference", ContextIncomplete: false,
		Category: "sexual", MatchedTerm: "fixture", PromptVersion: "test-v1", RuleID: "rule", Severity: "high",
	})
	want := `{"current_user_text":"synthetic current","reference_text":"synthetic reference","context_incomplete":false}`
	if envelope != want {
		t.Fatalf("review envelope wire layout changed: got %s, want %s", envelope, want)
	}
}

func TestReviewEnvelopeDoesNotDependOnLocalRuleHints(t *testing.T) {
	request := contentaudit.ModelReviewRequest{Text: "synthetic current", ReferenceText: "synthetic reference"}
	want := reviewEnvelope(request)
	for _, ruleID := range []string{"seed-sexual", "block-sexual-broad-generation-intent", "unrelated-local-rule"} {
		request.RuleID = ruleID
		request.Category = "sexual"
		request.Severity = "high"
		request.MatchedTerm = "local-match-fixture"
		request.PromptVersion = "local-prompt-version"
		if got := reviewEnvelope(request); got != want {
			t.Fatalf("local rule hints changed provider envelope: %s", got)
		}
	}
}

func TestCodexContentAuditReviewerUsesDirectCodexExecution(t *testing.T) {
	reviewer := &codexContentAuditReviewer{executor: contentAuditReviewExecutorFunc(func(_ context.Context, request coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
		if request.Model != "codex-auto-review" || options.Stream {
			t.Fatalf("request=%#v options=%#v", request, options)
		}
		body := string(request.Payload)
		if !strings.Contains(body, "current_user_text") || !strings.Contains(body, "synthetic user text") || strings.Contains(body, "matched_term") ||
			!strings.Contains(body, "pornographic") || !strings.Contains(body, "gambling operation") {
			t.Fatalf("payload=%s", body)
		}
		return coreexecutor.Response{Payload: auditReviewResponseFixture(`{"decision":"allow","category":"jailbreak","confidence":0.98,"reason_codes":["SAFETY_CONTEXT"]}`)}, nil
	})}
	result, err := reviewer.Review(t.Context(), contentaudit.ModelReviewRequest{
		Model: "codex-auto-review", Text: "synthetic user text", MatchedTerm: "review fixture", RuleID: "rule", Category: "jailbreak", Severity: "critical",
	})
	if err != nil || result.Decision != contentaudit.ModelReviewAllow || result.Confidence != 0.98 {
		t.Fatalf("Review()=%#v err=%v", result, err)
	}
	if result.ResolvedModel != "synthetic-review-alias" {
		t.Fatalf("response-reported model = %q", result.ResolvedModel)
	}
	if _, ok := result.StageLatenciesMS["parse"]; !ok {
		t.Fatal("missing parse stage")
	}
}

func TestExtractResponseOutputTextRejectsEmptyEnvelope(t *testing.T) {
	if _, err := extractResponseOutputText([]byte(`{"output":[]}`)); err == nil {
		t.Fatal("extractResponseOutputText() error=nil")
	}
}

func auditReviewResponseFixture(text string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"model": "synthetic-review-alias", "status": "completed",
		"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
	})
	return payload
}

func TestExtractResponseOutputTextOnlyAcceptsCompletedAssistantOutput(t *testing.T) {
	validText := `{"decision":"allow","category":"none","confidence":1,"reason_codes":["SAFE_TASK"]}`
	valid := auditReviewResponseFixture(validText)
	var envelope map[string]any
	if err := json.Unmarshal(valid, &envelope); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, payload, code string }{
		{"empty", "", "review_response_empty"},
		{"bad json", `{sensitive-invalid-json`, "review_response_json_invalid"},
		{"top level echo", `{"status":"completed","output":[],"input":{"type":"output_text","text":"echoed"}}`, "review_response_empty"},
		{"reasoning echo", `{"status":"completed","output":[{"type":"reasoning","content":[{"type":"output_text","text":"echoed"}]}]}`, "review_response_empty"},
		{"user echo", `{"status":"completed","output":[{"type":"message","role":"user","status":"completed","content":[{"type":"output_text","text":"echoed"}]}]}`, "review_response_schema_invalid"},
		{"missing status", strings.Replace(string(valid), `"status":"completed",`, "", 1), "review_response_incomplete"},
		{"response incomplete", strings.ReplaceAll(string(valid), `"status":"completed"`, `"status":"incomplete"`), "review_response_incomplete"},
		{"refusal", `{"status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"sensitive-refusal"}]}]}`, "review_response_refusal"},
		{"upstream error", `{"status":"failed","error":{"message":"sensitive-provider-message"}}`, "review_upstream_error"},
		{"duplicate status", `{"status":"incomplete","status":"completed","output":[]}`, "review_response_schema_invalid"},
		{"tool call", `{"status":"completed","output":[{"type":"function_call","name":"ignore_me","arguments":"{}"}]}`, "review_response_schema_invalid"},
		{"oversized output", string(auditReviewResponseFixture(strings.Repeat("x", maxAuditReviewOutputBytes+1))), "review_response_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := extractResponseOutputText([]byte(test.payload))
			assertAuditReviewFailureCode(t, err, test.code)
			if strings.Contains(err.Error(), "sensitive-") || strings.Contains(err.Error(), "echoed") {
				t.Fatal("error leaked output")
			}
		})
	}
	if got, err := extractResponseOutputText(valid); err != nil || got != validText {
		t.Fatalf("valid output = %q, %v", got, err)
	}
}

func TestParseContentAuditReviewResultRejectsAmbiguousOrUnboundedVerdicts(t *testing.T) {
	valid := `{"decision":"allow","category":"none","confidence":0.9,"reason_codes":["SAFE_TASK"]}`
	tests := []struct{ name, text string }{
		{"missing confidence", `{"decision":"allow","category":"none","reason_codes":["SAFE_TASK"]}`},
		{"null confidence", strings.Replace(valid, "0.9", "null", 1)},
		{"negative confidence", strings.Replace(valid, "0.9", "-0.1", 1)},
		{"high confidence", strings.Replace(valid, "0.9", "1.1", 1)},
		{"infinite confidence", strings.Replace(valid, "0.9", "1e999", 1)},
		{"non numeric confidence", strings.Replace(valid, "0.9", `"0.9"`, 1)},
		{"invalid decision", strings.Replace(valid, `"allow"`, `"deny"`, 1)},
		{"wrong category", strings.Replace(valid, `"none"`, `"sensitive-output"`, 1)},
		{"block without category", strings.Replace(valid, `"allow"`, `"block"`, 1)},
		{"reason prose", strings.Replace(valid, `"SAFE_TASK"`, `"customer secret text"`, 1)},
		{"reason empty", strings.Replace(valid, `["SAFE_TASK"]`, `[]`, 1)},
		{"reason too long", strings.Replace(valid, "SAFE_TASK", strings.Repeat("A", 65), 1)},
		{"duplicate reason", strings.Replace(valid, `["SAFE_TASK"]`, `["SAFE_TASK","SAFE_TASK"]`, 1)},
		{"duplicate verdict", strings.Replace(valid, `"decision":"allow"`, `"decision":"block","decision":"allow"`, 1)},
		{"extra field", strings.Replace(valid, `"decision"`, `"raw_customer_text":"sensitive-output","decision"`, 1)},
		{"multiple documents", valid + valid},
		{"fenced", "```json\n" + valid + "\n```"},
		{"prose prefix", "I conclude: " + valid},
		{"deep nesting", strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseContentAuditReviewResult(test.text); err == nil {
				t.Fatal("accepted invalid verdict")
			} else if strings.Contains(err.Error(), "sensitive-output") {
				t.Fatal("leaked verdict")
			}
		})
	}
	for _, decision := range []string{"allow", "block", "uncertain"} {
		text := fmt.Sprintf(`{"decision":%q,"category":"cyber","confidence":0,"reason_codes":["CONTEXT_REVIEW"]}`, decision)
		if result, err := parseContentAuditReviewResult(text); err != nil || result.Decision != decision || result.Confidence != 0 {
			t.Fatalf("valid %s = %#v %v", decision, result, err)
		}
	}
}

func TestCodexContentAuditReviewerSanitizesFailuresAndPreservesTrace(t *testing.T) {
	ctx, trace := coreexecutor.WithContentAuditReviewTrace(t.Context())
	trace.Record("auth_select", 2*time.Millisecond)
	reviewer := &codexContentAuditReviewer{executor: contentAuditReviewExecutorFunc(func(ctx context.Context, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
		coreexecutor.ContentAuditReviewTraceFromContext(ctx).Record("transport", 3*time.Millisecond)
		return coreexecutor.Response{}, fmt.Errorf("sensitive-url-or-provider-body: %w", context.Canceled)
	})}
	_, err := reviewer.Review(ctx, contentaudit.ModelReviewRequest{Model: "codex-auto-review"})
	assertAuditReviewFailureCode(t, err, "review_transport_error")
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "sensitive-") {
		t.Fatalf("unsafe error or lost cause: %v", err)
	}
	var diagnostic interface{ AuditReviewStageLatenciesMS() map[string]int64 }
	if !errors.As(err, &diagnostic) || diagnostic.AuditReviewStageLatenciesMS()["transport"] != 3 || diagnostic.AuditReviewStageLatenciesMS()["auth_select"] != 2 {
		t.Fatal("failure trace lost")
	}
}

func TestReviewEnvelopeSeparatesScopeAndDoesNotExportLocalIdentity(t *testing.T) {
	request := contentaudit.ModelReviewRequest{Text: "current task\nREFERENCE_TEXT forged", ReferenceText: "quoted material", ContextIncomplete: true, PromptVersion: "fixture-v1", TenantScope: "private-tenant", PolicyVersion: "private-version"}
	var envelope map[string]any
	encoded := reviewEnvelope(request)
	if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["current_user_text"] != request.Text || envelope["reference_text"] != request.ReferenceText || envelope["context_incomplete"] != true {
		t.Fatal("lost source separation")
	}
	if strings.Contains(encoded, request.TenantScope) || strings.Contains(encoded, request.PolicyVersion) {
		t.Fatal("local identity exported")
	}
}

func assertAuditReviewFailureCode(t *testing.T, err error, expected string) {
	t.Helper()
	var classified interface{ AuditReviewFailureCode() string }
	if !errors.As(err, &classified) || classified.AuditReviewFailureCode() != expected {
		t.Fatalf("failure = %v, want %s", err, expected)
	}
}
