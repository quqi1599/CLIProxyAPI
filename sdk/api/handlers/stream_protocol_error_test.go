package handlers

import (
	"net/http"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestValidateSSEDataJSONReturnsSecretSafeTypedFailure(t *testing.T) {
	const secret = "secret-stream-token"
	err := validateSSEDataJSON([]byte("data: {\"token\":\"" + secret + "\""))
	if err == nil {
		t.Fatal("validateSSEDataJSON() error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("validateSSEDataJSON() exposed upstream payload content")
	}
	if !strings.Contains(err.Error(), "category=malformed_sse_data_json") || !strings.Contains(err.Error(), "sha256=") {
		t.Fatalf("validateSSEDataJSON() error lacks stable metadata: %v", err)
	}
	typed, ok := failurecontract.As(err)
	if !ok {
		t.Fatalf("validateSSEDataJSON() error is not typed: %T", err)
	}
	if typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider || typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != invalidSSEDataJSONCode {
		t.Fatalf("unexpected failure classification: %+v", typed)
	}
}
