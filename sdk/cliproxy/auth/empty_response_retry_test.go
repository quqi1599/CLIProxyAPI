package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestDeliverableOutputTrackerClassifiesChatAndResponses(t *testing.T) {
	tests := []struct {
		name          string
		format        sdktranslator.Format
		payloads      []string
		wantOutput    bool
		wantTool      bool
		wantReasoning bool
		wantTerminal  bool
		wantError     bool
	}{
		{
			name:   "chat terminal only",
			format: sdktranslator.FormatOpenAI,
			payloads: []string{
				`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n",
				`data: {"choices":[{"index":0,"delta":{"content":" \n "},"finish_reason":"stop"}]}` + "\n\n",
				"data: [DONE]\n\n",
			},
			wantTerminal: true,
		},
		{
			name:       "chat short text",
			format:     sdktranslator.FormatOpenAI,
			payloads:   []string{`{"choices":[{"index":0,"delta":{"content":"好"},"finish_reason":null}]}`},
			wantOutput: true,
		},
		{
			name:       "chat tool call",
			format:     sdktranslator.FormatOpenAI,
			payloads:   []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`},
			wantOutput: true,
			wantTool:   true,
		},
		{
			name:   "chat fragmented tool call",
			format: sdktranslator.FormatOpenAI,
			payloads: []string{
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"arguments":""}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{}"}}]},"finish_reason":null}]}`,
			},
			wantOutput: true,
			wantTool:   true,
		},
		{
			name:   "chat reasoning only",
			format: sdktranslator.FormatOpenAI,
			payloads: []string{
				`{"choices":[{"index":0,"delta":{"reasoning_content":"internal reasoning"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			wantReasoning: true,
			wantTerminal:  true,
		},
		{
			name:         "chat content filter is meaningful terminal output",
			format:       sdktranslator.FormatOpenAI,
			payloads:     []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`},
			wantOutput:   true,
			wantTerminal: true,
		},
		{
			name:   "responses terminal only",
			format: sdktranslator.FormatOpenAIResponse,
			payloads: []string{
				`event: response.created` + "\n",
				`data: {"type":"response.created","response":{"id":"resp-1"}}` + "\n\n",
				`data: {"type":"response.completed","response":{"id":"resp-1","output":[]}}` + "\n\n",
			},
			wantTerminal: true,
		},
		{
			name:   "responses terminal with null error",
			format: sdktranslator.FormatOpenAIResponse,
			payloads: []string{
				`{"type":"response.completed","error":null,"response":{"id":"resp-1","output":[]}}`,
			},
			wantTerminal: true,
		},
		{
			name:         "responses explicit error waits for bootstrap failure",
			format:       sdktranslator.FormatOpenAIResponse,
			payloads:     []string{`{"type":"response.failed","response":{"id":"resp-1","status":"failed","error":{"code":"content_filter"}}}`},
			wantTerminal: true,
			wantError:    true,
		},
		{
			name:   "responses tool call",
			format: sdktranslator.FormatOpenAIResponse,
			payloads: []string{
				`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":""}}`,
			},
			wantOutput: true,
			wantTool:   true,
		},
		{
			name:   "responses reasoning summary only",
			format: sdktranslator.FormatOpenAIResponse,
			payloads: []string{
				`{"type":"response.reasoning_summary_text.delta","delta":"Checking the result"}`,
			},
			wantReasoning: true,
		},
		{
			name:   "responses completed reasoning item only",
			format: sdktranslator.FormatOpenAIResponse,
			payloads: []string{
				`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"Checking"}],"content":[{"type":"reasoning_text","text":"internal"}]}]}}`,
			},
			wantReasoning: true,
			wantTerminal:  true,
		},
		{
			name:       "unknown well formed event is conservative",
			format:     sdktranslator.FormatOpenAIResponse,
			payloads:   []string{`{"type":"response.future_output.delta","value":"x"}`},
			wantOutput: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := newDeliverableOutputTracker(test.format)
			for _, payload := range test.payloads {
				tracker.Observe([]byte(payload))
			}
			tracker.Finish()
			if tracker.deliverable != test.wantOutput {
				t.Fatalf("deliverable = %v, want %v", tracker.deliverable, test.wantOutput)
			}
			if tracker.sawToolCall != test.wantTool {
				t.Fatalf("sawToolCall = %v, want %v", tracker.sawToolCall, test.wantTool)
			}
			if tracker.sawReasoning != test.wantReasoning {
				t.Fatalf("sawReasoning = %v, want %v", tracker.sawReasoning, test.wantReasoning)
			}
			if tracker.sawTerminal != test.wantTerminal {
				t.Fatalf("sawTerminal = %v, want %v", tracker.sawTerminal, test.wantTerminal)
			}
			if tracker.sawError != test.wantError {
				t.Fatalf("sawError = %v, want %v", tracker.sawError, test.wantError)
			}
		})
	}
}

func TestDeliverableOutputTrackerHandlesSplitSSEFields(t *testing.T) {
	tracker := newDeliverableOutputTracker(sdktranslator.FormatOpenAIResponse)
	tracker.Observe([]byte("event: response.completed"))
	tracker.Observe([]byte(`data: {"type":"response.completed","response":{"id":"resp-1","output":[]}}`))
	tracker.Finish()

	if tracker.deliverable {
		t.Fatal("split terminal-only event must not be deliverable")
	}
	if !tracker.sawTerminal {
		t.Fatal("split terminal-only event was not recognized as terminal")
	}
}

func TestReadStreamBootstrapWithDeliveryRejectsTerminalOnlyStream(t *testing.T) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 3)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
	close(chunks)

	tracker := newDeliverableOutputTracker(sdktranslator.FormatOpenAI)
	result := readStreamBootstrapWithDelivery(context.Background(), chunks, time.Now(), tracker, emptyResponsePolicy{
		enabled:   true,
		format:    sdktranslator.FormatOpenAI,
		maxBytes:  defaultEmptyResponseBufferBytes,
		maxEvents: defaultEmptyResponseBufferEvents,
	})
	if result.err != nil {
		t.Fatalf("bootstrap error = %v", result.err)
	}
	if !result.closed || !result.emptyResponse {
		t.Fatalf("bootstrap result = closed:%v empty:%v, want terminal empty response", result.closed, result.emptyResponse)
	}
	if len(result.buffered) != 3 {
		t.Fatalf("buffered chunks = %d, want 3", len(result.buffered))
	}
}

func TestReadStreamBootstrapWithDeliveryStopsOnCanceledClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	chunks := make(chan cliproxyexecutor.StreamChunk)
	cancel()

	result := readStreamBootstrapWithDelivery(ctx, chunks, time.Now(), newDeliverableOutputTracker(sdktranslator.FormatOpenAI), emptyResponsePolicy{
		enabled:   true,
		format:    sdktranslator.FormatOpenAI,
		maxBytes:  defaultEmptyResponseBufferBytes,
		maxEvents: defaultEmptyResponseBufferEvents,
	})
	if result.err != context.Canceled {
		t.Fatalf("bootstrap error = %v, want context.Canceled", result.err)
	}
	if result.emptyResponse {
		t.Fatal("client cancellation must not be classified as an upstream empty response")
	}
}

func TestReadStreamBootstrapWithDeliveryStopsAtBufferLimit(t *testing.T) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"still thinking"},"finish_reason":null}]}`)}

	result := readStreamBootstrapWithDelivery(context.Background(), chunks, time.Now(), newDeliverableOutputTracker(sdktranslator.FormatOpenAI), emptyResponsePolicy{
		enabled:   true,
		format:    sdktranslator.FormatOpenAI,
		maxBytes:  1,
		maxEvents: 1,
	})
	if !result.bufferLimitReached {
		t.Fatalf("bufferLimitReached = false; result = %+v", result)
	}
	if result.emptyResponse {
		t.Fatal("an active stream at the bootstrap limit is not a completed empty response")
	}
}

type emptyResponseScenarioExecutor struct {
	id string

	mu             sync.Mutex
	streamCalls    []string
	executeCalls   []string
	streamErrors   map[string]error
	streamPayloads map[string][]string
	streamChunks   map[string][]cliproxyexecutor.StreamChunk
	executePayload map[string][]byte
}

func (e *emptyResponseScenarioExecutor) Identifier() string { return e.id }

func (e *emptyResponseScenarioExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	payload := append([]byte(nil), e.executePayload[auth.ID]...)
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: payload}, nil
}

func (e *emptyResponseScenarioExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	streamErr := e.streamErrors[auth.ID]
	payloads := append([]string(nil), e.streamPayloads[auth.ID]...)
	sequence := append([]cliproxyexecutor.StreamChunk(nil), e.streamChunks[auth.ID]...)
	e.mu.Unlock()

	chunks := make(chan cliproxyexecutor.StreamChunk, len(payloads)+len(sequence)+1)
	if streamErr != nil {
		chunks <- cliproxyexecutor.StreamChunk{Err: streamErr}
	} else if len(sequence) > 0 {
		for _, chunk := range sequence {
			chunk.Payload = append([]byte(nil), chunk.Payload...)
			chunks <- chunk
		}
	} else {
		for _, payload := range payloads {
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(payload)}
		}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"X-Upstream-Auth": {auth.ID}},
		Chunks:  chunks,
	}, nil
}

func (e *emptyResponseScenarioExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *emptyResponseScenarioExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *emptyResponseScenarioExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *emptyResponseScenarioExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.streamCalls...)
}

func (e *emptyResponseScenarioExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.executeCalls...)
}

func enabledEmptyResponseConfig(auditOnly bool) *internalconfig.Config {
	return &internalconfig.Config{
		EmptyResponseRetry: internalconfig.EmptyResponseRetryConfig{
			Enabled:        true,
			AuditOnly:      auditOnly,
			Models:         []string{"gpt-5.6-sol"},
			ClientProfiles: []string{"workbuddy"},
			SourceFormats:  []string{"openai", "openai-response"},
		},
	}
}

func emptyResponseRequestOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{
			cliproxyexecutor.ClientProfileMetadataKey: "workbuddy",
			cliproxyexecutor.RequestPathMetadataKey:   "/v1/chat/completions",
		},
	}
}

func registerEmptyResponseScenarioAuths(t *testing.T, manager *Manager, provider string, ids ...string) {
	t.Helper()
	auths := make([]*Auth, 0, len(ids))
	for index, id := range ids {
		auths = append(auths, openAICompatChannelBreakerAuth(
			id,
			provider,
			"https://channel-"+string(rune('a'+index))+".example/v1",
			10,
		))
	}
	registerGPTChannelFailoverAuths(t, manager, provider, "gpt-5.6-sol", auths)
}

func TestManagerEmptyResponseRetryHandlesAuthEmptyAndRecovery(t *testing.T) {
	const provider = "empty-response-retry"
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		streamErrors: map[string]error{
			"aa-auth": &Error{
				Code:       "unauthorized",
				Message:    "unauthorized",
				HTTPStatus: http.StatusUnauthorized,
			},
		},
		streamPayloads: map[string][]string{
			"ba-empty": {
				`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
				"data: [DONE]\n\n",
			},
			"ca-good": {
				`data: {"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":null}]}` + "\n\n",
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
				"data: [DONE]\n\n",
			},
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(enabledEmptyResponseConfig(false))
	manager.SetRetryConfig(3, 30*time.Second, 10)
	manager.RegisterExecutor(executor)
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa-auth", "ba-empty", "ca-good")

	result, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol"},
		emptyResponseRequestOptions(),
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	var response strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		response.Write(chunk.Payload)
	}
	if !strings.Contains(response.String(), "recovered") {
		t.Fatalf("response = %q, want recovered content", response.String())
	}
	if strings.Contains(response.String(), `"role":"assistant"`) && !strings.Contains(response.String(), "recovered") {
		t.Fatalf("response leaked the failed empty attempt: %q", response.String())
	}
	if got, want := executor.StreamCalls(), []string{"aa-auth", "ba-empty", "ca-good"}; !stringSlicesEqual(got, want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
}

func TestManagerEmptyResponseRetryHandlesNonStreamingResponse(t *testing.T) {
	const provider = "empty-response-nonstream"
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		executePayload: map[string][]byte{
			"aa-empty": []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"  "},"finish_reason":"stop"}]}`),
			"ba-good":  []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`),
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(enabledEmptyResponseConfig(false))
	manager.SetRetryConfig(3, 30*time.Second, 10)
	manager.RegisterExecutor(executor)
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa-empty", "ba-good")

	opts := emptyResponseRequestOptions()
	opts.Stream = false
	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol"},
		opts,
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if !strings.Contains(string(response.Payload), `"content":"ok"`) {
		t.Fatalf("response = %s, want non-empty backup response", response.Payload)
	}
	if got, want := executor.ExecuteCalls(), []string{"aa-empty", "ba-good"}; !stringSlicesEqual(got, want) {
		t.Fatalf("execute calls = %v, want %v", got, want)
	}
}

func TestManagerEmptyResponseRetryHandlesResponsesStream(t *testing.T) {
	const provider = "empty-responses-retry"
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		streamPayloads: map[string][]string{
			"aa-empty": {
				"event: response.created\n",
				`data: {"type":"response.created","response":{"id":"resp-empty"}}` + "\n\n",
				`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}` + "\n\n",
				`data: {"type":"response.completed","response":{"id":"resp-empty","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}]}}` + "\n\n",
			},
			"ba-good": {
				`data: {"type":"response.output_text.delta","delta":"recovered"}` + "\n\n",
				`data: {"type":"response.completed","response":{"id":"resp-good","output":[{"type":"message","content":[{"type":"output_text","text":"recovered"}]}]}}` + "\n\n",
			},
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(enabledEmptyResponseConfig(false))
	manager.SetRetryConfig(3, 30*time.Second, 10)
	manager.RegisterExecutor(executor)
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa-empty", "ba-good")

	opts := emptyResponseRequestOptions()
	opts.SourceFormat = sdktranslator.FormatOpenAIResponse
	opts.Metadata[cliproxyexecutor.RequestPathMetadataKey] = "/v1/responses"
	result, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol"},
		opts,
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	var response strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		response.Write(chunk.Payload)
	}
	if !strings.Contains(response.String(), "recovered") || strings.Contains(response.String(), "resp-empty") {
		t.Fatalf("responses stream = %q, want only recovered attempt", response.String())
	}
	if got, want := executor.StreamCalls(), []string{"aa-empty", "ba-good"}; !stringSlicesEqual(got, want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
}

func TestManagerResponsesBootstrapCapacityRetriesNextCredential(t *testing.T) {
	const (
		provider = "responses-bootstrap-capacity"
		model    = "gpt-5.6-terra"
	)
	capacityErr := &Error{
		Code:       "rate_limit_exceeded",
		Message:    "Selected model is at capacity. Please try a different model.",
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		streamChunks: map[string][]cliproxyexecutor.StreamChunk{
			"aa-capacity": {
				{Payload: []byte("event: response.created\n")},
				{Payload: []byte(`data: {"type":"response.created","response":{"id":"resp-capacity"}}` + "\n\n")},
				{Payload: []byte("event: response.failed\n")},
				{Payload: []byte(`data: {"type":"response.failed","response":{"id":"resp-capacity","status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Selected model is at capacity. Please try a different model."}}}` + "\n\n")},
				{Err: capacityErr},
			},
		},
		streamPayloads: map[string][]string{
			"ba-good": {
				`data: {"type":"response.output_text.delta","delta":"recovered"}` + "\n\n",
				`data: {"type":"response.completed","response":{"id":"resp-good","output":[{"type":"message","content":[{"type":"output_text","text":"recovered"}]}]}}` + "\n\n",
			},
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		EmptyResponseRetry: internalconfig.EmptyResponseRetryConfig{
			Enabled:        true,
			Models:         []string{model},
			ClientProfiles: []string{"workbuddy"},
			SourceFormats:  []string{"openai-response"},
		},
	})
	manager.SetRetryConfig(3, 30*time.Second, 10)
	manager.RegisterExecutor(executor)
	auths := []*Auth{
		openAICompatChannelBreakerAuth("aa-capacity", provider, "https://channel-a.example/v1", 10),
		openAICompatChannelBreakerAuth("ba-good", provider, "https://channel-b.example/v1", 10),
	}
	registerGPTChannelFailoverAuths(t, manager, provider, model, auths)

	opts := emptyResponseRequestOptions()
	opts.SourceFormat = sdktranslator.FormatOpenAIResponse
	opts.Metadata[cliproxyexecutor.RequestPathMetadataKey] = "/v1/responses"
	result, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		opts,
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	if got := result.Headers.Get("X-Upstream-Auth"); got != "ba-good" {
		t.Fatalf("upstream auth header = %q, want ba-good", got)
	}
	var response strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		response.Write(chunk.Payload)
	}
	if !strings.Contains(response.String(), "recovered") {
		t.Fatalf("response = %q, want recovered output", response.String())
	}
	if strings.Contains(response.String(), "resp-capacity") || strings.Contains(strings.ToLower(response.String()), "capacity") {
		t.Fatalf("failed bootstrap payload leaked downstream: %q", response.String())
	}
	if got, want := executor.StreamCalls(), []string{"aa-capacity", "ba-good"}; !stringSlicesEqual(got, want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
}

func TestManagerEmptyResponseRetryDoesNotReplayToolCall(t *testing.T) {
	const provider = "empty-response-tool-call"
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		streamPayloads: map[string][]string{
			"aa-tool": {
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"arguments":""}}]},"finish_reason":null}]}` + "\n\n",
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"write_file","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n",
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
			},
			"ba-unused": {
				`data: {"choices":[{"index":0,"delta":{"content":"must not run"},"finish_reason":"stop"}]}` + "\n\n",
			},
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(enabledEmptyResponseConfig(false))
	manager.RegisterExecutor(executor)
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa-tool", "ba-unused")

	result, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol"},
		emptyResponseRequestOptions(),
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	var response strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		response.Write(chunk.Payload)
	}
	if !strings.Contains(response.String(), "write_file") {
		t.Fatalf("tool stream = %q, want tool call", response.String())
	}
	if got, want := executor.StreamCalls(), []string{"aa-tool"}; !stringSlicesEqual(got, want) {
		t.Fatalf("stream calls = %v, want %v", got, want)
	}
}

func TestManagerEmptyResponseRetryDoesNotReplayAfterDeliverableOutput(t *testing.T) {
	const provider = "empty-response-committed"
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		streamChunks: map[string][]cliproxyexecutor.StreamChunk{
			"aa-partial": {
				{Payload: []byte(`data: {"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n")},
				{Err: &Error{Code: "upstream_closed", Message: "upstream closed", HTTPStatus: http.StatusBadGateway, Retryable: true}},
			},
		},
		streamPayloads: map[string][]string{
			"ba-unused": {
				`data: {"choices":[{"index":0,"delta":{"content":"duplicate"},"finish_reason":"stop"}]}` + "\n\n",
			},
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(enabledEmptyResponseConfig(false))
	manager.RegisterExecutor(executor)
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa-partial", "ba-unused")

	result, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol"},
		emptyResponseRequestOptions(),
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	var response strings.Builder
	var finalErr error
	for chunk := range result.Chunks {
		response.Write(chunk.Payload)
		if chunk.Err != nil {
			finalErr = chunk.Err
		}
	}
	if !strings.Contains(response.String(), "partial") || finalErr == nil {
		t.Fatalf("stream response = %q error=%v, want partial output followed by error", response.String(), finalErr)
	}
	if got, want := executor.StreamCalls(), []string{"aa-partial"}; !stringSlicesEqual(got, want) {
		t.Fatalf("stream calls = %v, want no replay after output", got)
	}
}

func TestManagerEmptyResponseRetryReturnsTypedFailureWhenAllChannelsAreEmpty(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	const provider = "empty-response-exhausted"
	emptyPayloads := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		streamPayloads: map[string][]string{
			"aa-empty": emptyPayloads,
			"ba-empty": emptyPayloads,
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(enabledEmptyResponseConfig(false))
	manager.SetRetryConfig(3, 30*time.Second, 10)
	manager.RegisterExecutor(executor)
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa-empty", "ba-empty")

	result, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol"},
		emptyResponseRequestOptions(),
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v, want typed stream failure", errExecute)
	}
	var finalErr error
	for chunk := range result.Chunks {
		if len(chunk.Payload) > 0 {
			t.Fatalf("unexpected empty-attempt payload reached downstream: %q", chunk.Payload)
		}
		if chunk.Err != nil {
			finalErr = chunk.Err
		}
	}
	if finalErr == nil {
		t.Fatal("missing final stream error")
	}
	if got := statusCodeFromError(finalErr); got != http.StatusBadGateway {
		t.Fatalf("final status = %d, want %d; error=%v", got, http.StatusBadGateway, finalErr)
	}
	if got := errorCodeFromError(finalErr); got != emptyUpstreamResponseErrorCode {
		t.Fatalf("final code = %q, want %q; error=%v", got, emptyUpstreamResponseErrorCode, finalErr)
	}

	var summary *log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "request_execution_summary" {
			entryCopy := entry
			summary = entryCopy
		}
	}
	if summary == nil {
		t.Fatal("missing request_execution_summary")
	}
	if summary.Data["final_success"] != false ||
		summary.Data["final_empty_response"] != true ||
		summary.Data["empty_response_exhausted"] != true {
		t.Fatalf("summary fields = %#v", summary.Data)
	}
	if summary.Data["empty_response_count"] != 6 ||
		summary.Data["empty_response_retry_count"] != 5 ||
		summary.Data["empty_response_upstream_count"] != 2 {
		t.Fatalf("summary counters = %#v", summary.Data)
	}
	for _, authID := range []string{"aa-empty", "ba-empty"} {
		auth, ok := manager.GetByID(authID)
		if !ok {
			t.Fatalf("auth %s not found", authID)
		}
		state := auth.ModelStates["gpt-5.6-sol"]
		if state == nil || state.Health.BreakerState != HealthBreakerOpen {
			t.Fatalf("auth %s model breaker = %+v, want open after repeated empty responses", authID, state)
		}
	}
}

func TestManagerEmptyResponseAuditDoesNotRetry(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	const provider = "empty-response-audit"
	executor := &emptyResponseScenarioExecutor{
		id: provider,
		streamPayloads: map[string][]string{
			"aa-empty": {
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
				"data: [DONE]\n\n",
			},
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(enabledEmptyResponseConfig(true))
	manager.RegisterExecutor(executor)
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa-empty")

	result, errExecute := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol"},
		emptyResponseRequestOptions(),
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	for range result.Chunks {
	}
	if got := executor.StreamCalls(); len(got) != 1 {
		t.Fatalf("audit stream calls = %v, want exactly one attempt", got)
	}

	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "empty_response_detected" {
			found = true
			if entry.Data["audit_only"] != true || entry.Data["would_retry_empty_response"] != true {
				t.Fatalf("audit fields = %#v", entry.Data)
			}
		}
	}
	if !found {
		t.Fatal("missing empty_response_detected audit log")
	}
}

func TestEmptyResponsePolicyUsesSafeDefaults(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		EmptyResponseRetry: internalconfig.EmptyResponseRetryConfig{Enabled: true},
	})
	policy := manager.emptyResponsePolicy("gpt-5.6-sol", emptyResponseRequestOptions())
	if !policy.enabled || policy.auditOnly {
		t.Fatalf("policy = %+v, want enabled enforcement defaults", policy)
	}
	responseOpts := emptyResponseRequestOptions()
	responseOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	if responsePolicy := manager.emptyResponsePolicy("gpt-5.6-sol", responseOpts); !responsePolicy.enabled {
		t.Fatalf("responses policy = %+v, want enabled safe default", responsePolicy)
	}

	opts := emptyResponseRequestOptions()
	opts.Metadata[cliproxyexecutor.ClientProfileMetadataKey] = "other-client"
	if policyOther := manager.emptyResponsePolicy("gpt-5.6-sol", opts); policyOther.enabled {
		t.Fatalf("unexpected policy for other client: %+v", policyOther)
	}
}

func TestEmptyResponsePolicyAcceptsPRDFormatAliases(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		EmptyResponseRetry: internalconfig.EmptyResponseRetryConfig{
			Enabled:        true,
			Models:         []string{"gpt-5.6-sol"},
			ClientProfiles: []string{"workbuddy"},
			SourceFormats:  []string{"openai_chat", "openai_responses"},
		},
	})

	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		opts := emptyResponseRequestOptions()
		opts.SourceFormat = format
		if policy := manager.emptyResponsePolicy("gpt-5.6-sol", opts); !policy.enabled {
			t.Fatalf("format %q policy = %+v, want enabled", format, policy)
		}
	}
}

func TestEmptyResponseFailureContract(t *testing.T) {
	errEmpty := newEmptyUpstreamResponseFailure()
	if got := statusCodeFromError(errEmpty); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", got, http.StatusBadGateway)
	}
	if got := errorCodeFromError(errEmpty); got != emptyUpstreamResponseErrorCode {
		t.Fatalf("code = %q, want %q", got, emptyUpstreamResponseErrorCode)
	}
	if !strings.Contains(errEmpty.Error(), `"code":"empty_upstream_response"`) ||
		!strings.Contains(errEmpty.Error(), `"type":"upstream_error"`) {
		t.Fatalf("public error envelope = %q", errEmpty.Error())
	}
	if !shouldFailoverGPTChannel(errEmpty, []string{"codex"}, "gpt-5.6-sol") {
		t.Fatal("empty response failure must trigger GPT channel failover")
	}
}

func TestRegisterEmptyResponseScenarioAuthsUsesUniqueRegistryEntries(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	provider := "empty-response-registry"
	manager.RegisterExecutor(&emptyResponseScenarioExecutor{id: provider})
	registerEmptyResponseScenarioAuths(t, manager, provider, "aa", "bb")

	for _, id := range []string{"aa", "bb"} {
		if !registry.GetGlobalRegistry().ClientSupportsModel(id, "gpt-5.6-sol") {
			t.Fatalf("registry does not contain %s", id)
		}
	}
}
