package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostModelExecuteStreamRequiresCurrentCallback(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		callbackID string
	}{
		{name: "empty"},
		{name: "unknown", callbackID: "missing-callback"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := New()
			called := false
			host.SetModelExecutor(&fakeHostModelExecutor{
				executeModelStream: func(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
					called = true
					return handlers.ModelExecutionStream{Chunks: make(chan handlers.ModelExecutionChunk)}, nil
				},
			})
			rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
				HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{Stream: true},
				HostCallbackID:            testCase.callbackID,
			})
			if errMarshal != nil {
				t.Fatalf("marshal request: %v", errMarshal)
			}
			_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
			if errCall == nil || !strings.Contains(errCall.Error(), "requires a registered host callback id") {
				t.Fatalf("callFromPlugin() error = %v, want registered callback error", errCall)
			}
			if called {
				t.Fatal("model executor was called before callback validation")
			}
			if got := hostModelStreamCountForTest(t, host); got != 0 {
				t.Fatalf("model stream count = %d, want 0", got)
			}
		})
	}
}

func TestHostModelExecuteStreamCleanupRegistrationFailureClosesStream(t *testing.T) {
	host := New()
	callbackID, closeCallback := host.openCallbackContext(context.Background())
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			ctxSeen <- ctx
			closeCallback()
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Chunks:     make(chan handlers.ModelExecutionChunk),
			}, nil
		},
	})
	rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{Stream: true},
		HostCallbackID:            callbackID,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "callback is no longer registered") {
		t.Fatalf("callFromPlugin() error = %v, want callback cleanup error", errCall)
	}
	streamCtx := <-ctxSeen
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after cleanup registration failed")
	}
	if got := hostModelStreamCountForTest(t, host); got != 0 {
		t.Fatalf("model stream count = %d, want 0", got)
	}
}

func TestHostModelExecuteStreamOpenFailureCancelsStream(t *testing.T) {
	host := New()
	callbackID, closeCallback := host.openCallbackContext(context.Background())
	defer closeCallback()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			ctxSeen <- ctx
			return handlers.ModelExecutionStream{StatusCode: http.StatusOK}, nil
		},
	})
	rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{Stream: true},
		HostCallbackID:            callbackID,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall == nil || !strings.Contains(errCall.Error(), errModelStreamBridgeUnavailable.Error()) {
		t.Fatalf("callFromPlugin() error = %v, want stream bridge unavailable error", errCall)
	}
	streamCtx := <-ctxSeen
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after open failed")
	}
	if got := hostModelStreamCountForTest(t, host); got != 0 {
		t.Fatalf("model stream count = %d, want 0", got)
	}
}

func TestHostModelExecuteStreamDetachesFromCallbackParentCancel(t *testing.T) {
	host := New()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModelStream: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			ctxSeen <- ctx
			return handlers.ModelExecutionStream{
				StatusCode: http.StatusOK,
				Chunks:     make(chan handlers.ModelExecutionChunk),
			}, nil
		},
	})
	parentCtx, cancelParent := context.WithCancel(context.Background())
	callbackID, closeCallback := host.openCallbackContext(parentCtx)
	defer closeCallback()

	rawReq, errMarshal := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
			Body:          []byte(`{"stream":true}`),
		},
		HostCallbackID: callbackID,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}

	var streamCtx context.Context
	select {
	case streamCtx = <-ctxSeen:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	cancelParent()
	select {
	case <-streamCtx.Done():
		t.Fatal("stream context was canceled by callback parent context")
	default:
	}

	closeCallback()
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after callback scope closed")
	}
	if got := hostModelStreamCountForTest(t, host); got != 0 {
		t.Fatalf("model stream count = %d, want 0", got)
	}
}
