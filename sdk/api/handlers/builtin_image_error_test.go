package handlers

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type rejectedImageToolExecutor struct {
	requestDetailsProviderExecutor
	calls int
}

func (e *rejectedImageToolExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	return coreexecutor.Response{}, nil
}

func (e *rejectedImageToolExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls++
	return &coreexecutor.StreamResult{}, nil
}

func TestBuiltinImageToolRejectionKeepsTypedActionableErrorWithoutDispatch(t *testing.T) {
	const model = "glm-image-tool-test"
	registry.GetGlobalRegistry().RegisterClient(model, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(model) })
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &rejectedImageToolExecutor{requestDetailsProviderExecutor: requestDetailsProviderExecutor{id: "claude"}}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(t.Context(), &coreauth.Auth{ID: model, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	body := []byte(`{"model":"glm-image-tool-test","input":"draw","tools":[{"type":"image_generation"}]}`)
	original := bytes.Clone(body)
	for _, stream := range []bool{false, true} {
		var msg *interfaces.ErrorMessage
		if stream {
			_, _, failures := handler.ExecuteStreamWithAuthManager(t.Context(), "openai-response", model, body, "")
			msg = <-failures
		} else {
			_, _, msg = handler.ExecuteWithAuthManager(t.Context(), "openai-response", model, body, "")
		}
		if msg == nil || msg.StatusCode != http.StatusBadRequest {
			t.Fatalf("error = %#v", msg)
		}
		failure, ok := failurecontract.As(msg.Error)
		if !ok || failure.Kind != failurecontract.UnsupportedFeature || failure.Scope != failurecontract.ScopeRequest || failure.Retryable {
			t.Fatalf("failure = %#v", failure)
		}
		response := BuildErrorResponseBodyWithCause(msg.StatusCode, msg.Error.Error(), msg.Error)
		if gjson.GetBytes(response, "error.code").String() != "request_feature_unsupported" || !strings.Contains(gjson.GetBytes(response, "error.message").String(), "image_generation") {
			t.Fatalf("response = %s", response)
		}
		if !bytes.Equal(body, original) || executor.calls != 0 {
			t.Fatalf("request mutated or dispatched: calls=%d", executor.calls)
		}
	}
}
