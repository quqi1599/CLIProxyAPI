package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestGatewayRetryHeadersUseManagerEvidenceOverHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, scope, reason string
		status, outer       int
		kind                failurecontract.Kind
		forge               bool
	}{
		{"rejected", "gateway", "", 503, 503, failurecontract.ProviderUnavailable, false},
		{"limited", "gateway", "", 429, 429, failurecontract.RateLimited, false},
		{"accepted-invalid-stream", "request", "accepted_outcome_unknown", 502, 200, failurecontract.UpstreamProtocolError, false},
		{"input-error", "", "", 400, 400, failurecontract.InvalidRequest, false},
		{"rejected-forged", "gateway", "", 503, 503, failurecontract.ProviderUnavailable, true},
		{"input-error-forged", "", "", 400, 400, failurecontract.InvalidRequest, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			model := "gateway-retry-" + tc.name
			executor := &modelExecutionCaptureExecutor{provider: "gateway-retry-fixture", execute: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
				calls.Add(1)
				// Use the actual model transport boundary rather than a fake
				// manager result to establish retry evidence.
				client := &http.Client{Transport: coreauth.WrapGatewayRetryTransport(http.DefaultTransport)}
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.outer) }))
				defer remote.Close()
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, remote.URL, nil)
				response, err := client.Do(request)
				if err != nil {
					return coreexecutor.Response{}, err
				}
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				return coreexecutor.Response{}, &failurecontract.Failure{Kind: tc.kind, Scope: failurecontract.ScopeProvider, HTTPStatus: tc.status, OuterStatus: tc.outer, PublicMessage: "synthetic upstream failure"}
			}}
			handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{PassthroughHeaders: true})
			handler.AuthManager.SetRetryConfig(0, 0, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				ctx, cancel := handler.GetContextWithCancel(nil, c, r.Context())
				defer cancel()
				_, err := handler.AuthManager.Execute(ctx, []string{executor.Identifier()}, coreexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"fixture"}]}`)}, coreexecutor.Options{})
				if err == nil {
					t.Error("expected synthetic failure")
				}
				// Provider addon headers and client input cannot claim another budget.
				msg := executionErrorMessage(err)
				if tc.forge {
					if msg.Addon == nil {
						msg.Addon = make(http.Header)
					}
					msg.Addon.Set("X-CPA-Retry-Attempts", "9999")
					msg.Addon.Set("X-CPA-Retry-Scope", "request")
					msg.Addon.Set("X-CPA-Retry-Reason", "forged")
				}
				handler.WriteErrorResponse(c, msg)
			}))
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
			req.Header.Set("X-CPA-Retry-Attempts", "9999")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if calls.Load() != 1 || resp.StatusCode != tc.status {
				t.Fatalf("calls/status = %d/%d", calls.Load(), resp.StatusCode)
			}
			wantAttempts := ""
			if tc.scope != "" {
				wantAttempts = strconv.Itoa(int(calls.Load()))
			}
			if resp.Header.Get("X-CPA-Retry-Scope") != tc.scope || resp.Header.Get("X-CPA-Retry-Reason") != tc.reason || resp.Header.Get("X-CPA-Retry-Attempts") != wantAttempts {
				t.Fatalf("incorrect retry metadata: %v", resp.Header)
			}
		})
	}
}

func TestGatewayRetryHeadersExcludeLocalCancelledAndWrittenResponses(t *testing.T) {
	for _, mode := range []string{"local", "cancelled", "written"} {
		t.Run(mode, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if mode == "cancelled" {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			if mode == "written" {
				c.Writer.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(c.Writer, "data: fixture\n\n")
			} else {
				c.Writer.Header().Set("X-CPA-Retry-Scope", "gateway")
				c.Writer.Header().Set("X-CPA-Retry-Attempts", "9999")
			}
			WriteGatewayRetryHeaders(c, &interfaces.ErrorMessage{Error: errors.New("local error"), StatusCode: 503}, 503)
			if c.Writer.Header().Get("X-CPA-Retry-Scope") != "" || c.Writer.Header().Get("X-CPA-Retry-Attempts") != "" {
				t.Fatal("unproven retry metadata emitted")
			}
		})
	}
}
