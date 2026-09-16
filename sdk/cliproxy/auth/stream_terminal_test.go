package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestStreamTerminalFailureOverridesEarlierStopAndHTTP200(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	ctx := logging.WithResponseStatusHolder(logging.WithRequestID(context.Background(), "terminal-error"))
	logging.SetResponseStatus(ctx, 200)
	ctx, trace := ensureRequestAttemptTrace(ctx)
	ctx = coreusage.WithRequestAttempt(ctx, trace.nextAttempt(""))
	ctx = WithStreamSummaryTracking(ctx)
	m := NewManager(nil, nil, nil)
	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Err: errors.New("upstream disconnected")}
	close(remaining)
	stream := m.wrapStreamResult(ctx, &Auth{ID: "terminal-auth"}, streamExecutionLogMeta{requestedModel: "gpt-5.6-sol", provider: "codex"}, "", nil,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{\"content\":\"partial\"}}]}\n\n")}}, remaining, nil, time.Now(), time.Millisecond, nil, nil)
	for range stream.Chunks {
	}
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "stream_execution_summary" {
			t.Fatal("published before downstream completion")
		}
	}
	MarkStreamSummaryDownstreamDone(ctx)
	MarkStreamSummaryDownstreamDone(ctx)
	var summaries int
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] != "stream_execution_summary" {
			continue
		}
		summaries++
		if entry.Data["final_success"] != false || entry.Data["http_status"] != 200 || entry.Data["terminal_outcome"] != "failed" || entry.Data["finish_reason"] != "error" {
			t.Fatalf("incorrect terminal fields: %#v", entry.Data)
		}
	}
	if summaries != 1 {
		t.Fatalf("terminal summaries=%d, want 1", summaries)
	}
}

func TestStreamTerminalConcurrentDrainReleasesTracking(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	m := NewManager(nil, nil, nil)
	var workers sync.WaitGroup
	const requests = 64
	for i := 0; i < requests; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			ctx := WithStreamSummaryTracking(logging.WithRequestID(context.Background(), fmt.Sprintf("burst-%d", i)))
			remaining := make(chan cliproxyexecutor.StreamChunk, 1)
			if i%2 == 0 {
				remaining <- cliproxyexecutor.StreamChunk{Err: errors.New("synthetic stream failure")}
			}
			close(remaining)
			stream := m.wrapStreamResult(ctx, &Auth{ID: fmt.Sprintf("burst-auth-%d", i)}, streamExecutionLogMeta{requestedModel: "test", provider: "claude"}, "", nil,
				[]cliproxyexecutor.StreamChunk{{Payload: []byte("partial")}}, remaining, nil, time.Now(), time.Millisecond, nil, nil)
			for range stream.Chunks {
			}
			MarkStreamSummaryDownstreamDone(ctx)
			MarkStreamSummaryDownstreamDone(ctx)
		}(i)
	}
	workers.Wait()
	completed, failed := 0, 0
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] != "stream_execution_summary" {
			continue
		}
		if entry.Data["final_success"] == true {
			completed++
		} else {
			failed++
		}
	}
	if completed != requests/2 || failed != requests/2 || m.ActiveStreamSnapshot().ActiveStreamsTotal != 0 {
		t.Fatalf("completed=%d failed=%d active=%d", completed, failed, m.ActiveStreamSnapshot().ActiveStreamsTotal)
	}
}

func TestStreamingAdmissionDoesNotPublishPrematureFinalSummary(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	ctx := logging.WithRequestID(context.Background(), "stream-admitted")
	m := NewManager(nil, nil, nil)
	runner := managerAttemptRunner[*cliproxyexecutor.StreamResult]{manager: m,
		runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (*cliproxyexecutor.StreamResult, error) {
			return &cliproxyexecutor.StreamResult{}, nil
		}}
	_, err := runManagerAttemptOperation(ctx, m, []string{"claude"}, cliproxyexecutor.Request{Model: "claude-sonnet-4-6"}, cliproxyexecutor.Options{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "request_execution_summary" {
			t.Fatal("admitted stream was marked complete")
		}
	}
}
