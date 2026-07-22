package executor

import (
	"net/http"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestOpenAICompatMalformedSSEEventErrorIsSecretSafeAndTyped(t *testing.T) {
	const secret = "secret-upstream-event"
	err := openAICompatMalformedSSEEventError([]byte("{\"token\":\"" + secret + "\""))
	if strings.Contains(err.Error(), secret) {
		t.Fatal("malformed SSE error exposed upstream event content")
	}
	if !strings.Contains(err.Error(), "category=malformed_sse_event") || !strings.Contains(err.Error(), "sha256=") {
		t.Fatalf("malformed SSE error lacks stable metadata: %v", err)
	}
	typed, ok := failurecontract.As(err)
	if !ok {
		t.Fatalf("malformed SSE error is not typed: %T", err)
	}
	if typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider || typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != openAICompatMalformedSSEEventCode {
		t.Fatalf("unexpected failure classification: %+v", typed)
	}
}
