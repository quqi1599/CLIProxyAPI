package pluginhost

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const streamBridgeTestTimeout = 2 * time.Second

func TestStreamBridgeCloseWithFullOutputDoesNotBlock(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	state := streamBridgeStateForTest(t, bridge, streamID)

	for i := 0; i < cap(chunks); i++ {
		if errEmit := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte{byte(i)}}); errEmit != nil {
			t.Fatalf("emit chunk %d: %v", i, errEmit)
		}
	}

	closeReturned := make(chan struct{})
	go func() {
		bridge.close(streamID, "stream canceled")
		close(closeReturned)
	}()
	waitForStreamBridgeSignal(t, closeReturned, "close return")
	waitForStreamBridgeSignal(t, state.finished, "stream pump exit")

	var payloads int
	for chunk := range chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected terminal error in saturated output: %v", chunk.Err)
		}
		if len(chunk.Payload) != 1 || chunk.Payload[0] != byte(payloads) {
			t.Fatalf("payload %d = %v", payloads, chunk.Payload)
		}
		payloads++
	}
	if payloads != cap(chunks) {
		t.Fatalf("payload count = %d, want %d", payloads, cap(chunks))
	}
}

func TestStreamBridgeCloseUnblocksEmitOnFullOutput(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	state := streamBridgeStateForTest(t, bridge, streamID)

	for i := 0; i < cap(chunks)-1; i++ {
		if errEmit := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte{byte(i)}}); errEmit != nil {
			t.Fatalf("emit chunk %d: %v", i, errEmit)
		}
	}
	emitResults := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(index int) {
			emitResults <- bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte{byte(100 + index)}})
		}(i)
	}

	select {
	case errEmit := <-emitResults:
		if errEmit != nil {
			t.Fatalf("first concurrent emit: %v", errEmit)
		}
	case <-time.After(streamBridgeTestTimeout):
		t.Fatal("timed out waiting for output to become full")
	}

	bridge.close(streamID, "stream canceled")
	select {
	case errEmit := <-emitResults:
		if errEmit == nil || !strings.Contains(errEmit.Error(), "is not open") {
			t.Fatalf("blocked emit error = %v, want stream-not-open error", errEmit)
		}
	case <-time.After(streamBridgeTestTimeout):
		t.Fatal("blocked emit did not return after close")
	}
	waitForStreamBridgeSignal(t, state.finished, "stream pump exit")

	var payloads int
	for chunk := range chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected terminal error in saturated output: %v", chunk.Err)
		}
		payloads++
	}
	if payloads != cap(chunks) {
		t.Fatalf("payload count = %d, want %d", payloads, cap(chunks))
	}
}

func TestStreamBridgeConcurrentEmitAndClose(t *testing.T) {
	const (
		rounds   = 25
		emitters = 32
	)
	for round := 0; round < rounds; round++ {
		bridge := newStreamBridge()
		streamID, chunks, cleanup := bridge.open(context.Background())
		state := streamBridgeStateForTest(t, bridge, streamID)
		start := make(chan struct{})
		errorsCh := make(chan error, emitters)
		var emitWG sync.WaitGroup
		emitWG.Add(emitters)
		for i := 0; i < emitters; i++ {
			go func(index int) {
				defer emitWG.Done()
				<-start
				errEmit := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte{byte(index)}})
				if errEmit != nil && !strings.Contains(errEmit.Error(), "is not open") {
					errorsCh <- errEmit
				}
			}(i)
		}

		consumerDone := make(chan struct{})
		go func() {
			defer close(consumerDone)
			for range chunks {
			}
		}()
		close(start)

		var closeWG sync.WaitGroup
		closeWG.Add(4)
		for i := 0; i < 4; i++ {
			go func() {
				defer closeWG.Done()
				bridge.close(streamID, "")
			}()
		}
		closeWG.Wait()
		emitWG.Wait()
		close(errorsCh)
		for errEmit := range errorsCh {
			t.Fatalf("round %d emit error: %v", round, errEmit)
		}
		waitForStreamBridgeSignal(t, state.finished, fmt.Sprintf("round %d stream pump exit", round))
		waitForStreamBridgeSignal(t, consumerDone, fmt.Sprintf("round %d consumer exit", round))
		cleanup()
	}
}

func TestStreamBridgeCloseIsIdempotent(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, cleanup := bridge.open(context.Background())
	state := streamBridgeStateForTest(t, bridge, streamID)

	var closeWG sync.WaitGroup
	closeWG.Add(16)
	for i := 0; i < 16; i++ {
		go func(index int) {
			defer closeWG.Done()
			if index%2 == 0 {
				bridge.close(streamID, "")
				return
			}
			cleanup()
		}(i)
	}
	closeWG.Wait()
	waitForStreamBridgeSignal(t, state.finished, "stream pump exit")
	if _, ok := <-chunks; ok {
		t.Fatal("stream remains open after repeated close")
	}
	if errEmit := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{}); errEmit == nil || !strings.Contains(errEmit.Error(), "is not open") {
		t.Fatalf("emit after close error = %v, want stream-not-open error", errEmit)
	}
}

func TestStreamBridgeCloseDeliversErrorWhenOutputHasCapacity(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	state := streamBridgeStateForTest(t, bridge, streamID)

	bridge.close(streamID, "upstream failed")
	waitForStreamBridgeSignal(t, state.finished, "stream pump exit")
	chunk, ok := <-chunks
	if !ok {
		t.Fatal("stream closed before terminal error")
	}
	if chunk.Err == nil || chunk.Err.Error() != "upstream failed" {
		t.Fatalf("terminal error = %v, want upstream failed", chunk.Err)
	}
	if _, ok = <-chunks; ok {
		t.Fatal("stream remains open after terminal error")
	}
}

func streamBridgeStateForTest(t *testing.T, bridge *streamBridge, streamID string) *streamBridgeState {
	t.Helper()
	bridge.mu.Lock()
	state := bridge.streams[streamID]
	bridge.mu.Unlock()
	if state == nil {
		t.Fatalf("stream %s is not open", streamID)
	}
	return state
}

func waitForStreamBridgeSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(streamBridgeTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}
