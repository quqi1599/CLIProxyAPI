package wsrelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/gorilla/websocket"
)

const inboundReadQuantum = 32 << 10

var (
	errInboundFrameTooLarge     = errors.New("wsrelay: inbound frame exceeds limit")
	errInboundMalformedJSON     = errors.New("wsrelay: inbound frame contains malformed JSON")
	errInboundMultipleJSON      = errors.New("wsrelay: inbound frame contains multiple JSON values")
	errInboundTrailingData      = errors.New("wsrelay: inbound frame contains trailing data")
	errInboundBudgetLeaseClosed = errors.New("wsrelay: inbound frame budget lease is closed")
)

// Message represents the JSON payload exchanged with websocket clients.
type Message struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
	release func()
}

// Release relinquishes the inbound byte budget retained for this message.
// Consumers of channels returned by Manager.Send must call Release after they
// finish reading Payload. Release is safe to call more than once.
func (m *Message) Release() {
	if m == nil || m.release == nil {
		return
	}
	m.release()
	m.release = nil
}

const (
	// MessageTypeHTTPReq identifies an HTTP-style request envelope.
	MessageTypeHTTPReq = "http_request"
	// MessageTypeHTTPResp identifies a non-streaming HTTP response envelope.
	MessageTypeHTTPResp = "http_response"
	// MessageTypeStreamStart marks the beginning of a streaming response.
	MessageTypeStreamStart = "stream_start"
	// MessageTypeStreamChunk carries a streaming response chunk.
	MessageTypeStreamChunk = "stream_chunk"
	// MessageTypeStreamEnd marks the completion of a streaming response.
	MessageTypeStreamEnd = "stream_end"
	// MessageTypeError carries an error response.
	MessageTypeError = "error"
	// MessageTypePing represents ping messages from clients.
	MessageTypePing = "ping"
	// MessageTypePong represents pong responses back to clients.
	MessageTypePong = "pong"
)

// inboundBudget bounds websocket frame bytes from incremental read through the
// decoded message consumer's explicit Release. One frame may become the
// progress owner while all other frames share capacity minus one full-frame
// reserve, preventing partial-read deadlocks when the global budget is saturated.
type inboundBudget struct {
	mutex      sync.Mutex
	limit      int64
	frameLimit int64
	used       int64
	peak       int64
	owner      *inboundFrameLease
	changed    chan struct{}
}

type inboundFrameLease struct {
	budget   *inboundBudget
	held     int64
	released bool
}

func newInboundBudget(limit, frameLimit int64) *inboundBudget {
	if frameLimit <= 0 {
		frameLimit = maxInboundMessageLen
	}
	if limit < frameLimit {
		limit = frameLimit
	}
	return &inboundBudget{
		limit:      limit,
		frameLimit: frameLimit,
		changed:    make(chan struct{}),
	}
}

func (b *inboundBudget) newFrameLease() *inboundFrameLease {
	return &inboundFrameLease{budget: b}
}

func (l *inboundFrameLease) reserve(ctx context.Context, size int64) error {
	if size <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b := l.budget
	if b == nil {
		return errInboundBudgetLeaseClosed
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mutex.Lock()
		if l.released {
			b.mutex.Unlock()
			return errInboundBudgetLeaseClosed
		}
		if l.held > b.frameLimit-size {
			b.mutex.Unlock()
			return errInboundFrameTooLarge
		}
		ownerChanged := false
		allowed := false
		switch {
		case b.owner == l:
			allowed = b.used <= b.limit-size
		default:
			nonOwnerUsed := b.used
			if b.owner != nil {
				nonOwnerUsed -= b.owner.held
			}
			allowed = nonOwnerUsed <= b.limit-b.frameLimit-size
			if !allowed && b.owner == nil && b.used <= b.limit-size {
				b.owner = l
				ownerChanged = true
				allowed = true
			}
		}
		if allowed {
			b.used += size
			l.held += size
			if b.used > b.peak {
				b.peak = b.used
			}
			if ownerChanged {
				b.signalLocked()
			}
			b.mutex.Unlock()
			return nil
		}
		changed := b.changed
		b.mutex.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (l *inboundFrameLease) releaseBytes(size int64) {
	if l == nil || l.budget == nil || size <= 0 {
		return
	}
	b := l.budget
	b.mutex.Lock()
	if size > l.held {
		size = l.held
	}
	l.held -= size
	b.used -= size
	b.signalLocked()
	b.mutex.Unlock()
}

func (l *inboundFrameLease) release() {
	if l == nil || l.budget == nil {
		return
	}
	b := l.budget
	b.mutex.Lock()
	if l.released {
		b.mutex.Unlock()
		return
	}
	l.released = true
	b.used -= l.held
	l.held = 0
	if b.owner == l {
		b.owner = nil
	}
	b.signalLocked()
	b.mutex.Unlock()
}

func (l *inboundFrameLease) heldBytes() int64 {
	if l == nil || l.budget == nil {
		return 0
	}
	l.budget.mutex.Lock()
	held := l.held
	l.budget.mutex.Unlock()
	return held
}

func (b *inboundBudget) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *inboundBudget) snapshot() (used, peak int64) {
	if b == nil {
		return 0, 0
	}
	b.mutex.Lock()
	used, peak = b.used, b.peak
	b.mutex.Unlock()
	return used, peak
}

type inboundBudgetReader struct {
	source io.Reader
	lease  *inboundFrameLease
	ctx    context.Context
}

func (r *inboundBudgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r == nil || r.source == nil || r.lease == nil || r.lease.budget == nil {
		return 0, errInboundBudgetLeaseClosed
	}
	held := r.lease.heldBytes()
	remaining := r.lease.budget.frameLimit - held
	if remaining <= 0 {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, errInboundFrameTooLarge
		}
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		if errors.Is(err, websocket.ErrReadLimit) {
			return 0, errInboundFrameTooLarge
		}
		if err != nil {
			return 0, err
		}
		return 0, io.ErrNoProgress
	}
	request := int64(len(p))
	if request > inboundReadQuantum {
		request = inboundReadQuantum
	}
	if request > remaining {
		request = remaining
	}
	if err := r.lease.reserve(r.ctx, request); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p[:int(request)])
	if unused := request - int64(n); unused > 0 {
		r.lease.releaseBytes(unused)
	}
	if errors.Is(err, websocket.ErrReadLimit) {
		err = errInboundFrameTooLarge
	}
	return n, err
}

func decodeInboundMessage(ctx context.Context, budget *inboundBudget, source io.Reader) (Message, error) {
	if budget == nil || source == nil {
		return Message{}, errInboundBudgetLeaseClosed
	}
	lease := budget.newFrameLease()
	keepLease := false
	defer func() {
		if !keepLease {
			lease.release()
		}
	}()
	decoder := json.NewDecoder(&inboundBudgetReader{source: source, lease: lease, ctx: ctx})
	var msg Message
	if err := decoder.Decode(&msg); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errInboundFrameTooLarge) {
			return Message{}, err
		}
		return Message{}, errInboundMalformedJSON
	}
	var trailing rejectAdditionalJSONValue
	switch err := decoder.Decode(&trailing); {
	case errors.Is(err, io.EOF):
		msg.release = lease.release
		keepLease = true
		return msg, nil
	case errors.Is(err, errInboundMultipleJSON):
		return Message{}, errInboundMultipleJSON
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, errInboundFrameTooLarge):
		return Message{}, err
	default:
		return Message{}, errInboundTrailingData
	}
}

type rejectAdditionalJSONValue struct{}

func (*rejectAdditionalJSONValue) UnmarshalJSON([]byte) error {
	return errInboundMultipleJSON
}
