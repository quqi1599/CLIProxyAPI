package wsrelay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const (
	maxHTTPResponseBodyBytes = 128 << 20
	maxStreamChunkBytes      = 16 << 20
	maxStreamResponseBytes   = 128 << 20

	wsrelayResponseBodyTooLargeCode   = "wsrelay_response_body_too_large"
	wsrelayStreamChunkTooLargeCode    = "wsrelay_stream_chunk_too_large"
	wsrelayStreamResponseTooLargeCode = "wsrelay_stream_response_too_large"
)

var (
	errHTTPResponseBodyTooLarge = errors.New("wsrelay: HTTP response body exceeds limit")
	errStreamChunkTooLarge      = errors.New("wsrelay: stream chunk exceeds limit")
	errStreamResponseTooLarge   = errors.New("wsrelay: stream response exceeds limit")
)

// HTTPRequest represents a proxied HTTP request delivered to websocket clients.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// HTTPResponse captures the response relayed back from websocket clients.
type HTTPResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// StreamEvent represents a streaming response event from clients.
type StreamEvent struct {
	Type    string
	Payload []byte
	Status  int
	Headers http.Header
	Err     error
}

// NonStream executes a non-streaming HTTP request using the websocket provider.
func (m *Manager) NonStream(ctx context.Context, provider string, req *HTTPRequest) (*HTTPResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("wsrelay: request is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	msg := Message{ID: uuid.NewString(), Type: MessageTypeHTTPReq, Payload: encodeRequest(req)}
	respCh, err := m.Send(requestCtx, provider, msg)
	if err != nil {
		return nil, err
	}
	var (
		streamMode bool
		streamResp *HTTPResponse
		streamBody bytes.Buffer
		budget     = responseByteBudget{limit: maxStreamResponseBytes}
	)
	for {
		select {
		case <-requestCtx.Done():
			return nil, requestCtx.Err()
		case msg, ok := <-respCh:
			if !ok {
				if streamMode {
					if streamResp == nil {
						streamResp = &HTTPResponse{Status: http.StatusOK, Headers: make(http.Header)}
					} else if streamResp.Headers == nil {
						streamResp.Headers = make(http.Header)
					}
					streamResp.Body = streamBody.Bytes()
					return streamResp, nil
				}
				return nil, errors.New("wsrelay: connection closed during response")
			}
			resp, errProcess, done := func() (*HTTPResponse, error, bool) {
				defer msg.Release()
				switch msg.Type {
				case MessageTypeHTTPResp:
					resp, errDecode := decodeResponse(msg.Payload)
					if errDecode != nil {
						return nil, errDecode, true
					}
					if errBudget := budget.add(len(resp.Body)); errBudget != nil {
						return nil, errBudget, true
					}
					if streamMode && streamBody.Len() > 0 && len(resp.Body) == 0 {
						resp.Body = streamBody.Bytes()
					}
					return resp, nil, true
				case MessageTypeError:
					return nil, decodeError(msg.Payload), true
				case MessageTypeStreamStart:
					decodedResp, errDecode := decodeResponse(msg.Payload)
					if errDecode != nil {
						return nil, errDecode, true
					}
					streamMode = true
					streamResp = decodedResp
					if errBudget := budget.add(len(streamResp.Body)); errBudget != nil {
						return nil, errBudget, true
					}
					if streamResp.Headers == nil {
						streamResp.Headers = make(http.Header)
					}
					streamBody.Reset()
					return nil, nil, false
				case MessageTypeStreamChunk:
					if !streamMode {
						streamMode = true
						streamResp = &HTTPResponse{Status: http.StatusOK, Headers: make(http.Header)}
					}
					chunk, errDecode := decodeChunk(msg.Payload)
					if errDecode != nil {
						return nil, errDecode, true
					}
					if errBudget := budget.add(len(chunk)); errBudget != nil {
						return nil, errBudget, true
					}
					if len(chunk) > 0 {
						_, _ = streamBody.Write(chunk)
					}
					return nil, nil, false
				case MessageTypeStreamEnd:
					if !streamMode {
						return &HTTPResponse{Status: http.StatusOK, Headers: make(http.Header)}, nil, true
					}
					if streamResp == nil {
						streamResp = &HTTPResponse{Status: http.StatusOK, Headers: make(http.Header)}
					} else if streamResp.Headers == nil {
						streamResp.Headers = make(http.Header)
					}
					streamResp.Body = streamBody.Bytes()
					return streamResp, nil, true
				default:
					return nil, nil, false
				}
			}()
			if done {
				return resp, errProcess
			}
		}
	}
}

// Stream executes a streaming HTTP request and returns channel with stream events.
func (m *Manager) Stream(ctx context.Context, provider string, req *HTTPRequest) (<-chan StreamEvent, error) {
	if req == nil {
		return nil, fmt.Errorf("wsrelay: request is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithCancel(ctx)
	msg := Message{ID: uuid.NewString(), Type: MessageTypeHTTPReq, Payload: encodeRequest(req)}
	respCh, err := m.Send(requestCtx, provider, msg)
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan StreamEvent)
	go func() {
		defer cancel()
		defer close(out)
		send := func(ev StreamEvent) bool {
			if ctx.Err() != nil {
				return false
			}
			select {
			case <-ctx.Done():
				return false
			case out <- ev:
				return true
			}
		}
		for {
			if requestCtx.Err() != nil {
				return
			}
			select {
			case <-requestCtx.Done():
				return
			case msg, ok := <-respCh:
				if !ok {
					if requestCtx.Err() != nil {
						return
					}
					_ = send(StreamEvent{Err: errors.New("wsrelay: stream closed")})
					return
				}
				keepGoing := func() bool {
					defer msg.Release()
					switch msg.Type {
					case MessageTypeStreamStart:
						resp, errDecode := decodeResponse(msg.Payload)
						if errDecode != nil {
							cancel()
							_ = send(StreamEvent{Type: MessageTypeError, Err: errDecode})
							return false
						}
						return send(StreamEvent{Type: MessageTypeStreamStart, Status: resp.Status, Headers: resp.Headers})
					case MessageTypeStreamChunk:
						chunk, errDecode := decodeChunk(msg.Payload)
						if errDecode != nil {
							cancel()
							_ = send(StreamEvent{Type: MessageTypeError, Err: errDecode})
							return false
						}
						return send(StreamEvent{Type: MessageTypeStreamChunk, Payload: chunk})
					case MessageTypeStreamEnd:
						_ = send(StreamEvent{Type: MessageTypeStreamEnd})
						return false
					case MessageTypeError:
						_ = send(StreamEvent{Type: MessageTypeError, Err: decodeError(msg.Payload)})
						return false
					case MessageTypeHTTPResp:
						resp, errDecode := decodeResponse(msg.Payload)
						if errDecode != nil {
							cancel()
							_ = send(StreamEvent{Type: MessageTypeError, Err: errDecode})
							return false
						}
						_ = send(StreamEvent{Type: MessageTypeHTTPResp, Status: resp.Status, Headers: resp.Headers, Payload: resp.Body})
						return false
					default:
						return true
					}
				}()
				if !keepGoing {
					return
				}
			}
		}
	}()
	return out, nil
}

func encodeRequest(req *HTTPRequest) map[string]any {
	headers := make(map[string]any, len(req.Headers))
	for key, values := range req.Headers {
		copyValues := make([]string, len(values))
		copy(copyValues, values)
		headers[key] = copyValues
	}
	return map[string]any{
		"method":  req.Method,
		"url":     req.URL,
		"headers": headers,
		"body":    string(req.Body),
		"sent_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func decodeResponse(payload map[string]any) (*HTTPResponse, error) {
	return decodeResponseWithLimit(payload, maxHTTPResponseBodyBytes)
}

func decodeResponseWithLimit(payload map[string]any, bodyLimit int) (*HTTPResponse, error) {
	if payload == nil {
		return &HTTPResponse{Status: http.StatusBadGateway, Headers: make(http.Header)}, nil
	}
	resp := &HTTPResponse{Status: http.StatusOK, Headers: make(http.Header)}
	if status, ok := payload["status"].(float64); ok {
		resp.Status = int(status)
	}
	if headers, ok := payload["headers"].(map[string]any); ok {
		for key, raw := range headers {
			switch v := raw.(type) {
			case []any:
				for _, item := range v {
					if str, ok := item.(string); ok {
						resp.Headers.Add(key, str)
					}
				}
			case []string:
				for _, str := range v {
					resp.Headers.Add(key, str)
				}
			case string:
				resp.Headers.Set(key, v)
			}
		}
	}
	if body, ok := payload["body"].(string); ok {
		var err error
		resp.Body, err = decodePayloadString(body, bodyLimit, errHTTPResponseBodyTooLarge)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func decodeChunk(payload map[string]any) ([]byte, error) {
	return decodeChunkWithLimit(payload, maxStreamChunkBytes)
}

func decodeChunkWithLimit(payload map[string]any, chunkLimit int) ([]byte, error) {
	if payload == nil {
		return nil, nil
	}
	if data, ok := payload["data"].(string); ok {
		return decodePayloadString(data, chunkLimit, errStreamChunkTooLarge)
	}
	return nil, nil
}

func decodePayloadString(payload string, limit int, limitErr error) ([]byte, error) {
	if len(payload) > limit {
		return nil, wsrelayLimitFailure(limitErr)
	}
	return []byte(payload), nil
}

type responseByteBudget struct {
	total int
	limit int
}

func (b *responseByteBudget) add(size int) error {
	if size < 0 || b.total > b.limit-size {
		return wsrelayLimitFailure(errStreamResponseTooLarge)
	}
	b.total += size
	return nil
}

func wsrelayLimitFailure(cause error) error {
	code := ""
	message := ""
	switch cause {
	case errHTTPResponseBodyTooLarge:
		code = wsrelayResponseBodyTooLargeCode
		message = "upstream websocket response body exceeded the configured limit"
	case errStreamChunkTooLarge:
		code = wsrelayStreamChunkTooLargeCode
		message = "upstream websocket stream chunk exceeded the configured limit"
	case errStreamResponseTooLarge:
		code = wsrelayStreamResponseTooLargeCode
		message = "upstream websocket stream response exceeded the configured limit"
	default:
		return cause
	}
	return &failurecontract.Failure{
		Kind:          failurecontract.UpstreamProtocolError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		ProviderCode:  code,
		Cause:         cause,
		PublicMessage: message,
	}
}

func decodeError(payload map[string]any) error {
	if payload == nil {
		return errors.New("wsrelay: unknown error")
	}
	message, _ := payload["error"].(string)
	status := 0
	if v, ok := payload["status"].(float64); ok {
		status = int(v)
	}
	if message == "" {
		message = "wsrelay: upstream error"
	}
	return fmt.Errorf("%s (status=%d)", message, status)
}
