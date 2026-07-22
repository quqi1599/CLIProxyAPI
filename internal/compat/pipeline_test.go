package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

func TestPipelineAppliesMatchingPoliciesInDeterministicOrder(t *testing.T) {
	executionOrder := make([]string, 0, 3)
	policy := func(id string, phase Phase, priority int, suffix string) Policy {
		value := validPolicy(id, phase, priority, "body."+id)
		value.Apply = func(_ context.Context, input []byte) (TransformResult, error) {
			executionOrder = append(executionOrder, id)
			return TransformResult{Payload: append(append([]byte(nil), input...), suffix...)}, nil
		}
		return value
	}
	registry, err := NewRegistry(
		policy("z", ProviderQuirkPatch, 10, "z"),
		policy("later", AmplificationGuard, -100, "l"),
		policy("a", ProviderQuirkPatch, 10, "a"),
		policy("first", ProviderQuirkPatch, -1, "f"),
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	result, err := NewPipeline(registry).Apply(context.Background(), MatchContext{}, []byte("x"))
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got, want := string(result.Payload), "xfazl"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
	if got, want := strings.Join(executionOrder, ","), "first,a,z,later"; got != want {
		t.Fatalf("execution order = %q, want %q", got, want)
	}
	if len(result.Report.Phases) != 2 || result.Report.Phases[0].Phase != ProviderQuirkPatch || result.Report.Phases[1].Phase != AmplificationGuard {
		t.Fatalf("phase report = %+v", result.Report.Phases)
	}
}

func TestPipelineIsIdempotentAndDoesNotMutateCallerInput(t *testing.T) {
	policy := validPolicy("idempotent", ProviderQuirkPatch, 0, "body.marker")
	policy.Apply = func(_ context.Context, input []byte) (TransformResult, error) {
		if bytes.HasSuffix(input, []byte("!")) {
			return TransformResult{Payload: input}, nil
		}
		input[0] = 'X'
		return TransformResult{Payload: append(input, '!')}, nil
	}
	registry, err := NewRegistry(policy)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	pipeline := NewPipeline(registry)
	input := []byte("abc")

	first, err := pipeline.Apply(context.Background(), MatchContext{}, input)
	if err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	second, err := pipeline.Apply(context.Background(), MatchContext{}, first.Payload)
	if err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if string(input) != "abc" {
		t.Fatalf("caller input mutated to %q", input)
	}
	if !bytes.Equal(first.Payload, second.Payload) || string(first.Payload) != "Xbc!" {
		t.Fatalf("pipeline is not idempotent: first=%q second=%q", first.Payload, second.Payload)
	}
}

func TestPipelineRejectsPolicyExpansionBeyondCostContract(t *testing.T) {
	policy := validPolicy("bounded", ProviderQuirkPatch, 0, "body.large")
	policy.Cost.MaxExpansionBytes = 2
	policy.Cost.MaxExpansionRatio = 0
	policy.Apply = func(_ context.Context, input []byte) (TransformResult, error) {
		return TransformResult{Payload: append(append([]byte(nil), input...), "too-large"...)}, nil
	}
	registry, err := NewRegistry(policy)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	result, err := NewPipeline(registry).Apply(context.Background(), MatchContext{}, []byte("x"))
	if err == nil {
		t.Fatal("Apply() unexpectedly accepted an over-limit transform")
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.InternalTransformError || typed.Scope != failurecontract.ScopeRequest || typed.Retryable {
		t.Fatalf("failure = %#v", typed)
	}
	if typed.ProviderCode != "compat_expansion_exceeded" || result.Payload != nil {
		t.Fatalf("failure code=%q payload=%q", typed.ProviderCode, result.Payload)
	}
	policyReport := result.Report.Phases[0].Policies[0]
	if !policyReport.Amplification.Exceeded || policyReport.Amplification.AllowedOutputBytes != 3 {
		t.Fatalf("amplification report = %#v", policyReport.Amplification)
	}
}

func TestPipelineReportsControlledMetadataWithoutPayload(t *testing.T) {
	const secret = "secret-prompt-123"
	policy := validPolicy("reported", RepairHistory, 0, "body.reasoning")
	policy.DowngradeIDs = []string{"strip.reasoning"}
	policy.Apply = func(_ context.Context, input []byte) (TransformResult, error) {
		return TransformResult{
			Payload:        append(append([]byte(nil), input...), '!'),
			SyntheticBytes: 1,
			Downgrades:     []string{"strip.reasoning"},
		}, nil
	}
	registry, err := NewRegistry(policy)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	ctx := internalpayload.WithTransformReport(context.Background(), int64(len(secret)))
	result, err := NewPipeline(registry).Apply(ctx, MatchContext{}, []byte(secret))
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	encoded, err := json.Marshal(result.Report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	if bytes.Contains(encoded, []byte(secret)) {
		t.Fatalf("pipeline report leaked payload: %s", encoded)
	}
	if result.Report.SyntheticBytes != 1 || result.Report.Phases[0].Downgrades[0] != "strip.reasoning" {
		t.Fatalf("pipeline report = %+v", result.Report)
	}
	transformReport, ok := internalpayload.TransformReportFromContext(ctx)
	if !ok || len(transformReport.Stages) != 1 {
		t.Fatalf("transform report = %+v, ok=%v", transformReport, ok)
	}
	stage := transformReport.Stages[0]
	if stage.Stage != "compat/RepairHistory" || len(stage.AppliedPolicies) != 1 || stage.AppliedPolicies[0] != "reported" || stage.Downgrades[0] != "strip.reasoning" {
		t.Fatalf("transform stage = %+v", stage)
	}
}

func TestPipelineDropsUntrustedErrorAndDowngradeMetadata(t *testing.T) {
	const secret = "secret-prompt-123"
	tests := []struct {
		name  string
		apply TransformFunc
	}{
		{
			name: "error",
			apply: func(context.Context, []byte) (TransformResult, error) {
				return TransformResult{}, errors.New(secret)
			},
		},
		{
			name: "wrapped cancellation",
			apply: func(context.Context, []byte) (TransformResult, error) {
				return TransformResult{}, fmt.Errorf("%s: %w", secret, context.Canceled)
			},
		},
		{
			name: "undeclared downgrade",
			apply: func(_ context.Context, input []byte) (TransformResult, error) {
				return TransformResult{Payload: input, Downgrades: []string{secret}}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := validPolicy("safe", ProviderQuirkPatch, 0, "body.safe")
			policy.Apply = test.apply
			registry, err := NewRegistry(policy)
			if err != nil {
				t.Fatalf("NewRegistry() error = %v", err)
			}
			result, err := NewPipeline(registry).Apply(context.Background(), MatchContext{}, []byte(secret))
			if err == nil {
				t.Fatal("Apply() unexpectedly succeeded")
			}
			encoded, marshalErr := json.Marshal(result.Report)
			if marshalErr != nil {
				t.Fatalf("json.Marshal(report) error = %v", marshalErr)
			}
			cause := errors.Unwrap(err)
			if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprint(cause), secret) || bytes.Contains(encoded, []byte(secret)) {
				t.Fatalf("error metadata leaked payload: error=%q cause=%q report=%s", err, cause, encoded)
			}
		})
	}
}

func TestPipelineApplyIncompleteMatchFailsClosed(t *testing.T) {
	policy := validPolicy("compat.constrained", ProviderQuirkPatch, 10, "body.compat")
	policy.Match = MatchSpec{
		ProviderFamily: "openai-compatible",
		Endpoints:      []EndpointKind{"chat"},
		Modes:          []ExecutionMode{"stream"},
	}
	policy.Apply = func(_ context.Context, input []byte) (TransformResult, error) {
		return TransformResult{Payload: append(append([]byte(nil), input...), 'x')}, nil
	}
	registry, err := NewRegistry(policy)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	input := []byte(`{"ok":true}`)
	result, err := NewPipeline(registry).Apply(context.Background(), MatchContext{}, input)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if string(result.Payload) != string(input) || !result.Report.ReusedInput || len(result.Report.Phases) != 0 {
		t.Fatalf("incomplete match applied constrained policy: %+v", result.Report)
	}

	result, err = NewPipeline(registry).Apply(context.Background(), MatchContext{
		ProviderFamily: "openai-compatible",
		Endpoint:       "chat",
		Mode:           "stream",
	}, input)
	if err != nil {
		t.Fatalf("Apply(complete) error = %v", err)
	}
	if string(result.Payload) != string(input)+"x" {
		t.Fatalf("complete match payload = %q", result.Payload)
	}
}

func BenchmarkPipelineApplyFewMatchesAmongHundredPolicies(b *testing.B) {
	policies := make([]Policy, 100)
	for index := range policies {
		id := fmt.Sprintf("policy-%03d", index)
		policies[index] = validPolicy(id, ProviderQuirkPatch, index, "body."+id)
		policies[index].Match.ProviderFamily = fmt.Sprintf("provider-%03d", index)
	}
	registry, err := NewRegistry(policies...)
	if err != nil {
		b.Fatal(err)
	}
	pipeline := NewPipeline(registry)
	input := bytes.Repeat([]byte("x"), 1024)
	match := MatchContext{ProviderFamily: "provider-050"}
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := pipeline.Apply(context.Background(), match, input); err != nil {
			b.Fatal(err)
		}
	}
}
