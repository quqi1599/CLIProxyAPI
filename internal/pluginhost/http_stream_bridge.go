package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var hostHTTPStreamMaxOpen = 256

type hostHTTPStreamBridge struct {
	next    atomic.Uint64
	mu      sync.Mutex
	streams map[string]hostHTTPStreamEntry
}

type hostHTTPStreamEntry struct {
	ownerCallbackID string
	chunks          <-chan pluginapi.HTTPStreamChunk
	cancel          context.CancelFunc
}

func newHostHTTPStreamBridge() *hostHTTPStreamBridge {
	return &hostHTTPStreamBridge{streams: make(map[string]hostHTTPStreamEntry)}
}

func (b *hostHTTPStreamBridge) open(ownerCallbackID string, chunks <-chan pluginapi.HTTPStreamChunk, cancel context.CancelFunc) (string, error) {
	if b == nil || chunks == nil {
		if cancel != nil {
			cancel()
		}
		return "", fmt.Errorf("host http stream bridge is unavailable")
	}
	id := strconv.FormatUint(b.next.Add(1), 10)
	b.mu.Lock()
	if hostHTTPStreamMaxOpen > 0 && len(b.streams) >= hostHTTPStreamMaxOpen {
		b.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return "", fmt.Errorf("too many open host http streams")
	}
	b.streams[id] = hostHTTPStreamEntry{
		ownerCallbackID: ownerCallbackID,
		chunks:          chunks,
		cancel:          cancel,
	}
	b.mu.Unlock()
	return id, nil
}

func (b *hostHTTPStreamBridge) read(ctx context.Context, id string) (pluginapi.HTTPStreamChunk, bool, error) {
	if b == nil || id == "" {
		return pluginapi.HTTPStreamChunk{}, true, fmt.Errorf("http stream id is required")
	}
	b.mu.Lock()
	entry, ok := b.streams[id]
	b.mu.Unlock()
	if !ok || entry.chunks == nil {
		return pluginapi.HTTPStreamChunk{}, true, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		b.close(id)
		return pluginapi.HTTPStreamChunk{}, true, ctx.Err()
	case chunk, ok := <-entry.chunks:
		if !ok {
			b.close(id)
			return pluginapi.HTTPStreamChunk{}, true, nil
		}
		if chunk.Err != nil {
			b.close(id)
			return chunk, true, nil
		}
		return chunk, false, nil
	}
}

func (b *hostHTTPStreamBridge) close(id string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	entry := b.streams[id]
	delete(b.streams, id)
	b.mu.Unlock()
	if entry.cancel != nil {
		entry.cancel()
	}
}
