package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexExecutionRetainsAmplificationGuard(t *testing.T) {
	// The Anthropic object becomes a JSON string in Responses arguments. Native
	// Responses capability must not override the actual executor's size guard.
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"test","input":{"values":[` + strings.Repeat(`"x",`, 150000) + `"x"]}}]}]}`)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	auth := &coreauth.Auth{ID: "synthetic-codex", Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "synthetic-only", "native_responses": "true"}}
	executor := NewCodexExecutor(&config.Config{})
	req := coreexecutor.Request{Model: "gpt-5.6-sol", Payload: body}
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body, Metadata: map[string]any{coreexecutor.ClientProfileMetadataKey: "workbuddy"}}
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			ctx := internalpayload.WithAmplificationMode(context.Background(), internalpayload.AmplificationModeEnforce)
			var err error
			if stream {
				_, err = executor.ExecuteStream(ctx, auth, req, opts)
			} else {
				_, err = executor.Execute(ctx, auth, req, opts)
			}
			failure, ok := failurecontract.As(err)
			if !ok || failure.HTTPStatus != http.StatusBadRequest || failure.ErrorCode() != "request_transform_expansion_exceeded" || hits.Load() != 0 {
				t.Fatalf("actual Codex transform guard bypassed: error=%v upstream_hits=%d", err, hits.Load())
			}
		})
	}
}
