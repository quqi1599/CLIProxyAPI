package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

type providerWebsocketTestFrameReader struct {
	started chan struct{}
	release <-chan struct{}
	reader  io.Reader
	once    sync.Once
}

func (r *providerWebsocketTestFrameReader) NextReader() (int, io.Reader, error) {
	r.once.Do(func() {
		if r.started != nil {
			close(r.started)
		}
	})
	if r.release != nil {
		<-r.release
	}
	return websocket.TextMessage, r.reader, nil
}

func (*providerWebsocketTestFrameReader) SetReadDeadline(time.Time) error {
	return nil
}

type providerWebsocketGatedPayloadReader struct {
	started chan<- struct{}
	release <-chan struct{}
	reader  *bytes.Reader
	once    sync.Once
}

func (r *providerWebsocketGatedPayloadReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		r.started <- struct{}{}
		<-r.release
	})
	return r.reader.Read(p)
}

type providerWebsocketFrameResult struct {
	frame codexWebsocketRead
	err   error
}

func TestProviderWebsocketIdleReadersDoNotReserveByteBudget(t *testing.T) {
	const (
		readerCount = 4
		readLimit   = int64(50)
	)
	budget := helps.NewByteBudget(128)
	headerRelease := make(chan struct{})
	results := make(chan providerWebsocketFrameResult, readerCount)
	started := make([]chan struct{}, readerCount)

	for i := range readerCount {
		started[i] = make(chan struct{})
		source := &providerWebsocketTestFrameReader{
			started: started[i],
			release: headerRelease,
			reader:  bytes.NewReader([]byte{'a' + byte(i)}),
		}
		go func() {
			frame, err := readProviderWebsocketFrameWithLimit(context.Background(), nil, source, budget, readLimit)
			results <- providerWebsocketFrameResult{frame: frame, err: err}
		}()
	}

	for _, readerStarted := range started {
		<-readerStarted
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("idle readers reserved %d bytes, want 0", got)
	}

	close(headerRelease)
	frames := make([]codexWebsocketRead, 0, readerCount)
	for range readerCount {
		result := <-results
		if result.err != nil {
			t.Fatalf("read frame: %v", result.err)
		}
		frames = append(frames, result.frame)
	}
	if got := budget.InUse(); got != readerCount {
		t.Fatalf("retained bytes = %d, want %d", got, readerCount)
	}
	for _, frame := range frames {
		frame.release()
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes after release = %d, want 0", got)
	}
}

func TestProviderWebsocketConcurrentReadsRespectByteBudget(t *testing.T) {
	const (
		readerCount = 4
		readLimit   = int64(50)
		payloadSize = 10
	)
	budget := helps.NewByteBudget(128)
	payloadRelease := make(chan struct{})
	payloadStarted := make(chan struct{}, readerCount)
	results := make(chan providerWebsocketFrameResult, readerCount)
	headerStarted := make([]chan struct{}, readerCount)

	for i := range readerCount {
		headerStarted[i] = make(chan struct{})
		payload := bytes.Repeat([]byte{byte('a' + i)}, payloadSize)
		source := &providerWebsocketTestFrameReader{
			started: headerStarted[i],
			reader: &providerWebsocketGatedPayloadReader{
				started: payloadStarted,
				release: payloadRelease,
				reader:  bytes.NewReader(payload),
			},
		}
		go func() {
			frame, err := readProviderWebsocketFrameWithLimit(context.Background(), nil, source, budget, readLimit)
			results <- providerWebsocketFrameResult{frame: frame, err: err}
		}()
	}

	for _, readerStarted := range headerStarted {
		<-readerStarted
	}
	<-payloadStarted
	<-payloadStarted
	select {
	case <-payloadStarted:
		t.Fatal("a third payload reader started while two 50-byte reservations were held")
	default:
	}
	if got := budget.InUse(); got != 2*readLimit {
		t.Fatalf("bytes during saturation = %d, want %d", got, 2*readLimit)
	}

	close(payloadRelease)
	frames := make([]codexWebsocketRead, 0, readerCount)
	for range readerCount {
		result := <-results
		if result.err != nil {
			t.Fatalf("read frame: %v", result.err)
		}
		frames = append(frames, result.frame)
	}
	if got := budget.InUse(); got != readerCount*payloadSize {
		t.Fatalf("retained bytes = %d, want %d", got, readerCount*payloadSize)
	}
	for _, frame := range frames {
		frame.release()
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes after release = %d, want 0", got)
	}
}

func TestProviderWebsocketCanceledBudgetWaitDoesNotReadOrLeak(t *testing.T) {
	budget := helps.NewByteBudget(100)
	held, err := budget.Acquire(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	source := &providerWebsocketTestFrameReader{
		started: started,
		reader:  bytes.NewReader([]byte("payload")),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan providerWebsocketFrameResult, 1)
	go func() {
		frame, errRead := readProviderWebsocketFrameWithLimit(ctx, nil, source, budget, 50)
		result <- providerWebsocketFrameResult{frame: frame, err: errRead}
	}()

	<-started
	cancel()
	got := <-result
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("read error = %v, want context canceled", got.err)
	}
	if got.frame.lease != nil {
		t.Fatal("canceled read returned a byte lease")
	}
	if inUse := budget.InUse(); inUse != 100 {
		t.Fatalf("bytes after cancellation = %d, want 100", inUse)
	}
	held.Release()
	if inUse := budget.InUse(); inUse != 0 {
		t.Fatalf("bytes after release = %d, want 0", inUse)
	}
}

func TestProviderWebsocketOversizeFrameReleasesByteBudget(t *testing.T) {
	budget := helps.NewByteBudget(8)
	source := &providerWebsocketTestFrameReader{reader: bytes.NewReader([]byte("12345"))}
	frame, err := readProviderWebsocketFrameWithLimit(context.Background(), nil, source, budget, 4)
	if !errors.Is(err, websocket.ErrReadLimit) {
		t.Fatalf("read error = %v, want websocket read limit", err)
	}
	if frame.lease != nil {
		t.Fatal("oversize frame returned a byte lease")
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes after oversize frame = %d, want 0", got)
	}
}

func TestProviderWebsocketFrameLeaseHeldThroughCodexAndXAIConsumption(t *testing.T) {
	tests := []struct {
		name string
		read func(context.Context, *codexWebsocketSession, *websocket.Conn, chan codexWebsocketRead) (codexWebsocketRead, error)
	}{
		{name: "codex", read: readCodexWebsocketMessage},
		{name: "xai", read: readXAIWebsocketMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := helps.NewByteBudget(16)
			lease, err := budget.Acquire(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			conn := &websocket.Conn{}
			sess := &codexWebsocketSession{readBudget: budget}
			readCh := make(chan codexWebsocketRead, 1)
			sess.setActive(readCh)
			if !sess.enqueueActiveRead(codexWebsocketRead{
				conn:    conn,
				msgType: websocket.TextMessage,
				payload: []byte("payload"),
				lease:   lease,
			}) {
				t.Fatal("failed to enqueue active frame")
			}

			frame, errRead := test.read(context.Background(), sess, conn, readCh)
			if errRead != nil {
				t.Fatal(errRead)
			}
			sess.clearActive(readCh)
			if got := budget.InUse(); got != 7 {
				t.Fatalf("bytes after dequeue = %d, want 7", got)
			}
			frame.release()
			if got := budget.InUse(); got != 0 {
				t.Fatalf("bytes after consumption = %d, want 0", got)
			}
		})
	}
}

func TestProviderWebsocketFrameLeaseReleasedOnDropAndCancel(t *testing.T) {
	budget := helps.NewByteBudget(20)
	firstLease, err := budget.Acquire(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := budget.Acquire(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}

	sess := &codexWebsocketSession{readBudget: budget}
	readCh := make(chan codexWebsocketRead, 1)
	sess.setActive(readCh)
	if !sess.enqueueActiveRead(codexWebsocketRead{payload: []byte("queued"), lease: firstLease}) {
		t.Fatal("failed to enqueue first frame")
	}
	enqueueResult := make(chan bool, 1)
	go func() {
		enqueueResult <- sess.enqueueActiveRead(codexWebsocketRead{payload: []byte("blocked"), lease: secondLease})
	}()

	sess.cancelActive()
	if enqueued := <-enqueueResult; enqueued {
		t.Fatal("blocked frame was enqueued after active request cancellation")
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes after drop and cancellation = %d, want 0", got)
	}
}

func TestProviderWebsocketSessionCloseReleasesQueuedFrame(t *testing.T) {
	tests := []struct {
		name  string
		close func(*codexWebsocketSession, string)
	}{
		{name: "codex", close: closeCodexWebsocketSession},
		{name: "xai", close: closeXAIWebsocketSession},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := helps.NewByteBudget(16)
			lease, err := budget.Acquire(context.Background(), 8)
			if err != nil {
				t.Fatal(err)
			}
			sess := &codexWebsocketSession{readBudget: budget}
			readCh := make(chan codexWebsocketRead, 1)
			sess.setActive(readCh)
			if !sess.enqueueActiveRead(codexWebsocketRead{payload: []byte("queued"), lease: lease}) {
				t.Fatal("failed to enqueue frame")
			}

			test.close(sess, "test_close")
			if got := budget.InUse(); got != 0 {
				t.Fatalf("bytes after close = %d, want 0", got)
			}
		})
	}
}

func TestProviderWebsocketMismatchedFrameDropReleasesLease(t *testing.T) {
	tests := []struct {
		name string
		read func(context.Context, *codexWebsocketSession, *websocket.Conn, chan codexWebsocketRead) (codexWebsocketRead, error)
	}{
		{name: "codex", read: readCodexWebsocketMessage},
		{name: "xai", read: readXAIWebsocketMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget := helps.NewByteBudget(8)
			lease, err := budget.Acquire(context.Background(), 4)
			if err != nil {
				t.Fatal(err)
			}
			readCh := make(chan codexWebsocketRead, 1)
			readCh <- codexWebsocketRead{conn: &websocket.Conn{}, lease: lease}
			close(readCh)

			_, errRead := test.read(context.Background(), &codexWebsocketSession{readBudget: budget}, &websocket.Conn{}, readCh)
			if errRead == nil {
				t.Fatal("read error = nil, want closed channel error")
			}
			if got := budget.InUse(); got != 0 {
				t.Fatalf("bytes after mismatched frame drop = %d, want 0", got)
			}
		})
	}
}
