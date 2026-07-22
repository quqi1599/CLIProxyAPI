package pluginhost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

func TestModelStreamBridgeOpenRejectsUnavailableStream(t *testing.T) {
	bridge := newModelStreamBridge()
	var canceled atomic.Int32
	id, errOpen := bridge.open("callback-1", nil, func() { canceled.Add(1) })
	if !errors.Is(errOpen, errModelStreamBridgeUnavailable) || id != "" {
		t.Fatalf("open() = %q, %v; want empty id and unavailable error", id, errOpen)
	}
	if canceled.Load() != 1 {
		t.Fatalf("cancel count = %d, want 1", canceled.Load())
	}
	if len(bridge.streams) != 0 {
		t.Fatalf("stream count = %d, want 0", len(bridge.streams))
	}
}

func TestModelStreamBridgeRejectsWhenTooManyOpen(t *testing.T) {
	bridge := newModelStreamBridge()
	ids := make([]string, 0, hostModelStreamMaxOpen)
	for range hostModelStreamMaxOpen {
		id, errOpen := bridge.open("callback-1", make(chan handlers.ModelExecutionChunk), nil)
		if errOpen != nil {
			t.Fatalf("open() before limit: %v", errOpen)
		}
		ids = append(ids, id)
	}

	var canceled atomic.Int32
	id, errOpen := bridge.open("callback-2", make(chan handlers.ModelExecutionChunk), func() { canceled.Add(1) })
	if !errors.Is(errOpen, errTooManyOpenModelStreams) || id != "" {
		t.Fatalf("open() at limit = %q, %v; want empty id and limit error", id, errOpen)
	}
	if canceled.Load() != 1 {
		t.Fatalf("rejected stream cancel count = %d, want 1", canceled.Load())
	}
	if len(bridge.streams) != hostModelStreamMaxOpen {
		t.Fatalf("stream count = %d, want %d", len(bridge.streams), hostModelStreamMaxOpen)
	}
	for _, streamID := range ids {
		bridge.close(streamID)
	}
}

func TestModelStreamBridgeConcurrentCloseUnblocksRead(t *testing.T) {
	bridge := newModelStreamBridge()
	var canceled atomic.Int32
	id, errOpen := bridge.open("callback-1", make(chan handlers.ModelExecutionChunk), func() { canceled.Add(1) })
	if errOpen != nil {
		t.Fatalf("open(): %v", errOpen)
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		chunk, done, errRead := bridge.read(context.Background(), id)
		if errRead != nil || !done || len(chunk.Payload) != 0 || chunk.Err != nil {
			t.Errorf("read() = %#v, %v, %v; want empty done result", chunk, done, errRead)
		}
	}()

	var closes sync.WaitGroup
	for range 16 {
		closes.Add(1)
		go func() {
			defer closes.Done()
			bridge.close(id)
		}()
	}
	closes.Wait()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("read remained blocked after concurrent close")
	}
	if canceled.Load() != 1 {
		t.Fatalf("cancel count = %d, want 1", canceled.Load())
	}
	if len(bridge.streams) != 0 {
		t.Fatalf("stream count = %d, want 0", len(bridge.streams))
	}
}

func TestModelStreamBridgeReadTerminalPathsReleaseOnce(t *testing.T) {
	t.Run("eof", func(t *testing.T) {
		bridge := newModelStreamBridge()
		chunks := make(chan handlers.ModelExecutionChunk)
		close(chunks)
		var canceled atomic.Int32
		id, errOpen := bridge.open("callback-1", chunks, func() { canceled.Add(1) })
		if errOpen != nil {
			t.Fatalf("open(): %v", errOpen)
		}
		_, done, errRead := bridge.read(context.Background(), id)
		if errRead != nil || !done {
			t.Fatalf("read() = done %v, error %v; want EOF done", done, errRead)
		}
		bridge.close(id)
		if canceled.Load() != 1 || len(bridge.streams) != 0 {
			t.Fatalf("cancel count = %d, stream count = %d; want 1, 0", canceled.Load(), len(bridge.streams))
		}
	})

	t.Run("caller cancel", func(t *testing.T) {
		bridge := newModelStreamBridge()
		var canceled atomic.Int32
		id, errOpen := bridge.open("callback-1", make(chan handlers.ModelExecutionChunk), func() { canceled.Add(1) })
		if errOpen != nil {
			t.Fatalf("open(): %v", errOpen)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, done, errRead := bridge.read(ctx, id)
		if !errors.Is(errRead, context.Canceled) || !done {
			t.Fatalf("read() = done %v, error %v; want canceled done", done, errRead)
		}
		bridge.close(id)
		if canceled.Load() != 1 || len(bridge.streams) != 0 {
			t.Fatalf("cancel count = %d, stream count = %d; want 1, 0", canceled.Load(), len(bridge.streams))
		}
	})
}
