package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIWebsocketsExecuteStreamSendsResponseCreateWithPreviousResponseID(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %q, want /responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xai-token" {
			t.Errorf("Authorization = %q, want Bearer xai-token", got)
		}
		if got := r.Header.Get("x-grok-conv-id"); got != "execution-session-1" {
			t.Errorf("x-grok-conv-id = %q, want execution-session-1", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-xai-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","stream":true,"previous_response_id":"resp-prev","instructions":"system prompt","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "execution-session-1",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	ctx = internalpayload.WithTransformReport(ctx, int64(len(req.Payload)))
	releaseReport := internalpayload.RetainTransformReport(ctx)

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var upstreamRequestBytes int64
	select {
	case payload := <-capturedPayload:
		upstreamRequestBytes = int64(len(payload))
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-prev" {
			t.Fatalf("previous_response_id = %q, want resp-prev; payload=%s", got, payload)
		}
		if gjson.GetBytes(payload, "stream").Exists() {
			t.Fatalf("stream must be omitted for xAI websocket payload: %s", payload)
		}
		if gjson.GetBytes(payload, "instructions").Exists() {
			t.Fatalf("instructions must be omitted when previous_response_id is set: %s", payload)
		}
		if got := gjson.GetBytes(payload, "prompt_cache_key").String(); got != "execution-session-1" {
			t.Fatalf("prompt_cache_key = %q, want execution-session-1; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "store").Bool(); !got {
			t.Fatalf("store = false, want true; payload=%s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before completed chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("chunk error = %v", chunk.Err)
		}
		if got := gjson.GetBytes(bytes.TrimSpace(chunk.Payload), "type").String(); got != "response.completed" {
			t.Fatalf("chunk type = %q, want response.completed; payload=%s", got, chunk.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for completed chunk")
	}
	for range result.Chunks {
	}
	assertTransformStageContract(t, ctx, releaseReport, "request_plan.xai.websocket_stream", upstreamRequestBytes)
}

func TestXAIWebsocketsExecuteStreamNormalizesReasoningTextEvents(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		events := [][]byte{
			[]byte(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}`),
			[]byte(`{"type":"response.content_part.added","sequence_number":2,"item_id":"rs_1","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":""}}`),
			[]byte(`{"type":"response.reasoning_text.delta","sequence_number":3,"item_id":"rs_1","output_index":0,"content_index":0,"delta":"thinking"}`),
			[]byte(`{"type":"response.reasoning_text.done","sequence_number":4,"item_id":"rs_1","output_index":0,"content_index":0,"text":"thinking"}`),
			[]byte(`{"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[],"content":[{"type":"reasoning_text","text":"thinking"}]}}`),
			[]byte(`{"type":"response.completed","sequence_number":6,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"grok-4.3","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`),
		}
		for _, event := range events {
			if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
				t.Errorf("write websocket event: %v", errWrite)
				return
			}
		}
	}))
	defer server.Close()

	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatCodex,
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var streamed bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	output := streamed.String()
	if strings.Contains(output, "reasoning_text") {
		t.Fatalf("stream contains xAI reasoning_text shape: %s", output)
	}
	for _, want := range []string{
		`"type":"response.reasoning_summary_part.added"`,
		`"type":"response.reasoning_summary_text.delta"`,
		`"type":"response.reasoning_summary_text.done"`,
		`"type":"response.reasoning_summary_part.done"`,
		`"part":{"type":"summary_text","text":"thinking"}`,
		`"summary_index":0`,
		`"summary":[{"type":"summary_text","text":"thinking"}]`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stream missing %q: %s", want, output)
		}
	}
	textDoneIndex := strings.Index(output, `"type":"response.reasoning_summary_text.done"`)
	partDoneIndex := strings.Index(output, `"type":"response.reasoning_summary_part.done"`)
	if textDoneIndex < 0 || partDoneIndex < 0 || textDoneIndex > partDoneIndex {
		t.Fatalf("reasoning done events are out of order: %s", output)
	}
}

func TestXAIWebsocketsExecuteStreamRewritesRepeatedResponseIDForDownstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPreviousIDs := make(chan string, 3)
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for i := 0; i < 3; i++ {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Errorf("read upstream websocket message: %v", errRead)
				return
			}
			previousID := gjson.GetBytes(payload, "previous_response_id").String()
			capturedPreviousIDs <- previousID
			completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-real","previous_response_id":%q,"output":[{"id":"rs_resp-real","type":"reasoning","status":"completed"}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`, previousID))
			if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
				t.Errorf("write completed websocket message: %v", errWrite)
				return
			}
		}
		<-releaseServer
	}))
	defer server.Close()
	defer close(releaseServer)

	exec := NewXAIWebsocketsExecutor(&config.Config{})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-id-map",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "xai-id-map-session",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	runRequest := func(previousID string) (string, string, string) {
		body := []byte(`{"model":"grok-4.3","input":[{"type":"message","role":"user","content":"hello"}]}`)
		if previousID != "" {
			body = []byte(fmt.Sprintf(`{"model":"grok-4.3","previous_response_id":%q,"input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`, previousID))
		}
		result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "grok-4.3", Payload: body}, opts)
		if err != nil {
			t.Fatalf("ExecuteStream() error = %v", err)
		}
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				t.Fatal("stream closed before completed chunk")
			}
			if chunk.Err != nil {
				t.Fatalf("chunk error = %v", chunk.Err)
			}
			payload := bytes.TrimSpace(chunk.Payload)
			return gjson.GetBytes(payload, "response.id").String(),
				gjson.GetBytes(payload, "response.output.0.id").String(),
				gjson.GetBytes(payload, "response.previous_response_id").String()
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for completed chunk")
		}
		return "", "", ""
	}

	firstDownstreamID, firstOutputID, firstResponsePrevious := runRequest("")
	if firstDownstreamID != "resp-real" {
		t.Fatalf("first downstream id = %q, want resp-real", firstDownstreamID)
	}
	if firstOutputID != "rs_resp-real" {
		t.Fatalf("first output item id = %q, want rs_resp-real", firstOutputID)
	}
	if firstResponsePrevious != "" {
		t.Fatalf("first response previous_response_id = %q, want empty", firstResponsePrevious)
	}
	firstUpstreamPrevious := <-capturedPreviousIDs
	if firstUpstreamPrevious != "" {
		t.Fatalf("first upstream previous_response_id = %q, want empty", firstUpstreamPrevious)
	}

	secondDownstreamID, secondOutputID, secondResponsePrevious := runRequest(firstDownstreamID)
	if secondDownstreamID == "" || secondDownstreamID == "resp-real" {
		t.Fatalf("second downstream id = %q, want synthetic id different from resp-real", secondDownstreamID)
	}
	if secondOutputID == "rs_resp-real" || !strings.Contains(secondOutputID, secondDownstreamID) {
		t.Fatalf("second output item id = %q, want rewritten id containing %q", secondOutputID, secondDownstreamID)
	}
	if secondResponsePrevious != firstDownstreamID {
		t.Fatalf("second response previous_response_id = %q, want %q", secondResponsePrevious, firstDownstreamID)
	}
	secondUpstreamPrevious := <-capturedPreviousIDs
	if secondUpstreamPrevious != "resp-real" {
		t.Fatalf("second upstream previous_response_id = %q, want resp-real", secondUpstreamPrevious)
	}

	thirdDownstreamID, thirdOutputID, thirdResponsePrevious := runRequest(secondDownstreamID)
	if thirdDownstreamID == "" || thirdDownstreamID == "resp-real" || thirdDownstreamID == secondDownstreamID {
		t.Fatalf("third downstream id = %q, want a new synthetic id", thirdDownstreamID)
	}
	if thirdOutputID == "rs_resp-real" || !strings.Contains(thirdOutputID, thirdDownstreamID) {
		t.Fatalf("third output item id = %q, want rewritten id containing %q", thirdOutputID, thirdDownstreamID)
	}
	if thirdResponsePrevious != secondDownstreamID {
		t.Fatalf("third response previous_response_id = %q, want %q", thirdResponsePrevious, secondDownstreamID)
	}
	thirdUpstreamPrevious := <-capturedPreviousIDs
	if thirdUpstreamPrevious != "resp-real" {
		t.Fatalf("third upstream previous_response_id = %q, want resp-real", thirdUpstreamPrevious)
	}
}

func TestXAIWebsocketsExecuteStreamCompactionTriggerUsesHTTPCompactWithRecordedContext(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedWebsocketPayload := make(chan []byte, 1)
	capturedCompactPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()

			for i := 0; i < 2; i++ {
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, payload, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Errorf("read upstream websocket message: %v", errRead)
					return
				}
				capturedWebsocketPayload <- bytes.Clone(payload)
				completed := []byte(`{"type":"response.completed","response":{"id":"resp-real","output":[{"type":"message","id":"out-1","role":"assistant","content":"first answer"}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
				if i == 1 {
					completed = []byte(`{"type":"response.completed","response":{"id":"resp-after-compact","output":[{"type":"message","id":"out-2","role":"assistant","content":"second answer"}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
				}
				if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
					t.Errorf("write completed websocket message: %v", errWrite)
					return
				}
			}
		case "/responses/compact":
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read compact body: %v", errRead)
				return
			}
			capturedCompactPayload <- bytes.Clone(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_compact","model":"grok-4.3","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
		default:
			t.Errorf("path = %q, want /responses", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	exec := NewXAIWebsocketsExecutor(&config.Config{})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-compaction",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "xai-compaction-session",
		},
	}

	result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","stream":true,"input":[{"type":"message","id":"msg-1","role":"user","content":"first"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream first turn error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	select {
	case payload := <-capturedWebsocketPayload:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
		}
		input := gjson.GetBytes(payload, "input")
		if !input.IsArray() || len(input.Array()) != 1 {
			t.Fatalf("input = %s, want one first-turn item", input.Raw)
		}
		if gjson.GetBytes(payload, "stream").Exists() {
			t.Fatalf("stream must be omitted for xAI websocket payload: %s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}

	compactResult, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","stream":true,"previous_response_id":"resp-real-xai-1","input":[{"type":"compaction_trigger"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream compaction trigger error: %v", err)
	}
	for chunk := range compactResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("compact stream chunk error = %v", chunk.Err)
		}
	}

	select {
	case payload := <-capturedCompactPayload:
		if xaiInputHasItemType(payload, "compaction_trigger") {
			t.Fatalf("compaction_trigger reached xai compact body: %s", payload)
		}
		input := gjson.GetBytes(payload, "input")
		if !input.IsArray() || len(input.Array()) != 2 {
			t.Fatalf("compact input = %s, want first request input plus response output", input.Raw)
		}
		if got := input.Array()[0].Get("id").String(); got != "msg-1" {
			t.Fatalf("compact input[0].id = %q, want msg-1; payload=%s", got, payload)
		}
		if got := input.Array()[1].Get("id").String(); got != "out-1" {
			t.Fatalf("compact input[1].id = %q, want out-1; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "" {
			t.Fatalf("compact previous_response_id = %q, want empty; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for compact HTTP payload")
	}

	nextResult, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","stream":true,"previous_response_id":"resp_compact","input":[{"type":"message","id":"msg-2","role":"user","content":"second"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream post-compaction turn error: %v", err)
	}
	for chunk := range nextResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("post-compaction stream chunk error = %v", chunk.Err)
		}
	}
	select {
	case payload := <-capturedWebsocketPayload:
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "" {
			t.Fatalf("post-compaction previous_response_id = %q, want empty; payload=%s", got, payload)
		}
		input := gjson.GetBytes(payload, "input")
		if !input.IsArray() || len(input.Array()) != 2 {
			t.Fatalf("post-compaction input = %s, want compaction item plus new message", input.Raw)
		}
		if got := input.Array()[0].Get("type").String(); got != "compaction" {
			t.Fatalf("post-compaction input[0].type = %q, want compaction; payload=%s", got, payload)
		}
		if got := input.Array()[1].Get("id").String(); got != "msg-2" {
			t.Fatalf("post-compaction input[1].id = %q, want msg-2; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-compaction websocket payload")
	}
}

func TestBuildXAIWebsocketRequestBodySetsStoreAndKeepsPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"grok-4.3","stream":true,"stream_options":{"include_usage":true},"background":true,"prompt_cache_key":"cache-1","previous_response_id":"resp-prev","instructions":"system prompt","input":[{"type":"message","role":"user","content":"hello"}]}`)

	payload := buildXAIWebsocketRequestBody(body)

	if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
		t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
	}
	if gjson.GetBytes(payload, "stream").Exists() {
		t.Fatalf("stream must be omitted for xAI websocket payload: %s", payload)
	}
	if gjson.GetBytes(payload, "stream_options").Exists() {
		t.Fatalf("stream_options must be omitted for xAI websocket payload: %s", payload)
	}
	if gjson.GetBytes(payload, "background").Exists() {
		t.Fatalf("background must be omitted for xAI websocket payload: %s", payload)
	}
	if got := gjson.GetBytes(payload, "prompt_cache_key").String(); got != "cache-1" {
		t.Fatalf("prompt_cache_key = %q, want cache-1; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "store").Bool(); !got {
		t.Fatalf("store = false, want true; payload=%s", payload)
	}
	if gjson.GetBytes(payload, "instructions").Exists() {
		t.Fatalf("instructions must be omitted when previous_response_id is set: %s", payload)
	}
}

func TestXAIWebsocketTranscriptItemLimitFailsAndClearsState(t *testing.T) {
	t.Parallel()

	var input strings.Builder
	input.WriteString(`{"input":[`)
	for idx := 0; idx < xaiWebsocketTranscriptMaxItems+1; idx++ {
		if idx > 0 {
			input.WriteByte(',')
		}
		input.WriteString(`{"type":"message","content":"x"}`)
	}
	input.WriteString(`]}`)

	state := &xaiWebsocketIDState{downstreamToUpstream: map[string]string{"old": "old"}}
	err := state.recordTranscriptTurn([]byte(input.String()), []byte(`{"response":{"output":[]}}`))
	assertXAIWebsocketSessionStateTooLarge(t, err)
	if len(state.transcriptInput) != 0 || state.transcriptBytes != 0 || len(state.downstreamToUpstream) != 0 {
		t.Fatalf("state was not cleared: items=%d bytes=%d mappings=%d", len(state.transcriptInput), state.transcriptBytes, len(state.downstreamToUpstream))
	}
}

func TestXAIWebsocketRetainedTranscriptCloneTracksReplaceAndDelete(t *testing.T) {
	store := &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	state := getXAIWebsocketIDState(store, "large-transcript-session")
	largeItem := []byte(`{"type":"message","content":"` + strings.Repeat("x", (1<<20)+1) + `"}`)
	before := internalpayload.CurrentLargeCloneMetrics()

	if errReplace := state.replaceTranscriptWithItems(largeItem); errReplace != nil {
		t.Fatalf("replace large transcript: %v", errReplace)
	}
	during := internalpayload.CurrentLargeCloneMetrics()
	if during.ActiveScopedCount != before.ActiveScopedCount+1 || during.ActiveScopedBytes < before.ActiveScopedBytes+(1<<20) {
		t.Fatalf("retained transcript scope: before=%+v during=%+v", before, during)
	}
	if during.PeakScopedCount < during.ActiveScopedCount || during.PeakScopedBytes < during.ActiveScopedBytes {
		t.Fatalf("retained transcript peak: %+v", during)
	}

	if errReplace := state.replaceTranscriptWithItems(largeItem); errReplace != nil {
		t.Fatalf("replace retained transcript: %v", errReplace)
	}
	replaced := internalpayload.CurrentLargeCloneMetrics()
	if replaced.ActiveScopedCount != during.ActiveScopedCount || replaced.ActiveScopedBytes != during.ActiveScopedBytes {
		t.Fatalf("replacing transcript leaked scope: during=%+v replaced=%+v", during, replaced)
	}

	deleteXAIWebsocketIDState(store, "large-transcript-session")
	after := internalpayload.CurrentLargeCloneMetrics()
	if after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("deleting transcript state leaked scope: before=%+v after=%+v", before, after)
	}
}

func TestXAIWebsocketTranscriptAggregateBudgetSaturatesAcrossSessions(t *testing.T) {
	item := []byte(`{"type":"message","content":"` + strings.Repeat("x", 256) + `"}`)
	existingItem := []byte(`{"type":"message","content":"existing"}`)
	itemBytes := int64(len(bytes.TrimSpace(item)))
	existingBytes := int64(len(bytes.TrimSpace(existingItem)))
	budget := newXAIWebsocketTranscriptBudget(itemBytes + itemBytes/2)
	store := &xaiWebsocketIDStateStore{
		sessions: make(map[string]*xaiWebsocketIDState),
		budget:   budget,
	}
	first := getXAIWebsocketIDState(store, "aggregate-first")
	second := getXAIWebsocketIDState(store, "aggregate-second")

	if errReplace := first.replaceTranscriptWithItems(item); errReplace != nil {
		t.Fatalf("retain first transcript: %v", errReplace)
	}
	if errReplace := second.replaceTranscriptWithItems(existingItem); errReplace != nil {
		t.Fatalf("retain existing second transcript: %v", errReplace)
	}
	if errMap := second.mapDownstreamToUpstream("downstream-existing", "upstream-existing"); errMap != nil {
		t.Fatalf("retain existing second mapping: %v", errMap)
	}
	if got := budget.InUse(); got != itemBytes+existingBytes {
		t.Fatalf("budget after initial transcripts = %d, want %d", got, itemBytes+existingBytes)
	}
	errCapacity := second.replaceTranscriptWithItems(item)
	assertXAIWebsocketSessionStateCapacity(t, errCapacity)
	discardXAIWebsocketIDStateOnError(store, "aggregate-second", second, errCapacity)
	if got := budget.InUse(); got != itemBytes+existingBytes {
		t.Fatalf("saturated budget = %d, want %d", got, itemBytes+existingBytes)
	}
	store.mu.Lock()
	current := store.sessions["aggregate-second"]
	store.mu.Unlock()
	if current != second {
		t.Fatalf("capacity error discarded current state: current=%p second=%p", current, second)
	}
	second.mu.Lock()
	closed := second.closed
	mapped := second.downstreamToUpstream["downstream-existing"]
	transcript := xaiMarshalRawMessages(second.transcriptInput)
	second.mu.Unlock()
	if closed || mapped != "upstream-existing" || !bytes.Contains(transcript, []byte(`"content":"existing"`)) {
		t.Fatalf("capacity rollback changed state: closed=%v mapped=%q transcript=%s", closed, mapped, transcript)
	}

	deleteXAIWebsocketIDState(store, "aggregate-first")
	if got := budget.InUse(); got != existingBytes {
		t.Fatalf("budget after first release = %d, want %d", got, existingBytes)
	}
	if errReplace := second.replaceTranscriptWithItems(item); errReplace != nil {
		t.Fatalf("reuse released aggregate budget: %v", errReplace)
	}
	deleteXAIWebsocketIDState(store, "aggregate-second")
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget after all releases = %d, want 0", got)
	}
}

func TestXAIWebsocketEmptyResetReleasesTranscriptBudget(t *testing.T) {
	largeItem := []byte(`{"type":"message","content":"` + strings.Repeat("x", (1<<20)+1) + `"}`)
	itemBytes := int64(len(bytes.TrimSpace(largeItem)))
	budget := newXAIWebsocketTranscriptBudget(4 << 20)
	state := &xaiWebsocketIDState{
		budget:               budget,
		downstreamToUpstream: make(map[string]string),
	}
	before := internalpayload.CurrentLargeCloneMetrics()
	if errReplace := state.replaceTranscriptWithItems(largeItem); errReplace != nil {
		t.Fatalf("retain transcript before reset: %v", errReplace)
	}
	if got := budget.InUse(); got != itemBytes {
		t.Fatalf("budget before reset = %d, want %d", got, itemBytes)
	}

	if errRecord := state.recordTranscriptTurn(
		[]byte(`{"input":[]}`),
		[]byte(`{"response":{"output":[]}}`),
	); errRecord != nil {
		t.Fatalf("empty transcript reset: %v", errRecord)
	}

	state.mu.Lock()
	items := len(state.transcriptInput)
	bytesInState := state.transcriptBytes
	releases := len(state.transcriptReleases)
	state.mu.Unlock()
	if items != 0 || bytesInState != 0 || releases != 0 {
		t.Fatalf("state after empty reset = items:%d bytes:%d releases:%d", items, bytesInState, releases)
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget after empty reset = %d, want 0", got)
	}
	after := internalpayload.CurrentLargeCloneMetrics()
	if after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("empty reset leaked retained transcript scope: before=%+v after=%+v", before, after)
	}
}

func TestXAIWebsocketEmptyResetRacesDeleteWithoutClearingReplacement(t *testing.T) {
	oldItem := []byte(`{"type":"message","content":"` + strings.Repeat("o", (1<<20)+1) + `"}`)
	replacementItem := []byte(`{"type":"message","content":"` + strings.Repeat("r", (1<<20)+1) + `"}`)
	replacementBytes := int64(len(bytes.TrimSpace(replacementItem)))
	budget := newXAIWebsocketTranscriptBudget(4 << 20)
	store := &xaiWebsocketIDStateStore{
		sessions: make(map[string]*xaiWebsocketIDState),
		budget:   budget,
	}
	before := internalpayload.CurrentLargeCloneMetrics()
	old := getXAIWebsocketIDState(store, "empty-reset-aba")
	if errReplace := old.replaceTranscriptWithItems(oldItem); errReplace != nil {
		t.Fatalf("retain old transcript: %v", errReplace)
	}

	start := make(chan struct{})
	resetDone := make(chan error, 1)
	deleteDone := make(chan struct{})
	go func() {
		<-start
		resetDone <- old.recordTranscriptTurn(
			[]byte(`{"input":[]}`),
			[]byte(`{"response":{"output":[]}}`),
		)
	}()
	go func() {
		<-start
		deleteXAIWebsocketIDStateIfMatch(store, "empty-reset-aba", old)
		close(deleteDone)
	}()
	close(start)
	<-deleteDone

	replacement := getXAIWebsocketIDState(store, "empty-reset-aba")
	if replacement == old {
		t.Fatal("replacement reused old state")
	}
	if errReplace := replacement.replaceTranscriptWithItems(replacementItem); errReplace != nil {
		t.Fatalf("retain replacement transcript: %v", errReplace)
	}
	if errReset := <-resetDone; errReset != nil && !errors.Is(errReset, errXAIWebsocketIDStateClosed) {
		t.Fatalf("raced reset error = %v, want nil or closed", errReset)
	}

	store.mu.Lock()
	current := store.sessions["empty-reset-aba"]
	store.mu.Unlock()
	replacement.mu.Lock()
	closed := replacement.closed
	transcript := xaiMarshalRawMessages(replacement.transcriptInput)
	replacement.mu.Unlock()
	replacementRetained := bytes.Contains(transcript, []byte(`"content":"rrrrrrrr`))
	if current != replacement || closed || !replacementRetained {
		t.Fatalf("replacement after raced reset = current:%p want:%p closed:%v retained:%v transcript_bytes:%d", current, replacement, closed, replacementRetained, len(transcript))
	}
	if got := budget.InUse(); got != replacementBytes {
		t.Fatalf("budget after raced reset = %d, want replacement bytes %d", got, replacementBytes)
	}

	deleteXAIWebsocketIDStateIfMatch(store, "empty-reset-aba", replacement)
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget after raced cleanup = %d, want 0", got)
	}
	after := internalpayload.CurrentLargeCloneMetrics()
	if after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("raced reset leaked retained transcript scope: before=%+v after=%+v", before, after)
	}
}

func TestXAIWebsocketCloseExecutionSessionFencesLateTranscriptInstall(t *testing.T) {
	budget := newXAIWebsocketTranscriptBudget(4 << 20)
	idStore := &xaiWebsocketIDStateStore{
		sessions: make(map[string]*xaiWebsocketIDState),
		budget:   budget,
	}
	state := getXAIWebsocketIDState(idStore, "close-fence-session")
	largeItem := []byte(`{"type":"message","content":"` + strings.Repeat("x", (1<<20)+1) + `"}`)
	before := internalpayload.CurrentLargeCloneMetrics()
	if errReplace := state.replaceTranscriptWithItems(largeItem); errReplace != nil {
		t.Fatalf("retain transcript before close: %v", errReplace)
	}
	if budget.InUse() == 0 {
		t.Fatal("transcript did not acquire aggregate budget")
	}

	exec := &XAIWebsocketsExecutor{
		store:   &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)},
		idStore: idStore,
	}
	exec.CloseExecutionSession("close-fence-session")
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget after close = %d, want 0", got)
	}
	after := internalpayload.CurrentLargeCloneMetrics()
	if after.ActiveScopedCount != before.ActiveScopedCount || after.ActiveScopedBytes != before.ActiveScopedBytes {
		t.Fatalf("close leaked retained transcript scope: before=%+v after=%+v", before, after)
	}
	state.mu.Lock()
	closed := state.closed
	items := len(state.transcriptInput)
	state.mu.Unlock()
	if !closed || items != 0 {
		t.Fatalf("closed state = closed:%v items:%d", closed, items)
	}
	if errReplace := state.replaceTranscriptWithItems([]byte(`{"type":"message","content":"late"}`)); !errors.Is(errReplace, errXAIWebsocketIDStateClosed) {
		t.Fatalf("late replace error = %v, want closed", errReplace)
	}
	if errRecord := state.recordTranscriptTurn(
		[]byte(`{"previous_response_id":"prev","input":[{"type":"message","content":"late"}]}`),
		[]byte(`{"response":{"output":[]}}`),
	); !errors.Is(errRecord, errXAIWebsocketIDStateClosed) {
		t.Fatalf("late record error = %v, want closed", errRecord)
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("late writes reacquired budget = %d, want 0", got)
	}
}

func TestDeleteXAIWebsocketIDStateRacesRecordWithoutRetainedLeak(t *testing.T) {
	budget := newXAIWebsocketTranscriptBudget(1 << 20)
	store := &xaiWebsocketIDStateStore{
		sessions: make(map[string]*xaiWebsocketIDState),
		budget:   budget,
	}
	state := getXAIWebsocketIDState(store, "record-delete-race")
	requestPayload := []byte(`{"previous_response_id":"prev","input":[{"type":"message","content":"input"}]}`)
	completedPayload := []byte(`{"response":{"output":[{"type":"message","content":"output"}]}}`)
	start := make(chan struct{})
	errCh := make(chan error, 64)
	var workers sync.WaitGroup
	for range 64 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if errRecord := state.recordTranscriptTurn(requestPayload, completedPayload); errRecord != nil && !errors.Is(errRecord, errXAIWebsocketIDStateClosed) {
				errCh <- errRecord
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		deleteXAIWebsocketIDState(store, "record-delete-race")
	}()
	close(start)
	workers.Wait()
	close(errCh)
	for errRecord := range errCh {
		t.Fatalf("record/delete race error: %v", errRecord)
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("record/delete race retained %d bytes, want 0", got)
	}
	state.mu.Lock()
	closed := state.closed
	items := len(state.transcriptInput)
	state.mu.Unlock()
	if !closed || items != 0 {
		t.Fatalf("raced state = closed:%v items:%d", closed, items)
	}
}

func TestDeleteXAIWebsocketIDStateIfMatchDoesNotDeleteReplacement(t *testing.T) {
	item := []byte(`{"type":"message","content":"replacement"}`)
	itemBytes := int64(len(bytes.TrimSpace(item)))
	budget := newXAIWebsocketTranscriptBudget(1 << 20)
	store := &xaiWebsocketIDStateStore{
		sessions: make(map[string]*xaiWebsocketIDState),
		budget:   budget,
	}
	stale := getXAIWebsocketIDState(store, "aba-session")
	deleteXAIWebsocketIDStateIfMatch(store, "aba-session", stale)
	replacement := getXAIWebsocketIDState(store, "aba-session")
	if replacement == stale {
		t.Fatal("replacement reused the closed state")
	}
	if errReplace := replacement.replaceTranscriptWithItems(item); errReplace != nil {
		t.Fatalf("retain replacement transcript: %v", errReplace)
	}

	deleteXAIWebsocketIDStateIfMatch(store, "aba-session", stale)
	store.mu.Lock()
	current := store.sessions["aba-session"]
	store.mu.Unlock()
	if current != replacement {
		t.Fatalf("stale delete removed replacement: current=%p replacement=%p", current, replacement)
	}
	replacement.mu.Lock()
	closed := replacement.closed
	replacement.mu.Unlock()
	if closed || budget.InUse() != itemBytes {
		t.Fatalf("replacement after stale delete = closed:%v budget:%d, want false/%d", closed, budget.InUse(), itemBytes)
	}
	deleteXAIWebsocketIDStateIfMatch(store, "aba-session", replacement)
	if got := budget.InUse(); got != 0 {
		t.Fatalf("replacement cleanup retained %d bytes, want 0", got)
	}
}

func TestClosedXAIWebsocketMapperCannotRepopulateIDState(t *testing.T) {
	store := &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	state := getXAIWebsocketIDState(store, "closed-mapper-session")
	mapper := &xaiWebsocketRequestIDMapper{
		state:                state,
		downstreamPreviousID: "downstream-prev",
		upstreamPreviousID:   "upstream-prev",
	}
	deleteXAIWebsocketIDStateIfMatch(store, "closed-mapper-session", state)
	if _, errMap := mapper.downstreamIDForUpstreamResponse("upstream-next"); !errors.Is(errMap, errXAIWebsocketIDStateClosed) {
		t.Fatalf("closed mapper error = %v, want closed", errMap)
	}
	state.mu.Lock()
	mappings := len(state.downstreamToUpstream)
	sequence := state.sequence
	state.mu.Unlock()
	if mappings != 0 || sequence != 0 || mapper.upstreamResponseID != "" || mapper.downstreamResponseID != "" {
		t.Fatalf("closed mapper mutated state: mappings=%d sequence=%d mapper=%+v", mappings, sequence, mapper)
	}
}

func TestXAIWebsocketIDMapLimitFailsAndClearsState(t *testing.T) {
	t.Parallel()

	state := &xaiWebsocketIDState{}
	for idx := 0; idx < xaiWebsocketIDMapMaxEntries; idx++ {
		if err := state.mapDownstreamToUpstream(fmt.Sprintf("downstream-%d", idx), fmt.Sprintf("upstream-%d", idx)); err != nil {
			t.Fatalf("map entry %d: %v", idx, err)
		}
	}
	err := state.mapDownstreamToUpstream("overflow", "upstream-overflow")
	assertXAIWebsocketSessionStateTooLarge(t, err)
	if len(state.downstreamToUpstream) != 0 || len(state.transcriptInput) != 0 || state.transcriptBytes != 0 {
		t.Fatalf("state was not cleared: mappings=%d items=%d bytes=%d", len(state.downstreamToUpstream), len(state.transcriptInput), state.transcriptBytes)
	}
}

func assertXAIWebsocketSessionStateTooLarge(t *testing.T, err error) {
	t.Helper()
	statusError, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error type %T does not expose StatusCode", err)
	}
	if got := statusError.StatusCode(); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}
	if got := gjson.Get(err.Error(), "error.code").String(); got != "session_state_too_large" {
		t.Fatalf("error code = %q, want session_state_too_large", got)
	}
}

func assertXAIWebsocketSessionStateCapacity(t *testing.T, err error) {
	t.Helper()
	statusError, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error type %T does not expose StatusCode", err)
	}
	if got := statusError.StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := gjson.Get(err.Error(), "error.code").String(); got != "session_state_capacity" {
		t.Fatalf("error code = %q, want session_state_capacity", got)
	}
	coded, ok := err.(interface{ ErrorCode() string })
	if !ok {
		t.Fatalf("error type %T does not expose ErrorCode", err)
	}
	if got := coded.ErrorCode(); got != "session_state_capacity" {
		t.Fatalf("typed error code = %q, want session_state_capacity", got)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("error type %T does not expose RetryAfter", err)
	}
	retryAfter := retryable.RetryAfter()
	if retryAfter == nil || *retryAfter != time.Second {
		t.Fatalf("RetryAfter = %v, want 1s", retryAfter)
	}
	headerProvider, ok := err.(interface{ Headers() http.Header })
	if !ok {
		t.Fatalf("error type %T does not expose Headers", err)
	}
	if got := headerProvider.Headers().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After header = %q, want 1", got)
	}
}

func TestXAIWebsocketsExecuteStreamCompletesGenerateFalseWarmup(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)
		created := []byte(`{"type":"response.created","response":{"id":"resp-warmup-1","object":"response","status":"in_progress","output":[]}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, created); errWrite != nil {
			t.Errorf("write created websocket message: %v", errWrite)
			return
		}
		<-releaseServer
	}))
	defer server.Close()
	defer close(releaseServer)

	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-warmup",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","generate":false,"input":[{"type":"message","role":"user","content":"warm up"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "generate").Bool(); got {
			t.Fatalf("generate = true, want false; payload=%s", payload)
		}
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "store").Bool(); !got {
			t.Fatalf("store = false, want true; payload=%s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}

	var gotTypes []string
	for {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				if len(gotTypes) != 2 {
					t.Fatalf("event types = %v, want response.created and response.completed", gotTypes)
				}
				return
			}
			if chunk.Err != nil {
				t.Fatalf("chunk error = %v", chunk.Err)
			}
			gotTypes = append(gotTypes, gjson.GetBytes(bytes.TrimSpace(chunk.Payload), "type").String())
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for warmup stream to close; event types so far: %v", gotTypes)
		}
	}
}

func TestXAIWebsocketsExecuteStreamStopsOnBareErrorPayload(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		payload := []byte(`{"error":{"message":"Request validation error: {\"code\":\"400\",\"error\":\"Argument not supported: instructions and previous_response_id together\"}","type":"api_error"}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
			t.Errorf("write error websocket message: %v", errWrite)
			return
		}
		<-releaseServer
	}))
	defer server.Close()
	defer close(releaseServer)

	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-error",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before error chunk")
		}
		if chunk.Err == nil {
			t.Fatalf("chunk error = nil, want upstream error; payload=%s", chunk.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bare upstream error")
	}
}

func TestParseXAIWebsocketErrorDoesNotExposeUpstreamBody(t *testing.T) {
	secret := "sentinel-xai-websocket-secret"
	payload := []byte(`{"status":400,"error":{"message":"invalid request ` + secret + `","type":"invalid_request_error","code":"invalid_request"}}`)

	err, ok := parseXAIWebsocketError(payload)
	if !ok || err == nil {
		t.Fatalf("parseXAIWebsocketError() = %v, %v; want error", err, ok)
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %T/%v, want 400", err, status)
	}
	coded, ok := err.(interface{ ErrorCode() string })
	if !ok || coded.ErrorCode() != "invalid_request" {
		t.Fatalf("error code = %T/%v, want invalid_request", err, coded)
	}
	if got := err.Error(); strings.Contains(got, secret) || strings.Contains(got, `"message"`) || !strings.Contains(got, `"sha256":`) || !strings.Contains(got, `"content_type":"application/json"`) {
		t.Fatalf("unsafe websocket error summary: %s", got)
	}
}

func TestXAIWebsocketsExecuteStreamSanitizesHandshakeError(t *testing.T) {
	secret := "sentinel-xai-handshake-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited `+secret+`","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	}))
	defer server.Close()

	exec := NewXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	_, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         true,
	})
	if err == nil {
		t.Fatal("ExecuteStream() error = nil, want handshake rejection")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %T/%v, want 429", err, status)
	}
	coded, ok := err.(interface{ ErrorCode() string })
	if !ok || coded.ErrorCode() != "rate_limit_exceeded" {
		t.Fatalf("error code = %T/%v, want rate_limit_exceeded", err, coded)
	}
	headerProvider, ok := err.(interface{ Headers() http.Header })
	if !ok || headerProvider.Headers().Get("Retry-After") != "3" {
		t.Fatalf("Retry-After = %T/%v, want 3", err, headerProvider)
	}
	if got := err.Error(); strings.Contains(got, secret) || strings.Contains(got, `"message"`) || !strings.Contains(got, `"sha256":`) || !strings.Contains(got, `"content_type":"application/json"`) {
		t.Fatalf("unsafe handshake error summary: %s", got)
	}
}
