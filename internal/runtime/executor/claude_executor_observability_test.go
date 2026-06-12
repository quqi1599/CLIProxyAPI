package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestRepairMiniMaxClaudeToolAdjacencyWithLog_EmitsRequestScopedFields(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"tool_use","id":"browser_back","name":"browser_back","input":{}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"text","text":"next user instruction"},
					{"type":"tool_result","tool_use_id":"browser_back","content":"ok"}
				]
			}
		]
	}`)
	meta := compatRepairLogMeta{
		requestedModel: "MiniMax-M3",
		upstreamModel:  "MiniMax-M3",
		provider:       "claude",
		executor:       "ClaudeExecutor",
		requestPath:    "/v1/chat/completions",
		compatKind:     "deepseek",
		messageCount:   2,
		toolCount:      1,
	}
	ctx := logging.WithRequestID(context.Background(), "req-repair-1")

	if _, err := repairMiniMaxClaudeToolAdjacencyForCompatWithLog(ctx, body, meta); err != nil {
		t.Fatalf("repairMiniMaxClaudeToolAdjacencyForCompatWithLog() error = %v", err)
	}

	entry := findCompatRepairEntry(t, hook.AllEntries(), "claude_tool_result_adjacency")
	if got := entry.Data["request_id"]; got != "req-repair-1" {
		t.Fatalf("request_id = %#v, want req-repair-1", got)
	}
	if got := entry.Data["repair_duration_ms"]; got == nil {
		t.Fatal("repair_duration_ms missing")
	}
	if got := entry.Data["payload_bytes_before"]; got == nil {
		t.Fatal("payload_bytes_before missing")
	}
	if got := entry.Data["message_count"]; got != 2 {
		t.Fatalf("message_count = %#v, want 2", got)
	}
	if got := entry.Data["tool_count"]; got != 1 {
		t.Fatalf("tool_count = %#v, want 1", got)
	}
	if _, exists := entry.Data["payload"]; exists {
		t.Fatal("unexpected raw payload field logged")
	}
}

func TestRepairClaudeToolUseHistoryWithLog_EmitsAggregatedFieldsOnly(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	body := []byte(`{
		"messages": [
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{}}]},
			{"role":"user","content":[{"type":"text","text":"not a tool result"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"late"}]}
		]
	}`)
	meta := compatRepairLogMeta{
		requestedModel: "MiniMax-M2.7-highspeed",
		upstreamModel:  "MiniMax-M2.7-highspeed",
		provider:       "claude",
		executor:       "ClaudeExecutor",
		requestPath:    "/v1/chat/completions",
		compatKind:     "minimax",
		messageCount:   3,
		toolCount:      1,
	}
	ctx := logging.WithRequestID(context.Background(), "req-repair-2")

	if _, err := repairClaudeToolUseHistoryWithCompatLog(ctx, body, meta); err != nil {
		t.Fatalf("repairClaudeToolUseHistoryWithCompatLog() error = %v", err)
	}

	entry := findCompatRepairEntry(t, hook.AllEntries(), "claude_tool_use_history")
	if got := entry.Data["repairs_count"]; got == nil {
		t.Fatal("repairs_count missing")
	}
	if got := entry.Data["merged_tool_result_messages"]; got == nil {
		t.Fatal("merged_tool_result_messages missing")
	}
	if got := entry.Data["request_path"]; got != "/v1/chat/completions" {
		t.Fatalf("request_path = %#v, want /v1/chat/completions", got)
	}
	if _, exists := entry.Data["messages"]; exists {
		t.Fatal("unexpected raw messages field logged")
	}
}

func TestRejectLargeClaudeCompatToolHistory_RejectsBeforeRepair(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()

	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{}}]}]}`)
	meta := compatRepairLogMeta{
		requestedModel: "claude-sonnet-4-6",
		upstreamModel:  "MiniMax-M3",
		provider:       "claude",
		executor:       "ClaudeExecutor",
		requestPath:    "/v1/messages",
		compatKind:     "minimax",
		messageCount:   546,
		toolCount:      490,
		toolShape: coreusage.ToolShape{
			DeclaredToolCount: 78,
			InteractionCount:  490,
			MCPToolCount:      54,
		},
	}
	ctx := logging.WithRequestID(context.Background(), "req-large-tool-history")

	err := rejectLargeClaudeCompatToolHistory(ctx, body, meta, claudeCompatPreflight{hasToolUse: true})
	if err == nil {
		t.Fatal("expected large tool history rejection, got nil")
	}
	se, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", se.StatusCode(), http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "large_claude_tool_history") {
		t.Fatalf("error = %q, want large_claude_tool_history marker", err.Error())
	}
	for _, want := range []string{
		"too many prior tool calls",
		"not a channel outage",
		"Start a new conversation",
		"summarize or compress",
		"native Claude route",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}

	entry := findCompatRepairGuardEntry(t, hook.AllEntries())
	if got := entry.Data["request_id"]; got != "req-large-tool-history" {
		t.Fatalf("request_id = %#v, want req-large-tool-history", got)
	}
	if got := entry.Data["event"]; got != "compat_repair_guard" {
		t.Fatalf("event = %#v, want compat_repair_guard", got)
	}
	if got := entry.Data["reason"]; got != "message_tool_history" {
		t.Fatalf("reason = %#v, want message_tool_history", got)
	}
	if got := entry.Data["message_count"]; got != 546 {
		t.Fatalf("message_count = %#v, want 546", got)
	}
	if got := entry.Data["tool_interaction_count"]; got != 490 {
		t.Fatalf("tool_interaction_count = %#v, want 490", got)
	}
	if _, exists := entry.Data["payload"]; exists {
		t.Fatal("unexpected raw payload field logged")
	}
}

func TestRejectLargeClaudeCompatToolHistory_AllowsSmallToolHistory(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{}}]}]}`)
	meta := compatRepairLogMeta{
		requestedModel: "claude-sonnet-4-6",
		upstreamModel:  "MiniMax-M3",
		provider:       "claude",
		executor:       "ClaudeExecutor",
		requestPath:    "/v1/messages",
		compatKind:     "minimax",
		messageCount:   20,
		toolCount:      8,
	}
	if err := rejectLargeClaudeCompatToolHistory(context.Background(), body, meta, claudeCompatPreflight{hasToolUse: true}); err != nil {
		t.Fatalf("unexpected rejection for small tool history: %v", err)
	}
}

func findCompatRepairEntry(t *testing.T, entries []*log.Entry, repairType string) *log.Entry {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry == nil {
			continue
		}
		if entry.Data["event"] == "compat_repair" && entry.Data["repair_type"] == repairType {
			return entry
		}
	}
	t.Fatalf("compat_repair log entry not found for %s", repairType)
	return nil
}

func findCompatRepairGuardEntry(t *testing.T, entries []*log.Entry) *log.Entry {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry == nil {
			continue
		}
		if entry.Data["event"] == "compat_repair_guard" {
			return entry
		}
	}
	t.Fatal("compat_repair_guard log entry not found")
	return nil
}
