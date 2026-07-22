package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	requestlogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate  = "response.create"
	wsRequestTypeAppend  = "response.append"
	wsEventTypeError     = "error"
	wsEventTypeCompleted = "response.completed"
	wsEventTypeDone      = "response.done"
	wsDoneMarker         = "[DONE]"
	wsTurnStateHeader    = "x-codex-turn-state"
	wsTimelineBodyKey    = "WEBSOCKET_TIMELINE_OVERRIDE"

	responsesWebsocketMaxInputItems       = 4096
	responsesWebsocketTimelineMaxEvents   = 4096
	responsesWebsocketTimelineMaxBytes    = 64 << 20
	responsesWebsocketMaxConnections      = 128
	responsesWebsocketMaxConnectionsPerIP = 16
	responsesWebsocketFrameBudgetBytes    = 128 << 20
	responsesWebsocketRetainedBudgetBytes = 128 << 20
	responsesWebsocketReadChunkBytes      = 32 << 10
	websocketTimelineTruncationReserve    = 256
	websocketTimelineTruncationEventType  = "timeline_truncated"

	responsesWebsocketConnectionLimitCode      = "responses_websocket_connection_limit"
	responsesWebsocketConnectionLimitPerIPCode = "responses_websocket_connection_limit_per_ip"
	responsesWebsocketFrameCapacityCode        = "responses_websocket_frame_capacity"
	responsesWebsocketFrameTooLargeCode        = "responses_websocket_frame_too_large"
	responsesWebsocketRetainedCapacityCode     = "responses_websocket_retained_capacity"
)

var responsesWebsocketReadLimit int64 = responsesWebsocketFrameBudgetBytes
var responsesWebsocketTranscriptReplayLimitBytes = 64 << 20

var (
	errResponsesWebsocketInputNotArray     = errors.New("websocket request requires array field: input")
	errResponsesWebsocketInputTooManyItems = errors.New("websocket input item limit exceeded")
	errResponsesWebsocketFrameCapacity     = errors.New("responses websocket frame capacity exhausted")
	errResponsesWebsocketFrameTooLarge     = errors.New("responses websocket frame too large")
	errResponsesWebsocketRetainedCapacity  = errors.New("responses websocket retained capacity exhausted")
)

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type responsesWebsocketConnectionLimiter struct {
	mu       sync.Mutex
	maxTotal int
	maxPerIP int
	total    int
	byIP     map[string]int
}

type responsesWebsocketConnectionLease struct {
	limiter *responsesWebsocketConnectionLimiter
	peerIP  string
	once    sync.Once
}

type responsesWebsocketFrameLimiter struct {
	mu       sync.Mutex
	maxBytes int64
	inUse    int64
	peak     int64
}

type responsesWebsocketFrameLease struct {
	limiter  *responsesWebsocketFrameLimiter
	mu       sync.Mutex
	weight   int64
	released bool
}

func newResponsesWebsocketConnectionLimiter(maxTotal, maxPerIP int) *responsesWebsocketConnectionLimiter {
	if maxTotal <= 0 {
		maxTotal = responsesWebsocketMaxConnections
	}
	if maxPerIP <= 0 {
		maxPerIP = responsesWebsocketMaxConnectionsPerIP
	}
	if maxPerIP > maxTotal {
		maxPerIP = maxTotal
	}
	return &responsesWebsocketConnectionLimiter{
		maxTotal: maxTotal,
		maxPerIP: maxPerIP,
		byIP:     make(map[string]int),
	}
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketConnectionLimiter() *responsesWebsocketConnectionLimiter {
	if h == nil {
		return nil
	}
	h.responsesWebsocketLimiterOnce.Do(func() {
		if h.responsesWebsocketLimiter == nil {
			h.responsesWebsocketLimiter = newResponsesWebsocketConnectionLimiter(0, 0)
		}
	})
	return h.responsesWebsocketLimiter
}

func (l *responsesWebsocketConnectionLimiter) acquire(peerIP string) (*responsesWebsocketConnectionLease, string) {
	if l == nil {
		return nil, responsesWebsocketConnectionLimitCode
	}
	peerIP = strings.TrimSpace(peerIP)
	if peerIP == "" {
		peerIP = "unknown"
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxTotal {
		return nil, responsesWebsocketConnectionLimitCode
	}
	if l.byIP[peerIP] >= l.maxPerIP {
		return nil, responsesWebsocketConnectionLimitPerIPCode
	}
	l.total++
	l.byIP[peerIP]++
	return &responsesWebsocketConnectionLease{limiter: l, peerIP: peerIP}, ""
}

func (l *responsesWebsocketConnectionLease) release() {
	if l == nil || l.limiter == nil {
		return
	}
	l.once.Do(func() {
		limiter := l.limiter
		limiter.mu.Lock()
		if limiter.total > 0 {
			limiter.total--
		}
		if count := limiter.byIP[l.peerIP]; count <= 1 {
			delete(limiter.byIP, l.peerIP)
		} else {
			limiter.byIP[l.peerIP] = count - 1
		}
		limiter.mu.Unlock()
	})
}

func newResponsesWebsocketFrameLimiter(maxBytes int64) *responsesWebsocketFrameLimiter {
	if maxBytes <= 0 {
		maxBytes = responsesWebsocketFrameBudgetBytes
	}
	return &responsesWebsocketFrameLimiter{maxBytes: maxBytes}
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketFrameTransformLimiter() *responsesWebsocketFrameLimiter {
	if h == nil {
		return nil
	}
	h.responsesWebsocketFrameLimiterOnce.Do(func() {
		if h.responsesWebsocketFrameLimiter == nil {
			h.responsesWebsocketFrameLimiter = newResponsesWebsocketFrameLimiter(0)
		}
	})
	return h.responsesWebsocketFrameLimiter
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketRetainedStateLimiter() *responsesWebsocketFrameLimiter {
	if h == nil {
		return nil
	}
	h.responsesWebsocketRetainedLimiterOnce.Do(func() {
		if h.responsesWebsocketRetainedLimiter == nil {
			h.responsesWebsocketRetainedLimiter = newResponsesWebsocketFrameLimiter(responsesWebsocketRetainedBudgetBytes)
		}
	})
	return h.responsesWebsocketRetainedLimiter
}

func (l *responsesWebsocketFrameLimiter) acquire(weight int64) *responsesWebsocketFrameLease {
	if l == nil || weight < 0 {
		return nil
	}
	lease := &responsesWebsocketFrameLease{limiter: l}
	if !lease.resize(weight) {
		return nil
	}
	return lease
}

func (l *responsesWebsocketFrameLease) resize(weight int64) bool {
	if l == nil || l.limiter == nil || weight < 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false
	}

	limiter := l.limiter
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	delta := weight - l.weight
	if delta > 0 && (delta > limiter.maxBytes || limiter.inUse > limiter.maxBytes-delta) {
		return false
	}
	limiter.inUse += delta
	if limiter.inUse > limiter.peak {
		limiter.peak = limiter.inUse
	}
	l.weight = weight
	return true
}

func (l *responsesWebsocketFrameLease) release() {
	if l == nil || l.limiter == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	limiter := l.limiter
	limiter.mu.Lock()
	limiter.inUse -= l.weight
	if limiter.inUse < 0 {
		limiter.inUse = 0
	}
	limiter.mu.Unlock()
	l.weight = 0
}

func responsesWebsocketPeerIP(req *http.Request) string {
	if req == nil {
		return "unknown"
	}
	remoteAddr := strings.TrimSpace(req.RemoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
		return strings.ToLower(host)
	}
	if ip := net.ParseIP(strings.Trim(remoteAddr, "[]")); ip != nil {
		return ip.String()
	}
	if remoteAddr == "" {
		return "unknown"
	}
	return strings.ToLower(remoteAddr)
}

func writeResponsesWebsocketConnectionLimit(c *gin.Context, code string) {
	message := "responses websocket connection limit reached"
	if code == responsesWebsocketConnectionLimitPerIPCode {
		message = "responses websocket connection limit reached for client IP"
	}
	c.Header("Retry-After", "1")
	c.AbortWithStatusJSON(http.StatusTooManyRequests, handlers.ErrorResponse{Error: handlers.ErrorDetail{
		Message: message,
		Type:    "rate_limit_error",
		Code:    code,
	}})
}

func readResponsesWebsocketFrame(reader io.Reader, limit int64, lease *responsesWebsocketFrameLease) ([]byte, error) {
	if reader == nil || limit <= 0 || lease == nil {
		return nil, errResponsesWebsocketFrameTooLarge
	}
	payload := make([]byte, 0, min(limit, int64(responsesWebsocketReadChunkBytes)))
	chunk := make([]byte, responsesWebsocketReadChunkBytes)
	emptyReads := 0
	for int64(len(payload)) < limit {
		remaining := limit - int64(len(payload))
		readBuffer := chunk
		if remaining < int64(len(readBuffer)) {
			readBuffer = readBuffer[:remaining]
		}
		n, errRead := reader.Read(readBuffer)
		if n > 0 {
			emptyReads = 0
			targetBytes := int64(len(payload) + n)
			if !lease.resize(targetBytes) {
				return nil, errResponsesWebsocketFrameCapacity
			}
			payload = append(payload, readBuffer[:n]...)
		} else if errRead == nil {
			emptyReads++
			if emptyReads >= 100 {
				return nil, io.ErrNoProgress
			}
		}
		if errRead != nil {
			if errors.Is(errRead, io.EOF) {
				return payload, nil
			}
			if strings.Contains(strings.ToLower(errRead.Error()), "read limit") {
				return nil, errResponsesWebsocketFrameTooLarge
			}
			return nil, errRead
		}
	}

	var probe [1]byte
	n, errRead := reader.Read(probe[:])
	if n > 0 {
		return nil, errResponsesWebsocketFrameTooLarge
	}
	if errRead != nil && !errors.Is(errRead, io.EOF) {
		if strings.Contains(strings.ToLower(errRead.Error()), "read limit") {
			return nil, errResponsesWebsocketFrameTooLarge
		}
		return nil, errRead
	}
	return payload, nil
}

func writeResponsesWebsocketFrameCapacityError(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender) error {
	payload := []byte(`{"type":"error","status":503,"error":{"message":"responses websocket frame capacity exhausted","type":"server_error","code":"responses_websocket_frame_capacity"}}`)
	if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now()); errWrite != nil {
		return errWrite
	}
	return conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseTryAgainLater, responsesWebsocketFrameCapacityCode),
		time.Now().Add(time.Second),
	)
}

func writeResponsesWebsocketRetainedCapacityError(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender) error {
	payload := []byte(`{"type":"error","status":503,"error":{"message":"responses websocket retained capacity exhausted","type":"server_error","code":"responses_websocket_retained_capacity"}}`)
	if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now()); errWrite != nil {
		return errWrite
	}
	return conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseTryAgainLater, responsesWebsocketRetainedCapacityCode),
		time.Now().Add(time.Second),
	)
}

func writeResponsesWebsocketFrameTooLargeClose(conn *websocket.Conn) error {
	return conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseMessageTooBig, responsesWebsocketFrameTooLargeCode),
		time.Now().Add(time.Second),
	)
}

type websocketTimelineAppender interface {
	Append(eventType string, payload []byte, timestamp time.Time)
}

type websocketTimelineLog struct {
	enabled bool
	source  *requestlogging.FileBodySource
	builder *strings.Builder

	eventCount int
	byteCount  int
	truncated  bool
	maxEvents  int
	maxBytes   int
}

func newWebsocketTimelineLog(enabled bool, source *requestlogging.FileBodySource) *websocketTimelineLog {
	if !enabled {
		return &websocketTimelineLog{}
	}
	if source == nil {
		return newInMemoryWebsocketTimelineLog()
	}
	return &websocketTimelineLog{
		enabled:   true,
		source:    source,
		maxEvents: responsesWebsocketTimelineMaxEvents,
		maxBytes:  responsesWebsocketTimelineMaxBytes,
	}
}

func newInMemoryWebsocketTimelineLog() *websocketTimelineLog {
	return &websocketTimelineLog{
		enabled:   true,
		builder:   &strings.Builder{},
		maxEvents: responsesWebsocketTimelineMaxEvents,
		maxBytes:  responsesWebsocketTimelineMaxBytes,
	}
}

func websocketTimelineSourceFromContext(c *gin.Context) *requestlogging.FileBodySource {
	if c == nil {
		return nil
	}
	value, exists := c.Get(requestlogging.WebsocketTimelineSourceContextKey)
	if !exists {
		return nil
	}
	source, ok := value.(*requestlogging.FileBodySource)
	if !ok {
		return nil
	}
	return source
}

func (l *websocketTimelineLog) BeginRequest() {
	// Events share one append-only FileBodySource part for the session. Keeping
	// this method preserves request-boundary call sites without creating a temp
	// file for every websocket frame.
}

func (l *websocketTimelineLog) Append(eventType string, payload []byte, timestamp time.Time) {
	if l == nil || !l.enabled || l.truncated {
		return
	}
	payloadMetadata := websocketTimelinePayloadMetadata(payload)
	if len(payloadMetadata) == 0 {
		return
	}
	timestampText := timestamp.Format(time.RFC3339Nano)
	separatorBytes := 0
	if l.byteCount > 0 {
		separatorBytes = 1
	}
	dataByteLimit := l.maxBytes - min(l.maxBytes, websocketTimelineTruncationReserve)
	dataBytes := websocketTimelineEventSize(eventType, payloadMetadata, timestampText)
	if l.eventCount >= l.maxEvents || l.byteCount+separatorBytes+dataBytes > dataByteLimit {
		l.appendTruncationMarker(timestamp)
		return
	}
	data := formatWebsocketTimelineEventParts(eventType, payloadMetadata, timestampText, dataBytes)
	if !l.write(data) {
		return
	}
	l.eventCount++
}

func (l *websocketTimelineLog) SetContext(c *gin.Context) {
	if l == nil || !l.enabled {
		return
	}
	if l.source != nil {
		if l.source.HasPayload() {
			c.Set(requestlogging.WebsocketTimelineSourceContextKey, l.source)
			return
		}
		if errCleanup := l.source.Cleanup(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up empty websocket timeline log parts")
		}
	}
	if l.builder != nil {
		setWebsocketTimelineBody(c, l.builder.String())
	}
}

func (l *websocketTimelineLog) String() string {
	if l == nil || !l.enabled {
		return ""
	}
	if l.source != nil {
		data, errRead := l.source.Bytes()
		if errRead != nil {
			return ""
		}
		return string(data)
	}
	if l.builder == nil {
		return ""
	}
	return l.builder.String()
}

func (l *websocketTimelineLog) appendTruncationMarker(timestamp time.Time) {
	if l == nil || l.truncated {
		return
	}
	l.truncated = true
	marker := formatWebsocketTimelineEvent(
		websocketTimelineTruncationEventType,
		[]byte(`{"reason":"session_budget_exceeded"}`),
		timestamp,
	)
	separatorBytes := 0
	if l.byteCount > 0 {
		separatorBytes = 1
	}
	if len(marker) == 0 || l.byteCount+separatorBytes+len(marker) > l.maxBytes {
		return
	}
	if l.write(marker) {
		l.eventCount++
	}
}

func (l *websocketTimelineLog) write(data []byte) bool {
	if l == nil || len(data) == 0 {
		return false
	}
	prependNewline := l.byteCount > 0
	if l.source != nil {
		if prependNewline {
			if errWrite := l.source.AppendBytes([]byte("\n")); errWrite != nil {
				log.WithError(errWrite).Warn("failed to write websocket request detail log separator")
				return false
			}
			l.byteCount++
		}
		if errWrite := l.source.AppendBytes(data); errWrite != nil {
			log.WithError(errWrite).Warn("failed to write websocket request detail log")
			return false
		}
	} else if l.builder != nil {
		if prependNewline {
			l.builder.WriteByte('\n')
			l.byteCount++
		}
		l.builder.Write(data)
	} else {
		return false
	}
	l.byteCount += len(data)
	return true
}

func writeWebsocketTimelineBuilder(builder *strings.Builder, data []byte) {
	if builder == nil || len(data) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.Write(data)
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	frameReadLimit, _ := handlers.WebsocketPayloadBodyLimits(c, responsesWebsocketReadLimit)
	frameReadLimit = min(frameReadLimit, int64(responsesWebsocketFrameBudgetBytes))
	limiter := h.responsesWebsocketConnectionLimiter()
	lease, limitCode := limiter.acquire(responsesWebsocketPeerIP(c.Request))
	if lease == nil {
		writeResponsesWebsocketConnectionLimit(c, limitCode)
		return
	}
	defer lease.release()

	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	conn.SetReadLimit(frameReadLimit + 1)
	passthroughSessionID := uuid.NewString()
	toolCacheKey := websocketDownstreamSessionKey(c.Request)
	retainedLimiter := h.responsesWebsocketRetainedStateLimiter()
	toolOutputCache := newWebsocketToolOutputCacheWithRetainedBudget(0, websocketToolOutputCacheMaxPerSession, websocketToolOutputCacheMaxBytes, retainedLimiter)
	toolCallCache := newWebsocketToolOutputCacheWithRetainedBudget(0, websocketToolOutputCacheMaxPerSession, websocketToolOutputCacheMaxBytes, retainedLimiter)
	clientIP := websocketClientAddress(c)
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientIP)

	requestLogEnabled := h != nil && h.Cfg != nil && h.Cfg.RequestLog
	wsTimelineLog := newWebsocketTimelineLog(requestLogEnabled, websocketTimelineSourceFromContext(c))

	wsDone := make(chan struct{})
	defer close(wsDone)

	if h != nil && h.AuthManager != nil {
		type upstreamDisconnectSubscriber interface {
			UpstreamDisconnectChan(sessionID string) <-chan error
		}
		for _, provider := range []string{"codex", "xai"} {
			exec, ok := h.AuthManager.Executor(provider)
			if !ok || exec == nil {
				continue
			}
			if subscriber, ok := exec.(upstreamDisconnectSubscriber); ok && subscriber != nil {
				disconnectCh := subscriber.UpstreamDisconnectChan(passthroughSessionID)
				if disconnectCh != nil {
					go func() {
						select {
						case <-wsDone:
							return
						case <-disconnectCh:
							_ = conn.Close()
						}
					}()
				}
			}
		}
	}

	var wsTerminateErr error
	defer func() {
		toolOutputCache.close()
		toolCallCache.close()
		if wsTerminateErr != nil {
			appendWebsocketTimelineDisconnect(wsTimelineLog, wsTerminateErr, time.Now())
			// log.Infof("responses websocket: session closing id=%s reason=%v", passthroughSessionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", passthroughSessionID)
		}
		if h != nil && h.AuthManager != nil {
			h.AuthManager.CloseExecutionSession(passthroughSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", passthroughSessionID)
		}
		wsTimelineLog.SetContext(c)
		if errClose := conn.Close(); errClose != nil {
			log.Warnf("responses websocket: close connection error: %v", errClose)
		}
	}()

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	lastRequestLease := retainedLimiter.acquire(0)
	lastResponseOutputLease := retainedLimiter.acquire(int64(len(lastResponseOutput)))
	if lastRequestLease == nil || lastResponseOutputLease == nil {
		lastRequestLease.release()
		lastResponseOutputLease.release()
		wsTerminateErr = errResponsesWebsocketRetainedCapacity
		if errWrite := writeResponsesWebsocketRetainedCapacityError(conn, wsTimelineLog); errWrite != nil {
			wsTerminateErr = errWrite
		}
		return
	}
	lastRequestRelease := func() {}
	lastResponseOutputRelease := func() {}
	defer func() {
		lastRequestRelease()
		lastResponseOutputRelease()
		lastRequestLease.release()
		lastResponseOutputLease.release()
	}()
	installLastRequest := func(next []byte, nextLease *responsesWebsocketFrameLease, nextRelease func()) {
		lastRequestRelease()
		lastRequestLease.release()
		lastRequest = next
		lastRequestLease = nextLease
		lastRequestRelease = nextRelease
	}
	replaceLastRequest := func(next []byte) bool {
		nextLease := retainedLimiter.acquire(int64(len(next)))
		if nextLease == nil {
			return false
		}
		nextRelease := internalpayload.RetainBytesScoped(next)
		installLastRequest(next, nextLease, nextRelease)
		return true
	}
	installLastResponseOutput := func(next []byte, nextLease *responsesWebsocketFrameLease, nextRelease func()) {
		lastResponseOutputRelease()
		lastResponseOutputLease.release()
		lastResponseOutput = next
		lastResponseOutputLease = nextLease
		lastResponseOutputRelease = nextRelease
	}
	replaceLastResponseOutput := func(next []byte) bool {
		nextLease := retainedLimiter.acquire(int64(len(next)))
		if nextLease == nil {
			return false
		}
		nextRelease := internalpayload.RetainBytesScoped(next)
		installLastResponseOutput(next, nextLease, nextRelease)
		return true
	}
	writeRetainedCapacityError := func() {
		wsTerminateErr = errResponsesWebsocketRetainedCapacity
		if errWrite := writeResponsesWebsocketRetainedCapacityError(conn, wsTimelineLog); errWrite != nil {
			wsTerminateErr = errWrite
		}
	}
	lastResponseID := ""
	var lastResponsePendingToolCallIDs []string
	pinnedAuthID := ""
	passthroughModelName := ""
	sessionAuthByID := func(authID string) (*coreauth.Auth, bool) {
		if h == nil || h.AuthManager == nil {
			return nil, false
		}
		if auth, ok := h.AuthManager.GetExecutionSessionAuthByID(passthroughSessionID, authID); ok {
			return auth, true
		}
		return h.AuthManager.GetByID(authID)
	}
	forceTranscriptReplayNextRequest := false
	var activeFrameLease *responsesWebsocketFrameLease
	defer func() {
		activeFrameLease.release()
	}()

	for {
		msgType, frameReader, errReadMessage := conn.NextReader()
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			if websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				log.Infof("responses websocket: client disconnected id=%s", passthroughSessionID)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		frameLease := h.responsesWebsocketFrameTransformLimiter().acquire(0)
		if frameLease == nil {
			wsTerminateErr = errResponsesWebsocketFrameCapacity
			if errWrite := writeResponsesWebsocketFrameCapacityError(conn, wsTimelineLog); errWrite != nil {
				wsTerminateErr = errWrite
			}
			return
		}
		activeFrameLease = frameLease
		payload, errReadFrame := readResponsesWebsocketFrame(frameReader, frameReadLimit, frameLease)
		if errReadFrame != nil {
			frameLease.release()
			wsTerminateErr = errReadFrame
			if errors.Is(errReadFrame, errResponsesWebsocketFrameCapacity) {
				if errWrite := writeResponsesWebsocketFrameCapacityError(conn, wsTimelineLog); errWrite != nil {
					wsTerminateErr = errWrite
				}
			} else if errors.Is(errReadFrame, errResponsesWebsocketFrameTooLarge) {
				handlers.RecordWebsocketPayloadBodyWithLimit(c, frameReadLimit+1, frameReadLimit, true)
				if errWrite := writeResponsesWebsocketFrameTooLargeClose(conn); errWrite != nil {
					wsTerminateErr = errWrite
				}
			}
			return
		}
		handlers.RecordWebsocketPayloadBodyWithLimit(c, int64(len(payload)), frameReadLimit, false)
		wsTimelineLog.BeginRequest()
		wsTimelineLog.Append("request", payload, time.Now())

		requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
		if requestModelName == "" {
			requestModelName = passthroughModelName
		}
		if requestModelName == "" {
			requestModelName = strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		}
		useUpstreamWebsocketPassthrough := h.responsesWebsocketUsesUpstreamWebsocketPassthrough(requestModelName)
		allowIncrementalInputWithPreviousResponseID := false
		allowCompactionReplayBypass := false
		if !useUpstreamWebsocketPassthrough {
			// Downstream websocket with CPA-mediated HTTP/SSE upstream always uses
			// merged transcript replay. previous_response_id is only safe for
			// end-to-end upstream websocket passthrough.
			if pinnedAuthID != "" {
				if pinnedAuth, ok := sessionAuthByID(pinnedAuthID); ok && pinnedAuth != nil {
					allowCompactionReplayBypass = responsesWebsocketAuthSupportsCompactionReplay(pinnedAuth)
				}
			} else {
				allowCompactionReplayBypass = h.websocketUpstreamSupportsCompactionReplayForModel(requestModelName)
			}
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var errMsg *interfaces.ErrorMessage
		if useUpstreamWebsocketPassthrough {
			requestJSON, errMsg = normalizeResponsesWebsocketPassthroughRequest(payload, requestModelName)
		} else {
			requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithIncrementalState(
				payload,
				lastRequest,
				lastResponseOutput,
				lastResponseID,
				lastResponsePendingToolCallIDs,
				allowIncrementalInputWithPreviousResponseID,
				allowCompactionReplayBypass,
			)
		}
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg, handlers.PassthroughHeadersEnabled(h.Cfg))
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload_metadata=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
				)
				frameLease.release()
				return
			}
			frameLease.release()
			continue
		}
		if !useUpstreamWebsocketPassthrough && shouldHandleResponsesWebsocketPrewarmLocally(payload, lastRequest, allowIncrementalInputWithPreviousResponseID) {
			updatedLastRequest = internalpayload.CloneBytes(requestJSON)
			if updated, errDelete := sjson.DeleteBytes(requestJSON, "generate"); errDelete == nil {
				requestJSON = updated
			}
			if updated, errDelete := sjson.DeleteBytes(updatedLastRequest, "generate"); errDelete == nil {
				updatedLastRequest = updated
			}
			if !replaceLastRequest(updatedLastRequest) || !replaceLastResponseOutput([]byte("[]")) {
				writeRetainedCapacityError()
				frameLease.release()
				return
			}
			lastResponseID = ""
			lastResponsePendingToolCallIDs = nil
			if errWrite := writeResponsesWebsocketSyntheticPrewarm(c, conn, requestJSON, wsTimelineLog, passthroughSessionID); errWrite != nil {
				wsTerminateErr = errWrite
				frameLease.release()
				return
			}
			frameLease.release()
			continue
		}

		previousLastRequest := []byte(nil)
		previousLastResponseOutput := []byte(nil)
		var previousLastRequestLease *responsesWebsocketFrameLease
		var previousLastResponseOutputLease *responsesWebsocketFrameLease
		releasePreviousLastRequest := func() {}
		releasePreviousLastResponseOutput := func() {}
		if !useUpstreamWebsocketPassthrough {
			previousLastRequestLease = retainedLimiter.acquire(int64(len(lastRequest)))
			previousLastResponseOutputLease = retainedLimiter.acquire(int64(len(lastResponseOutput)))
			if previousLastRequestLease == nil || previousLastResponseOutputLease == nil {
				previousLastRequestLease.release()
				previousLastResponseOutputLease.release()
				writeRetainedCapacityError()
				frameLease.release()
				return
			}
			previousLastRequest, releasePreviousLastRequest = internalpayload.CloneBytesScoped(lastRequest, "responses_websocket.rollback.last_request")
			previousLastResponseOutput, releasePreviousLastResponseOutput = internalpayload.CloneBytesScoped(lastResponseOutput, "responses_websocket.rollback.last_response_output")
		}
		releasePreviousState := func() {
			releasePreviousLastRequest()
			releasePreviousLastResponseOutput()
			previousLastRequestLease.release()
			previousLastResponseOutputLease.release()
		}
		previousLastResponseID := lastResponseID
		previousLastResponsePendingToolCallIDs := append([]string(nil), lastResponsePendingToolCallIDs...)
		forcedTranscriptReplay := forceTranscriptReplayNextRequest
		if useUpstreamWebsocketPassthrough {
			if modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String()); modelName != "" {
				passthroughModelName = modelName
			}
			if forcedTranscriptReplay {
				forceTranscriptReplayNextRequest = false
			}
		} else {
			requestJSON = repairResponsesWebsocketToolCallsWithCaches(toolOutputCache, toolCallCache, toolCacheKey, requestJSON)
			requestJSON = dedupeResponsesWebsocketInputItemsByID(requestJSON)
			updatedLastRequestLease := retainedLimiter.acquire(int64(len(requestJSON)))
			if updatedLastRequestLease == nil {
				releasePreviousState()
				writeRetainedCapacityError()
				frameLease.release()
				return
			}
			updatedLastRequest, updatedLastRequestRelease := internalpayload.CloneBytesScoped(requestJSON, "responses_websocket.last_request")
			installLastRequest(updatedLastRequest, updatedLastRequestLease, updatedLastRequestRelease)
			if forcedTranscriptReplay {
				forceTranscriptReplayNextRequest = false
			}
		}

		modelName := gjson.GetBytes(requestJSON, "model").String()
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
		lastAttemptedAuthID := ""
		if pinnedAuthID != "" {
			cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
		} else {
			cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
				authID = strings.TrimSpace(authID)
				if authID == "" || h == nil || h.AuthManager == nil {
					return
				}
				lastAttemptedAuthID = authID
				selectedAuth, ok := sessionAuthByID(authID)
				if !ok || selectedAuth == nil {
					return
				}
				if websocketUpstreamSupportsIncrementalInput(selectedAuth.Attributes, selectedAuth.Metadata) {
					pinnedAuthID = authID
				}
			})
		}
		dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")

		completedOutputLease := retainedLimiter.acquire(int64(len([]byte("[]"))))
		if completedOutputLease == nil {
			cliCancel(errResponsesWebsocketRetainedCapacity)
			releasePreviousState()
			writeRetainedCapacityError()
			frameLease.release()
			return
		}
		completedOutput, completedResponseID, completedPendingToolCallIDs, forwardErrMsg, errForward := h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, wsTimelineLog, passthroughSessionID, toolCallCache, toolCacheKey, completedOutputLease)
		if errForward != nil {
			releasePreviousState()
			completedOutputLease.release()
			if errors.Is(errForward, errResponsesWebsocketRetainedCapacity) {
				writeRetainedCapacityError()
				frameLease.release()
				return
			}
			wsTerminateErr = errForward
			log.Warnf("responses websocket: forward failed id=%s", passthroughSessionID)
			frameLease.release()
			return
		}
		if forwardErrMsg == nil && !useUpstreamWebsocketPassthrough && lastAttemptedAuthID != "" {
			if selectedAuth, ok := sessionAuthByID(lastAttemptedAuthID); ok && selectedAuth != nil {
				if websocketUpstreamSupportsIncrementalInput(selectedAuth.Attributes, selectedAuth.Metadata) {
					pinnedAuthID = lastAttemptedAuthID
				}
			}
		}
		if shouldReleaseResponsesWebsocketPinnedAuth(forwardErrMsg) {
			completedOutputLease.release()
			pinnedAuthID = ""
			forceTranscriptReplayNextRequest = true
			if useUpstreamWebsocketPassthrough {
				passthroughModelName = ""
			} else {
				lastRequestRelease()
				lastResponseOutputRelease()
				lastRequestLease.release()
				lastResponseOutputLease.release()
				lastRequest = previousLastRequest
				lastRequestLease = previousLastRequestLease
				previousLastRequestLease = nil
				lastRequestRelease = releasePreviousLastRequest
				releasePreviousLastRequest = func() {}
				lastResponseOutput = previousLastResponseOutput
				lastResponseOutputLease = previousLastResponseOutputLease
				previousLastResponseOutputLease = nil
				lastResponseOutputRelease = releasePreviousLastResponseOutput
				releasePreviousLastResponseOutput = func() {}
				lastResponseID = previousLastResponseID
				lastResponsePendingToolCallIDs = previousLastResponsePendingToolCallIDs
			}
			releasePreviousState()
			frameLease.release()
			continue
		}
		if !useUpstreamWebsocketPassthrough {
			installLastResponseOutput(completedOutput, completedOutputLease, internalpayload.RetainBytesScoped(completedOutput))
			completedOutputLease = nil
			lastResponseID = strings.TrimSpace(completedResponseID)
			lastResponsePendingToolCallIDs = append([]string(nil), completedPendingToolCallIDs...)
		} else {
			completedOutputLease.release()
		}
		releasePreviousState()
		frameLease.release()
	}
}

func websocketClientAddress(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.ClientIP())
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}

	// Keep the same sticky turn-state across reconnects when provided by the client.
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func normalizeResponsesWebsocketRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithMode(rawJSON, lastRequest, lastResponseOutput, true, true)
}

func normalizeResponsesWebsocketRequestWithMode(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithLastResponseID(rawJSON, lastRequest, lastResponseOutput, "", allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
}

func normalizeResponsesWebsocketRequestWithLastResponseID(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, lastResponseID string, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithIncrementalState(rawJSON, lastRequest, lastResponseOutput, lastResponseID, nil, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
}

func normalizeResponsesWebsocketRequestWithIncrementalState(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, lastResponseID string, lastResponsePendingToolCallIDs []string, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	if !json.Valid(rawJSON) {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid websocket request JSON"),
		}
	}
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		// log.Infof("responses websocket: response.create request")
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, lastResponseID, lastResponsePendingToolCallIDs, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	case wsRequestTypeAppend:
		// log.Infof("responses websocket: response.append request")
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, lastResponseID, lastResponsePendingToolCallIDs, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	default:
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = internalpayload.CloneBytes(rawJSON)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	input := gjson.GetBytes(normalized, "input")
	if !input.Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	} else if _, errScan := scanResponsesWebsocketInput(input); errScan != nil {
		return nil, nil, responsesWebsocketInputError(errScan)
	}

	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	return normalized, normalized, nil
}

func normalizeResponseSubsequentRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, lastResponseID string, lastResponsePendingToolCallIDs []string, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request received before response.create"),
		}
	}

	nextInput := gjson.GetBytes(rawJSON, "input")
	if !nextInput.Exists() {
		return nil, lastRequest, responsesWebsocketInputError(errResponsesWebsocketInputNotArray)
	}
	nextInputSummary, errScanInput := scanResponsesWebsocketInput(nextInput)
	if errScanInput != nil {
		return nil, lastRequest, responsesWebsocketInputError(errScanInput)
	}

	// Compaction can cause clients to replace local websocket history with a new
	// compact transcript on the next `response.create`. When the input already
	// contains historical model output items, treating it as an incremental append
	// duplicates stale turn-state and can leave late orphaned function_call items.
	if shouldReplaceWebsocketTranscriptFromSummary(rawJSON, nextInputSummary) {
		normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
		return normalized, normalized, nil
	}

	// Websocket v2 mode uses response.create with previous_response_id + incremental input.
	// Do not expand it into a full input transcript; upstream expects the incremental payload.
	if allowIncrementalInputWithPreviousResponseID {
		prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String())
		if prev == "" {
			if !nextInputSummary.satisfiesPendingToolCalls(lastResponsePendingToolCallIDs) {
				normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
				return normalized, normalized, nil
			}
			prev = strings.TrimSpace(lastResponseID)
		}
		if prev != "" {
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = internalpayload.CloneBytes(rawJSON)
			}
			normalized, _ = sjson.SetBytes(normalized, "previous_response_id", prev)
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			return normalized, normalized, nil
		}
	}

	// When the client sends a compact replay for a downstream that can consume it
	// directly, the input already carries the canonical history. In that case,
	// skip merging with stale lastRequest/lastResponseOutput to avoid breaking
	// function_call / function_call_output pairings.
	// See: https://github.com/router-for-me/CLIProxyAPI/issues/2207
	var mergedInput string
	if allowCompactionReplayBypass && nextInputSummary.containsFullTranscript {
		log.Infof("responses websocket: full transcript detected, skipping stale merge (input items=%d)", nextInputSummary.count)
		mergedInput = nextInput.Raw
		if dedupedInput, errDedupeInput := dedupeResponsesWebsocketInput(mergedInput); errDedupeInput == nil {
			mergedInput = dedupedInput
		}
	} else {
		appendInputRaw := nextInput.Raw
		if nextInputSummary.containsFullTranscript {
			appendInputRaw = nextInputSummary.withoutCompactionItems()
		}

		existingInput := gjson.GetBytes(lastRequest, "input")
		existingInputRaw := normalizeJSONArrayRaw([]byte(existingInput.Raw))
		normalizedLastResponseOutput := normalizeJSONArrayRaw(lastResponseOutput)
		if websocketTranscriptReplayTooLarge(existingInputRaw, normalizedLastResponseOutput, appendInputRaw) {
			return nil, lastRequest, &interfaces.ErrorMessage{
				StatusCode: http.StatusRequestEntityTooLarge,
				Error: fmt.Errorf(
					"websocket transcript exceeds %d bytes; send a compact replay, continue with previous_response_id, or reconnect",
					responsesWebsocketTranscriptReplayLimitBytes,
				),
			}
		}
		var errMerge error
		mergedInput, errMerge = mergeAndDedupeResponsesWebsocketInputs(existingInputRaw, normalizedLastResponseOutput, appendInputRaw)
		if errMerge != nil {
			if errors.Is(errMerge, errResponsesWebsocketInputTooManyItems) {
				return nil, lastRequest, responsesWebsocketInputError(errMerge)
			}
			return nil, lastRequest, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("invalid request input: %w", errMerge),
			}
		}
	}
	if len(mergedInput) > responsesWebsocketTranscriptReplayLimitBytes {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusRequestEntityTooLarge,
			Error: fmt.Errorf(
				"websocket transcript exceeds %d bytes; send a compact replay, continue with previous_response_id, or reconnect",
				responsesWebsocketTranscriptReplayLimitBytes,
			),
		}
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = internalpayload.CloneBytes(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("failed to merge websocket input: %w", errSet),
		}
	}
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return normalized, normalized, nil
}

type responsesWebsocketInputItem struct {
	raw      string
	itemType string
	role     string
	id       string
	callID   string
}

type responsesWebsocketInputSummary struct {
	items                    []responsesWebsocketInputItem
	count                    int
	hasTranscriptReplacement bool
	containsFullTranscript   bool
	outputCallIDs            map[string]struct{}
}

func scanResponsesWebsocketInput(input gjson.Result) (responsesWebsocketInputSummary, error) {
	if !input.IsArray() {
		return responsesWebsocketInputSummary{}, errResponsesWebsocketInputNotArray
	}

	summary := responsesWebsocketInputSummary{
		items: make([]responsesWebsocketInputItem, 0, min(responsesWebsocketMaxInputItems, 16)),
	}
	var scanErr error
	input.ForEach(func(_, value gjson.Result) bool {
		summary.count++
		if summary.count > responsesWebsocketMaxInputItems {
			scanErr = errResponsesWebsocketInputTooManyItems
			return false
		}

		fields := gjson.GetMany(value.Raw, "type", "role", "id", "call_id")
		item := responsesWebsocketInputItem{
			raw:      value.Raw,
			itemType: strings.TrimSpace(fields[0].String()),
			role:     strings.TrimSpace(fields[1].String()),
			id:       strings.TrimSpace(fields[2].String()),
			callID:   strings.TrimSpace(fields[3].String()),
		}
		summary.items = append(summary.items, item)

		switch item.itemType {
		case "function_call", "custom_tool_call":
			summary.hasTranscriptReplacement = true
		case "message":
			if item.role == "assistant" {
				summary.hasTranscriptReplacement = true
			}
		case "compaction", "compaction_summary":
			summary.containsFullTranscript = true
		case "function_call_output", "custom_tool_call_output":
			if item.callID != "" {
				if summary.outputCallIDs == nil {
					summary.outputCallIDs = make(map[string]struct{})
				}
				summary.outputCallIDs[item.callID] = struct{}{}
			}
		}
		return true
	})
	return summary, scanErr
}

func scanResponsesWebsocketRawArray(raw string) (responsesWebsocketInputSummary, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "[]"
	}
	if !gjson.Valid(raw) {
		return responsesWebsocketInputSummary{}, fmt.Errorf("invalid JSON array")
	}
	return scanResponsesWebsocketInput(gjson.Parse(raw))
}

func responsesWebsocketInputError(err error) *interfaces.ErrorMessage {
	if errors.Is(err, errResponsesWebsocketInputTooManyItems) {
		return &interfaces.ErrorMessage{
			StatusCode: http.StatusRequestEntityTooLarge,
			Error:      fmt.Errorf("websocket input exceeds %d items", responsesWebsocketMaxInputItems),
		}
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errResponsesWebsocketInputNotArray,
	}
}

func shouldReplaceWebsocketTranscriptFromSummary(rawJSON []byte, summary responsesWebsocketInputSummary) bool {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) != "" {
		return false
	}
	return summary.hasTranscriptReplacement
}

func (summary responsesWebsocketInputSummary) satisfiesPendingToolCalls(pendingCallIDs []string) bool {
	for _, callID := range pendingCallIDs {
		callID = strings.TrimSpace(callID)
		if callID == "" {
			continue
		}
		if _, ok := summary.outputCallIDs[callID]; !ok {
			return false
		}
	}
	return true
}

func (summary responsesWebsocketInputSummary) withoutCompactionItems() string {
	return buildResponsesWebsocketInput(summary.items, func(_ int, item responsesWebsocketInputItem) bool {
		return item.itemType != "compaction" && item.itemType != "compaction_summary"
	})
}

func buildResponsesWebsocketInput(items []responsesWebsocketInputItem, keep func(int, responsesWebsocketInputItem) bool) string {
	var builder strings.Builder
	capacity := 2 + len(items)
	for _, item := range items {
		capacity += len(item.raw)
	}
	builder.Grow(capacity)
	builder.WriteByte('[')
	wrote := false
	for index, item := range items {
		if keep != nil && !keep(index, item) {
			continue
		}
		if wrote {
			builder.WriteByte(',')
		}
		builder.WriteString(item.raw)
		wrote = true
	}
	builder.WriteByte(']')
	return builder.String()
}

func shouldReplaceWebsocketTranscript(rawJSON []byte, nextInput gjson.Result) bool {
	if !nextInput.Exists() {
		return false
	}
	summary, errScan := scanResponsesWebsocketInput(nextInput)
	return errScan == nil && shouldReplaceWebsocketTranscriptFromSummary(rawJSON, summary)
}

func inputSatisfiesPendingToolCalls(input gjson.Result, pendingCallIDs []string) bool {
	if len(pendingCallIDs) == 0 {
		return true
	}
	summary, errScan := scanResponsesWebsocketInput(input)
	if errScan != nil {
		return false
	}
	return summary.satisfiesPendingToolCalls(pendingCallIDs)
}

func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = internalpayload.CloneBytes(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return internalpayload.CloneBytes(normalized)
}

func dedupeFunctionCallsByCallID(rawArray string) (string, error) {
	summary, errScan := scanResponsesWebsocketRawArray(rawArray)
	if errScan != nil {
		return "", errScan
	}
	seenCallIDs := make(map[string]struct{}, summary.count)
	out := buildResponsesWebsocketInput(summary.items, func(_ int, item responsesWebsocketInputItem) bool {
		if !isResponsesToolCallType(item.itemType) || item.callID == "" {
			return true
		}
		if _, exists := seenCallIDs[item.callID]; exists {
			return false
		}
		seenCallIDs[item.callID] = struct{}{}
		return true
	})
	return out, nil
}

func dedupeResponsesWebsocketInput(rawArray string) (string, error) {
	summary, errScan := scanResponsesWebsocketRawArray(rawArray)
	if errScan != nil {
		return "", errScan
	}
	return dedupeResponsesWebsocketInputItems(summary.items), nil
}

func dedupeResponsesWebsocketInputItems(items []responsesWebsocketInputItem) string {
	active := make([]bool, len(items))
	seenCallIDs := make(map[string]struct{}, len(items))
	for index, item := range items {
		active[index] = true
		if !isResponsesToolCallType(item.itemType) || item.callID == "" {
			continue
		}
		if _, exists := seenCallIDs[item.callID]; exists {
			active[index] = false
			continue
		}
		seenCallIDs[item.callID] = struct{}{}
	}

	referencedCallIDs := make(map[string]struct{}, len(items))
	for index, item := range items {
		if !active[index] {
			continue
		}
		switch item.itemType {
		case "function_call_output", "custom_tool_call_output":
			if item.callID != "" {
				referencedCallIDs[item.callID] = struct{}{}
			}
		}
	}

	keepIndexByID := make(map[string]int, len(items))
	keepReferencedByID := make(map[string]bool, len(items))
	for index, item := range items {
		if !active[index] || item.id == "" {
			continue
		}
		_, referenced := referencedCallIDs[item.callID]
		referenced = referenced && item.callID != ""
		if _, seen := keepIndexByID[item.id]; !seen {
			keepIndexByID[item.id] = index
			keepReferencedByID[item.id] = referenced
			continue
		}
		if referenced || !keepReferencedByID[item.id] {
			keepIndexByID[item.id] = index
			keepReferencedByID[item.id] = referenced
		}
	}

	out := buildResponsesWebsocketInput(items, func(index int, item responsesWebsocketInputItem) bool {
		return active[index] && (item.id == "" || keepIndexByID[item.id] == index)
	})
	return out
}

func dedupeResponsesWebsocketInputItemsByID(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return payload
	}
	dedupedInput, errDedupe := dedupeInputItemsByID(input.Raw)
	if errDedupe != nil || dedupedInput == input.Raw {
		return payload
	}
	updated, errSet := sjson.SetRawBytes(payload, "input", []byte(dedupedInput))
	if errSet != nil {
		return payload
	}
	return updated
}

func dedupeInputItemsByID(rawArray string) (string, error) {
	summary, errScan := scanResponsesWebsocketRawArray(rawArray)
	if errScan != nil {
		return "", errScan
	}
	items := summary.items

	// Collect the call_ids that are still referenced by tool-call output
	// items. When several input items share the same id, the one we keep must
	// preserve any call_id that has a matching output; otherwise the upstream
	// rejects the request with "No tool call found for function call output".
	referencedCallIDs := make(map[string]struct{}, len(items))
	for i := range items {
		switch items[i].itemType {
		case "function_call_output", "custom_tool_call_output":
			if items[i].callID != "" {
				referencedCallIDs[items[i].callID] = struct{}{}
			}
		}
	}

	// For each id, choose the index to keep. The default is the last
	// occurrence (matching the original dedupe behavior), but we never replace
	// an item whose call_id still has a matching output with one that does not.
	// This keeps a single item per id while ensuring retained tool calls stay
	// paired with their outputs.
	keepIndexByID := make(map[string]int, len(items))
	keepReferencedByID := make(map[string]bool, len(items))
	for i := range items {
		itemID := items[i].id
		if itemID == "" {
			continue
		}
		_, referenced := referencedCallIDs[items[i].callID]
		referenced = referenced && items[i].callID != ""
		if _, seen := keepIndexByID[itemID]; !seen {
			keepIndexByID[itemID] = i
			keepReferencedByID[itemID] = referenced
			continue
		}
		if referenced || !keepReferencedByID[itemID] {
			keepIndexByID[itemID] = i
			keepReferencedByID[itemID] = referenced
		}
	}

	out := buildResponsesWebsocketInput(items, func(index int, item responsesWebsocketInputItem) bool {
		return item.id == "" || keepIndexByID[item.id] == index
	})
	return out, nil
}

func websocketUpstreamSupportsIncrementalInput(attributes map[string]string, metadata map[string]any) bool {
	if len(attributes) > 0 {
		if raw := strings.TrimSpace(attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(metadata) == 0 {
		return false
	}
	raw, ok := metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsIncrementalInputForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	for _, auth := range auths {
		if responsesWebsocketAuthSupportsIncrementalInput(auth) {
			return true
		}
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsCompactionReplayForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	if len(auths) == 0 {
		return false
	}
	for _, auth := range auths {
		if !responsesWebsocketAuthSupportsCompactionReplay(auth) {
			return false
		}
	}
	return true
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketAvailableAuthsForModel(modelName string) ([]*coreauth.Auth, string) {
	if h == nil || h.AuthManager == nil {
		return nil, ""
	}
	resolvedModelName := responsesWebsocketResolvedModelName(modelName)
	providerSet, modelKey := responsesWebsocketProviderSetForModel(resolvedModelName, h.AuthManager)
	if len(providerSet) == 0 {
		return nil, modelKey
	}

	registryRef := registry.GetGlobalRegistry()
	now := time.Now()
	auths := h.AuthManager.List()
	available := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if !responsesWebsocketAuthMatchesModel(auth, providerSet, modelKey, registryRef, now) {
			continue
		}
		available = append(available, auth)
	}
	return available, modelKey
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketUsesUpstreamWebsocketPassthrough(modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if h == nil || h.AuthManager == nil || modelName == "" {
		return false
	}
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	if len(auths) == 0 {
		return false
	}
	provider := ""
	for _, auth := range auths {
		if auth == nil {
			return false
		}
		authProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if authProvider != "codex" && authProvider != "xai" {
			return false
		}
		if provider == "" {
			provider = authProvider
			if _, ok := h.AuthManager.Executor(provider); !ok {
				return false
			}
		} else if authProvider != provider {
			return false
		}
		if !websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata) {
			return false
		}
	}
	return provider != ""
}

func responsesWebsocketAuthSupportsIncrementalInput(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata)
}

func normalizeResponsesWebsocketPassthroughRequest(rawJSON []byte, modelName string) ([]byte, *interfaces.ErrorMessage) {
	if !json.Valid(rawJSON) {
		return nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid websocket request JSON"),
		}
	}

	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate, wsRequestTypeAppend:
	default:
		return nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
	if input := gjson.GetBytes(rawJSON, "input"); input.Exists() {
		if _, errScan := scanResponsesWebsocketInput(input); errScan != nil {
			return nil, responsesWebsocketInputError(errScan)
		}
	}

	normalized := internalpayload.CloneBytes(rawJSON)
	if strings.TrimSpace(gjson.GetBytes(normalized, "model").String()) == "" {
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			return nil, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("missing model in response.create request"),
			}
		}
		normalized, _ = sjson.SetBytes(normalized, "model", modelName)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return normalized, nil
}

func responsesWebsocketResolvedModelName(modelName string) string {
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			return fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		}
		return resolvedBase
	}
	return util.ResolveAutoModel(modelName)
}

func responsesWebsocketProviderSetForModel(resolvedModelName string, authManager *coreauth.Manager) (map[string]struct{}, string) {
	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)
	providers := handlers.ResolveProvidersForModel(baseModel, authManager)
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = handlers.ResolveProvidersForModel(resolvedModelName, authManager)
	}
	if len(providers) == 0 {
		if hintedBase, _ := handlers.NormalizePublicModelHint(baseModel); hintedBase != "" && hintedBase != baseModel {
			providers = handlers.ResolveProvidersForModel(hintedBase, authManager)
		}
	}
	if len(providers) == 0 {
		if hintedBase, _ := handlers.NormalizePublicModelHint(resolvedModelName); hintedBase != "" && hintedBase != resolvedModelName {
			providers = handlers.ResolveProvidersForModel(hintedBase, authManager)
		}
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	modelKey := baseModel
	if modelKey == "" {
		modelKey = strings.TrimSpace(resolvedModelName)
	}
	return providerSet, modelKey
}

func responsesWebsocketAuthMatchesModel(auth *coreauth.Auth, providerSet map[string]struct{}, modelKey string, registryRef *registry.ModelRegistry, now time.Time) bool {
	if auth == nil {
		return false
	}
	providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
	if _, ok := providerSet[providerKey]; !ok {
		return false
	}
	if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(auth.ID, modelKey) {
		return false
	}
	return responsesWebsocketAuthAvailableForModel(auth, modelKey, now)
}

func responsesWebsocketAuthSupportsCompactionReplay(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func responsesWebsocketAuthAvailableForModel(auth *coreauth.Auth, modelName string, now time.Time) bool {
	if auth == nil {
		return false
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if modelName != "" && len(auth.ModelStates) > 0 {
		state, ok := auth.ModelStates[modelName]
		if (!ok || state == nil) && modelName != "" {
			baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
			if baseModel != "" && baseModel != modelName {
				state, ok = auth.ModelStates[baseModel]
			}
		}
		if ok && state != nil {
			if state.Status == coreauth.StatusDisabled {
				return false
			}
			if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
				return false
			}
			return true
		}
	}
	if auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return false
	}
	return true
}

func shouldHandleResponsesWebsocketPrewarmLocally(rawJSON []byte, lastRequest []byte, allowIncrementalInputWithPreviousResponseID bool) bool {
	if allowIncrementalInputWithPreviousResponseID || len(lastRequest) != 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	generateResult := gjson.GetBytes(rawJSON, "generate")
	return generateResult.Exists() && !generateResult.Bool()
}

func writeResponsesWebsocketSyntheticPrewarm(
	c *gin.Context,
	conn *websocket.Conn,
	requestJSON []byte,
	wsTimelineLog websocketTimelineAppender,
	sessionID string,
) error {
	payloads, errPayloads := syntheticResponsesWebsocketPrewarmPayloads(requestJSON)
	if errPayloads != nil {
		return errPayloads
	}
	for i := 0; i < len(payloads); i++ {
		markAPIResponseTimestamp(c)
		if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
			log.Warnf(
				"responses websocket: downstream_out write failed id=%s event=%s",
				sessionID,
				websocketPayloadEventType(payloads[i]),
			)
			return errWrite
		}
	}
	return nil
}

func syntheticResponsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, error) {
	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String())

	createdPayload := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
	var errSet error
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		createdPayload, errSet = sjson.SetBytes(createdPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	completedPayload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		completedPayload, errSet = sjson.SetBytes(completedPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	return [][]byte{createdPayload, completedPayload}, nil
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	return mergeJSONArraysRaw(existingRaw, appendRaw)
}

func mergeJSONArraysRaw(rawArrays ...string) (string, error) {
	items, errCollect := collectResponsesWebsocketInputItems(rawArrays...)
	if errCollect != nil {
		return "", errCollect
	}
	return buildResponsesWebsocketInput(items, nil), nil
}

func mergeAndDedupeResponsesWebsocketInputs(rawArrays ...string) (string, error) {
	items, errCollect := collectResponsesWebsocketInputItems(rawArrays...)
	if errCollect != nil {
		return "", errCollect
	}
	return dedupeResponsesWebsocketInputItems(items), nil
}

func collectResponsesWebsocketInputItems(rawArrays ...string) ([]responsesWebsocketInputItem, error) {
	items := make([]responsesWebsocketInputItem, 0)
	for _, rawArray := range rawArrays {
		summary, errScan := scanResponsesWebsocketRawArray(rawArray)
		if errScan != nil {
			return nil, errScan
		}
		if len(items)+summary.count > responsesWebsocketMaxInputItems {
			return nil, errResponsesWebsocketInputTooManyItems
		}
		items = append(items, summary.items...)
	}
	return items, nil
}

func websocketTranscriptReplayTooLarge(existingInputRaw, lastResponseOutputRaw, appendInputRaw string) bool {
	if responsesWebsocketTranscriptReplayLimitBytes <= 0 {
		return false
	}
	total := len(existingInputRaw) + len(lastResponseOutputRaw) + len(strings.TrimSpace(appendInputRaw))
	return total > responsesWebsocketTranscriptReplayLimitBytes
}

// inputContainsFullTranscript returns true when the input array carries compact
// replay markers that indicate the client already sent the full conversation
// transcript. Merging that input with stale lastRequest/lastResponseOutput
// would duplicate or break function_call/function_call_output pairings, so the
// caller should use the input as-is.
//
// Assistant messages alone are not enough to classify the payload as a replay:
// incremental websocket requests may legitimately append assistant items.
func inputContainsFullTranscript(input gjson.Result) bool {
	summary, errScan := scanResponsesWebsocketInput(input)
	return errScan == nil && summary.containsFullTranscript
}

func inputWithoutCompactionItems(input gjson.Result) string {
	summary, errScan := scanResponsesWebsocketInput(input)
	if errScan != nil {
		return normalizeJSONArrayRaw([]byte(input.Raw))
	}
	return summary.withoutCompactionItems()
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "[]"
	}
	result := gjson.Parse(trimmed)
	if result.Type == gjson.JSON && result.IsArray() {
		return trimmed
	}
	return "[]"
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog websocketTimelineAppender,
	sessionID string,
	toolCallCache *websocketToolOutputCache,
	toolCacheKey string,
	completedOutputLease *responsesWebsocketFrameLease,
) ([]byte, string, []string, *interfaces.ErrorMessage, error) {
	completed := false
	completedOutput := []byte("[]")
	completedResponseID := ""
	pendingToolCallIDs := make(map[string]struct{})
	for {
		if data == nil && errs == nil {
			if !completed {
				errMsg := &interfaces.ErrorMessage{
					StatusCode: http.StatusRequestTimeout,
					Error:      fmt.Errorf("stream closed before response.completed"),
				}
				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
				markAPIResponseTimestamp(c)
				errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg, handlers.PassthroughHeadersEnabled(h.Cfg))
				log.Infof(
					"responses websocket: downstream_out id=%s type=%d event=%s payload_metadata=%s",
					sessionID,
					websocket.TextMessage,
					websocketPayloadEventType(errorPayload),
					websocketPayloadPreview(errorPayload),
				)
				if errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s",
						sessionID,
						websocketPayloadEventType(errorPayload),
					)
					cancel(errMsg.Error)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errWrite
				}
				cancel(errMsg.Error)
				return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
			}
			cancel(nil)
			return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, nil
		}

		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, c.Request.Context().Err()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
				markAPIResponseTimestamp(c)
				errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg, handlers.PassthroughHeadersEnabled(h.Cfg))
				log.Infof(
					"responses websocket: downstream_out id=%s type=%d event=%s payload_metadata=%s",
					sessionID,
					websocket.TextMessage,
					websocketPayloadEventType(errorPayload),
					websocketPayloadPreview(errorPayload),
				)
				if errWrite != nil {
					cancel(errMsg.Error)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, errWrite
				}
			}
			if errMsg != nil {
				cancel(errMsg.Error)
			} else {
				cancel(nil)
			}
			return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), errMsg, nil
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}

			payloads := websocketJSONPayloadsFromChunk(chunk)
			for i := range payloads {
				recordResponsesWebsocketToolCallsFromPayloadWithCache(toolCallCache, toolCacheKey, payloads[i])
				recordPendingToolCallIDsFromPayload(pendingToolCallIDs, payloads[i])
				eventType := gjson.GetBytes(payloads[i], "type").String()
				var payloadErrMsg *interfaces.ErrorMessage
				if eventType == wsEventTypeError {
					payloadErrMsg = responsesWebsocketErrorMessageFromPayload(payloads[i])
					if h != nil {
						h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), payloadErrMsg)
					}
				} else if isResponsesWebsocketCompletionEvent(eventType) {
					nextCompletedOutput := responseCompletedOutputRawFromPayload(payloads[i])
					if completedOutputLease != nil && !completedOutputLease.resize(int64(len(nextCompletedOutput))) {
						cancel(errResponsesWebsocketRetainedCapacity)
						return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, errResponsesWebsocketRetainedCapacity
					}
					completed = true
					completedOutput = internalpayload.CloneStringBytes(nextCompletedOutput)
					completedResponseID = responseCompletedIDFromPayload(payloads[i])
				}
				markAPIResponseTimestamp(c)
				if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s",
						sessionID,
						websocketPayloadEventType(payloads[i]),
					)
					cancel(errWrite)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), nil, errWrite
				}
				if payloadErrMsg != nil {
					cancel(payloadErrMsg.Error)
					return completedOutput, completedResponseID, sortedStringSet(pendingToolCallIDs), payloadErrMsg, nil
				}
			}
		}
	}
}

func shouldReleaseResponsesWebsocketPinnedAuth(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	status := errMsg.StatusCode
	if status <= 0 && errMsg.Error != nil {
		if se, ok := errMsg.Error.(interface{ StatusCode() int }); ok && se != nil {
			status = se.StatusCode()
		}
	}
	switch status {
	case http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusRequestTimeout,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	if errMsg.Error != nil {
		msg := strings.ToLower(errMsg.Error.Error())
		switch {
		case strings.Contains(msg, "stream closed before response.completed"),
			strings.Contains(msg, "previous_response_not_found"),
			strings.Contains(msg, "ws_failed"),
			strings.Contains(msg, "upstream stream closed before first payload"),
			strings.Contains(msg, "empty_stream"):
			return true
		}
	}
	return false
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	return internalpayload.CloneStringBytes(responseCompletedOutputRawFromPayload(payload))
}

func responseCompletedOutputRawFromPayload(payload []byte) string {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() {
		return output.Raw
	}
	return "[]"
}

func responseCompletedIDFromPayload(payload []byte) string {
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func recordPendingToolCallIDsFromPayload(pending map[string]struct{}, payload []byte) {
	if pending == nil || len(payload) == 0 {
		return
	}
	updatePendingToolCallIDsFromItem(pending, gjson.GetBytes(payload, "item"))
	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			updatePendingToolCallIDsFromItem(pending, item)
			return true
		})
	}
}

func updatePendingToolCallIDsFromItem(pending map[string]struct{}, item gjson.Result) {
	if pending == nil || !item.Exists() {
		return
	}
	switch strings.TrimSpace(item.Get("type").String()) {
	case "function_call", "custom_tool_call":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			pending[callID] = struct{}{}
		}
	case "function_call_output", "custom_tool_call_output":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			delete(pending, callID)
		}
	}
}

func sortedStringSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
	lines := bytes.Split(chunk, []byte("\n"))
	for i := range lines {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, internalpayload.CloneBytes(line))
		}
	}

	if len(payloads) > 0 {
		return payloads
	}

	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, internalpayload.CloneBytes(trimmed))
	}
	return payloads
}

func writeResponsesWebsocketError(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender, errMsg *interfaces.ErrorMessage, passthroughHeaders bool) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
			errText = http.StatusText(status)
		}
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
	}
	status, errText = handlers.NormalizeKnownUserError(status, errText, nil)

	body := handlers.BuildErrorResponseBody(status, errText)
	payload := []byte(`{}`)
	var errSet error
	payload, errSet = sjson.SetBytes(payload, "type", wsEventTypeError)
	if errSet != nil {
		return nil, errSet
	}
	payload, errSet = sjson.SetBytes(payload, "status", status)
	if errSet != nil {
		return nil, errSet
	}

	if errMsg != nil && errMsg.Addon != nil {
		headers := make(map[string]string)
		for key, values := range handlers.FilterErrorAddonHeaders(errMsg.Addon, passthroughHeaders) {
			if len(values) == 0 {
				continue
			}
			headers[key] = values[0]
		}
		if len(headers) > 0 {
			headersJSON, errMarshal := json.Marshal(headers)
			if errMarshal != nil {
				return nil, errMarshal
			}
			payload, errSet = sjson.SetRawBytes(payload, "headers", headersJSON)
			if errSet != nil {
				return nil, errSet
			}
		}
	}

	if len(body) > 0 && json.Valid(body) {
		errorNode := gjson.GetBytes(body, "error")
		if errorNode.Exists() {
			payload, errSet = sjson.SetRawBytes(payload, "error", []byte(errorNode.Raw))
		} else {
			payload, errSet = sjson.SetRawBytes(payload, "error", body)
		}
		if errSet != nil {
			return nil, errSet
		}
	}

	if !gjson.GetBytes(payload, "error").Exists() {
		payload, errSet = sjson.SetBytes(payload, "error.type", "server_error")
		if errSet != nil {
			return nil, errSet
		}
		payload, errSet = sjson.SetBytes(payload, "error.message", errText)
		if errSet != nil {
			return nil, errSet
		}
	}

	return payload, writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now())
}

func appendWebsocketEvent(builder *strings.Builder, eventType string, payload []byte) {
	if builder == nil {
		return
	}
	payloadMetadata := websocketTimelinePayloadMetadata(payload)
	if len(payloadMetadata) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString("websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(payloadMetadata)
	builder.WriteString("\n")
}

func websocketPayloadEventType(payload []byte) string {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if !safeResponsesWebsocketErrorCode(eventType) {
		return "-"
	}
	return eventType
}

func websocketPayloadPreview(payload []byte) string {
	return string(requestlogging.SummarizeBodyForLog(payload, "application/json"))
}

func isResponsesWebsocketCompletionEvent(eventType string) bool {
	return eventType == wsEventTypeCompleted || eventType == wsEventTypeDone
}

func responsesWebsocketErrorMessageFromPayload(payload []byte) *interfaces.ErrorMessage {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		status = http.StatusInternalServerError
	}

	errText := "upstream websocket error"
	code := strings.TrimSpace(gjson.GetBytes(payload, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(payload, "code").String())
	}
	if safeResponsesWebsocketErrorCode(code) {
		errText += " code=" + code
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", errText)}
}

func safeResponsesWebsocketErrorCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, character := range code {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func setWebsocketTimelineBody(c *gin.Context, body string) {
	setWebsocketBody(c, wsTimelineBodyKey, body)
}

func setWebsocketBody(c *gin.Context, key string, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(key, []byte(trimmedBody))
}

func writeResponsesWebsocketPayload(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender, payload []byte, timestamp time.Time) error {
	if wsTimelineLog != nil {
		wsTimelineLog.Append("response", payload, timestamp)
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func appendWebsocketTimelineDisconnect(timeline websocketTimelineAppender, err error, timestamp time.Time) {
	if err == nil {
		return
	}
	if timeline != nil {
		timeline.Append("disconnect", []byte(err.Error()), timestamp)
	}
}

func appendWebsocketTimelineEvent(builder *strings.Builder, eventType string, payload []byte, timestamp time.Time) {
	if builder == nil {
		return
	}
	writeWebsocketTimelineBuilder(builder, formatWebsocketTimelineEvent(eventType, payload, timestamp))
}

func formatWebsocketTimelineEvent(eventType string, payload []byte, timestamp time.Time) []byte {
	payloadMetadata := websocketTimelinePayloadMetadata(payload)
	if len(payloadMetadata) == 0 {
		return nil
	}
	timestampText := timestamp.Format(time.RFC3339Nano)
	size := websocketTimelineEventSize(eventType, payloadMetadata, timestampText)
	return formatWebsocketTimelineEventParts(eventType, payloadMetadata, timestampText, size)
}

func websocketTimelinePayloadMetadata(payload []byte) []byte {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return nil
	}
	return requestlogging.SummarizeBodyForLog(trimmedPayload, "application/websocket-frame")
}

func websocketTimelineEventSize(eventType string, trimmedPayload []byte, timestampText string) int {
	return len("Timestamp: ") + len(timestampText) + len("\nEvent: websocket.") + len(eventType) + 1 + len(trimmedPayload) + 1
}

func formatWebsocketTimelineEventParts(eventType string, trimmedPayload []byte, timestampText string, size int) []byte {
	var builder bytes.Buffer
	builder.Grow(size)
	builder.WriteString("Timestamp: ")
	builder.WriteString(timestampText)
	builder.WriteString("\nEvent: websocket.")
	builder.WriteString(eventType)
	builder.WriteByte('\n')
	builder.Write(trimmedPayload)
	builder.WriteByte('\n')
	return builder.Bytes()
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
