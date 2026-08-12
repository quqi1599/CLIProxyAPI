package executor

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexExecutorHTTPDoFailuresAreCanonical(t *testing.T) {
	tests := []struct {
		name       string
		stream     bool
		cause      error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "proxy failure before non-stream response",
			cause:      errors.New("proxy CONNECT failed"),
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_transport_error",
		},
		{
			name:       "TLS handshake failure before non-stream response",
			cause:      tls.RecordHeaderError{Msg: "unexpected TLS record"},
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_transport_error",
		},
		{
			name:       "DNS timeout before stream response",
			stream:     true,
			cause:      &net.DNSError{Err: "i/o timeout", Name: "chatgpt.com", IsTimeout: true},
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "upstream_timeout",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", codexRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.cause
			}))
			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test"}}
			request := cliproxyexecutor.Request{
				Model:   "gpt-5.6-sol",
				Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
			}
			options := cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Stream:       tc.stream,
			}

			var err error
			if tc.stream {
				_, err = executor.ExecuteStream(ctx, auth, request, options)
			} else {
				_, err = executor.Execute(ctx, auth, request, options)
			}
			failure, ok := failurecontract.As(err)
			if !ok {
				t.Fatalf("error = %T %v, want canonical failure", err, err)
			}
			if failure.Kind != failurecontract.TransportError || failure.Scope != failurecontract.ScopeProvider ||
				failure.HTTPStatus != tc.wantStatus || failure.OuterStatus != tc.wantStatus ||
				failure.SemanticCode != tc.wantCode || !failure.Retryable ||
				failure.StreamPhase != failurecontract.StreamPhaseBeforeOutput || failure.OutputCommitted ||
				!errors.Is(err, tc.cause) {
				t.Fatalf("failure = %+v", failure)
			}
		})
	}
}

func TestCodexExecutorRequestBuildErrorIsNotPromotedToRetryableTransport(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "test",
		"base_url": "http://[invalid",
	}}, cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err == nil {
		t.Fatal("Execute() error = nil, want request construction failure")
	}
	if failure, ok := failurecontract.As(err); ok && failure.Retryable {
		t.Fatalf("request construction failure was promoted to retryable transport: %+v", failure)
	}
}

func TestCodexImageTransientHTTPDoFailurePreservesOriginalCause(t *testing.T) {
	cause := io.ErrUnexpectedEOF
	mapped := codexOpenAIImageStreamStatusErr(cause)
	if !errors.Is(mapped, cause) {
		t.Fatalf("mapped error %T %v lost original cause", mapped, mapped)
	}
	err := newCodexHTTPDoStatusErr(context.Background(), mapped, false)
	failure, ok := failurecontract.As(err)
	if !ok || failure.HTTPStatus != http.StatusGatewayTimeout || failure.SemanticCode != "upstream_timeout" || !failure.Retryable {
		t.Fatalf("canonical image transport failure = %+v", failure)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("canonical error %T %v lost original cause", err, err)
	}
}
