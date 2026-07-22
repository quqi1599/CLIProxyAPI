package helps

import (
	"bytes"
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ProviderExchange owns the shared HTTP request, transport, and response
// logging steps for one provider attempt.
type ProviderExchange struct {
	Config   *config.Config
	Auth     *cliproxyauth.Auth
	Provider string
	Reporter *UsageReporter
}

// ProviderExchangeRequest describes one provider HTTP request. ApplyHeaders is
// the provider-specific credential and header hook.
type ProviderExchangeRequest struct {
	Method       string
	URL          string
	Body         []byte
	ApplyHeaders func(*http.Request) error
}

// ProviderExchangeResponse keeps the upstream response and its cached logging
// state together. Call ReadBounded for buffered responses; streaming callers
// transfer HTTPResponse.Body ownership to the shared bounded SSE reader.
type ProviderExchangeResponse struct {
	HTTPResponse *http.Response
	ResponseLog  *APIResponseLogRuntime
}

// Do builds and sends one provider request without adding a client timeout.
func (exchange ProviderExchange) Do(ctx context.Context, request ProviderExchangeRequest) (*ProviderExchangeResponse, error) {
	httpRequest, errRequest := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if errRequest != nil {
		return nil, errRequest
	}
	if request.ApplyHeaders != nil {
		if errHeaders := request.ApplyHeaders(httpRequest); errHeaders != nil {
			return nil, errHeaders
		}
	}

	var authID, authLabel, authType, authValue string
	if exchange.Auth != nil {
		authID = exchange.Auth.ID
		authLabel = exchange.Auth.Label
		authType, authValue = exchange.Auth.AccountInfo()
	}
	RecordAPIRequest(ctx, exchange.Config, UpstreamRequestLog{
		URL:       request.URL,
		Method:    request.Method,
		Headers:   httpRequest.Header.Clone(),
		Body:      request.Body,
		Provider:  exchange.Provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := NewUtlsHTTPClient(ctx, exchange.Config, exchange.Auth, 0)
	if exchange.Reporter != nil {
		httpClient = exchange.Reporter.TrackHTTPClient(httpClient)
	}
	httpResponse, errDo := httpClient.Do(httpRequest)
	if errDo != nil {
		RecordAPIResponseError(ctx, exchange.Config, errDo)
		return nil, errDo
	}
	RecordAPIResponseMetadata(ctx, exchange.Config, httpResponse.StatusCode, httpResponse.Header.Clone())
	return &ProviderExchangeResponse{
		HTTPResponse: httpResponse,
		ResponseLog:  NewAPIResponseLogRuntime(ctx, exchange.Config),
	}, nil
}

// ReadBounded consumes and closes the response body through the shared upstream
// reader and records exactly one body or read-error entry.
func (response *ProviderExchangeResponse) ReadBounded(limits UpstreamBodyLimits) ([]byte, error) {
	if response == nil {
		return ReadBoundedUpstreamHTTPResponse(nil, limits)
	}
	data, errRead := ReadBoundedUpstreamHTTPResponse(response.HTTPResponse, limits)
	if errRead != nil {
		if response.ResponseLog != nil {
			response.ResponseLog.RecordError(errRead)
		}
		return data, errRead
	}
	if response.ResponseLog != nil {
		response.ResponseLog.AppendChunk(data)
	}
	return data, nil
}
