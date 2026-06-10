package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestManager_ActiveStreamSnapshotTracksLiveStreams(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()

	id1 := m.activeStreams.start("claude", "MiniMax-M3", "/v1/chat/completions", now.Add(-5*time.Second))
	id2 := m.activeStreams.start("openai", "gpt-5.4", "/v1/responses", now.Add(-2*time.Second))
	t.Cleanup(func() {
		m.activeStreams.stop(id1)
		m.activeStreams.stop(id2)
	})

	snapshot := m.activeStreams.snapshot(now)
	if got := snapshot.ActiveStreamsTotal; got != 2 {
		t.Fatalf("active stream total = %d, want 2", got)
	}
	if got := snapshot.ActiveStreamsByModel["MiniMax-M3"]; got != 1 {
		t.Fatalf("MiniMax-M3 count = %d, want 1", got)
	}
	if got := snapshot.ActiveStreamsByProvider["claude"]; got != 1 {
		t.Fatalf("claude count = %d, want 1", got)
	}
	if got := snapshot.ActiveStreamsByEndpoint["/v1/chat/completions"]; got != 1 {
		t.Fatalf("/v1/chat/completions count = %d, want 1", got)
	}
	if snapshot.StreamAgeP50Ms <= 0 || snapshot.StreamAgeP95Ms <= 0 || snapshot.StreamAgeMaxMs <= 0 {
		t.Fatalf("expected positive stream age metrics, got %+v", snapshot)
	}
}

func TestManager_WrapStreamResult_LogsStreamExecutionSummary(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-stream-1"}
	ctx := logging.WithRequestID(context.Background(), "req-stream-1")
	meta := streamExecutionLogMeta{
		requestedModel: "MiniMax-M3",
		upstreamModel:  "MiniMax-M3",
		provider:       "claude",
		executor:       "ClaudeExecutor",
		requestPath:    "/v1/chat/completions",
		compatKind:     "xiaomi",
		toolShape: coreusage.ToolShape{
			ToolTypes:         "mcp,web_search_call",
			ToolNameHashes:    "0123456789abcdef",
			DeclaredToolCount: 2,
			InteractionCount:  3,
			MCPToolCount:      1,
			BuiltinToolCount:  1,
		},
	}

	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Payload: []byte("chunk-two")}
	close(remaining)

	startedAt := time.Now().Add(-250 * time.Millisecond)
	result := m.wrapStreamResult(
		ctx,
		auth,
		meta,
		"",
		nil,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte("chunk-one")}},
		remaining,
		nil,
		startedAt,
		40*time.Millisecond,
		nil,
	)

	chunks := 0
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		chunks++
	}
	if chunks != 2 {
		t.Fatalf("chunks forwarded = %d, want 2", chunks)
	}

	entry := findStreamExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["request_id"]; got != "req-stream-1" {
		t.Fatalf("request_id = %#v, want req-stream-1", got)
	}
	if got := entry.Data["provider"]; got != "claude" {
		t.Fatalf("provider = %#v, want claude", got)
	}
	if got := entry.Data["executor"]; got != "ClaudeExecutor" {
		t.Fatalf("executor = %#v, want ClaudeExecutor", got)
	}
	if got := entry.Data["requested_model"]; got != "MiniMax-M3" {
		t.Fatalf("requested_model = %#v, want MiniMax-M3", got)
	}
	if got := entry.Data["chunks_count"]; got != 2 {
		t.Fatalf("chunks_count = %#v, want 2", got)
	}
	wantBytes := int64(len("chunk-one") + len("chunk-two"))
	if got := entry.Data["bytes_out"]; got != wantBytes {
		t.Fatalf("bytes_out = %#v, want %d", got, wantBytes)
	}
	if got := entry.Data["time_to_first_chunk_ms"]; got != int64(40) {
		t.Fatalf("time_to_first_chunk_ms = %#v, want 40", got)
	}
	if got := entry.Data["output_tokens"]; got != int64(0) {
		t.Fatalf("output_tokens = %#v, want 0", got)
	}
	if got := entry.Data["stream_output_tokens_observed"]; got != false {
		t.Fatalf("stream_output_tokens_observed = %#v, want false", got)
	}
	if got := entry.Data["tokens_per_second"]; got != float64(0) {
		t.Fatalf("tokens_per_second = %#v, want 0", got)
	}
	if got := entry.Data["finish_reason"]; got != "done" {
		t.Fatalf("finish_reason = %#v, want done", got)
	}
	if got := entry.Data["client_gone"]; got != false {
		t.Fatalf("client_gone = %#v, want false", got)
	}
	if got := entry.Data["compat_kind"]; got != "xiaomi" {
		t.Fatalf("compat_kind = %#v, want xiaomi", got)
	}
	if got := entry.Data["tool_types"]; got != "mcp,web_search_call" {
		t.Fatalf("tool_types = %#v, want mcp,web_search_call", got)
	}
	if got := entry.Data["tool_name_hashes"]; got != "0123456789abcdef" {
		t.Fatalf("tool_name_hashes = %#v, want hash", got)
	}
	if got := entry.Data["declared_tool_count"]; got != 2 {
		t.Fatalf("declared_tool_count = %#v, want 2", got)
	}
	if got := entry.Data["tool_interaction_count"]; got != 3 {
		t.Fatalf("tool_interaction_count = %#v, want 3", got)
	}
}

func TestManager_WrapStreamResult_LogsSemanticFinishReasonAndUsage(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previousLevel)

	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-stream-2"}
	ctx := logging.WithRequestID(context.Background(), "req-stream-2")
	meta := streamExecutionLogMeta{
		requestedModel: "gpt-5.5",
		upstreamModel:  "gpt-5.5",
		provider:       "openai",
		executor:       "CodexExecutor",
		requestPath:    "/v1/responses",
	}

	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"completion_tokens\":5,\"total_tokens\":12,\"prompt_tokens\":7}}\n")}
	close(remaining)

	startedAt := time.Now().Add(-1100 * time.Millisecond)
	result := m.wrapStreamResult(
		ctx,
		auth,
		meta,
		"",
		nil,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n")}},
		remaining,
		nil,
		startedAt,
		100*time.Millisecond,
		nil,
	)

	for range result.Chunks {
	}

	entry := findStreamExecutionSummaryEntry(t, hook.AllEntries())
	if got := entry.Data["finish_reason"]; got != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", got)
	}
	if got := entry.Data["stream_output_tokens"]; got != int64(5) {
		t.Fatalf("stream_output_tokens = %#v, want 5", got)
	}
	if got := entry.Data["stream_output_tokens_observed"]; got != true {
		t.Fatalf("stream_output_tokens_observed = %#v, want true", got)
	}
	if got := entry.Data["output_tokens"]; got != int64(5) {
		t.Fatalf("output_tokens = %#v, want 5", got)
	}
	tps, ok := entry.Data["tokens_per_second"].(float64)
	if !ok {
		t.Fatalf("tokens_per_second type = %T, want float64", entry.Data["tokens_per_second"])
	}
	if tps <= 0 {
		t.Fatalf("tokens_per_second = %v, want > 0", tps)
	}
}

func findStreamExecutionSummaryEntry(t *testing.T, entries []*log.Entry) *log.Entry {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i] == nil {
			continue
		}
		if entries[i].Data["event"] == "stream_execution_summary" {
			return entries[i]
		}
	}
	t.Fatalf("stream_execution_summary log entry not found; entries=%d", len(entries))
	return nil
}
