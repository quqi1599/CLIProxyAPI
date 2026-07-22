package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	websocketToolOutputCacheMaxPerSession = 256
	websocketToolOutputCacheMaxBytes      = 8 << 20
	websocketToolOutputCacheTTL           = 30 * time.Minute
	websocketToolSessionKeyMaxBytes       = 256
)

type websocketToolOutputCache struct {
	mu            sync.Mutex
	ttl           time.Duration
	maxPerSession int
	maxBytes      int
	bytes         int
	sessions      map[string]*websocketToolOutputSession
	retainedLease *responsesWebsocketFrameLease
	closed        bool
}

type websocketToolOutputSession struct {
	lastSeen time.Time
	outputs  map[string]json.RawMessage
	order    []string
	bytes    int
}

func newWebsocketToolOutputCache(ttl time.Duration, maxPerSession int) *websocketToolOutputCache {
	return newWebsocketToolOutputCacheWithBudget(ttl, maxPerSession, websocketToolOutputCacheMaxBytes)
}

func newWebsocketToolOutputCacheWithBudget(ttl time.Duration, maxPerSession, maxBytes int) *websocketToolOutputCache {
	return newWebsocketToolOutputCacheWithRetainedBudget(ttl, maxPerSession, maxBytes, nil)
}

func newWebsocketToolOutputCacheWithRetainedBudget(ttl time.Duration, maxPerSession, maxBytes int, limiter *responsesWebsocketFrameLimiter) *websocketToolOutputCache {
	if ttl < 0 {
		ttl = websocketToolOutputCacheTTL
	}
	if maxPerSession <= 0 {
		maxPerSession = websocketToolOutputCacheMaxPerSession
	}
	if maxBytes <= 0 {
		maxBytes = websocketToolOutputCacheMaxBytes
	}
	cache := &websocketToolOutputCache{
		ttl:           ttl,
		maxPerSession: maxPerSession,
		maxBytes:      maxBytes,
		sessions:      make(map[string]*websocketToolOutputSession),
	}
	if limiter != nil {
		cache.retainedLease = limiter.acquire(0)
	}
	return cache
}

func (c *websocketToolOutputCache) record(sessionKey string, callID string, item json.RawMessage) {
	sessionKey = normalizeResponsesWebsocketSessionKey(sessionKey)
	callID = strings.TrimSpace(callID)
	if c == nil || sessionKey == "" || callID == "" {
		return
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}

	c.cleanupLocked(now)

	session, ok := c.sessions[sessionKey]
	if len(item) > c.maxBytes {
		if ok && session != nil {
			c.bytes -= removeWebsocketToolOutput(session, callID)
			c.resizeRetainedLocked(c.bytes)
		}
		return
	}
	if !ok || session == nil {
		session = &websocketToolOutputSession{
			lastSeen: now,
			outputs:  make(map[string]json.RawMessage),
		}
		c.sessions[sessionKey] = session
	}
	session.lastSeen = now

	c.bytes -= removeWebsocketToolOutput(session, callID)
	for len(session.order) >= c.maxPerSession || session.bytes+len(item) > c.maxBytes {
		c.bytes -= removeWebsocketToolOutput(session, session.order[0])
	}
	if !c.resizeRetainedLocked(c.bytes + len(item)) {
		c.resizeRetainedLocked(c.bytes)
		if len(session.outputs) == 0 {
			delete(c.sessions, sessionKey)
		}
		return
	}
	item = append(json.RawMessage(nil), item...)
	session.order = append(session.order, callID)
	session.outputs[callID] = item
	session.bytes += len(item)
	c.bytes += len(item)
}

func removeWebsocketToolOutput(session *websocketToolOutputSession, callID string) int {
	if session == nil {
		return 0
	}
	removedBytes := 0
	if previous, exists := session.outputs[callID]; exists {
		removedBytes = len(previous)
		session.bytes -= removedBytes
		delete(session.outputs, callID)
	}
	for i := range session.order {
		if session.order[i] == callID {
			session.order = append(session.order[:i], session.order[i+1:]...)
			return removedBytes
		}
	}
	return removedBytes
}

func (c *websocketToolOutputCache) get(sessionKey string, callID string) (json.RawMessage, bool) {
	sessionKey = normalizeResponsesWebsocketSessionKey(sessionKey)
	callID = strings.TrimSpace(callID)
	if sessionKey == "" || callID == "" || c == nil {
		return nil, false
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false
	}

	c.cleanupLocked(now)

	session, ok := c.sessions[sessionKey]
	if !ok || session == nil {
		return nil, false
	}
	session.lastSeen = now
	item, ok := session.outputs[callID]
	if !ok || len(item) == 0 {
		return nil, false
	}
	return append(json.RawMessage(nil), item...), true
}

func (c *websocketToolOutputCache) cleanupLocked(now time.Time) {
	if c == nil || c.ttl <= 0 {
		return
	}

	for key, session := range c.sessions {
		if session == nil {
			delete(c.sessions, key)
			continue
		}
		if now.Sub(session.lastSeen) > c.ttl {
			c.bytes -= session.bytes
			delete(c.sessions, key)
		}
	}
	c.resizeRetainedLocked(c.bytes)
}

func (c *websocketToolOutputCache) deleteSession(sessionKey string) {
	sessionKey = normalizeResponsesWebsocketSessionKey(sessionKey)
	if sessionKey == "" || c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if session := c.sessions[sessionKey]; session != nil {
		c.bytes -= session.bytes
	}
	delete(c.sessions, sessionKey)
	c.resizeRetainedLocked(c.bytes)
}

func (c *websocketToolOutputCache) resizeRetainedLocked(bytes int) bool {
	if bytes < 0 {
		bytes = 0
	}
	if c.retainedLease == nil {
		return true
	}
	return c.retainedLease.resize(int64(bytes))
}

func (c *websocketToolOutputCache) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.bytes = 0
	c.sessions = nil
	lease := c.retainedLease
	c.retainedLease = nil
	c.mu.Unlock()
	lease.release()
}

func websocketDownstreamSessionKey(req *http.Request) string {
	if req == nil {
		return ""
	}
	if requestID := normalizeResponsesWebsocketSessionKey(req.Header.Get("X-Client-Request-Id")); requestID != "" {
		return requestID
	}
	if raw := strings.TrimSpace(req.Header.Get("X-Codex-Turn-Metadata")); raw != "" {
		if sessionID := normalizeResponsesWebsocketSessionKey(gjson.Get(raw, "session_id").String()); sessionID != "" {
			return sessionID
		}
	}
	if sessionID := normalizeResponsesWebsocketSessionKey(req.Header.Get("Session-Id")); sessionID != "" {
		return sessionID
	}
	if sessionID := normalizeResponsesWebsocketSessionKey(req.Header.Get("Session_id")); sessionID != "" {
		return sessionID
	}
	return ""
}

func normalizeResponsesWebsocketSessionKey(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if len(sessionKey) > websocketToolSessionKeyMaxBytes {
		return ""
	}
	return sessionKey
}

func repairResponsesWebsocketToolCallsWithCache(cache *websocketToolOutputCache, sessionKey string, payload []byte) []byte {
	return repairResponsesWebsocketToolCallsWithCaches(cache, nil, sessionKey, payload)
}

func repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache *websocketToolOutputCache, sessionKey string, payload []byte) []byte {
	sessionKey = normalizeResponsesWebsocketSessionKey(sessionKey)
	if sessionKey == "" || outputCache == nil || len(payload) == 0 {
		return payload
	}

	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return payload
	}

	allowOrphanOutputs := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != ""
	updatedRaw, errRepair := repairResponsesToolCallsArray(outputCache, callCache, sessionKey, input.Raw, allowOrphanOutputs)
	if errRepair != nil || updatedRaw == "" || updatedRaw == input.Raw {
		return payload
	}

	updated, errSet := sjson.SetRawBytes(payload, "input", []byte(updatedRaw))
	if errSet != nil {
		return payload
	}
	return updated
}

func repairResponsesToolCallsArray(outputCache, callCache *websocketToolOutputCache, sessionKey string, rawArray string, allowOrphanOutputs bool) (string, error) {
	rawArray = strings.TrimSpace(rawArray)
	if rawArray == "" {
		return "[]", nil
	}

	var items []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(rawArray), &items); errUnmarshal != nil {
		return "", errUnmarshal
	}

	// First pass: record tool outputs and remember which call_ids have outputs in this payload.
	outputPresent := make(map[string]struct{}, len(items))
	callPresent := make(map[string]struct{}, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
		switch {
		case isResponsesToolCallOutputType(itemType):
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID == "" {
				continue
			}
			outputPresent[callID] = struct{}{}
			outputCache.record(sessionKey, callID, item)
		case isResponsesToolCallType(itemType):
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID == "" {
				continue
			}
			callPresent[callID] = struct{}{}
			if callCache != nil {
				callCache.record(sessionKey, callID, item)
			}
		}
	}

	filtered := make([]json.RawMessage, 0, len(items))
	insertedCalls := make(map[string]struct{}, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
		if isResponsesToolCallOutputType(itemType) {
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID == "" {
				// Upstream rejects tool outputs without a call_id; drop it.
				continue
			}

			if _, ok := callPresent[callID]; ok {
				filtered = append(filtered, item)
				continue
			}

			if allowOrphanOutputs {
				filtered = append(filtered, item)
				continue
			}

			if callCache != nil {
				if cached, ok := callCache.get(sessionKey, callID); ok {
					if _, already := insertedCalls[callID]; !already {
						filtered = append(filtered, cached)
						insertedCalls[callID] = struct{}{}
						callPresent[callID] = struct{}{}
					}
					filtered = append(filtered, item)
					continue
				}
			}

			// Drop orphaned function_call_output items; upstream rejects transcripts with missing calls.
			continue
		}
		if !isResponsesToolCallType(itemType) {
			filtered = append(filtered, item)
			continue
		}

		callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
		if callID == "" {
			// Upstream rejects tool calls without a call_id; drop it.
			continue
		}

		if _, ok := outputPresent[callID]; ok {
			filtered = append(filtered, item)
			continue
		}

		if allowOrphanOutputs {
			filtered = append(filtered, item)
			continue
		}

		if cached, ok := outputCache.get(sessionKey, callID); ok {
			filtered = append(filtered, item)
			filtered = append(filtered, cached)
			outputPresent[callID] = struct{}{}
			continue
		}

		// Drop orphaned function_call items; upstream rejects transcripts with missing outputs.
	}

	out, errMarshal := json.Marshal(filtered)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

func recordResponsesWebsocketToolCallsFromPayloadWithCache(cache *websocketToolOutputCache, sessionKey string, payload []byte) {
	sessionKey = normalizeResponsesWebsocketSessionKey(sessionKey)
	if sessionKey == "" || cache == nil || len(payload) == 0 {
		return
	}

	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	switch eventType {
	case "response.completed":
		output := gjson.GetBytes(payload, "response.output")
		if !output.Exists() || !output.IsArray() {
			return
		}
		for _, item := range output.Array() {
			if !isResponsesToolCallType(item.Get("type").String()) {
				continue
			}
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID == "" {
				continue
			}
			cache.record(sessionKey, callID, json.RawMessage(item.Raw))
		}
	case "response.output_item.added", "response.output_item.done":
		item := gjson.GetBytes(payload, "item")
		if !item.Exists() || !item.IsObject() {
			return
		}
		if !isResponsesToolCallType(item.Get("type").String()) {
			return
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			return
		}
		cache.record(sessionKey, callID, json.RawMessage(item.Raw))
	}
}

func isResponsesToolCallType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func isResponsesToolCallOutputType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call_output", "custom_tool_call_output":
		return true
	default:
		return false
	}
}
