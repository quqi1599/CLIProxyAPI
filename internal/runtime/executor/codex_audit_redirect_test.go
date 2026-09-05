package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexAuditReviewRejectsRedirectWithoutChangingSharedClient(t *testing.T) {
	for _, redirectStatus := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(redirectStatus), func(t *testing.T) {
			var sourceCalls, targetCalls atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || string(body) != `{"synthetic":"review"}` {
					t.Error("normal redirect did not replay the expected synthetic request")
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sourceCalls.Add(1)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(redirectStatus)
			}))
			defer source.Close()
			cfg := &config.Config{}
			executor := NewCodexExecutor(cfg)
			sharedClient := helps.NewUtlsHTTPClient(t.Context(), cfg, nil, 0)
			if sharedClient.CheckRedirect != nil {
				t.Fatal("fixture expects default shared redirect behavior")
			}
			dispatch := func(ctx context.Context) int {
				t.Helper()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, source.URL, strings.NewReader(`{"synthetic":"review"}`))
				if err != nil {
					t.Fatal(err)
				}
				response, err := executor.HttpRequest(ctx, nil, request)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if errClose := response.Body.Close(); errClose != nil {
						t.Error(errClose)
					}
				}()
				return response.StatusCode
			}
			auditCtx, _ := coreexecutor.WithContentAuditReviewTrace(t.Context())
			if status := dispatch(auditCtx); status != redirectStatus {
				t.Fatalf("audit status = %d, want original redirect", status)
			}
			if sourceCalls.Load() != 1 || targetCalls.Load() != 0 {
				t.Fatalf("audit dispatched source=%d target=%d, want exactly one source and zero target", sourceCalls.Load(), targetCalls.Load())
			}
			if sharedAfter := helps.NewUtlsHTTPClient(t.Context(), cfg, nil, 0); sharedAfter != sharedClient || sharedAfter.CheckRedirect != nil {
				t.Fatal("audit modified the shared client's redirect policy")
			}
			if status := dispatch(t.Context()); status != http.StatusOK || targetCalls.Load() != 1 || sourceCalls.Load() != 2 {
				t.Fatalf("normal redirect behavior changed: status=%d source=%d target=%d", status, sourceCalls.Load(), targetCalls.Load())
			}
		})
	}
}
