package executor

import (
	"crypto/sha256"
	"fmt"
	"net/http"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const openAICompatMalformedSSEEventCode = "upstream_sse_malformed_event"

func openAICompatMalformedSSEEventError(event []byte) error {
	digest := sha256.Sum256(event)
	return &failurecontract.Failure{
		Kind:          failurecontract.UpstreamProtocolError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		ProviderCode:  openAICompatMalformedSSEEventCode,
		PublicMessage: fmt.Sprintf("upstream protocol error: category=malformed_sse_event length=%d sha256=%x", len(event), digest),
	}
}
