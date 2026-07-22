package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (h *Host) callHostModelExecuteStream(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostModelExecutionRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model execution stream request: %w", errUnmarshal)
	}
	if !req.Stream {
		return nil, fmt.Errorf("host.model.execute_stream requires stream=true")
	}
	executor := h.currentModelExecutor()
	if executor == nil {
		return nil, fmt.Errorf("host model executor is unavailable")
	}
	if req.HostCallbackID == "" || h == nil || h.callbackContexts == nil {
		return nil, fmt.Errorf("host.model.execute_stream requires a registered host callback id")
	}
	h.callbackContexts.mu.RLock()
	callbackEntry, callbackExists := h.callbackContexts.contexts[req.HostCallbackID]
	h.callbackContexts.mu.RUnlock()
	if !callbackExists || callbackEntry.ctx == nil {
		return nil, fmt.Errorf("host.model.execute_stream requires a registered host callback id")
	}
	skipPluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	// Detach request cancellation while preserving callback values; callback cleanup owns the model stream lifetime.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(callbackEntry.ctx))
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	stream, errMsg := executor.ExecuteModelStream(streamCtx, modelExecutionRequestFromPlugin(req.HostModelExecutionRequest, skipPluginID))
	if errMsg != nil {
		return nil, modelExecutionError(errMsg)
	}
	streamID := ""
	var errOpen error
	if h.modelStreams != nil {
		streamID, errOpen = h.modelStreams.open(req.HostCallbackID, stream.Chunks, cancel)
	}
	if errOpen != nil {
		return nil, errOpen
	}
	if streamID == "" {
		return nil, errModelStreamBridgeUnavailable
	}
	result, errMarshal := marshalRPCResult(pluginapi.HostModelStreamResponse{
		StatusCode: stream.StatusCode,
		Headers:    cloneHeader(stream.Headers),
		StreamID:   streamID,
	})
	if errMarshal != nil {
		h.modelStreams.close(streamID)
		return nil, errMarshal
	}
	if !h.addCallbackCleanup(req.HostCallbackID, func() {
		h.modelStreams.close(streamID)
	}) {
		return nil, fmt.Errorf("host model stream callback is no longer registered")
	}
	cancel = nil
	return result, nil
}

func (h *Host) callHostModelStreamRead(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostModelStreamReadRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model stream read request: %w", errUnmarshal)
	}
	if h == nil || h.modelStreams == nil {
		return nil, fmt.Errorf("host model stream bridge is unavailable")
	}
	chunk, done, errRead := h.modelStreams.read(ctx, req.StreamID)
	if errRead != nil {
		return nil, errRead
	}
	resp := pluginapi.HostModelStreamReadResponse{
		Payload: append([]byte(nil), chunk.Payload...),
		Done:    done,
	}
	if chunk.Err != nil {
		resp.Error = chunk.Err.Error()
		resp.Done = true
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostModelStreamClose(request []byte) ([]byte, error) {
	var req pluginapi.HostModelStreamCloseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host model stream close request: %w", errUnmarshal)
	}
	if h != nil && h.modelStreams != nil {
		h.modelStreams.close(req.StreamID)
	}
	return marshalRPCResult(rpcEmptyResponse{})
}
