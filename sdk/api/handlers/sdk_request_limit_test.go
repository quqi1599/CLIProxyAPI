package handlers

import (
	"context"
	"net/http"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSDKRawJSONEntryPointsRejectOverEmergencyCeilingBeforeClone(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, nil)
	oversized := make([]byte, int(maxDecodedRequestBodyBytes+1))
	before := internalpayload.CurrentLargeCloneMetrics()
	assertRejected := func(name string, errMsg *interfaces.ErrorMessage) {
		t.Helper()
		if errMsg == nil || errMsg.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s error = %+v, want HTTP 413", name, errMsg)
		}
		typed, ok := failurecontract.As(errMsg.Error)
		if !ok || typed.Kind != failurecontract.RequestTooLarge || typed.Scope != failurecontract.ScopeRequest || typed.HTTPStatus != http.StatusRequestEntityTooLarge || typed.ProviderCode != "request_too_large" {
			t.Fatalf("%s typed error = %#v, want request-scoped request_too_large", name, typed)
		}
	}
	readStreamError := func(name string, errChan <-chan *interfaces.ErrorMessage) *interfaces.ErrorMessage {
		t.Helper()
		if errChan == nil {
			t.Fatalf("%s error channel is nil", name)
		}
		errMsg, ok := <-errChan
		if !ok {
			t.Fatalf("%s error channel closed without an error", name)
		}
		return errMsg
	}

	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "oversized", oversized, "")
	assertRejected("execute", errMsg)
	_, _, errMsg = handler.ExecuteImageWithAuthManager(context.Background(), "openai", "oversized", oversized, "")
	assertRejected("image execute", errMsg)
	_, _, errMsg = handler.ExecuteCountWithAuthManager(context.Background(), "openai", "oversized", oversized, "")
	assertRejected("count", errMsg)
	_, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "oversized", oversized, "")
	assertRejected("stream", readStreamError("stream", errChan))
	_, _, errChan = handler.ExecuteImageStreamWithAuthManager(context.Background(), "openai", "oversized", oversized, "")
	assertRejected("image stream", readStreamError("image stream", errChan))

	websocketContext := coreexecutor.WithDownstreamWebsocket(context.Background())
	_, errMsg = handler.ExecuteModel(websocketContext, ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "claude",
		Model:         "oversized",
		Body:          oversized,
	})
	assertRejected("plugin model execute", errMsg)
	_, errMsg = handler.ExecuteModelStream(websocketContext, ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "claude",
		Model:         "oversized",
		Stream:        true,
		Body:          oversized,
	})
	assertRejected("plugin model stream", errMsg)

	after := internalpayload.CurrentLargeCloneMetrics()
	if after.Count != before.Count || after.Bytes != before.Bytes {
		t.Fatalf("rejected SDK input was cloned: before=%+v after=%+v", before, after)
	}
}

func TestSDKRawJSONEmergencyCeilingAcceptsExactLimit(t *testing.T) {
	body := make([]byte, int(maxDecodedRequestBodyBytes))
	var handler *BaseAPIHandler
	_, release, err := handler.inspectAndAcquireAdmission(context.Background(), body, &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("exact-limit body rejected: %v", err)
	}
	release()
}

func TestDownstreamWebsocketKeepsBoundedTranscriptReplayBudget(t *testing.T) {
	ctx := coreexecutor.WithDownstreamWebsocket(context.Background())
	var handler *BaseAPIHandler
	body := make([]byte, int(downstreamWebsocketBodyBytes+1))
	accepted := body[:downstreamWebsocketBodyBytes]
	_, release, err := handler.inspectAndAcquireAdmission(ctx, accepted, &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("bounded websocket replay rejected: %v", err)
	}
	release()

	_, _, err = handler.inspectAndAcquireAdmission(ctx, body, &modelExecutionOptions{})
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.RequestTooLarge || typed.Scope != failurecontract.ScopeRequest || typed.HTTPStatus != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized websocket replay error = %#v, want request-scoped HTTP 413", typed)
	}
}
