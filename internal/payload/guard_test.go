package payload

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestObserveAmplificationDefaultAllowance(t *testing.T) {
	tests := []struct {
		name         string
		input        int64
		output       int64
		wantAllowed  int64
		wantExceeded bool
	}{
		{name: "small exact fixed allowance", input: 1024, output: 1024 + DefaultMaxExpansionBytes, wantAllowed: 1024 + DefaultMaxExpansionBytes},
		{name: "small one byte over fixed allowance", input: 1024, output: 1024 + DefaultMaxExpansionBytes + 1, wantAllowed: 1024 + DefaultMaxExpansionBytes, wantExceeded: true},
		{name: "large exact percentage allowance", input: 2 * 1024 * 1024, output: 2560 * 1024, wantAllowed: 2560 * 1024},
		{name: "large one byte over percentage allowance", input: 2 * 1024 * 1024, output: 2560*1024 + 1, wantAllowed: 2560 * 1024, wantExceeded: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observation := ObserveAmplification(tt.input, tt.output, AmplificationOverride{})
			if observation.AllowedOutputBytes != tt.wantAllowed || observation.Exceeded != tt.wantExceeded {
				t.Fatalf("observation = %#v", observation)
			}
			if observation.OverrideApplied || observation.PolicyID != DefaultAmplificationPolicyID {
				t.Fatalf("default observation unexpectedly applied override: %#v", observation)
			}
		})
	}
}

func TestObserveAmplificationPolicyOverride(t *testing.T) {
	observation := ObserveAmplification(1024, 1800, AmplificationOverride{
		PolicyID:          "compat.expanding-policy",
		MaxExpansionBytes: 512,
		MaxExpansionRatio: 2,
	})
	if !observation.OverrideApplied || observation.PolicyID != "compat.expanding-policy" {
		t.Fatalf("override metadata = %#v", observation)
	}
	if observation.AllowedOutputBytes != 2048 || observation.Exceeded {
		t.Fatalf("override allowance = %#v", observation)
	}

	withoutPolicyID := ObserveAmplification(1024, 1024+DefaultMaxExpansionBytes+1, AmplificationOverride{MaxExpansionBytes: math.MaxInt64})
	if withoutPolicyID.OverrideApplied || withoutPolicyID.PolicyID != DefaultAmplificationPolicyID || !withoutPolicyID.Exceeded {
		t.Fatalf("anonymous override must be ignored: %#v", withoutPolicyID)
	}
}

func TestObserveAmplificationIsJSONSafeAtZeroAndOverflow(t *testing.T) {
	zero := ObserveAmplification(0, 1, AmplificationOverride{})
	if zero.Ratio != 0 || zero.ExpansionBytes != 1 {
		t.Fatalf("zero-input observation = %#v", zero)
	}

	overflow := ObserveAmplification(math.MaxInt64, math.MaxInt64, AmplificationOverride{})
	if overflow.AllowedOutputBytes != math.MaxInt64 || overflow.Exceeded {
		t.Fatalf("overflow observation = %#v", overflow)
	}

	invalidRatio := ObserveAmplification(1024, 1024+DefaultMaxExpansionBytes+1, AmplificationOverride{
		PolicyID:          "invalid",
		MaxExpansionRatio: math.Inf(1),
	})
	if invalidRatio.OverrideApplied || invalidRatio.PolicyID != DefaultAmplificationPolicyID || !invalidRatio.Exceeded {
		t.Fatalf("infinite ratio must be ignored: %#v", invalidRatio)
	}

	belowOne := ObserveAmplification(1024, 1024+DefaultMaxExpansionBytes+1, AmplificationOverride{
		PolicyID:          "invalid",
		MaxExpansionRatio: 0.50,
	})
	if belowOne.OverrideApplied || belowOne.PolicyID != DefaultAmplificationPolicyID || !belowOne.Exceeded {
		t.Fatalf("ratio below one must be ignored: %#v", belowOne)
	}
}

func TestEnforceRequestTransformDefaultBoundaryAndOverride(t *testing.T) {
	const inputBytes = int64(1 << 20)
	defaultLimit := defaultAllowedOutputBytes(inputBytes)
	ctx := WithTransformReport(context.Background(), inputBytes)
	if err := EnforceRequestTransform(ctx, "legacy.translate.test", inputBytes, defaultLimit, AmplificationOverride{}); err != nil {
		t.Fatalf("default boundary rejected: %v", err)
	}
	if err := EnforceRequestTransform(ctx, "legacy.translate.test", inputBytes, defaultLimit+1, AmplificationOverride{}); err == nil {
		t.Fatal("default overflow was not rejected")
	}

	override := AmplificationOverride{PolicyID: "legacy.test.double", MaxExpansionRatio: 2}
	if err := EnforceRequestTransform(ctx, "legacy.translate.test", inputBytes, 2*inputBytes, override); err != nil {
		t.Fatalf("controlled override rejected: %v", err)
	}
	report, ok := TransformReportFromContext(ctx)
	if !ok || len(report.Stages) != 3 || !report.Stages[1].Amplification.Exceeded || report.Stages[2].Amplification.PolicyID != override.PolicyID {
		t.Fatalf("transform report stages = %#v", report.Stages)
	}
	if policies := report.Stages[1].AppliedPolicies; len(policies) != 1 || policies[0] != DefaultAmplificationPolicyID {
		t.Fatalf("default stage policies = %#v", policies)
	}
}

func TestEnforceRequestTransformFailureIsTypedAndDiagnostic(t *testing.T) {
	const stage = "legacy.translate.diagnostic"
	const inputBytes = int64(1)
	allowedOutputBytes := defaultAllowedOutputBytes(inputBytes)
	err := EnforceRequestTransform(context.Background(), stage, inputBytes, allowedOutputBytes+1, AmplificationOverride{})
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InvalidRequest || typed.Scope != failurecontract.ScopeRequest || typed.HTTPStatus != http.StatusBadRequest || typed.ProviderCode != requestTransformExpansionCode {
		t.Fatalf("typed failure = %#v", typed)
	}
	diagnostic, ok := typed.Cause.(*requestTransformExpansionError)
	if !ok || diagnostic.PolicyID != DefaultAmplificationPolicyID || diagnostic.Stage != stage || diagnostic.InputBytes != inputBytes || diagnostic.OutputBytes != allowedOutputBytes+1 || diagnostic.AllowedOutputBytes != allowedOutputBytes {
		t.Fatalf("diagnostic metadata = %#v", diagnostic)
	}
	for _, token := range []string{"policy_id=" + DefaultAmplificationPolicyID, "stage=" + stage, "input_bytes=1", "output_bytes=262146", "allowed_output_bytes=262145"} {
		if !strings.Contains(typed.Cause.Error(), token) {
			t.Fatalf("diagnostic error %q missing %q", typed.Cause.Error(), token)
		}
	}
	if strings.Contains(typed.PublicMessage, stage) || strings.Contains(typed.PublicMessage, DefaultAmplificationPolicyID) {
		t.Fatalf("public message exposed diagnostic metadata: %q", typed.PublicMessage)
	}
}

func TestEnforceRequestTransformObserveAndEnforceModes(t *testing.T) {
	const inputBytes = int64(1024)
	outputBytes := defaultAllowedOutputBytes(inputBytes) + 1

	observeCtx := WithAmplificationMode(WithTransformReport(context.Background(), inputBytes), AmplificationModeObserve)
	if err := EnforceRequestTransform(observeCtx, "legacy.translate.observe", inputBytes, outputBytes, AmplificationOverride{}); err != nil {
		t.Fatalf("observe mode rejected request: %v", err)
	}
	observeReport, ok := TransformReportFromContext(observeCtx)
	if !ok || len(observeReport.Stages) != 1 || !observeReport.Stages[0].Amplification.Exceeded {
		t.Fatalf("observe report = %#v", observeReport)
	}

	enforceCtx := WithAmplificationMode(context.Background(), AmplificationModeEnforce)
	err := EnforceRequestTransform(enforceCtx, "legacy.translate.enforce", inputBytes, outputBytes, AmplificationOverride{})
	typed, ok := failurecontract.As(err)
	if !ok || typed.ProviderCode != requestTransformExpansionCode {
		t.Fatalf("enforce failure = %#v", typed)
	}

	if err := EnforceRequestTransform(context.Background(), "legacy.translate.default", inputBytes, outputBytes, AmplificationOverride{}); err == nil {
		t.Fatal("unconfigured context must preserve explicit enforce semantics")
	}
}

func BenchmarkObserveAmplification(b *testing.B) {
	override := AmplificationOverride{PolicyID: "compat.example.v1", MaxExpansionRatio: 1.50}
	b.ReportAllocs()
	for idx := 0; idx < b.N; idx++ {
		_ = ObserveAmplification(8*1024*1024, 10*1024*1024, override)
	}
}
