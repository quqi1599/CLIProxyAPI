package handlers

import (
	"crypto/sha256"
	"fmt"
	"net/http"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const invalidSSEDataJSONCode = "upstream_sse_invalid_json"

func invalidSSEDataJSONError(data []byte) error {
	digest := sha256.Sum256(data)
	return &failurecontract.Failure{
		Kind:          failurecontract.UpstreamProtocolError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		ProviderCode:  invalidSSEDataJSONCode,
		PublicMessage: fmt.Sprintf("upstream protocol error: category=malformed_sse_data_json length=%d sha256=%x", len(data), digest),
	}
}
