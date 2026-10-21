package helps

import (
	"fmt"
	"net/http"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestFailFromErrorsPreservesDetachedUpstreamDiagnostic(t *testing.T) {
	maximum, actual := int64(64), int64(85)
	upstream := &failurecontract.Failure{
		Kind: failurecontract.InvalidRequest, Scope: failurecontract.ScopeRequest,
		HTTPStatus: http.StatusBadRequest, OuterStatus: http.StatusOK,
		SemanticCode: "string_above_max_length", SemanticType: "invalid_request_error",
		StreamPhase: failurecontract.StreamPhaseAfterOutput, OutputCommitted: true,
		PublicMessage: "upstream request rejected",
		ErrorDiagnostic: failurecontract.UpstreamErrorDiagnostic{
			Param: "input[42].call_id", ReportedMaxLength: &maximum, ReportedActualLength: &actual,
		},
	}
	got := failFromErrors(fmt.Errorf("wrapped: %w", upstream))
	if !got.Canonical || got.StatusCode != 400 || got.OuterStatus != 200 || got.Retryable ||
		!got.OutputCommitted || got.UpstreamErrorParam != "input[].call_id" ||
		got.UpstreamReportedMaxLength == nil || *got.UpstreamReportedMaxLength != 64 ||
		got.UpstreamReportedActualLength == nil || *got.UpstreamReportedActualLength != 85 ||
		got.UpstreamLengthUnit != "" {
		t.Fatalf("usage lost safe upstream diagnostics: %+v", got)
	}
	maximum, actual = 999, 1000
	if *got.UpstreamReportedMaxLength != 64 || *got.UpstreamReportedActualLength != 85 {
		t.Fatal("queued usage retained mutable diagnostic pointers")
	}
	record := (&UsageReporter{}).buildRecordForModel("gpt-fixture", usage.Detail{}, true, got)
	if record.ProviderStatusCode != 200 || record.Fail.UpstreamErrorParam != "input[].call_id" ||
		record.Fail.UpstreamReportedMaxLength == nil || *record.Fail.UpstreamReportedMaxLength != 64 {
		t.Fatalf("usage reporter dropped upstream diagnostics: %+v", record.Fail)
	}
}

func TestFailFromErrorsSanitizesConstructedUpstreamDiagnostic(t *testing.T) {
	negative := int64(-1)
	got := failFromErrors(&failurecontract.Failure{
		Kind: failurecontract.InvalidRequest, HTTPStatus: http.StatusBadRequest,
		ErrorDiagnostic: failurecontract.UpstreamErrorDiagnostic{
			Param: "private-customer-field", ReportedMaxLength: &negative,
			ReportedActualLength: &negative, LengthUnit: "private-unit",
		},
	})
	if got.UpstreamErrorParam != "" || got.UpstreamReportedMaxLength != nil ||
		got.UpstreamReportedActualLength != nil || got.UpstreamLengthUnit != "" {
		t.Fatalf("uncontrolled metadata entered usage: %+v", got)
	}
	empty := failFromErrors(&failurecontract.Failure{HTTPStatus: http.StatusBadRequest})
	if empty.UpstreamErrorParam != "" || empty.UpstreamReportedMaxLength != nil ||
		empty.UpstreamReportedActualLength != nil || empty.UpstreamLengthUnit != "" {
		t.Fatal("absent diagnostic fields were invented")
	}
}
