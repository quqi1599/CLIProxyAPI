package contentaudit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReviewFailureKeepsCausePrivateAndDiagnosticsImmutable(t *testing.T) {
	failure := &ReviewFailure{Code: "review_transport_error", Cause: fmt.Errorf("sensitive-upstream-data: %w", context.Canceled), StageLatenciesMS: map[string]int64{"transport": 5}}
	if !errors.Is(failure, context.Canceled) || strings.Contains(failure.Error(), "sensitive") {
		t.Fatal("cause lost or leaked")
	}
	stages := failure.AuditReviewStageLatenciesMS()
	stages["transport"] = 99
	if failure.AuditReviewStageLatenciesMS()["transport"] != 5 {
		t.Fatal("diagnostics alias failure")
	}
	failure.Code = "invalid secret text"
	if failure.AuditReviewFailureCode() != "review_error" || strings.Contains(failure.Error(), "secret") {
		t.Fatal("invalid failure code leaked")
	}
}
