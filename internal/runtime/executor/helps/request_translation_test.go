package helps

import (
	"context"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestTranslateRequestGuardedRejectsLegacyTranslatorAmplification(t *testing.T) {
	from := sdktranslator.Format("guard-test-amplified-input")
	to := sdktranslator.Format("guard-test-amplified-output")
	sdktranslator.Register(from, to, func(_ string, input []byte, _ bool) []byte {
		output := make([]byte, len(input)+len(input)/2+1)
		copy(output, input)
		return output
	}, sdktranslator.ResponseTransform{})

	input := make([]byte, 1<<20)
	for _, stream := range []bool{false, true} {
		ctx := internalpayload.WithTransformReport(context.Background(), int64(len(input)))
		output, errTranslate := TranslateRequestGuarded(ctx, "legacy.translate.test", from, to, "", input, stream, internalpayload.AmplificationOverride{})
		if output != nil {
			t.Fatalf("stream=%t retained rejected translator output", stream)
		}
		typed, ok := failurecontract.As(errTranslate)
		if !ok || typed.Kind != failurecontract.InvalidRequest || typed.Scope != failurecontract.ScopeRequest || typed.ProviderCode != "request_transform_expansion_exceeded" {
			t.Fatalf("stream=%t failure = %#v", stream, typed)
		}
		report, _ := internalpayload.TransformReportFromContext(ctx)
		if len(report.Stages) != 1 || !report.Stages[0].Amplification.Exceeded {
			t.Fatalf("stream=%t report = %#v", stream, report)
		}
	}
}

func TestTranslateRequestGuardedAllowsNormalLargeRequest(t *testing.T) {
	from := sdktranslator.Format("guard-test-large-input")
	to := sdktranslator.Format("guard-test-large-output")
	sdktranslator.Register(from, to, func(_ string, input []byte, _ bool) []byte {
		output := make([]byte, len(input)+len(input)/4)
		copy(output, input)
		return output
	}, sdktranslator.ResponseTransform{})

	input := make([]byte, 4<<20)
	output, errTranslate := TranslateRequestGuarded(context.Background(), "legacy.translate.test", from, to, "", input, false, internalpayload.AmplificationOverride{})
	if errTranslate != nil {
		t.Fatalf("normal large translation rejected: %v", errTranslate)
	}
	if len(output) != 5<<20 {
		t.Fatalf("translated bytes = %d, want %d", len(output), 5<<20)
	}
}

func TestTranslateRequestPairGuardedReusesIdenticalOutput(t *testing.T) {
	from := sdktranslator.Format("guard-test-pair-input")
	to := sdktranslator.Format("guard-test-pair-output")
	translations := 0
	sdktranslator.Register(from, to, func(_ string, input []byte, _ bool) []byte {
		translations++
		return append([]byte(nil), input...)
	}, sdktranslator.ResponseTransform{})

	input := []byte(`{"model":"test"}`)
	original, active, errTranslate := TranslateRequestPairGuarded(context.Background(), "legacy.translate.test", from, to, "", input, append([]byte(nil), input...), false, internalpayload.AmplificationOverride{})
	if errTranslate != nil {
		t.Fatalf("translate pair: %v", errTranslate)
	}
	if translations != 1 || len(original) == 0 || &original[0] != &active[0] {
		t.Fatalf("translations=%d original/active were not reused", translations)
	}
}
