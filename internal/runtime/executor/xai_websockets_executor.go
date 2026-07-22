// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements an xAI executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	xaiWebsocketTranscriptMaxItems       = 1024
	xaiWebsocketTranscriptMaxBytes       = 32 << 20
	xaiWebsocketTranscriptAggregateBytes = 128 << 20
	xaiWebsocketIDMapMaxEntries          = 2048
)

var errXAIWebsocketIDStateClosed = errors.New("xai websocket session state is closed")

// XAIWebsocketsExecutor executes xAI Responses requests using a WebSocket transport.
type XAIWebsocketsExecutor struct {
	*XAIExecutor

	store   *codexWebsocketSessionStore
	idStore *xaiWebsocketIDStateStore
}

var globalXAIWebsocketSessionStore = &codexWebsocketSessionStore{
	sessions: make(map[string]*codexWebsocketSession),
}

var globalXAIWebsocketIDStates = &xaiWebsocketIDStateStore{
	sessions: make(map[string]*xaiWebsocketIDState),
	budget:   newXAIWebsocketTranscriptBudget(xaiWebsocketTranscriptAggregateBytes),
}

type xaiWebsocketIDStateStore struct {
	mu       sync.Mutex
	sessions map[string]*xaiWebsocketIDState
	budget   *xaiWebsocketTranscriptBudget
}

type xaiWebsocketIDState struct {
	mu                   sync.Mutex
	closed               bool
	budget               *xaiWebsocketTranscriptBudget
	downstreamToUpstream map[string]string
	sequence             int
	transcriptInput      []json.RawMessage
	transcriptReleases   []func()
	transcriptBytes      int64
}

type xaiWebsocketTranscriptBudget struct {
	mu    sync.Mutex
	limit int64
	inUse int64
}

type xaiWebsocketRequestIDMapper struct {
	state                *xaiWebsocketIDState
	downstreamPreviousID string
	upstreamPreviousID   string
	upstreamResponseID   string
	downstreamResponseID string
}

func NewXAIWebsocketsExecutor(cfg *config.Config) *XAIWebsocketsExecutor {
	return &XAIWebsocketsExecutor{
		XAIExecutor: NewXAIExecutor(cfg),
		store:       globalXAIWebsocketSessionStore,
		idStore:     globalXAIWebsocketIDStates,
	}
}

func getXAIWebsocketIDState(store *xaiWebsocketIDStateStore, sessionID string) *xaiWebsocketIDState {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.sessions == nil {
		store.sessions = make(map[string]*xaiWebsocketIDState)
	}
	if store.budget == nil {
		store.budget = newXAIWebsocketTranscriptBudget(xaiWebsocketTranscriptAggregateBytes)
	}
	if state := store.sessions[sessionID]; state != nil {
		return state
	}
	state := &xaiWebsocketIDState{
		downstreamToUpstream: make(map[string]string),
		budget:               store.budget,
	}
	store.sessions[sessionID] = state
	return state
}

func deleteXAIWebsocketIDState(store *xaiWebsocketIDStateStore, sessionID string) {
	deleteXAIWebsocketIDStateIfMatch(store, sessionID, nil)
}

func deleteXAIWebsocketIDStateIfMatch(store *xaiWebsocketIDStateStore, sessionID string, expected *xaiWebsocketIDState) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || store == nil {
		return
	}
	store.mu.Lock()
	state := store.sessions[sessionID]
	if expected != nil {
		state = expected
		if store.sessions[sessionID] == expected {
			delete(store.sessions, sessionID)
		}
	} else {
		delete(store.sessions, sessionID)
	}
	if state != nil {
		state.mu.Lock()
		state.closeLocked()
		state.mu.Unlock()
	}
	store.mu.Unlock()
}

func newXAIWebsocketTranscriptBudget(limit int64) *xaiWebsocketTranscriptBudget {
	if limit < 0 {
		limit = 0
	}
	return &xaiWebsocketTranscriptBudget{limit: limit}
}

func (b *xaiWebsocketTranscriptBudget) resize(currentBytes, nextBytes int64) bool {
	if b == nil {
		return true
	}
	currentBytes = max(currentBytes, 0)
	nextBytes = max(nextBytes, 0)
	delta := nextBytes - currentBytes
	b.mu.Lock()
	defer b.mu.Unlock()
	if delta > 0 && (delta > b.limit || b.inUse > b.limit-delta) {
		return false
	}
	b.inUse += delta
	if b.inUse < 0 {
		b.inUse = 0
	}
	return true
}

func (b *xaiWebsocketTranscriptBudget) InUse() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

func newXAIWebsocketRequestIDMapper(store *xaiWebsocketIDStateStore, sessionID string, downstreamRequest []byte) *xaiWebsocketRequestIDMapper {
	state := getXAIWebsocketIDState(store, sessionID)
	if state == nil {
		return nil
	}
	downstreamPreviousID := strings.TrimSpace(gjson.GetBytes(downstreamRequest, "previous_response_id").String())
	upstreamPreviousID := downstreamPreviousID
	if downstreamPreviousID != "" {
		upstreamPreviousID = state.upstreamIDForDownstream(downstreamPreviousID)
	}
	return &xaiWebsocketRequestIDMapper{
		state:                state,
		downstreamPreviousID: downstreamPreviousID,
		upstreamPreviousID:   upstreamPreviousID,
	}
}

func (s *xaiWebsocketIDState) upstreamIDForDownstream(downstreamID string) string {
	downstreamID = strings.TrimSpace(downstreamID)
	if s == nil || downstreamID == "" {
		return downstreamID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return downstreamID
	}
	if upstreamID, ok := s.downstreamToUpstream[downstreamID]; ok {
		return strings.TrimSpace(upstreamID)
	}
	return downstreamID
}

func xaiWebsocketSessionStateTooLargeError() error {
	return statusErr{
		code:      http.StatusRequestEntityTooLarge,
		msg:       `{"error":{"message":"websocket session state exceeds the safe limit; start a new session","type":"invalid_request_error","code":"session_state_too_large"}}`,
		errorCode: "session_state_too_large",
	}
}

func xaiWebsocketSessionStateCapacityError() error {
	retryAfter := time.Second
	return statusErr{
		code:       http.StatusServiceUnavailable,
		msg:        `{"error":{"message":"websocket session state capacity is temporarily exhausted; retry later","type":"server_error","code":"session_state_capacity"}}`,
		errorCode:  "session_state_capacity",
		retryAfter: &retryAfter,
		headers:    http.Header{"Retry-After": {"1"}},
	}
}

func isXAIWebsocketSessionStateCapacityError(err error) bool {
	var coded interface{ ErrorCode() string }
	return errors.As(err, &coded) && coded.ErrorCode() == "session_state_capacity"
}

func shouldDiscardXAIWebsocketIDState(err error) bool {
	if errors.Is(err, errXAIWebsocketIDStateClosed) {
		return true
	}
	var status interface{ StatusCode() int }
	return errors.As(err, &status) && status.StatusCode() == http.StatusRequestEntityTooLarge
}

func discardXAIWebsocketIDStateOnError(store *xaiWebsocketIDStateStore, sessionID string, expected *xaiWebsocketIDState, err error) {
	if !shouldDiscardXAIWebsocketIDState(err) {
		return
	}
	deleteXAIWebsocketIDStateIfMatch(store, sessionID, expected)
}

func xaiWebsocketSessionStateErrorReason(err error) string {
	if isXAIWebsocketSessionStateCapacityError(err) {
		return "session_state_capacity"
	}
	if errors.Is(err, errXAIWebsocketIDStateClosed) {
		return "session_state_closed"
	}
	return "session_state_too_large"
}

func (s *xaiWebsocketIDState) clearLocked() {
	if s == nil {
		return
	}
	s.downstreamToUpstream = make(map[string]string)
	s.sequence = 0
	s.clearTranscriptLocked()
}

func (s *xaiWebsocketIDState) closeLocked() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	s.clearLocked()
}

func (s *xaiWebsocketIDState) clearTranscriptLocked() {
	if s == nil {
		return
	}
	if s.budget != nil {
		_ = s.budget.resize(s.transcriptBytes, 0)
	}
	releaseXAIWebsocketTranscriptItems(s.transcriptReleases)
	s.transcriptInput = nil
	s.transcriptReleases = nil
	s.transcriptBytes = 0
}

func retainXAIWebsocketTranscriptItems(items []json.RawMessage) []func() {
	if len(items) == 0 {
		return nil
	}
	releases := make([]func(), 0, len(items))
	for _, item := range items {
		releases = append(releases, internalpayload.RetainBytesScoped(item))
	}
	return releases
}

func releaseXAIWebsocketTranscriptItems(releases []func()) {
	for _, release := range releases {
		if release != nil {
			release()
		}
	}
}

func (s *xaiWebsocketIDState) mapDownstreamToUpstream(downstreamID string, upstreamID string) error {
	downstreamID = strings.TrimSpace(downstreamID)
	if s == nil || downstreamID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errXAIWebsocketIDStateClosed
	}
	if s.downstreamToUpstream == nil {
		s.downstreamToUpstream = make(map[string]string)
	}
	if _, exists := s.downstreamToUpstream[downstreamID]; !exists && len(s.downstreamToUpstream) >= xaiWebsocketIDMapMaxEntries {
		s.clearLocked()
		return xaiWebsocketSessionStateTooLargeError()
	}
	s.downstreamToUpstream[downstreamID] = strings.TrimSpace(upstreamID)
	return nil
}

func (s *xaiWebsocketIDState) snapshotTranscriptInput() []byte {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if len(s.transcriptInput) == 0 {
		return nil
	}
	return xaiMarshalRawMessages(s.transcriptInput)
}

func (s *xaiWebsocketIDState) prependTranscriptInput(payload []byte) ([]byte, error) {
	if s == nil || len(payload) == 0 {
		return payload, nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errXAIWebsocketIDStateClosed
	}
	prefix := xaiMarshalRawMessages(s.transcriptInput)
	s.mu.Unlock()
	if len(prefix) <= 2 {
		return payload, nil
	}
	current := bytes.TrimSpace([]byte(gjson.GetBytes(payload, "input").Raw))
	if len(current) < 2 || current[0] != '[' || current[len(current)-1] != ']' {
		current = []byte("[]")
	}
	mergedBytes := len(prefix) + len(current) - 2
	if len(prefix) > 2 && len(current) > 2 {
		mergedBytes++
	}
	if mergedBytes > xaiWebsocketTranscriptMaxBytes {
		s.mu.Lock()
		s.clearLocked()
		s.mu.Unlock()
		return nil, xaiWebsocketSessionStateTooLargeError()
	}
	merged := make([]byte, 0, mergedBytes)
	merged = append(merged, '[')
	merged = append(merged, prefix[1:len(prefix)-1]...)
	if len(prefix) > 2 && len(current) > 2 {
		merged = append(merged, ',')
	}
	merged = append(merged, current[1:len(current)-1]...)
	merged = append(merged, ']')
	out, errSet := sjson.SetRawBytes(payload, "input", merged)
	if errSet != nil {
		return nil, errSet
	}
	return out, nil
}

func (s *xaiWebsocketIDState) recordTranscriptTurn(requestPayload []byte, completedPayload []byte) error {
	if s == nil || len(requestPayload) == 0 || len(completedPayload) == 0 {
		return nil
	}
	resetTranscript := strings.TrimSpace(gjson.GetBytes(requestPayload, "previous_response_id").String()) == ""
	existingItems := 0
	var existingBytes int64
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errXAIWebsocketIDStateClosed
	}
	if !resetTranscript {
		existingItems = len(s.transcriptInput)
		existingBytes = s.transcriptBytes
	}
	s.mu.Unlock()
	inputItems, inputBytes, overflow := xaiJSONRawMessages(
		gjson.GetBytes(requestPayload, "input"),
		xaiWebsocketTranscriptMaxItems-existingItems,
		xaiWebsocketTranscriptMaxBytes-existingBytes,
	)
	if overflow {
		s.mu.Lock()
		s.clearLocked()
		s.mu.Unlock()
		return xaiWebsocketSessionStateTooLargeError()
	}
	outputItems, outputBytes, overflow := xaiJSONRawMessages(
		gjson.GetBytes(completedPayload, "response.output"),
		xaiWebsocketTranscriptMaxItems-existingItems-len(inputItems),
		xaiWebsocketTranscriptMaxBytes-existingBytes-inputBytes,
	)
	if overflow {
		s.mu.Lock()
		s.clearLocked()
		s.mu.Unlock()
		return xaiWebsocketSessionStateTooLargeError()
	}
	if len(inputItems) == 0 && len(outputItems) == 0 {
		if !resetTranscript {
			return nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return errXAIWebsocketIDStateClosed
		}
		s.clearTranscriptLocked()
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errXAIWebsocketIDStateClosed
	}
	addedItems := len(inputItems) + len(outputItems)
	addedBytes := inputBytes + outputBytes
	currentItems := len(s.transcriptInput)
	currentBytes := s.transcriptBytes
	if resetTranscript {
		currentItems = 0
		currentBytes = 0
	}
	if currentItems+addedItems > xaiWebsocketTranscriptMaxItems || currentBytes+addedBytes > xaiWebsocketTranscriptMaxBytes {
		s.clearLocked()
		return xaiWebsocketSessionStateTooLargeError()
	}
	nextBytes := s.transcriptBytes + addedBytes
	if resetTranscript {
		nextBytes = addedBytes
	}
	if s.budget != nil && !s.budget.resize(s.transcriptBytes, nextBytes) {
		return xaiWebsocketSessionStateCapacityError()
	}
	if resetTranscript {
		releaseXAIWebsocketTranscriptItems(s.transcriptReleases)
		s.transcriptInput = nil
		s.transcriptReleases = nil
		s.transcriptBytes = 0
	}
	s.transcriptInput = append(s.transcriptInput, inputItems...)
	s.transcriptInput = append(s.transcriptInput, outputItems...)
	s.transcriptReleases = append(s.transcriptReleases, retainXAIWebsocketTranscriptItems(inputItems)...)
	s.transcriptReleases = append(s.transcriptReleases, retainXAIWebsocketTranscriptItems(outputItems)...)
	s.transcriptBytes += addedBytes
	return nil
}

func (s *xaiWebsocketIDState) replaceTranscriptWithItems(items ...[]byte) error {
	if s == nil {
		return nil
	}
	next := make([]json.RawMessage, 0, len(items))
	var nextBytes int64
	for _, item := range items {
		item = bytes.TrimSpace(item)
		if len(item) == 0 || !json.Valid(item) {
			continue
		}
		if len(next) == xaiWebsocketTranscriptMaxItems || nextBytes+int64(len(item)) > xaiWebsocketTranscriptMaxBytes {
			s.mu.Lock()
			s.clearLocked()
			s.mu.Unlock()
			return xaiWebsocketSessionStateTooLargeError()
		}
		next = append(next, internalpayload.CloneBytes(item))
		nextBytes += int64(len(item))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errXAIWebsocketIDStateClosed
	}
	if s.budget != nil && !s.budget.resize(s.transcriptBytes, nextBytes) {
		return xaiWebsocketSessionStateCapacityError()
	}
	releaseXAIWebsocketTranscriptItems(s.transcriptReleases)
	s.transcriptInput = next
	s.transcriptReleases = retainXAIWebsocketTranscriptItems(next)
	s.transcriptBytes = nextBytes
	return nil
}

func xaiJSONRawMessages(result gjson.Result, maxItems int, maxBytes int64) ([]json.RawMessage, int64, bool) {
	if !result.Exists() || !result.IsArray() {
		return nil, 0, false
	}
	if maxItems < 0 || maxBytes < 0 {
		return nil, 0, true
	}
	capacity := min(maxItems, 64)
	out := make([]json.RawMessage, 0, capacity)
	var totalBytes int64
	overflow := false
	result.ForEach(func(_, item gjson.Result) bool {
		raw := bytes.TrimSpace([]byte(item.Raw))
		if len(raw) == 0 || !json.Valid(raw) {
			return true
		}
		if len(out) >= maxItems || int64(len(raw)) > maxBytes-totalBytes {
			overflow = true
			return false
		}
		out = append(out, internalpayload.CloneBytes(raw))
		totalBytes += int64(len(raw))
		return true
	})
	if overflow {
		return nil, 0, true
	}
	return out, totalBytes, false
}

func xaiMarshalRawMessages(items []json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(bytes.TrimSpace(item))
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func preserveXAIInputIDsFromDownstreamTail(payload []byte, downstream []byte) []byte {
	if len(payload) == 0 || len(downstream) == 0 {
		return payload
	}
	src := gjson.GetBytes(downstream, "input")
	dst := gjson.GetBytes(payload, "input")
	if !src.Exists() || !dst.Exists() || !src.IsArray() || !dst.IsArray() {
		return payload
	}
	srcItems := src.Array()
	dstItems := dst.Array()
	if len(srcItems) == 0 || len(dstItems) < len(srcItems) {
		return payload
	}
	offset := len(dstItems) - len(srcItems)
	updatedItems := make([][]byte, len(dstItems))
	for i := range dstItems {
		updatedItems[i] = []byte(dstItems[i].Raw)
	}
	changed := false
	for i, srcItem := range srcItems {
		id := strings.TrimSpace(srcItem.Get("id").String())
		if id == "" {
			continue
		}
		dstIndex := offset + i
		if strings.TrimSpace(dstItems[dstIndex].Get("id").String()) != "" {
			continue
		}
		next, errSet := sjson.SetBytes(updatedItems[dstIndex], "id", id)
		if errSet != nil {
			continue
		}
		updatedItems[dstIndex] = next
		changed = true
	}
	if !changed {
		return payload
	}
	out, errSet := sjson.SetRawBytes(payload, "input", internalpayload.BuildRaw(updatedItems))
	if errSet != nil {
		return payload
	}
	return out
}

func (m *xaiWebsocketRequestIDMapper) upstreamRequestPayload(payload []byte) ([]byte, error) {
	if m == nil || len(payload) == 0 || m.downstreamPreviousID == m.upstreamPreviousID {
		return payload, nil
	}
	if m.upstreamPreviousID == "" {
		out, errDelete := sjson.DeleteBytes(payload, "previous_response_id")
		if errDelete == nil {
			if m.downstreamPreviousID != "" && m.state != nil {
				return m.state.prependTranscriptInput(out)
			}
			return out, nil
		}
		return nil, errDelete
	}
	out, errSet := sjson.SetBytes(payload, "previous_response_id", m.upstreamPreviousID)
	if errSet != nil {
		return nil, errSet
	}
	return out, nil
}

func (m *xaiWebsocketRequestIDMapper) downstreamResponsePayload(payload []byte) ([]byte, error) {
	if m == nil || len(payload) == 0 {
		return payload, nil
	}
	upstreamResponseID := strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
	downstreamResponseID, err := m.downstreamIDForUpstreamResponse(upstreamResponseID)
	if err != nil {
		return nil, err
	}
	if downstreamResponseID == "" {
		return payload, nil
	}
	return rewriteXAIWebsocketDownstreamIDs(payload, m.upstreamResponseID, downstreamResponseID, m.upstreamPreviousID, m.downstreamPreviousID), nil
}

func (m *xaiWebsocketRequestIDMapper) downstreamIDForUpstreamResponse(upstreamResponseID string) (string, error) {
	upstreamResponseID = strings.TrimSpace(upstreamResponseID)
	if m == nil || m.state == nil {
		return upstreamResponseID, nil
	}
	if m.upstreamResponseID != "" {
		return m.downstreamResponseID, nil
	}
	if upstreamResponseID == "" {
		return "", nil
	}

	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.closed {
		return "", errXAIWebsocketIDStateClosed
	}
	m.upstreamResponseID = upstreamResponseID
	m.downstreamResponseID = upstreamResponseID
	if m.downstreamPreviousID != "" && m.upstreamPreviousID != "" && upstreamResponseID == m.upstreamPreviousID {
		m.state.sequence++
		m.downstreamResponseID = fmt.Sprintf("%s-xai-%d", upstreamResponseID, m.state.sequence)
	}
	if m.state.downstreamToUpstream == nil {
		m.state.downstreamToUpstream = make(map[string]string)
	}
	newEntries := 0
	if _, exists := m.state.downstreamToUpstream[upstreamResponseID]; !exists {
		newEntries++
	}
	if m.downstreamResponseID != upstreamResponseID {
		if _, exists := m.state.downstreamToUpstream[m.downstreamResponseID]; !exists {
			newEntries++
		}
	}
	if len(m.state.downstreamToUpstream)+newEntries > xaiWebsocketIDMapMaxEntries {
		m.state.clearLocked()
		return "", xaiWebsocketSessionStateTooLargeError()
	}
	m.state.downstreamToUpstream[upstreamResponseID] = upstreamResponseID
	m.state.downstreamToUpstream[m.downstreamResponseID] = upstreamResponseID
	return m.downstreamResponseID, nil
}

func rewriteXAIWebsocketDownstreamIDs(payload []byte, upstreamResponseID string, downstreamResponseID string, upstreamPreviousID string, downstreamPreviousID string) []byte {
	upstreamResponseID = strings.TrimSpace(upstreamResponseID)
	downstreamResponseID = strings.TrimSpace(downstreamResponseID)
	upstreamPreviousID = strings.TrimSpace(upstreamPreviousID)
	downstreamPreviousID = strings.TrimSpace(downstreamPreviousID)
	if len(payload) == 0 || (upstreamResponseID == downstreamResponseID && upstreamPreviousID == downstreamPreviousID) {
		return payload
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return payload
	}
	if !rewriteXAIWebsocketDownstreamIDValue(value, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID, "") {
		return payload
	}
	out, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return payload
	}
	return out
}

func rewriteXAIWebsocketDownstreamIDValue(value any, upstreamResponseID string, downstreamResponseID string, upstreamPreviousID string, downstreamPreviousID string, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for childKey, childValue := range typed {
			if childString, ok := childValue.(string); ok {
				replaced := rewriteXAIWebsocketDownstreamIDString(childString, childKey, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID)
				if replaced != childString {
					typed[childKey] = replaced
					changed = true
				}
				continue
			}
			if rewriteXAIWebsocketDownstreamIDValue(childValue, upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID, childKey) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for i := range typed {
			if rewriteXAIWebsocketDownstreamIDValue(typed[i], upstreamResponseID, downstreamResponseID, upstreamPreviousID, downstreamPreviousID, key) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func rewriteXAIWebsocketDownstreamIDString(value string, key string, upstreamResponseID string, downstreamResponseID string, upstreamPreviousID string, downstreamPreviousID string) string {
	switch key {
	case "id", "item_id":
		if upstreamResponseID != "" && downstreamResponseID != "" && downstreamResponseID != upstreamResponseID && strings.Contains(value, upstreamResponseID) {
			return strings.ReplaceAll(value, upstreamResponseID, downstreamResponseID)
		}
	case "previous_response_id":
		if upstreamPreviousID != "" && downstreamPreviousID != "" && value == upstreamPreviousID {
			return downstreamPreviousID
		}
	}
	return value
}

func (e *XAIWebsocketsExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.XAIExecutor == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai websockets executor: executor is nil")
	}
	return e.XAIExecutor.Execute(ctx, auth, req, opts)
}

func (e *XAIWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if e == nil || e.XAIExecutor == nil {
		return nil, fmt.Errorf("xai websockets executor: executor is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	executionSessionID := executionSessionIDFromOptions(opts)
	stateSessionID := xaiExecutionSessionID(req, opts)
	if stateSessionID == "" {
		stateSessionID = executionSessionID
	}
	idMapper := newXAIWebsocketRequestIDMapper(e.idStore, stateSessionID, req.Payload)
	if xaiInputHasItemType(req.Payload, "compaction_trigger") {
		return e.executeCompactionTriggerFromWebsocketContext(ctx, auth, req, opts, idMapper)
	}
	transformStarted := time.Now()

	token, baseURL := xaiCreds(auth)
	if baseURL == "" {
		baseURL = xaiauth.DefaultAPIBaseURL
	}

	prepared, err := e.prepareResponsesWebsocketRequest(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	if idMapper != nil {
		prepared.body, err = idMapper.upstreamRequestPayload(prepared.body)
		if err != nil {
			discardXAIWebsocketIDStateOnError(e.idStore, stateSessionID, idMapper.state, err)
			return nil, err
		}
	}
	prepared.body = preserveXAIInputIDsFromDownstreamTail(prepared.body, req.Payload)

	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, e.Identifier())

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildXAIResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}
	wsHeaders := applyXAIWebsocketHeaders(http.Header{}, auth, token, prepared.sessionID)
	wsReqBody := buildXAIWebsocketRequestBody(prepared.body)
	if err = internalpayload.EnforceRequestTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:       "request_plan.xai.websocket_stream",
		InputBytes:  int64(len(req.Payload)),
		OutputBytes: int64(len(wsReqBody)),
		Duration:    time.Since(transformStarted),
	}, internalpayload.AmplificationOverride{}); err != nil {
		return nil, err
	}
	warmupRequest := xaiWebsocketGenerateFalse(wsReqBody)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		if sess != nil {
			sess.reqMu.Lock()
		}
	}

	wsReqLog := helps.UpstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   wsHeaders.Clone(),
		Body:      wsReqBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)
	logXAIWebsocketRequest(executionSessionID, authID, wsURL, wsReqBody)

	conn, respHS, errDial := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
	}
	if errDial != nil {
		bodyErr, errBody := websocketHandshakeBody(respHS)
		if respHS != nil {
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header.Clone(), bodyErr)
		}
		if errBody != nil {
			helps.RecordAPIWebsocketError(ctx, e.cfg, "handshake_body", errBody)
			if sess != nil {
				sess.reqMu.Unlock()
			}
			return nil, errBody
		}
		if respHS != nil && respHS.StatusCode > 0 {
			if sess != nil {
				sess.reqMu.Unlock()
			}
			return nil, newUpstreamStatusErr(respHS.StatusCode, respHS.Header, respHS.Header.Get("Content-Type"), bodyErr)
		}
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		if sess != nil {
			sess.reqMu.Unlock()
		}
		return nil, errDial
	}
	recordAPIWebsocketHandshake(ctx, e.cfg, respHS)
	reporter.StartResponseTTFT()

	if sess == nil {
		logXAIWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, providerWebsocketReadQueueSize)
		sess.setActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		helps.RecordAPIWebsocketError(ctx, e.cfg, "send", errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)
			connRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry != nil || connRetry == nil {
				closeHTTPResponseBody(respHSRetry, "xai websockets executor: close handshake response body error")
				helps.RecordAPIWebsocketError(ctx, e.cfg, "dial_retry", errDialRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errDialRetry
			}
			retryTransformStarted := time.Now()
			wsReqBodyRetry := buildXAIWebsocketRequestBody(prepared.body)
			internalpayload.RecordTransformStageSince(ctx, internalpayload.TransformStageReport{
				Stage:       "request_plan.xai.websocket_stream_retry",
				InputBytes:  int64(len(req.Payload)),
				OutputBytes: int64(len(wsReqBodyRetry)),
			}, retryTransformStarted, internalpayload.AmplificationOverride{})
			helps.RecordAPIWebsocketRequest(ctx, e.cfg, helps.UpstreamRequestLog{
				URL:       wsURL,
				Method:    "WEBSOCKET",
				Headers:   wsHeaders.Clone(),
				Body:      wsReqBodyRetry,
				Provider:  e.Identifier(),
				AuthID:    authID,
				AuthLabel: authLabel,
				AuthType:  authType,
				AuthValue: authValue,
			})
			logXAIWebsocketRequest(executionSessionID, authID, wsURL, wsReqBodyRetry)
			recordAPIWebsocketHandshake(ctx, e.cfg, respHSRetry)
			reporter.StartResponseTTFT()
			if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry != nil {
				helps.RecordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
				e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errSendRetry
			}
			conn = connRetry
			wsReqBody = wsReqBodyRetry
		} else {
			logXAIWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("xai websockets executor: close websocket error: %v", errClose)
			}
			return nil, errSend
		}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		terminateReason := "completed"
		var terminateErr error

		defer close(out)
		defer func() {
			if sess != nil {
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return
			}
			logXAIWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("xai websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		recordedTranscript := false
		var activeFrame codexWebsocketRead
		defer activeFrame.release()
		for {
			activeFrame.release()
			activeFrame = codexWebsocketRead{}
			if ctx != nil && ctx.Err() != nil {
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
				return
			}
			frame, errRead := readXAIWebsocketMessage(ctx, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
					return
				}
				errRead = mapCodexWebsocketReadError(errRead)
				terminateReason = "read_error"
				terminateErr = errRead
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read", errRead)
				reporter.PublishFailure(ctx, errRead)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			activeFrame = frame
			msgType := frame.msgType
			payload := frame.payload
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					errBinary := fmt.Errorf("xai websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = errBinary
					helps.RecordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", errBinary)
					reporter.PublishFailure(ctx, errBinary)
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: errBinary})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			reporter.MarkFirstResponseByte()
			helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)

			if wsErr, ok := parseXAIWebsocketError(payload); ok {
				terminateReason = "upstream_error"
				terminateErr = wsErr
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
				reporter.PublishFailure(ctx, wsErr)
				if sess != nil {
					e.invalidateUpstreamConnWithoutDisconnectNotify(sess, conn, "upstream_error", wsErr)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}

			for _, payload := range xaiNormalizeReasoningSummaryDataEvents(payload) {
				eventType := gjson.GetBytes(payload, "type").String()
				isTerminalEvent := eventType == "response.completed" || eventType == "response.done" || eventType == "error"
				warmupCompletedPayload := []byte(nil)
				switch eventType {
				case "response.created":
					if warmupRequest {
						warmupCompletedPayload = buildXAIWebsocketWarmupCompletedPayload(payload)
						logXAIWebsocketWarmupCompleted(executionSessionID, authID, wsURL, payload)
					}
				case "response.output_item.done":
					xaiCollectOutputItemDone(payload, outputItemsByIndex, &outputItemsFallback)
				case "response.completed":
					logXAIWebsocketTerminalResponse(executionSessionID, authID, wsURL, eventType, payload)
					if detail, ok := helps.ParseCodexUsage(payload); ok {
						reporter.Publish(ctx, detail)
					}
					payload = xaiPatchCompletedOutput(payload, outputItemsByIndex, outputItemsFallback)
					payload = xaiNormalizeReasoningSummaryData(payload)
					if !warmupRequest && idMapper != nil && idMapper.state != nil && !recordedTranscript {
						if errState := idMapper.state.recordTranscriptTurn(req.Payload, payload); errState != nil {
							reason := xaiWebsocketSessionStateErrorReason(errState)
							discardXAIWebsocketIDStateOnError(e.idStore, stateSessionID, idMapper.state, errState)
							if sess != nil && !isXAIWebsocketSessionStateCapacityError(errState) {
								e.invalidateUpstreamConnWithoutDisconnectNotify(sess, conn, reason, errState)
							}
							terminateReason = reason
							terminateErr = errState
							reporter.PublishFailure(ctx, errState)
							_ = send(cliproxyexecutor.StreamChunk{Err: errState})
							return
						}
						recordedTranscript = true
					}
				case "response.done":
					logXAIWebsocketTerminalResponse(executionSessionID, authID, wsURL, eventType, payload)
					if detail, ok := helps.ParseCodexUsage(payload); ok {
						reporter.Publish(ctx, detail)
					}
					if !warmupRequest && idMapper != nil && idMapper.state != nil && !recordedTranscript {
						if errState := idMapper.state.recordTranscriptTurn(req.Payload, payload); errState != nil {
							reason := xaiWebsocketSessionStateErrorReason(errState)
							discardXAIWebsocketIDStateOnError(e.idStore, stateSessionID, idMapper.state, errState)
							if sess != nil && !isXAIWebsocketSessionStateCapacityError(errState) {
								e.invalidateUpstreamConnWithoutDisconnectNotify(sess, conn, reason, errState)
							}
							terminateReason = reason
							terminateErr = errState
							reporter.PublishFailure(ctx, errState)
							_ = send(cliproxyexecutor.StreamChunk{Err: errState})
							return
						}
						recordedTranscript = true
					}
				}

				if cliproxyexecutor.DownstreamWebsocket(ctx) {
					downstreamPayload := payload
					downstreamWarmupCompletedPayload := warmupCompletedPayload
					if idMapper != nil {
						var errState error
						downstreamPayload, errState = idMapper.downstreamResponsePayload(payload)
						if errState != nil {
							reason := xaiWebsocketSessionStateErrorReason(errState)
							discardXAIWebsocketIDStateOnError(e.idStore, stateSessionID, idMapper.state, errState)
							if sess != nil && !isXAIWebsocketSessionStateCapacityError(errState) {
								e.invalidateUpstreamConnWithoutDisconnectNotify(sess, conn, reason, errState)
							}
							terminateReason = reason
							terminateErr = errState
							reporter.PublishFailure(ctx, errState)
							_ = send(cliproxyexecutor.StreamChunk{Err: errState})
							return
						}
						if len(warmupCompletedPayload) > 0 {
							downstreamWarmupCompletedPayload, errState = idMapper.downstreamResponsePayload(warmupCompletedPayload)
							if errState != nil {
								reason := xaiWebsocketSessionStateErrorReason(errState)
								discardXAIWebsocketIDStateOnError(e.idStore, stateSessionID, idMapper.state, errState)
								if sess != nil && !isXAIWebsocketSessionStateCapacityError(errState) {
									e.invalidateUpstreamConnWithoutDisconnectNotify(sess, conn, reason, errState)
								}
								terminateReason = reason
								terminateErr = errState
								reporter.PublishFailure(ctx, errState)
								_ = send(cliproxyexecutor.StreamChunk{Err: errState})
								return
							}
						}
					}
					if !send(cliproxyexecutor.StreamChunk{Payload: downstreamPayload}) {
						terminateReason = "context_done"
						terminateErr = ctx.Err()
						return
					}
					if len(downstreamWarmupCompletedPayload) > 0 {
						if !send(cliproxyexecutor.StreamChunk{Payload: downstreamWarmupCompletedPayload}) {
							terminateReason = "context_done"
							terminateErr = ctx.Err()
							return
						}
						return
					}
					if isTerminalEvent {
						return
					}
					continue
				}

				payload = normalizeCodexWebsocketCompletion(payload)
				line := encodeCodexWebsocketAsSSE(payload)
				chunks := sdktranslator.TranslateStream(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, line, &param)
				for i := range chunks {
					if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
						terminateReason = "context_done"
						terminateErr = ctx.Err()
						return
					}
				}
				if len(warmupCompletedPayload) > 0 {
					line = encodeCodexWebsocketAsSSE(warmupCompletedPayload)
					chunks = sdktranslator.TranslateStream(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, line, &param)
					for i := range chunks {
						if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
							terminateReason = "context_done"
							terminateErr = ctx.Err()
							return
						}
					}
					return
				}
				if eventType == "response.completed" || eventType == "response.done" {
					return
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, nil
}

func (e *XAIWebsocketsExecutor) executeCompactionTriggerFromWebsocketContext(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, idMapper *xaiWebsocketRequestIDMapper) (*cliproxyexecutor.StreamResult, error) {
	if idMapper == nil || idMapper.state == nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: "xai websocket compaction context is unavailable"}
	}
	transcriptInput := idMapper.state.snapshotTranscriptInput()
	if len(transcriptInput) == 0 {
		return nil, statusErr{code: http.StatusBadRequest, msg: "xai websocket compaction context is empty"}
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	log.Infof(
		"xai websockets: compact fallback session=%s auth=%s input_items=%d",
		xaiExecutionSessionID(req, opts),
		strings.TrimSpace(authID),
		len(gjson.ParseBytes(transcriptInput).Array()),
	)
	compactPayload, err := buildXAIWebsocketCompactionPayload(req.Payload, transcriptInput)
	if err != nil {
		return nil, err
	}
	compactReq := req
	compactReq.Payload = compactPayload

	prepared, data, headers, err := e.XAIExecutor.executeCompactRequest(ctx, auth, compactReq, opts)
	if err != nil {
		return nil, err
	}

	responseID := xaiCompactionResponseID(data)
	if errState := idMapper.state.replaceTranscriptWithItems(xaiCompactionOutputItem(data, responseID)); errState != nil {
		discardXAIWebsocketIDStateOnError(e.idStore, xaiExecutionSessionID(req, opts), idMapper.state, errState)
		return nil, errState
	}
	if errState := idMapper.state.mapDownstreamToUpstream(responseID, ""); errState != nil {
		discardXAIWebsocketIDStateOnError(e.idStore, xaiExecutionSessionID(req, opts), idMapper.state, errState)
		return nil, errState
	}

	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")

	chunks := xaiBuildCompactionTriggerStreamChunks(prepared, data)
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func buildXAIWebsocketCompactionPayload(payload []byte, transcriptInput []byte) ([]byte, error) {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if len(transcriptInput) == 0 {
		transcriptInput = []byte("[]")
	}
	out := internalpayload.CloneBytes(payload)
	var err error
	out, err = sjson.SetRawBytes(out, "input", transcriptInput)
	if err != nil {
		return nil, err
	}
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	return out, nil
}

func xaiWebsocketGenerateFalse(payload []byte) bool {
	generate := gjson.GetBytes(payload, "generate")
	return generate.Exists() && !generate.Bool()
}

func buildXAIWebsocketWarmupCompletedPayload(createdPayload []byte) []byte {
	completed := []byte(`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	if sequence := gjson.GetBytes(createdPayload, "sequence_number"); sequence.Exists() {
		completed, _ = sjson.SetBytes(completed, "sequence_number", sequence.Int()+1)
	}
	if response := gjson.GetBytes(createdPayload, "response"); response.Exists() && response.IsObject() {
		responsePayload := []byte(response.Raw)
		responsePayload, _ = sjson.SetBytes(responsePayload, "status", "completed")
		if !gjson.GetBytes(responsePayload, "output").Exists() {
			responsePayload, _ = sjson.SetRawBytes(responsePayload, "output", []byte("[]"))
		}
		if !gjson.GetBytes(responsePayload, "usage").Exists() {
			responsePayload, _ = sjson.SetRawBytes(responsePayload, "usage", []byte(`{"input_tokens":0,"output_tokens":0,"total_tokens":0}`))
		}
		completed, _ = sjson.SetRawBytes(completed, "response", responsePayload)
	}
	return completed
}

func parseXAIWebsocketError(payload []byte) (error, bool) {
	if wsErr, ok := parseCodexWebsocketError(payload); ok {
		return wsErr, true
	}
	if len(payload) == 0 || !gjson.GetBytes(payload, "error").Exists() {
		return nil, false
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		status = xaiBareWebsocketErrorStatus(payload)
	}
	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "type", "error")
	out, _ = sjson.SetBytes(out, "status", status)
	if errNode := gjson.GetBytes(payload, "error"); errNode.Exists() {
		out, _ = sjson.SetRawBytes(out, "error", []byte(errNode.Raw))
	}
	return newUpstreamStatusErr(status, nil, "application/json", out), true
}

func xaiBareWebsocketErrorStatus(payload []byte) int {
	for _, path := range []string{"error.code", "error.status", "code"} {
		raw := strings.TrimSpace(gjson.GetBytes(payload, path).String())
		if raw == "" {
			continue
		}
		status, errAtoi := strconv.Atoi(raw)
		if errAtoi == nil && status > 0 {
			return status
		}
	}
	message := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	if strings.Contains(message, `"code":"400"`) || strings.Contains(message, "Request validation error") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (e *XAIWebsocketsExecutor) prepareResponsesWebsocketRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*xaiPreparedRequest, error) {
	prepared, err := e.prepareResponsesRequest(ctx, req, opts, true)
	if err != nil {
		return nil, err
	}
	if previousResponseID := strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String()); previousResponseID != "" {
		prepared.body, _ = sjson.SetBytes(prepared.body, "previous_response_id", previousResponseID)
	}
	return prepared, nil
}

func (e *XAIWebsocketsExecutor) dialXAIWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(e.cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = false
	if ctx == nil {
		ctx = context.Background()
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if conn != nil {
		conn.SetReadLimit(providerWebsocketReadLimit)
		conn.EnableWriteCompression(false)
	}
	return conn, resp, err
}

func (e *XAIWebsocketsExecutor) getOrCreateSession(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || e == nil {
		return nil
	}
	store := e.store
	if store == nil {
		store = globalXAIWebsocketSessionStore
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.sessions == nil {
		store.sessions = make(map[string]*codexWebsocketSession)
	}
	if sess, ok := store.sessions[sessionID]; ok && sess != nil {
		return sess
	}
	sess := &codexWebsocketSession{
		sessionID:            sessionID,
		upstreamDisconnectCh: make(chan error, 1),
	}
	store.sessions[sessionID] = sess
	return sess
}

func (e *XAIWebsocketsExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	sess := e.getOrCreateSession(sessionID)
	if sess == nil {
		return nil
	}
	return sess.upstreamDisconnectCh
}

func (e *XAIWebsocketsExecutor) ensureUpstreamConn(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID string, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if sess == nil {
		return e.dialXAIWebsocket(ctx, auth, wsURL, headers)
	}

	sess.connMu.Lock()
	conn := sess.conn
	if conn != nil {
		if sess.readerConn != conn {
			readerCtx, readerCancel := context.WithCancel(context.Background())
			readerDone := make(chan struct{})
			previousCancel := sess.readerCancel
			sess.readerConn = conn
			sess.readerCancel = readerCancel
			sess.readerDone = readerDone
			sess.connMu.Unlock()
			if previousCancel != nil {
				previousCancel()
			}
			configureXAIWebsocketConn(sess, conn)
			go func() {
				defer close(readerDone)
				e.readUpstreamLoop(readerCtx, sess, conn)
			}()
			return conn, nil, nil
		}
		sess.connMu.Unlock()
		return conn, nil, nil
	}
	sess.connMu.Unlock()

	conn, resp, errDial := e.dialXAIWebsocket(ctx, auth, wsURL, headers)
	if errDial != nil {
		return nil, resp, errDial
	}

	sess.connMu.Lock()
	if sess.conn != nil {
		previous := sess.conn
		sess.connMu.Unlock()
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("xai websockets executor: close websocket error: %v", errClose)
		}
		return previous, nil, nil
	}
	sess.conn = conn
	sess.wsURL = wsURL
	sess.authID = authID
	sess.readerConn = conn
	readerCtx, readerCancel := context.WithCancel(context.Background())
	readerDone := make(chan struct{})
	sess.readerCancel = readerCancel
	sess.readerDone = readerDone
	sess.connMu.Unlock()

	configureXAIWebsocketConn(sess, conn)
	go func() {
		defer close(readerDone)
		e.readUpstreamLoop(readerCtx, sess, conn)
	}()
	logXAIWebsocketConnected(sess.sessionID, authID, wsURL)
	return conn, resp, nil
}

func configureXAIWebsocketConn(sess *codexWebsocketSession, conn *websocket.Conn) {
	if sess == nil || conn == nil {
		return
	}
	conn.SetReadLimit(providerWebsocketReadLimit)
	conn.SetPingHandler(func(appData string) error {
		sess.writeMu.Lock()
		defer sess.writeMu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Time{})
	})
}

func readXAIWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (codexWebsocketRead, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sess == nil {
		if conn == nil {
			return codexWebsocketRead{}, fmt.Errorf("xai websockets executor: websocket conn is nil")
		}
		return readProviderWebsocketFrame(ctx, conn, providerWebsocketReadBudget(nil))
	}
	if conn == nil {
		return codexWebsocketRead{}, fmt.Errorf("xai websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return codexWebsocketRead{}, fmt.Errorf("xai websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return codexWebsocketRead{}, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return codexWebsocketRead{}, fmt.Errorf("xai websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				ev.release()
				continue
			}
			if ev.err != nil {
				ev.release()
				return codexWebsocketRead{}, ev.err
			}
			return ev, nil
		}
	}
}

func (e *XAIWebsocketsExecutor) readUpstreamLoop(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		frame, errRead := readProviderWebsocketFrame(ctx, conn, providerWebsocketReadBudget(sess))
		if errRead != nil {
			if ctx != nil && ctx.Err() != nil {
				return
			}
			sess.finishActive(codexWebsocketRead{conn: conn, err: errRead})
			e.invalidateUpstreamConn(sess, conn, "upstream_disconnected", errRead)
			return
		}

		if frame.msgType != websocket.TextMessage {
			if frame.msgType == websocket.BinaryMessage {
				frame.release()
				errBinary := fmt.Errorf("xai websockets executor: unexpected binary message")
				sess.finishActive(codexWebsocketRead{conn: conn, err: errBinary})
				e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
				return
			}
			frame.release()
			continue
		}

		sess.enqueueActiveRead(frame)
	}
}

func (e *XAIWebsocketsExecutor) invalidateUpstreamConn(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	e.invalidateUpstreamConnWithNotify(sess, conn, reason, err, true)
}

func (e *XAIWebsocketsExecutor) invalidateUpstreamConnWithoutDisconnectNotify(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	e.invalidateUpstreamConnWithNotify(sess, conn, reason, err, false)
}

func (e *XAIWebsocketsExecutor) invalidateUpstreamConnWithNotify(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error, notify bool) {
	if sess == nil || conn == nil {
		return
	}

	sess.connMu.Lock()
	current := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sessionID := sess.sessionID
	if current == nil || current != conn {
		sess.connMu.Unlock()
		return
	}
	sess.conn = nil
	var readerCancel context.CancelFunc
	if sess.readerConn == conn {
		readerCancel = sess.readerCancel
		sess.readerConn = nil
		sess.readerCancel = nil
		sess.readerDone = nil
	}
	sess.connMu.Unlock()

	if readerCancel != nil {
		readerCancel()
	}
	logXAIWebsocketDisconnected(sessionID, authID, wsURL, reason, err)
	if notify {
		sess.notifyUpstreamDisconnect(err)
	}
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("xai websockets executor: close websocket error: %v", errClose)
	}
}

func (e *XAIWebsocketsExecutor) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil || sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		return
	}

	deleteXAIWebsocketIDState(e.idStore, sessionID)

	store := e.store
	if store == nil {
		store = globalXAIWebsocketSessionStore
	}
	store.mu.Lock()
	sess := store.sessions[sessionID]
	delete(store.sessions, sessionID)
	store.mu.Unlock()
	e.closeExecutionSession(sess, "session_closed")
}

func (e *XAIWebsocketsExecutor) closeExecutionSession(sess *codexWebsocketSession, reason string) {
	closeXAIWebsocketSession(sess, reason)
}

func closeXAIWebsocketSession(sess *codexWebsocketSession, reason string) {
	if sess == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "session_closed"
	}

	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sess.conn = nil
	var readerCancel context.CancelFunc
	var readerDone <-chan struct{}
	if sess.readerConn == conn {
		readerCancel = sess.readerCancel
		readerDone = sess.readerDone
		sess.readerConn = nil
		sess.readerCancel = nil
		sess.readerDone = nil
	}
	sessionID := sess.sessionID
	sess.connMu.Unlock()

	if readerCancel != nil {
		readerCancel()
	}
	sess.cancelActive()
	if conn == nil {
		if readerDone != nil {
			<-readerDone
		}
		return
	}
	logXAIWebsocketDisconnected(sessionID, authID, wsURL, reason, nil)
	if errClose := conn.Close(); errClose != nil {
		log.Errorf("xai websockets executor: close websocket error: %v", errClose)
	}
	if readerDone != nil {
		<-readerDone
	}
}

func buildXAIWebsocketRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	wsReqBody := internalpayload.CloneBytes(body)
	wsReqBody, _ = sjson.SetBytes(wsReqBody, "type", "response.create")
	wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "stream")
	wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "stream_options")
	wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "background")
	wsReqBody, _ = sjson.SetBytes(wsReqBody, "store", true)
	if strings.TrimSpace(gjson.GetBytes(wsReqBody, "previous_response_id").String()) != "" {
		wsReqBody, _ = sjson.DeleteBytes(wsReqBody, "instructions")
	}
	return wsReqBody
}

func buildXAIResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("xai websockets executor: unsupported responses websocket URL scheme %q", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("xai websockets executor: responses websocket URL host is empty")
	}
	return parsed.String(), nil
}

func applyXAIWebsocketHeaders(headers http.Header, auth *cliproxyauth.Auth, token string, sessionID string) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		headers.Set("x-grok-conv-id", sessionID)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)
	return headers
}

func logXAIWebsocketConnected(sessionID string, authID string, wsURL string) {
	log.Infof("xai websockets: upstream connected session=%s auth=%s url=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL))
}

func logXAIWebsocketRequest(sessionID string, authID string, wsURL string, payload []byte) {
	if len(payload) == 0 {
		log.Infof("xai websockets: upstream request sent session=%s auth=%s url=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL))
		return
	}
	generateValue := "default"
	if generate := gjson.GetBytes(payload, "generate"); generate.Exists() {
		generateValue = strings.TrimSpace(generate.Raw)
	}
	log.Infof(
		"xai websockets: upstream request sent session=%s auth=%s url=%s event=%s previous_response_id=%s generate=%s input_items=%d",
		strings.TrimSpace(sessionID),
		strings.TrimSpace(authID),
		strings.TrimSpace(wsURL),
		strings.TrimSpace(gjson.GetBytes(payload, "type").String()),
		strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()),
		generateValue,
		len(gjson.GetBytes(payload, "input").Array()),
	)
}

func logXAIWebsocketWarmupCompleted(sessionID string, authID string, wsURL string, payload []byte) {
	log.Infof(
		"xai websockets: upstream warmup completed session=%s auth=%s url=%s response_id=%s",
		strings.TrimSpace(sessionID),
		strings.TrimSpace(authID),
		strings.TrimSpace(wsURL),
		strings.TrimSpace(gjson.GetBytes(payload, "response.id").String()),
	)
}

func logXAIWebsocketTerminalResponse(sessionID string, authID string, wsURL string, eventType string, payload []byte) {
	log.Infof(
		"xai websockets: upstream terminal response session=%s auth=%s url=%s event=%s response_id=%s previous_response_id=%s",
		strings.TrimSpace(sessionID),
		strings.TrimSpace(authID),
		strings.TrimSpace(wsURL),
		strings.TrimSpace(eventType),
		strings.TrimSpace(gjson.GetBytes(payload, "response.id").String()),
		strings.TrimSpace(gjson.GetBytes(payload, "response.previous_response_id").String()),
	)
}

func logXAIWebsocketDisconnected(sessionID string, authID string, wsURL string, reason string, err error) {
	if err != nil {
		log.Infof("xai websockets: upstream disconnected session=%s auth=%s url=%s reason=%s err=%v", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason), err)
		return
	}
	log.Infof("xai websockets: upstream disconnected session=%s auth=%s url=%s reason=%s", strings.TrimSpace(sessionID), strings.TrimSpace(authID), strings.TrimSpace(wsURL), strings.TrimSpace(reason))
}

// CloseXAIWebsocketSessionsForAuthID closes all active xAI upstream websocket sessions
// associated with the supplied auth ID.
func CloseXAIWebsocketSessionsForAuthID(authID string, reason string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "auth_removed"
	}

	store := globalXAIWebsocketSessionStore
	if store == nil {
		return
	}

	type sessionItem struct {
		sessionID string
		sess      *codexWebsocketSession
	}

	store.mu.Lock()
	items := make([]sessionItem, 0, len(store.sessions))
	for sessionID, sess := range store.sessions {
		items = append(items, sessionItem{sessionID: sessionID, sess: sess})
	}
	store.mu.Unlock()

	matches := make([]sessionItem, 0)
	for i := range items {
		sess := items[i].sess
		if sess == nil {
			continue
		}
		sess.connMu.Lock()
		sessAuthID := strings.TrimSpace(sess.authID)
		sess.connMu.Unlock()
		if sessAuthID == authID {
			matches = append(matches, items[i])
		}
	}
	if len(matches) == 0 {
		return
	}

	toClose := make([]*codexWebsocketSession, 0, len(matches))
	store.mu.Lock()
	for i := range matches {
		current, ok := store.sessions[matches[i].sessionID]
		if !ok || current == nil || current != matches[i].sess {
			continue
		}
		delete(store.sessions, matches[i].sessionID)
		deleteXAIWebsocketIDState(globalXAIWebsocketIDStates, matches[i].sessionID)
		toClose = append(toClose, current)
	}
	store.mu.Unlock()

	for i := range toClose {
		closeXAIWebsocketSession(toClose[i], reason)
	}
}

// XAIAutoExecutor routes xAI stream requests to the websocket transport only
// when the downstream transport is websocket and the selected auth enables
// websockets. Non-stream requests keep using the HTTP implementation.
type XAIAutoExecutor struct {
	httpExec *XAIExecutor
	wsExec   *XAIWebsocketsExecutor
}

func NewXAIAutoExecutor(cfg *config.Config) *XAIAutoExecutor {
	return &XAIAutoExecutor{
		httpExec: NewXAIExecutor(cfg),
		wsExec:   NewXAIWebsocketsExecutor(cfg),
	}
}

func (e *XAIAutoExecutor) Identifier() string { return "xai" }

func (e *XAIAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *XAIAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("xai auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *XAIAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai auto executor: executor is nil")
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *XAIAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("xai auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && xaiWebsocketsEnabled(auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *XAIAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("xai auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *XAIAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *XAIAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *XAIAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

func xaiWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}
