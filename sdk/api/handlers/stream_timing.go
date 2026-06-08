package handlers

import (
	"context"
	"net/http"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type StreamBodyWriter interface {
	Write([]byte) (int, error)
}

type observedStreamWriter struct {
	ctx    context.Context
	writer StreamBodyWriter
}

func (w observedStreamWriter) Write(payload []byte) (int, error) {
	if w.writer == nil {
		return 0, nil
	}
	startedAt := time.Now()
	n, err := w.writer.Write(payload)
	coreauth.ObserveStreamDownstreamWrite(w.ctx, time.Since(startedAt))
	return n, err
}

func MeasureStreamWrite(ctx context.Context, writer StreamBodyWriter, write func(StreamBodyWriter)) {
	if write == nil {
		return
	}
	write(observedStreamWriter{ctx: ctx, writer: writer})
}

func FlushObservedStream(ctx context.Context, flusher http.Flusher) {
	if flusher == nil {
		return
	}
	startedAt := time.Now()
	flusher.Flush()
	coreauth.ObserveStreamDownstreamFlush(ctx, time.Since(startedAt))
}

func WriteStreamChunkAndFlush(ctx context.Context, writer StreamBodyWriter, flusher http.Flusher, write func(StreamBodyWriter)) {
	MeasureStreamWrite(ctx, writer, write)
	FlushObservedStream(ctx, flusher)
}
