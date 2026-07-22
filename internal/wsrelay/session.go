package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	readTimeout          = 60 * time.Second
	writeTimeout         = 10 * time.Second
	maxInboundMessageLen = 64 << 20 // 64 MiB
	heartbeatInterval    = 30 * time.Second
	pendingMailboxSize   = 8
)

var (
	errClosed          = errors.New("websocket session closed")
	errMailboxOverflow = errors.New("wsrelay: pending request mailbox overflow")
)

type pendingRequest struct {
	id       string
	ch       chan Message
	mailbox  chan Message
	terminal chan Message
	abort    chan struct{}
	done     chan struct{}

	stateMutex sync.Mutex
	terminated bool
	abortOnce  sync.Once
}

func newPendingRequest(id string) *pendingRequest {
	pr := &pendingRequest{
		id:       id,
		ch:       make(chan Message),
		mailbox:  make(chan Message, pendingMailboxSize),
		terminal: make(chan Message, 1),
		abort:    make(chan struct{}),
		done:     make(chan struct{}),
	}
	go pr.drain()
	return pr
}

func (pr *pendingRequest) enqueue(msg Message) bool {
	if pr == nil {
		msg.Release()
		return false
	}
	pr.stateMutex.Lock()
	defer pr.stateMutex.Unlock()
	if pr.terminated {
		msg.Release()
		return false
	}
	if isTerminalMessage(msg.Type) {
		pr.terminated = true
		pr.terminal <- msg
		return true
	}
	select {
	case pr.mailbox <- msg:
		return false
	default:
		msg.Release()
		pr.terminated = true
		pr.terminal <- requestErrorMessage(pr.id, errMailboxOverflow)
		return true
	}
}

func (pr *pendingRequest) terminate(msg Message) {
	if pr == nil {
		msg.Release()
		return
	}
	pr.stateMutex.Lock()
	defer pr.stateMutex.Unlock()
	if pr.terminated {
		msg.Release()
		return
	}
	pr.terminated = true
	pr.terminal <- msg
}

func (pr *pendingRequest) abandon() {
	if pr == nil {
		return
	}
	pr.abortOnce.Do(func() {
		close(pr.abort)
	})
}

func (pr *pendingRequest) drain() {
	defer close(pr.done)
	defer close(pr.ch)
	defer pr.releaseQueued()
	for {
		select {
		case <-pr.abort:
			return
		case msg := <-pr.mailbox:
			if !pr.deliver(msg) {
				return
			}
		case terminal := <-pr.terminal:
			for {
				select {
				case <-pr.abort:
					terminal.Release()
					return
				case msg := <-pr.mailbox:
					if !pr.deliver(msg) {
						terminal.Release()
						return
					}
				default:
					_ = pr.deliver(terminal)
					return
				}
			}
		}
	}
}

func (pr *pendingRequest) deliver(msg Message) bool {
	select {
	case <-pr.abort:
		msg.Release()
		return false
	case pr.ch <- msg:
		// Ownership transfers to the channel consumer. The message must stay
		// charged while that consumer reads or copies Payload.
		return true
	}
}

func (pr *pendingRequest) releaseQueued() {
	for {
		select {
		case msg := <-pr.mailbox:
			msg.Release()
		default:
			goto terminal
		}
	}

terminal:
	select {
	case msg := <-pr.terminal:
		msg.Release()
	default:
	}
}

type session struct {
	conn       *websocket.Conn
	manager    *Manager
	provider   string
	id         string
	closed     chan struct{}
	readCtx    context.Context
	cancelRead context.CancelFunc
	closeOnce  sync.Once
	writeMutex sync.Mutex
	pending    sync.Map // map[string]*pendingRequest
	lease      *connectionLease
}

func newSession(conn *websocket.Conn, mgr *Manager, id string, lease *connectionLease) *session {
	readCtx, cancelRead := context.WithCancel(context.Background())
	s := &session{
		conn:       conn,
		manager:    mgr,
		provider:   "",
		id:         id,
		closed:     make(chan struct{}),
		readCtx:    readCtx,
		cancelRead: cancelRead,
		lease:      lease,
	}
	conn.SetReadLimit(maxInboundMessageLen)
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})
	s.startHeartbeat()
	return s
}

func (s *session) startHeartbeat() {
	if s == nil || s.conn == nil {
		return
	}
	ticker := time.NewTicker(heartbeatInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-ticker.C:
				s.writeMutex.Lock()
				err := s.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(writeTimeout))
				s.writeMutex.Unlock()
				if err != nil {
					s.cleanup(err)
					return
				}
			}
		}
	}()
}

func (s *session) run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	stopCancel := context.AfterFunc(ctx, func() {
		s.cleanup(ctx.Err())
	})
	defer stopCancel()
	defer s.cleanup(errClosed)
	readCtx := s.readCtx
	if readCtx == nil {
		readCtx = ctx
	}
	for {
		// NextReader handles control frames internally. Text and binary data
		// frames intentionally share the same single-JSON-value contract.
		messageType, reader, err := s.conn.NextReader()
		if err != nil {
			s.cleanup(err)
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			s.cleanup(errors.New("wsrelay: unsupported websocket data frame"))
			return
		}
		msg, err := decodeInboundMessage(readCtx, s.manager.inbound, reader)
		if err != nil {
			s.cleanup(err)
			return
		}
		s.dispatch(msg)
	}
}

func (s *session) dispatch(msg Message) {
	if msg.Type == MessageTypePing {
		msg.Release()
		_ = s.send(context.Background(), Message{ID: msg.ID, Type: MessageTypePong})
		return
	}
	if value, ok := s.pending.Load(msg.ID); ok {
		req := value.(*pendingRequest)
		if req.enqueue(msg) {
			s.pending.CompareAndDelete(msg.ID, req)
		}
		return
	}
	if isTerminalMessage(msg.Type) && s.manager != nil {
		s.manager.logDebugf("wsrelay: received terminal message for unknown id %s (provider=%s)", msg.ID, s.provider)
	}
	msg.Release()
}

func (s *session) send(ctx context.Context, msg Message) error {
	select {
	case <-s.closed:
		return errClosed
	default:
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if err := s.conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

func (s *session) request(ctx context.Context, msg Message) (<-chan Message, error) {
	if msg.ID == "" {
		return nil, fmt.Errorf("wsrelay: message id is required")
	}
	select {
	case <-s.closed:
		return nil, errClosed
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req := newPendingRequest(msg.ID)
	if _, loaded := s.pending.LoadOrStore(msg.ID, req); loaded {
		req.abandon()
		return nil, fmt.Errorf("wsrelay: duplicate message id %s", msg.ID)
	}
	select {
	case <-s.closed:
		s.pending.CompareAndDelete(msg.ID, req)
		req.abandon()
		return nil, errClosed
	default:
	}
	if err := s.send(ctx, msg); err != nil {
		s.pending.CompareAndDelete(msg.ID, req)
		req.abandon()
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			s.pending.CompareAndDelete(msg.ID, req)
			req.abandon()
		case <-req.done:
		}
	}()
	return req.ch, nil
}

func (s *session) cleanup(cause error) {
	s.closeOnce.Do(func() {
		if cause == nil {
			cause = errClosed
		}
		if s.cancelRead != nil {
			s.cancelRead()
		}
		close(s.closed)
		s.pending.Range(func(key, _ any) bool {
			if value, loaded := s.pending.LoadAndDelete(key); loaded {
				req := value.(*pendingRequest)
				req.terminate(requestErrorMessage(key.(string), cause))
			}
			return true
		})
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.lease.release()
		if s.manager != nil {
			s.manager.handleSessionClosed(s, cause)
		}
	})
}

func isTerminalMessage(messageType string) bool {
	return messageType == MessageTypeHTTPResp || messageType == MessageTypeError || messageType == MessageTypeStreamEnd
}

func requestErrorMessage(id string, err error) Message {
	return Message{ID: id, Type: MessageTypeError, Payload: map[string]any{"error": err.Error()}}
}
