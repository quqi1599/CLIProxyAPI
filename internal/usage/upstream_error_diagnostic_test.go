package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func TestFailureMetadataLoggerUpstreamErrorDiagnosticReadback(t *testing.T) {
	maximum, actual, invalid := int64(64), int64(85), int64(-1)
	for _, tc := range []struct {
		name, param, wantParam string
		maximum, actual        *int64
		unit                   string
		canonical              bool
		wantLengths            bool
	}{
		{name: "reported_length", param: "input[42].call_id", wantParam: "input[].call_id", maximum: &maximum, actual: &actual, canonical: true, wantLengths: true},
		{name: "custom_schema_parent", param: "tools[2].function.parameters.private_customer_property", wantParam: "tools[].function.parameters", canonical: true},
		{name: "missing_diagnostic", canonical: true},
		{name: "untrusted_fields", param: "private-field.authorization", maximum: &invalid, actual: &invalid, unit: "private-unit", canonical: true},
		{name: "noncanonical_ignored", param: "input[42].call_id", maximum: &maximum, actual: &actual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.StandardLogger()
			oldOutput, oldFormatter, oldLevel := logger.Out, logger.Formatter, logger.Level
			log.SetOutput(&buf)
			log.SetFormatter(&log.JSONFormatter{})
			log.SetLevel(log.WarnLevel)
			t.Cleanup(func() {
				log.SetOutput(oldOutput)
				log.SetFormatter(oldFormatter)
				log.SetLevel(oldLevel)
			})
			ctx := internallogging.WithRequestID(context.Background(), "length-fixture")
			(&FailureMetadataLogger{}).HandleUsage(ctx, coreusage.Record{
				Model: "gpt-fixture", Failed: true, ProviderStatusCode: http.StatusOK,
				ErrorCode: "string_above_max_length",
				Fail: coreusage.Failure{
					Canonical: tc.canonical, StatusCode: http.StatusBadRequest, OuterStatus: http.StatusOK,
					SemanticCode: "string_above_max_length", SemanticType: "invalid_request_error",
					Kind: "invalid_request", Scope: "request", OutputCommitted: true, StreamPhase: "after_output",
					Body:               "private-upstream-prose private-prompt sk-private-fixture",
					UpstreamErrorParam: tc.param, UpstreamReportedMaxLength: tc.maximum,
					UpstreamReportedActualLength: tc.actual, UpstreamLengthUnit: tc.unit,
				},
			})
			var stored map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &stored); err != nil {
				t.Fatal(err, buf.String())
			}
			requireJSONField(t, stored, "event", "failure_metadata")
			requireJSONField(t, stored, "request_id", "length-fixture")
			requireJSONField(t, stored, "upstream_error_code", "string_above_max_length")
			if tc.canonical {
				requireJSONNumberField(t, stored, "normalized_status", 400)
				requireJSONNumberField(t, stored, "outer_status", 200)
				requireJSONField(t, stored, "failure_scope", "request")
				requireJSONBoolField(t, stored, "retryable", false)
				requireJSONBoolField(t, stored, "output_committed", true)
			}
			if tc.wantParam == "" {
				requireMissingJSONField(t, stored, "upstream_error_param")
			} else {
				requireJSONField(t, stored, "upstream_error_param", tc.wantParam)
			}
			if tc.wantLengths {
				requireJSONNumberField(t, stored, "upstream_reported_max_length", 64)
				requireJSONNumberField(t, stored, "upstream_reported_actual_length", 85)
			} else {
				requireMissingJSONField(t, stored, "upstream_reported_max_length")
				requireMissingJSONField(t, stored, "upstream_reported_actual_length")
			}
			requireMissingJSONField(t, stored, "upstream_length_unit")
			for _, secret := range []string{"private-upstream-prose", "private-prompt", "sk-private-fixture", "private_customer_property", "private-field", "private-unit", "input[42]"} {
				if strings.Contains(buf.String(), secret) {
					t.Fatalf("failure metadata retained private or uncontrolled diagnostic text: %q", secret)
				}
			}
		})
	}
}

func TestFailureMetadataLoggerDoesNotEmitDiagnosticsForSuccess(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	(&FailureMetadataLogger{}).HandleUsage(context.Background(), coreusage.Record{
		Failed: false, Fail: coreusage.Failure{Canonical: true, UpstreamErrorParam: "input[].call_id"},
	})
	if buf.Len() != 0 {
		t.Fatal("successful request emitted a failure diagnostic")
	}
}

func TestFailureMetadataLoggerProductionFormatRetainsLengthDiagnostics(t *testing.T) {
	var buf bytes.Buffer
	logger := log.StandardLogger()
	oldOutput, oldFormatter, oldLevel := logger.Out, logger.Formatter, logger.Level
	log.SetOutput(&buf)
	log.SetFormatter(&internallogging.LogFormatter{})
	log.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFormatter(oldFormatter)
		log.SetLevel(oldLevel)
	})
	maximum, actual := int64(64), int64(85)
	for _, unit := range []string{"", "characters"} {
		buf.Reset()
		(&FailureMetadataLogger{}).HandleUsage(context.Background(), coreusage.Record{
			Model: "gpt-fixture", Failed: true, ErrorCode: "string_above_max_length",
			Fail: coreusage.Failure{
				Canonical: true, StatusCode: http.StatusBadRequest,
				Kind: "invalid_request", Scope: "request", SemanticCode: "string_above_max_length",
				Body: "private-upstream-prose", UpstreamErrorParam: "input[42].call_id",
				UpstreamReportedMaxLength: &maximum, UpstreamReportedActualLength: &actual,
				UpstreamLengthUnit: unit,
			},
		})
		for _, field := range []string{"upstream_error_param=input[].call_id", "upstream_reported_max_length=64", "upstream_reported_actual_length=85"} {
			if !strings.Contains(buf.String(), field) {
				t.Fatalf("production formatter dropped %s: %s", field, buf.String())
			}
		}
		if strings.Contains(buf.String(), "upstream_length_unit=") != (unit != "") {
			t.Fatalf("production formatter changed unit presence: %s", buf.String())
		}
		if strings.Contains(buf.String(), "private-upstream-prose") || strings.Contains(buf.String(), "input[42]") {
			t.Fatalf("production formatter leaked uncontrolled diagnostic data")
		}
	}
}
