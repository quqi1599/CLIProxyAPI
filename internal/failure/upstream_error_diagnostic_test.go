package failure

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUpstreamErrorDiagnosticParameterVocabulary(t *testing.T) {
	for _, tc := range []struct{ param, want string }{
		{"input[2].call_id", "input[].call_id"},
		{"messages[100].tool_calls[3].id", "messages[].tool_calls[].id"},
		{"tools[4].function.parameters.properties.private_customer", "tools[].function.parameters"},
		{"metadata.private_customer", "metadata"},
		{"instructions", "instructions"},
		{"private_customer", ""},
		{"input[private_customer].call_id", ""},
		{"input[9999999].call_id", ""},
		{"input[1].call_id\nprivate_customer", ""},
		{"https://private.invalid", ""},
		{strings.Repeat("input.", 60), ""},
	} {
		t.Run(tc.param, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"code": "string_above_max_length", "param": tc.param})
			diagnostic := ParseUpstreamErrorDiagnostic(raw)
			if diagnostic.Param != tc.want || diagnostic.ReportedMaxLength != nil || diagnostic.ReportedActualLength != nil || diagnostic.LengthUnit != "" {
				t.Fatalf("unexpected diagnostic: %+v", diagnostic)
			}
			if normalized := diagnostic.Normalized(); normalized.Param != tc.want {
				t.Fatalf("normalization changed a canonical path: %+v", normalized)
			}
		})
	}
}

func TestUpstreamErrorDiagnosticExactReportedLengthTemplate(t *testing.T) {
	const template = "Expected a string with maximum length 1024, but got a string with length 1025 instead."
	for _, prefix := range []string{"", "Invalid 'input[2].call_id': string too long. "} {
		raw, _ := json.Marshal(map[string]any{
			"code": "string_above_max_length", "param": "input[2].call_id", "message": prefix + template,
		})
		got := ParseUpstreamErrorDiagnostic(raw)
		if got.Param != "input[].call_id" || got.ReportedMaxLength == nil || *got.ReportedMaxLength != 1024 ||
			got.ReportedActualLength == nil || *got.ReportedActualLength != 1025 || got.LengthUnit != "" {
			t.Fatalf("reported length or unknown unit changed: %+v", got)
		}
	}
	for _, message := range []string{
		"private request: " + template,
		template + " private value",
		"Invalid 'input[1].call_id': string too long. " + template,
		"maximum length 1024; actual 1025",
		"Expected a string with maximum length -1, but got a string with length 1025 instead.",
		"Expected a string with maximum length 1024, but got a string with length 1 instead.",
	} {
		raw, _ := json.Marshal(map[string]any{"code": "string_above_max_length", "param": "input[2].call_id", "message": message})
		got := ParseUpstreamErrorDiagnostic(raw)
		if got.Param != "input[].call_id" || got.ReportedMaxLength != nil || got.ReportedActualLength != nil || got.LengthUnit != "" {
			t.Fatalf("unrecognized or contradictory template accepted: %+v", got)
		}
	}
	raw, _ := json.Marshal(map[string]any{"code": "invalid_request_error", "param": "input[2].call_id", "message": template})
	if got := ParseUpstreamErrorDiagnostic(raw); got.ReportedMaxLength != nil || got.ReportedActualLength != nil {
		t.Fatalf("lengths inferred without the explicit code: %+v", got)
	}
}

func TestUpstreamErrorDiagnosticRejectsAmbiguityAndBounds(t *testing.T) {
	for _, body := range []string{
		`{"param":"input[2].call_id","param":"instructions"}`,
		`{"param":"input[2].call_id","pa\u0072am":"instructions"}`,
		`{"param":"input[2].call_id","extension":{"same":1,"same":2}}`,
		`{"code":"string_above_max_length","param":"input[2].call_id","message":"Expected a string with maximum length 1000000001, but got a string with length 1000000002 instead."}`,
		`{"code":"string_above_max_length","param":"input[2].call_id","message":"Expected a string with maximum length 99999999999999999999999999999999, but got a string with length 99999999999999999999999999999999 instead."}`,
		`{"param":"input[2].call_id","extension":"` + strings.Repeat("x", 16<<10) + `"}`,
		`{"param":"input[2].call_id","extension":` + strings.Repeat("[", 9) + "0" + strings.Repeat("]", 9) + "}",
		`{"param":"input[2].call_id","extension":[` + strings.Repeat("0,", 256) + "0]}",
		`{"param":"input[2].call_id"} trailing`,
	} {
		if got := ParseUpstreamErrorDiagnostic([]byte(body)); got != (UpstreamErrorDiagnostic{}) {
			t.Fatalf("ambiguous or excessive input retained diagnostics: %+v", got)
		}
	}
	body := []byte(`{"error":{"code":"string_above_max_length","param":"input[2].call_id"},"error":{"code":"server_error"}}`)
	if got := ParseUpstreamErrorDiagnosticAt(body, "error"); got != (UpstreamErrorDiagnostic{}) {
		t.Fatalf("duplicated outer envelope retained diagnostics: %+v", got)
	}
}

func TestUpstreamErrorDiagnosticNormalizationDetachesPointers(t *testing.T) {
	limit, actual := int64(64), int64(85)
	source := UpstreamErrorDiagnostic{Param: "input[2].call_id", ReportedMaxLength: &limit, ReportedActualLength: &actual, LengthUnit: "private-unit"}
	got := source.Normalized()
	limit, actual = 1, 2
	if got.Param != "input[].call_id" || *got.ReportedMaxLength != 64 || *got.ReportedActualLength != 85 || got.LengthUnit != "" {
		t.Fatalf("diagnostic was not safely detached: %+v", got)
	}
	typed := &Failure{ErrorDiagnostic: got}
	clone := Classify(typed)
	*got.ReportedMaxLength = 3
	if *clone.ErrorDiagnostic.ReportedMaxLength != 64 {
		t.Fatal("Classify retained mutable length pointers")
	}
	for _, value := range []int64{-1, maxReportedStringLength + 1} {
		if normalized := (UpstreamErrorDiagnostic{Param: "input[].call_id", ReportedMaxLength: &value}).Normalized(); normalized != (UpstreamErrorDiagnostic{}) {
			t.Fatalf("invalid numeric diagnostic retained: %+v", normalized)
		}
	}
}
