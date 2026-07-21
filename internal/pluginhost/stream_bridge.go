package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamBridge struct {
	next    atomic.Uint64
	mu      sync.Mutex
	streams map[string]*streamBridgeState
}

type streamBridgeState struct {
	id         string
	emits      chan streamBridgeEmit
	chunks     chan pluginapi.ExecutorStreamChunk
	done       chan struct{}
	finished   chan struct{}
	closeOnce  sync.Once
	closeError string
}

type streamBridgeEmit struct {
	ctx    context.Context
	chunk  pluginapi.ExecutorStreamChunk
	result chan error
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func newStreamBridge() *streamBridge {
	return &streamBridge{streams: make(map[string]*streamBridgeState)}
}

func (b *streamBridge) open(ctx context.Context) (string, <-chan pluginapi.ExecutorStreamChunk, func()) {
	if b == nil {
		chunks := make(chan pluginapi.ExecutorStreamChunk)
		close(chunks)
		return "", chunks, func() {}
	}
	id := strconv.FormatUint(b.next.Add(1), 10)
	state := &streamBridgeState{
		id:       id,
		emits:    make(chan streamBridgeEmit),
		chunks:   make(chan pluginapi.ExecutorStreamChunk, 16),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	b.mu.Lock()
	b.streams[id] = state
	b.mu.Unlock()
	go state.run()
	cleanup := func() {
		b.close(id, "")
	}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				b.close(id, ctx.Err().Error())
			case <-state.finished:
			}
		}()
	}
	return id, state.chunks, cleanup
}

func (b *streamBridge) emit(ctx context.Context, id string, chunk pluginapi.ExecutorStreamChunk) error {
	if b == nil || id == "" {
		return fmt.Errorf("stream id is required")
	}
	b.mu.Lock()
	state := b.streams[id]
	b.mu.Unlock()
	if state == nil {
		return fmt.Errorf("stream %s is not open", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	emit := streamBridgeEmit{
		ctx:    ctx,
		chunk:  chunk,
		result: make(chan error, 1),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-state.done:
		return fmt.Errorf("stream %s is not open", id)
	case state.emits <- emit:
	}
	return <-emit.result
}

func (b *streamBridge) close(id string, errorMessage string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	state := b.streams[id]
	if state != nil {
		delete(b.streams, id)
	}
	b.mu.Unlock()
	if state == nil {
		return
	}
	state.stop(errorMessage)
}

func (s *streamBridgeState) run() {
	defer close(s.finished)
	defer close(s.chunks)
	for {
		select {
		case <-s.done:
			s.emitCloseError()
			return
		case emit := <-s.emits:
			select {
			case <-s.done:
				emit.result <- fmt.Errorf("stream %s is not open", s.id)
			case <-emit.ctx.Done():
				emit.result <- emit.ctx.Err()
			case s.chunks <- emit.chunk:
				emit.result <- nil
			}
		}
	}
}

func (s *streamBridgeState) stop(errorMessage string) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.closeError = errorMessage
		close(s.done)
	})
}

func (s *streamBridgeState) emitCloseError() {
	if s == nil || s.closeError == "" {
		return
	}
	// Closing must never wait for a saturated output channel. Payloads already
	// accepted into chunks remain readable; the terminal error is best effort
	// when the downstream has stopped consuming.
	select {
	case s.chunks <- pluginapi.ExecutorStreamChunk{Err: fmt.Errorf("%s", s.closeError)}:
	default:
	}
}
