package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
type RoundRobinSelector struct {
	mu      sync.Mutex
	cursors map[string]int
	maxKeys int
}

// SpreadSelector distributes requests across all available credentials using
// configured priority and recent load, with channel-aware GPT health metrics.
type SpreadSelector struct {
	mu             sync.Mutex
	cursors        map[string]int
	currentWeights map[string]map[string]int
	load           *spreadLoadTracker
	maxKeys        int
}

// FillFirstSelector selects the first available credential (deterministic ordering).
// This "burns" one account before moving to the next, which can help stagger
// rolling-window subscription caps (e.g. chat message limits).
type FillFirstSelector struct{}

// SequentialFillSelector selects credentials sequentially without jumping back.
// Unlike FillFirstSelector which always picks the first available (by ID),
// this selector "sticks" to the current credential until it becomes unavailable,
// then advances to the next one. When a previously used credential recovers,
// it won't jump back - ensuring balanced usage across all credentials.
//
// For mixed-provider requests, a two-level sticky selection is used:
// first stick to the current provider until all its credentials are
// exhausted (in cooldown/unavailable), then advance to the next provider.
// Within each provider, the same sticky sequential selection applies.
type SequentialFillSelector struct {
	mu             sync.Mutex
	current        map[string]string // actualProvider:model -> current auth ID
	stickyProvider map[string]string // model -> current provider name (sticky)
}

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model    string
	resetIn  time.Duration
	provider string
}

func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
	}
}

func (e *modelCooldownError) Error() string {
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

func (e *modelCooldownError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	wait := e.resetIn
	if wait < 0 {
		wait = 0
	}
	return &wait
}

func authPriority(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return parsed
}

const (
	healthScoreDefault       = 100
	healthScoreStepSuccess   = 15
	healthRecoveryStep       = 5
	healthRecoveryInterval   = 2 * time.Minute
	healthPriorityMultiplier = 100
	healthBreakerThreshold   = 50
	healthHalfOpenSuccesses  = 2
	healthHalfOpenInterval   = 20 * time.Second
	healthHalfOpenActiveTTL  = 5 * time.Second
	quotaHalfOpenInterval    = 2 * time.Second
	quotaHalfOpenActiveTTL   = 20 * time.Second
	health429OpenFailures    = 20

	channelBreakerOpenFailures  = 3
	channelBreakerStateLimit    = 4096
	channelBreakerErrorCode     = "channel_circuit_open"
	channelBreakerStatusMessage = "channel temporarily unavailable after consecutive failures"

	spreadLoadHalfLife          = 20 * time.Second
	spreadLoadInflightWeight    = 4
	spreadLoadOverTargetPower   = 3
	spreadLoadDefaultKeyLimit   = 4096
	spreadLoadInactiveRecordTTL = 10 * time.Minute
	spreadOutcomeEWMAAlpha      = 0.2
	spreadMinSuccessFactor      = 0.1
	spreadMinTTFTFactor         = 0.25
	spreadMaxTTFTFactor         = 2.0
	spreadAffinityTTFTRatio     = 1.5
	spreadAffinitySuccessGap    = 0.2
)

func healthStateKnown(state HealthState) bool {
	return state.Observed
}

func resolveHealthState(auth *Auth, model string) HealthState {
	if auth == nil {
		return HealthState{}
	}
	channelHealth := HealthState{}
	if isGPTRetryRoute([]string{auth.Provider}, model) && healthStateKnown(auth.Health) {
		channelHealth = auth.Health
	}
	if model != "" && len(auth.ModelStates) > 0 {
		if state, ok := auth.ModelStates[model]; ok && state != nil && healthStateKnown(state.Health) {
			return lessHealthyState(channelHealth, state.Health)
		}
		baseModel := canonicalModelKey(model)
		if baseModel != "" && baseModel != model {
			if state, ok := auth.ModelStates[baseModel]; ok && state != nil && healthStateKnown(state.Health) {
				return lessHealthyState(channelHealth, state.Health)
			}
		}
	}
	if healthStateKnown(channelHealth) {
		return channelHealth
	}
	if healthStateKnown(auth.Health) {
		return auth.Health
	}
	return HealthState{}
}

func lessHealthyState(left, right HealthState) HealthState {
	if !healthStateKnown(left) {
		return right
	}
	if !healthStateKnown(right) {
		return left
	}
	rank := func(state HealthState) int {
		switch state.BreakerState {
		case HealthBreakerOpen:
			return 2
		case HealthBreakerHalfOpen:
			return 1
		default:
			return 0
		}
	}
	if leftRank, rightRank := rank(left), rank(right); leftRank != rightRank {
		if leftRank > rightRank {
			return left
		}
		return right
	}
	if left.Score <= right.Score {
		return left
	}
	return right
}

func recoveredHealthScore(state HealthState, now time.Time) int {
	if !healthStateKnown(state) {
		return healthScoreDefault
	}
	score := state.Score
	if !state.LastUpdatedAt.IsZero() && now.After(state.LastUpdatedAt) && healthRecoveryInterval > 0 {
		recoveryTicks := int(now.Sub(state.LastUpdatedAt) / healthRecoveryInterval)
		if recoveryTicks > 0 {
			score += recoveryTicks * healthRecoveryStep
		}
	}
	if score < 0 {
		score = 0
	}
	if score > healthScoreDefault {
		score = healthScoreDefault
	}
	return score
}

func healthTier(auth *Auth, model string, now time.Time) int {
	score := recoveredHealthScore(resolveHealthState(auth, model), now)
	if score < 0 {
		return 0
	}
	tier := score / 10
	if tier > 10 {
		tier = 10
	}
	return tier
}

func effectiveSelectionPriority(auth *Auth, model string, now time.Time) int {
	return authPriority(auth)*healthPriorityMultiplier + healthTier(auth, model, now)
}

func effectiveSelectionPriorityForRoute(auth *Auth, model string, now time.Time, includeHealth bool) int {
	if !includeHealth {
		return authPriority(auth)*healthPriorityMultiplier + healthScoreDefault/10
	}
	return effectiveSelectionPriority(auth, model, now)
}

func spreadSelectionWeight(auth *Auth, model string, now time.Time) int {
	provider := ""
	if auth != nil {
		provider = auth.Provider
	}
	return spreadSelectionWeightForRoute(auth, model, now, isGPTRetryRoute([]string{provider}, model), true)
}

func spreadSelectionWeightForRoute(auth *Auth, model string, now time.Time, gptRoute, includeHealth bool) int {
	priority := authPriority(auth)
	if priority < 0 {
		priority = 0
	}
	if priority > 20 {
		priority = 20
	}
	baseWeight := priority + 1
	score := healthScoreDefault
	if includeHealth {
		score = recoveredHealthScore(resolveHealthState(auth, model), now)
	}
	if score < 10 {
		score = 10
	}
	weight := (baseWeight*score + healthScoreDefault - 1) / healthScoreDefault
	if gptRoute {
		weight = baseWeight * score
	}
	if weight < 1 {
		return 1
	}
	return weight
}

type spreadLoadRecord struct {
	recentHits      float64
	inFlight        int
	successEWMA     float64
	ttftEWMA        time.Duration
	outcomeObserved bool
	ttftObserved    bool
	updatedAt       time.Time
}

type spreadLoadTracker struct {
	records       map[string]map[string]*spreadLoadRecord
	channelByAuth map[string]map[string]string
}

func newSpreadLoadTracker() *spreadLoadTracker {
	return &spreadLoadTracker{
		records:       make(map[string]map[string]*spreadLoadRecord),
		channelByAuth: make(map[string]map[string]string),
	}
}

func (t *spreadLoadTracker) ensureKey(key string, limit int) map[string]*spreadLoadRecord {
	if t.records == nil {
		t.records = make(map[string]map[string]*spreadLoadRecord)
	}
	if records := t.records[key]; records != nil {
		return records
	}
	if limit <= 0 {
		limit = spreadLoadDefaultKeyLimit
	}
	if len(t.records) >= limit {
		t.records = make(map[string]map[string]*spreadLoadRecord)
		t.channelByAuth = make(map[string]map[string]string)
	}
	records := make(map[string]*spreadLoadRecord)
	t.records[key] = records
	return records
}

func (t *spreadLoadTracker) bindAuth(key, authID, channelKey string, now time.Time, limit int) {
	authID = strings.TrimSpace(authID)
	channelKey = strings.TrimSpace(channelKey)
	if authID == "" || channelKey == "" {
		return
	}
	records := t.ensureKey(key, limit)
	if t.channelByAuth == nil {
		t.channelByAuth = make(map[string]map[string]string)
	}
	bindings := t.channelByAuth[key]
	if bindings == nil {
		bindings = make(map[string]string)
		t.channelByAuth[key] = bindings
	}
	if _, exists := bindings[authID]; !exists && len(bindings) >= spreadLoadDefaultKeyLimit {
		bindings = make(map[string]string)
		t.channelByAuth[key] = bindings
	}
	if _, alreadyBound := bindings[authID]; !alreadyBound && authID != channelKey {
		if source := records[authID]; source != nil {
			target := records[channelKey]
			if target == nil {
				target = &spreadLoadRecord{updatedAt: now}
				records[channelKey] = target
			}
			mergeSpreadLoadRecord(target, source, now)
			delete(records, authID)
		}
	}
	bindings[authID] = channelKey
}

func (t *spreadLoadTracker) recordKey(key, authID string) string {
	authID = strings.TrimSpace(authID)
	if bindings := t.channelByAuth[key]; bindings != nil {
		if channelKey := strings.TrimSpace(bindings[authID]); channelKey != "" {
			return channelKey
		}
	}
	return authID
}

func mergeSpreadLoadRecord(target, source *spreadLoadRecord, now time.Time) {
	if target == nil || source == nil || target == source {
		return
	}
	decaySpreadHits(target, now)
	decaySpreadHits(source, now)
	target.recentHits += source.recentHits
	target.inFlight += source.inFlight
	if source.outcomeObserved {
		if target.outcomeObserved {
			target.successEWMA = (target.successEWMA + source.successEWMA) / 2
		} else {
			target.successEWMA = source.successEWMA
			target.outcomeObserved = true
		}
	}
	if source.ttftObserved {
		if target.ttftObserved {
			target.ttftEWMA = (target.ttftEWMA + source.ttftEWMA) / 2
		} else {
			target.ttftEWMA = source.ttftEWMA
			target.ttftObserved = true
		}
	}
	if source.updatedAt.After(target.updatedAt) {
		target.updatedAt = source.updatedAt
	}
}

func decaySpreadHits(record *spreadLoadRecord, now time.Time) {
	if record == nil || record.updatedAt.IsZero() || !now.After(record.updatedAt) {
		return
	}
	elapsed := now.Sub(record.updatedAt)
	if elapsed <= 0 {
		return
	}
	if spreadLoadHalfLife <= 0 {
		record.recentHits = 0
		record.updatedAt = now
		return
	}
	record.recentHits *= math.Pow(0.5, float64(elapsed)/float64(spreadLoadHalfLife))
	if record.recentHits < 0.01 {
		record.recentHits = 0
	}
	record.updatedAt = now
}

func (t *spreadLoadTracker) snapshot(key string, authKeys []string, now time.Time, limit int) map[string]spreadLoadRecord {
	records := t.ensureKey(key, limit)
	active := make(map[string]struct{}, len(authKeys))
	out := make(map[string]spreadLoadRecord, len(authKeys))
	for _, authKey := range authKeys {
		active[authKey] = struct{}{}
		record := records[authKey]
		if record == nil {
			record = &spreadLoadRecord{updatedAt: now}
			records[authKey] = record
		}
		decaySpreadHits(record, now)
		out[authKey] = *record
	}
	for authKey, record := range records {
		if _, ok := active[authKey]; ok {
			continue
		}
		decaySpreadHits(record, now)
		if record.inFlight <= 0 && record.recentHits == 0 && now.Sub(record.updatedAt) > spreadLoadInactiveRecordTTL {
			delete(records, authKey)
		}
	}
	return out
}

func (t *spreadLoadTracker) markPicked(key, authKey string, now time.Time, limit int) {
	if authKey == "" {
		return
	}
	authKey = t.recordKey(key, authKey)
	record := t.ensureKey(key, limit)[authKey]
	if record == nil {
		record = &spreadLoadRecord{updatedAt: now}
		t.records[key][authKey] = record
	}
	decaySpreadHits(record, now)
	record.recentHits++
	record.inFlight++
	record.updatedAt = now
}

func (t *spreadLoadTracker) markDone(authID, model string, now time.Time) {
	authID = strings.TrimSpace(authID)
	modelKey := canonicalModelKey(model)
	if authID == "" || modelKey == "" || t == nil || len(t.records) == 0 {
		return
	}
	suffix := ":" + modelKey
	for key := range t.records {
		if !strings.HasSuffix(key, suffix) {
			continue
		}
		t.markRouteDone(key, authID, now)
	}
}

func (t *spreadLoadTracker) markRouteDone(key, authID string, now time.Time) {
	if t == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(authID) == "" {
		return
	}
	records := t.records[key]
	record := records[t.recordKey(key, authID)]
	if record == nil {
		return
	}
	decaySpreadHits(record, now)
	if record.inFlight > 0 {
		record.inFlight--
	}
	record.updatedAt = now
}

func (t *spreadLoadTracker) markResult(authID, model string, success bool, ttft time.Duration, now time.Time) {
	authID = strings.TrimSpace(authID)
	modelKey := canonicalModelKey(model)
	if authID == "" || modelKey == "" || t == nil || len(t.records) == 0 {
		return
	}
	suffix := ":" + modelKey
	for key := range t.records {
		if !strings.HasSuffix(key, suffix) {
			continue
		}
		t.markRouteResult(key, authID, success, ttft, true, now)
	}
}

func (t *spreadLoadTracker) markRouteResult(key, authID string, success bool, ttft time.Duration, release bool, now time.Time) {
	if t == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(authID) == "" {
		return
	}
	records := t.records[key]
	record := records[t.recordKey(key, authID)]
	if record == nil {
		return
	}
	decaySpreadHits(record, now)
	if release && record.inFlight > 0 {
		record.inFlight--
	}
	sample := 0.0
	if success {
		sample = 1
	}
	if !record.outcomeObserved {
		record.successEWMA = sample
		record.outcomeObserved = true
	} else {
		record.successEWMA = spreadOutcomeEWMAAlpha*sample + (1-spreadOutcomeEWMAAlpha)*record.successEWMA
	}
	if success && ttft > 0 {
		if !record.ttftObserved {
			record.ttftEWMA = ttft
			record.ttftObserved = true
		} else {
			record.ttftEWMA = time.Duration(spreadOutcomeEWMAAlpha*float64(ttft) + (1-spreadOutcomeEWMAAlpha)*float64(record.ttftEWMA))
		}
	}
	record.updatedAt = now
}

func adjustedSpreadSelectionWeight(baseWeight int, authLoad, averageLoad float64) int {
	if baseWeight < 1 {
		baseWeight = 1
	}
	if authLoad <= 0 {
		return baseWeight
	}
	if averageLoad < 1 {
		averageLoad = 1
	}
	loadRatio := authLoad / averageLoad
	if loadRatio <= 1 {
		return baseWeight
	}
	penalty := math.Pow(loadRatio, spreadLoadOverTargetPower)
	if penalty < 1 {
		penalty = 1
	}
	adjusted := int(math.Ceil(float64(baseWeight) / penalty))
	if adjusted < 1 {
		return 1
	}
	return adjusted
}

func adjustedGPTSpreadSelectionWeight(baseWeight int, record spreadLoadRecord, averageTTFT time.Duration) int {
	if baseWeight < 1 {
		baseWeight = 1
	}
	successFactor := 1.0
	if record.outcomeObserved {
		successFactor = record.successEWMA
		if successFactor < spreadMinSuccessFactor {
			successFactor = spreadMinSuccessFactor
		}
	}
	ttftFactor := 1.0
	if record.ttftObserved && record.ttftEWMA > 0 && averageTTFT > 0 {
		ttftFactor = float64(averageTTFT) / float64(record.ttftEWMA)
		if ttftFactor < spreadMinTTFTFactor {
			ttftFactor = spreadMinTTFTFactor
		}
		if ttftFactor > spreadMaxTTFTFactor {
			ttftFactor = spreadMaxTTFTFactor
		}
	}
	loadDivisor := float64(record.inFlight)
	if loadDivisor < 1 {
		loadDivisor = 1
	}
	adjusted := int(math.Round(float64(baseWeight) * successFactor * ttftFactor / loadDivisor))
	if adjusted < 1 {
		return 1
	}
	return adjusted
}

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

func authWebsocketsEnabled(auth *Auth) bool {
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

func preferCodexWebsocketAuths(ctx context.Context, provider string, available []*Auth) []*Auth {
	if len(available) == 0 {
		return available
	}
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return available
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return available
	}

	wsEnabled := make([]*Auth, 0, len(available))
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		if authWebsocketsEnabled(candidate) {
			wsEnabled = append(wsEnabled, candidate)
		}
	}
	if len(wsEnabled) > 0 {
		return wsEnabled
	}
	return available
}

func collectAvailableByPriority(auths []*Auth, model string, now time.Time, gptRoute bool) (available map[int][]*Auth, cooldownCount int, earliest time.Time) {
	available = make(map[int][]*Auth)
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		includeHealth := gptRoute || !isGPTRetryRoute([]string{candidate.Provider}, model)
		blocked, reason, next := isAuthBlockedForModelRoute(candidate, model, now, includeHealth)
		if !blocked {
			priority := effectiveSelectionPriorityForRoute(candidate, model, now, includeHealth)
			available[priority] = append(available[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}
	return available, cooldownCount, earliest
}

func collectAvailableIgnoringPriority(auths []*Auth, model string, now time.Time, gptRoute bool) (available []*Auth, cooldownCount int, earliest time.Time) {
	available = make([]*Auth, 0, len(auths))
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		includeHealth := gptRoute || !isGPTRetryRoute([]string{candidate.Provider}, model)
		blocked, reason, next := isAuthBlockedForModelRoute(candidate, model, now, includeHealth)
		if !blocked {
			available = append(available, candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}
	return available, cooldownCount, earliest
}

func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	return getAvailableAuthsForRoute(auths, provider, model, now, isGPTRetryRoute([]string{provider}, model))
}

func getAvailableAuthsForRoute(auths []*Auth, provider, model string, now time.Time, gptRoute bool) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, model, now, gptRoute)
	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}

	available := availableByPriority[bestPriority]
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

func getSpreadAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	return getSpreadAvailableAuthsForRoute(auths, provider, model, now, isGPTRetryRoute([]string{provider}, model))
}

func getSpreadAvailableAuthsForRoute(auths []*Auth, provider, model string, now time.Time, gptRoute bool) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	available, cooldownCount, earliest := collectAvailableIgnoringPriority(auths, model, now, gptRoute)
	if len(available) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

// Pick selects the next available auth for the provider in a round-robin manner.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuthsForRoute(auths, provider, model, now, isGPTRequestRoute(ctx, []string{provider}, model))
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	s.ensureCursorKey(key, limit)
	index := s.cursors[key]
	if index >= 2_147_483_640 {
		index = 0
	}
	s.cursors[key] = index + 1
	s.mu.Unlock()
	return available[index%len(available)], nil
}

// Pick selects the next available auth in an even spread across every non-blocked credential.
func (s *SpreadSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	gptRoute := isGPTRequestRoute(ctx, []string{provider}, model)
	available, err := getSpreadAvailableAuthsForRoute(auths, provider, model, now, gptRoute)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	if s.currentWeights == nil {
		s.currentWeights = make(map[string]map[string]int)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	groups, parentOrder := groupByVirtualParent(available)
	if len(parentOrder) > 1 {
		groupKey := key + "::group"
		s.ensureCursorKey(groupKey, limit)
		if _, exists := s.cursors[groupKey]; !exists {
			s.cursors[groupKey] = rand.IntN(len(parentOrder))
		}
		groupIndex := s.cursors[groupKey]
		if groupIndex >= 2_147_483_640 {
			groupIndex = 0
		}
		s.cursors[groupKey] = groupIndex + 1

		selectedParent := parentOrder[groupIndex%len(parentOrder)]
		group := groups[selectedParent]

		innerKey := key + "::cred:" + selectedParent
		s.ensureCursorKey(innerKey, limit)
		innerIndex := s.cursors[innerKey]
		if innerIndex >= 2_147_483_640 {
			innerIndex = 0
		}
		s.cursors[innerKey] = innerIndex + 1
		s.mu.Unlock()
		return group[innerIndex%len(group)], nil
	}

	selected := s.pickWeightedLocked(key, model, now, available, limit, gptRoute)
	s.mu.Unlock()
	if selected == nil {
		return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	return selected, nil
}

func (s *SpreadSelector) ensureCursorKey(key string, limit int) {
	if _, ok := s.cursors[key]; !ok && len(s.cursors) >= limit {
		s.cursors = make(map[string]int)
	}
}

func (s *SpreadSelector) ensureWeightKey(key string, limit int) {
	if _, ok := s.currentWeights[key]; !ok && len(s.currentWeights) >= limit {
		s.currentWeights = make(map[string]map[string]int)
	}
}

func (s *SpreadSelector) pickWeightedLocked(key, model string, now time.Time, available []*Auth, limit int, gptRoute bool) *Auth {
	if gptRoute {
		return s.pickGPTWeightedLocked(key, model, now, available, limit)
	}
	return s.pickLegacyWeightedLocked(key, model, now, available, limit)
}

func (s *SpreadSelector) pickLegacyWeightedLocked(key, model string, now time.Time, available []*Auth, limit int) *Auth {
	if len(available) == 0 {
		return nil
	}
	weightKey := key + "::weighted"
	s.ensureWeightKey(weightKey, limit)
	state := s.currentWeights[weightKey]
	if state == nil {
		state = make(map[string]int, len(available))
		s.currentWeights[weightKey] = state
	}

	active := make(map[string]struct{}, len(available))
	authKeys := make([]string, 0, len(available))
	authKeyByIndex := make([]string, len(available))
	for i, auth := range available {
		authKey := strings.TrimSpace(auth.ID)
		if authKey == "" {
			authKey = fmt.Sprintf("__index_%d", i)
		}
		authKeys = append(authKeys, authKey)
		authKeyByIndex[i] = authKey
	}
	if s.load == nil {
		s.load = newSpreadLoadTracker()
	}
	loadSnapshot := s.load.snapshot(key, authKeys, now, limit)
	totalLoad := 0.0
	for _, authKey := range authKeys {
		record := loadSnapshot[authKey]
		totalLoad += record.recentHits + float64(record.inFlight*spreadLoadInflightWeight)
	}
	averageLoad := 0.0
	if len(authKeys) > 0 {
		averageLoad = totalLoad / float64(len(authKeys))
	}

	totalWeight := 0
	var selected *Auth
	selectedKey := ""
	selectedScore := 0
	for i, auth := range available {
		authKey := authKeyByIndex[i]
		active[authKey] = struct{}{}
		includeHealth := !isGPTRetryRoute([]string{auth.Provider}, model)
		baseWeight := spreadSelectionWeightForRoute(auth, model, now, false, includeHealth)
		record := loadSnapshot[authKey]
		authLoad := record.recentHits + float64(record.inFlight*spreadLoadInflightWeight)
		weight := adjustedSpreadSelectionWeight(baseWeight, authLoad, averageLoad)
		totalWeight += weight
		state[authKey] += weight
		if selected == nil || state[authKey] > selectedScore {
			selected = auth
			selectedKey = authKey
			selectedScore = state[authKey]
		}
	}
	for authKey := range state {
		if _, ok := active[authKey]; !ok {
			delete(state, authKey)
		}
	}
	if selected != nil && totalWeight > 0 {
		state[selectedKey] -= totalWeight
		s.load.markPicked(key, selectedKey, now, limit)
	}
	return selected
}

type spreadChannelGroup struct {
	auths      []*Auth
	baseWeight int
}

func (s *SpreadSelector) pickGPTWeightedLocked(key, model string, now time.Time, available []*Auth, limit int) *Auth {
	if len(available) == 0 {
		return nil
	}
	if s.load == nil {
		s.load = newSpreadLoadTracker()
	}

	groupsByKey := make(map[string]*spreadChannelGroup, len(available))
	channelKeys := make([]string, 0, len(available))
	for i, auth := range available {
		channelKey := routingChannelBaseKey(auth)
		if channelKey == "" {
			channelKey = fmt.Sprintf("__index_%d", i)
		}
		group := groupsByKey[channelKey]
		if group == nil {
			group = &spreadChannelGroup{}
			groupsByKey[channelKey] = group
			channelKeys = append(channelKeys, channelKey)
		}
		group.auths = append(group.auths, auth)
		if weight := spreadSelectionWeightForRoute(auth, model, now, true, true); weight > group.baseWeight {
			group.baseWeight = weight
		}
		if auth != nil && strings.TrimSpace(auth.ID) != "" {
			s.load.bindAuth(key, auth.ID, channelKey, now, limit)
		}
	}
	sort.Strings(channelKeys)

	weightKey := key + "::weighted"
	s.ensureWeightKey(weightKey, limit)
	state := s.currentWeights[weightKey]
	if state == nil {
		state = make(map[string]int, len(channelKeys))
		s.currentWeights[weightKey] = state
	}
	loadSnapshot := s.load.snapshot(key, channelKeys, now, limit)
	var ttftTotal time.Duration
	ttftCount := 0
	for _, channelKey := range channelKeys {
		record := loadSnapshot[channelKey]
		if record.ttftObserved && record.ttftEWMA > 0 {
			ttftTotal += record.ttftEWMA
			ttftCount++
		}
	}
	var averageTTFT time.Duration
	if ttftCount > 0 {
		averageTTFT = ttftTotal / time.Duration(ttftCount)
	}

	active := make(map[string]struct{}, len(channelKeys))
	totalWeight := 0
	selectedKey := ""
	selectedScore := 0
	for _, channelKey := range channelKeys {
		group := groupsByKey[channelKey]
		active[channelKey] = struct{}{}
		weight := adjustedGPTSpreadSelectionWeight(group.baseWeight, loadSnapshot[channelKey], averageTTFT)
		totalWeight += weight
		state[channelKey] += weight
		if selectedKey == "" || state[channelKey] > selectedScore {
			selectedKey = channelKey
			selectedScore = state[channelKey]
		}
	}
	for channelKey := range state {
		if _, ok := active[channelKey]; !ok {
			delete(state, channelKey)
		}
	}
	group := groupsByKey[selectedKey]
	if group == nil || len(group.auths) == 0 || totalWeight <= 0 {
		return nil
	}
	state[selectedKey] -= totalWeight
	s.load.markPicked(key, selectedKey, now, limit)

	cursorKey := key + "::channel:" + selectedKey
	s.ensureCursorKey(cursorKey, limit)
	index := s.cursors[cursorKey]
	if index >= 2_147_483_640 {
		index = 0
	}
	s.cursors[cursorKey] = index + 1
	return group.auths[index%len(group.auths)]
}

func (s *SpreadSelector) MarkDone(authID, model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load == nil {
		return
	}
	s.load.markDone(authID, model, time.Now())
}

func (s *SpreadSelector) MarkPicked(provider, model, authID string) {
	if s == nil || strings.TrimSpace(authID) == "" {
		return
	}
	key := strings.TrimSpace(provider) + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load == nil {
		s.load = newSpreadLoadTracker()
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = spreadLoadDefaultKeyLimit
	}
	s.load.markPicked(key, authID, time.Now(), limit)
}

func (s *SpreadSelector) markPickedAuth(provider, model string, auth *Auth) {
	if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	key := strings.TrimSpace(provider) + ":" + canonicalModelKey(model)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load == nil {
		s.load = newSpreadLoadTracker()
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = spreadLoadDefaultKeyLimit
	}
	recordKey := auth.ID
	if isGPTRetryRoute([]string{provider}, model) || isGPTRetryRoute([]string{auth.Provider}, model) {
		recordKey = routingChannelBaseKey(auth)
		if recordKey == "" {
			recordKey = auth.ID
		}
	}
	s.load.bindAuth(key, auth.ID, recordKey, now, limit)
	s.load.markPicked(key, auth.ID, now, limit)
}

func (s *SpreadSelector) MarkResult(authID, model string, success bool, ttft time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load == nil {
		return
	}
	s.load.markResult(authID, model, success, ttft, time.Now())
}

func (s *SpreadSelector) MarkRouteDone(provider, authID, model string) {
	if s == nil {
		return
	}
	key := strings.TrimSpace(provider) + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load == nil {
		return
	}
	s.load.markRouteDone(key, authID, time.Now())
}

func (s *SpreadSelector) MarkRouteResult(provider, authID, model string, success bool, ttft time.Duration, release bool) {
	if s == nil {
		return
	}
	key := strings.TrimSpace(provider) + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load == nil {
		return
	}
	s.load.markRouteResult(key, authID, success, ttft, release, time.Now())
}

func (s *SpreadSelector) keepAffinity(provider, model, authID string, auths []*Auth) bool {
	if s == nil || len(auths) < 2 || strings.TrimSpace(authID) == "" {
		return true
	}
	key := strings.TrimSpace(provider) + ":" + canonicalModelKey(model)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load == nil {
		s.load = newSpreadLoadTracker()
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = spreadLoadDefaultKeyLimit
	}
	groups := make(map[string]*spreadChannelGroup, len(auths))
	channelKeys := make([]string, 0, len(auths))
	var bound *Auth
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		channelKey := routingChannelBaseKey(auth)
		if channelKey == "" {
			channelKey = auth.ID
		}
		group := groups[channelKey]
		if group == nil {
			group = &spreadChannelGroup{}
			groups[channelKey] = group
			channelKeys = append(channelKeys, channelKey)
		}
		group.auths = append(group.auths, auth)
		if weight := spreadSelectionWeightForRoute(auth, model, now, true, true); weight > group.baseWeight {
			group.baseWeight = weight
		}
		s.load.bindAuth(key, auth.ID, channelKey, now, limit)
		if auth.ID == authID {
			bound = auth
		}
	}
	if bound == nil {
		return true
	}
	boundChannel := routingChannelBaseKey(bound)
	if boundChannel == "" {
		boundChannel = bound.ID
	}
	if len(groups) < 2 {
		return true
	}
	records := s.load.snapshot(key, channelKeys, now, limit)
	boundRecord := records[boundChannel]
	minInflight := boundRecord.inFlight
	bestSuccess := boundRecord.successEWMA
	fastestTTFT := boundRecord.ttftEWMA
	bestHealthWeight := groups[boundChannel].baseWeight
	for channelKey, group := range groups {
		if channelKey == boundChannel {
			continue
		}
		record := records[channelKey]
		if record.inFlight < minInflight {
			minInflight = record.inFlight
		}
		if record.outcomeObserved && (!boundRecord.outcomeObserved || record.successEWMA > bestSuccess) {
			bestSuccess = record.successEWMA
		}
		if record.ttftObserved && record.ttftEWMA > 0 && (!boundRecord.ttftObserved || fastestTTFT <= 0 || record.ttftEWMA < fastestTTFT) {
			fastestTTFT = record.ttftEWMA
		}
		if group.baseWeight > bestHealthWeight {
			bestHealthWeight = group.baseWeight
		}
	}
	if boundRecord.inFlight > minInflight+1 {
		return false
	}
	if boundRecord.outcomeObserved && bestSuccess-boundRecord.successEWMA >= spreadAffinitySuccessGap {
		return false
	}
	if boundRecord.ttftObserved && fastestTTFT > 0 && float64(boundRecord.ttftEWMA) > float64(fastestTTFT)*spreadAffinityTTFTRatio {
		return false
	}
	return groups[boundChannel].baseWeight*2 >= bestHealthWeight
}

// groupByVirtualParent groups auths by their gemini_virtual_parent attribute.
// Returns a map of parentID -> auths and a sorted slice of parent IDs for stable iteration.
// Only auths with a non-empty gemini_virtual_parent are grouped; if any auth lacks
// this attribute, nil/nil is returned so the caller falls back to flat round-robin.
func groupByVirtualParent(auths []*Auth) (map[string][]*Auth, []string) {
	if len(auths) == 0 {
		return nil, nil
	}
	groups := make(map[string][]*Auth)
	for _, a := range auths {
		parent := ""
		if a.Attributes != nil {
			parent = strings.TrimSpace(a.Attributes["gemini_virtual_parent"])
		}
		if parent == "" {
			return nil, nil
		}
		groups[parent] = append(groups[parent], a)
	}
	parentOrder := make([]string, 0, len(groups))
	for parent := range groups {
		parentOrder = append(parentOrder, parent)
	}
	sort.Strings(parentOrder)
	return groups, parentOrder
}

// ensureCursorKey ensures the cursor map has capacity for the given key.
// Must be called with s.mu held.
func (s *RoundRobinSelector) ensureCursorKey(key string, limit int) {
	if _, ok := s.cursors[key]; !ok && len(s.cursors) >= limit {
		s.cursors = make(map[string]int)
	}
}

// Pick selects the first available auth for the provider in a deterministic manner.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuthsForRoute(auths, provider, model, now, isGPTRequestRoute(ctx, []string{provider}, model))
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	return available[0], nil
}

// Pick selects credentials sequentially without jumping back to earlier ones.
// For mixed-provider requests, it sticks to the current provider until all its
// credentials are exhausted, then advances to the next provider.
func (s *SequentialFillSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuthsForRoute(auths, provider, model, now, isGPTRequestRoute(ctx, []string{provider}, model))
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current == nil {
		s.current = make(map[string]string)
	}

	// Single provider path: flat sticky selection.
	if provider != "mixed" {
		return s.pickSticky(provider, model, available), nil
	}

	// Mixed provider path: group by actual provider.
	groups := make(map[string][]*Auth)
	for _, auth := range available {
		groups[auth.Provider] = append(groups[auth.Provider], auth)
	}

	// Single actual provider in the mix: no rotation needed.
	if len(groups) == 1 {
		for p := range groups {
			return s.pickSticky(p, model, groups[p]), nil
		}
	}

	// Sticky provider selection: stick to the current provider as long as it
	// has available credentials. Only advance when the current provider is
	// exhausted (all its credentials are in cooldown/unavailable).
	if s.stickyProvider == nil {
		s.stickyProvider = make(map[string]string)
	}

	// Sort provider names for deterministic ordering.
	providers := make([]string, 0, len(groups))
	for p := range groups {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	// If we have a sticky provider and it still has available credentials, use it.
	if cp := s.stickyProvider[model]; cp != "" {
		if auths, ok := groups[cp]; ok {
			return s.pickSticky(cp, model, auths), nil
		}
		// Current provider exhausted, advance to the next one.
		next := providers[0]
		for _, p := range providers {
			if p > cp {
				next = p
				break
			}
		}
		s.stickyProvider[model] = next
		return s.pickSticky(next, model, groups[next]), nil
	}

	// First access: start with the first provider.
	s.stickyProvider[model] = providers[0]
	return s.pickSticky(providers[0], model, groups[providers[0]]), nil
}

// pickSticky selects a credential from the given group with sticky sequential behavior.
// Must be called with s.mu held.
func (s *SequentialFillSelector) pickSticky(provider, model string, available []*Auth) *Auth {
	key := provider + ":" + model
	currentID := s.current[key]

	// First access: randomly select a starting credential.
	if currentID == "" {
		i := rand.IntN(len(available))
		s.current[key] = available[i].ID
		return available[i]
	}

	// Sticky: if current credential is still available, keep using it.
	for _, auth := range available {
		if auth.ID == currentID {
			return auth
		}
	}

	// Advance: find the first credential with ID > currentID.
	for _, auth := range available {
		if auth.ID > currentID {
			s.current[key] = auth.ID
			return auth
		}
	}

	// Wrap around: all subsequent credentials unavailable, start from beginning.
	s.current[key] = available[0].ID
	return available[0]
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	provider := ""
	if auth != nil {
		provider = auth.Provider
	}
	return isAuthBlockedForModelRoute(auth, model, now, isGPTRetryRoute([]string{provider}, model))
}

func isAuthBlockedForModelRoute(auth *Auth, model string, now time.Time, gptRoute bool) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if isCodexAuth(auth) && !isCodexAPIKeyAuth(auth) {
		if hasUnauthorizedAuthFailure(auth) {
			return true, blockReasonOther, time.Time{}
		}
		return false, blockReasonNone, time.Time{}
	}
	authBlocked, authReason, authNext := authLevelBlockState(auth, now)
	if gptRoute {
		if blocked, next := healthBlockStateForAuth(auth, model, now); blocked {
			return true, blockReasonOther, next
		}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, ok := auth.ModelStates[model]
			if (!ok || state == nil) && model != "" {
				baseModel := canonicalModelKey(model)
				if baseModel != "" && baseModel != model {
					state, ok = auth.ModelStates[baseModel]
				}
			}
			if ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				if authBlocked {
					return true, authReason, authNext
				}
				return false, blockReasonNone, time.Time{}
			}
		}
		if authBlocked {
			return true, authReason, authNext
		}
		return false, blockReasonNone, time.Time{}
	}
	if authBlocked {
		return true, authReason, authNext
	}
	return false, blockReasonNone, time.Time{}
}

func healthBlockStateForAuth(auth *Auth, model string, now time.Time) (bool, time.Time) {
	if auth == nil {
		return false, time.Time{}
	}
	if isCodexAuth(auth) && !isCodexAPIKeyAuth(auth) {
		return false, time.Time{}
	}
	state := resolveHealthState(auth, model)
	if state.BreakerState != HealthBreakerOpen || state.OpenUntil.IsZero() || !state.OpenUntil.After(now) {
		return false, time.Time{}
	}
	return true, state.OpenUntil
}

func authLevelBlockState(auth *Auth, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return false, blockReasonNone, time.Time{}
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		next := auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if auth.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

// sessionPattern matches Claude Code user_id format:
// user_{hash}_account__session_{uuid}
var sessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// SessionAffinitySelector wraps another selector with session-sticky behavior.
// It extracts session ID from multiple sources and maintains session-to-auth
// mappings with automatic failover when the bound auth becomes unavailable.
type SessionAffinitySelector struct {
	fallback Selector
	cache    *SessionCache
}

// SessionAffinityConfig configures the session affinity selector.
type SessionAffinityConfig struct {
	Fallback Selector
	TTL      time.Duration
}

// NewSessionAffinitySelector creates a new session-aware selector.
func NewSessionAffinitySelector(fallback Selector) *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: fallback,
		TTL:      time.Hour,
	})
}

// NewSessionAffinitySelectorWithConfig creates a selector with custom configuration.
func NewSessionAffinitySelectorWithConfig(cfg SessionAffinityConfig) *SessionAffinitySelector {
	if cfg.Fallback == nil {
		cfg.Fallback = &RoundRobinSelector{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = time.Hour
	}
	return &SessionAffinitySelector{
		fallback: cfg.Fallback,
		cache:    NewSessionCache(cfg.TTL),
	}
}

func sessionAffinityBinding(cache *SessionCache, key string, refresh bool) (string, string, bool) {
	if cache == nil {
		return "", "", false
	}
	if refresh {
		return cache.GetAndRefreshBinding(key)
	}
	return cache.GetBinding(key)
}

func stageSessionAffinityBinding(ctx context.Context, cache *SessionCache, key, authID, channelKey string, deferred bool) {
	if cache == nil || key == "" || authID == "" {
		return
	}
	if deferred {
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			trace.stageSessionBinding(cache, key, authID, channelKey)
			return
		}
	}
	cache.SetBinding(key, authID, channelKey)
}

func markSessionAffinityPicked(selector Selector, provider, model string, auth *Auth) {
	switch current := selector.(type) {
	case *SpreadSelector:
		current.markPickedAuth(provider, model, auth)
	case *SessionAffinitySelector:
		markSessionAffinityPicked(current.fallback, provider, model, auth)
	}
}

func keepSessionAffinity(selector Selector, provider, model, authID string, auths []*Auth) bool {
	switch current := selector.(type) {
	case *SpreadSelector:
		return current.keepAffinity(provider, model, authID, auths)
	case *SessionAffinitySelector:
		return keepSessionAffinity(current.fallback, provider, model, authID, auths)
	default:
		return true
	}
}

func withoutRoutingChannel(auths []*Auth, excludedKey string) []*Auth {
	if excludedKey == "" {
		return auths
	}
	out := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || routingChannelBaseKey(auth) == excludedKey {
			continue
		}
		out = append(out, auth)
	}
	return out
}

func inRoutingChannel(auths []*Auth, boundKey string) []*Auth {
	if boundKey == "" {
		return nil
	}
	out := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth != nil && routingChannelBaseKey(auth) == boundKey {
			out = append(out, auth)
		}
	}
	return out
}

func authByID(auths []*Auth, authID string) *Auth {
	for _, auth := range auths {
		if auth != nil && auth.ID == authID {
			return auth
		}
	}
	return nil
}

// Pick selects an auth with session affinity when possible.
// Priority for session ID extraction:
//  1. metadata.user_id (Claude Code format with _session_{uuid}) - highest priority
//  2. X-Session-ID header
//  3. Session_id header (Codex)
//  4. X-Client-Request-Id header (PI)
//  5. metadata.user_id (non-Claude Code format)
//  6. conversation_id field in request body
//  7. Stable hash from first few messages content (fallback)
//
// Note: The cache key includes provider, session ID, and model to handle cases where
// a session uses multiple models (e.g., gemini-2.5-pro and gemini-3-flash-preview)
// that may be supported by different auth credentials, and to avoid cross-provider conflicts.
func (s *SessionAffinitySelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	entry := selectorLogEntry(ctx)
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		entry.Debugf("session-affinity: no session ID extracted, falling back to default selector | provider=%s model=%s", provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, auths)
	}

	now := time.Now()
	gptRoute := isGPTRequestRoute(ctx, []string{provider}, model)
	gptSpread := gptRoute && selectorUsesSpread(s.fallback)
	var (
		available []*Auth
		err       error
	)
	if gptSpread {
		available, err = getSpreadAvailableAuthsForRoute(auths, provider, model, now, gptRoute)
	} else {
		available, err = getAvailableAuthsForRoute(auths, provider, model, now, gptRoute)
	}
	if err != nil {
		return nil, err
	}

	cacheKey := provider + "::" + primaryID + "::" + model
	deferBinding := gptRoute && requestAttemptTraceFromContext(ctx) != nil

	pickFallback := func(excludedAuthID, excludedChannelKey string) (*Auth, error) {
		candidates := auths
		if gptRoute {
			candidates = available
			if excludedChannelKey == "" {
				if excluded := authByID(auths, excludedAuthID); excluded != nil {
					excludedChannelKey = routingChannelBaseKey(excluded)
				}
			}
			candidates = withoutRoutingChannel(candidates, excludedChannelKey)
		}
		auth, errPick := s.fallback.Pick(ctx, provider, model, opts, candidates)
		if errPick != nil {
			return nil, errPick
		}
		channelKey := ""
		if gptRoute {
			channelKey = routingChannelBaseKey(auth)
		}
		stageSessionAffinityBinding(ctx, s.cache, cacheKey, auth.ID, channelKey, deferBinding)
		return auth, nil
	}
	pickBound := func(cachedAuthID, cachedChannelKey string) (*Auth, bool, error) {
		bound := authByID(available, cachedAuthID)
		if !gptRoute {
			if bound == nil {
				return nil, false, nil
			}
			return bound, true, nil
		}
		anchor := authByID(auths, cachedAuthID)
		if cachedChannelKey == "" && anchor != nil {
			cachedChannelKey = routingChannelBaseKey(anchor)
		}
		if cachedChannelKey == "" {
			return nil, false, nil
		}
		channelAuths := inRoutingChannel(available, cachedChannelKey)
		if len(channelAuths) == 0 {
			return nil, false, nil
		}
		healthAuthID := cachedAuthID
		if bound == nil {
			healthAuthID = channelAuths[0].ID
		}
		if !keepSessionAffinity(s.fallback, provider, model, healthAuthID, available) {
			return nil, false, nil
		}
		selected := bound
		if gptSpread || selected == nil {
			var errPick error
			selected, errPick = s.fallback.Pick(ctx, provider, model, opts, channelAuths)
			if errPick != nil {
				return nil, false, errPick
			}
		} else {
			markSessionAffinityPicked(s.fallback, provider, model, bound)
		}
		stageSessionAffinityBinding(ctx, s.cache, cacheKey, selected.ID, cachedChannelKey, deferBinding)
		return selected, true, nil
	}

	if cachedAuthID, cachedChannelKey, ok := sessionAffinityBinding(s.cache, cacheKey, !deferBinding); ok {
		auth, keep, errPick := pickBound(cachedAuthID, cachedChannelKey)
		if errPick != nil {
			return nil, errPick
		}
		if keep {
			entry.Infof("session-affinity: cache hit | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
			return auth, nil
		}
		auth, errPick = pickFallback(cachedAuthID, cachedChannelKey)
		if errPick != nil {
			return nil, errPick
		}
		entry.Infof("session-affinity: cache hit but auth unavailable, reselected | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
		return auth, nil
	}

	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey := provider + "::" + fallbackID + "::" + model
		if cachedAuthID, cachedChannelKey, ok := s.cache.GetBinding(fallbackKey); ok {
			auth, keep, errPick := pickBound(cachedAuthID, cachedChannelKey)
			if errPick != nil {
				return nil, errPick
			}
			if keep {
				if !gptRoute {
					stageSessionAffinityBinding(ctx, s.cache, cacheKey, auth.ID, "", deferBinding)
				}
				entry.Infof("session-affinity: fallback cache hit | session=%s fallback=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
				return auth, nil
			}
			if gptRoute {
				auth, errPick = pickFallback(cachedAuthID, cachedChannelKey)
				if errPick != nil {
					return nil, errPick
				}
				entry.Infof("session-affinity: fallback cache hit but channel unavailable, reselected | session=%s fallback=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
				return auth, nil
			}
		}
	}

	auth, errPick := pickFallback("", "")
	if errPick != nil {
		return nil, errPick
	}
	entry.Infof("session-affinity: cache miss, new binding | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
	return auth, nil
}

func (s *SessionAffinitySelector) MarkDone(authID, model string) {
	if s == nil || s.fallback == nil {
		return
	}
	if selector, ok := s.fallback.(loadAwareSelector); ok {
		selector.MarkDone(authID, model)
	}
}

func (s *SessionAffinitySelector) MarkResult(authID, model string, success bool, ttft time.Duration) {
	if s == nil || s.fallback == nil {
		return
	}
	if selector, ok := s.fallback.(interface {
		MarkResult(string, string, bool, time.Duration)
	}); ok {
		selector.MarkResult(authID, model, success, ttft)
		return
	}
	if selector, ok := s.fallback.(loadAwareSelector); ok {
		selector.MarkDone(authID, model)
	}
}

func (s *SessionAffinitySelector) MarkRouteDone(provider, authID, model string) {
	if s == nil || s.fallback == nil {
		return
	}
	if selector, ok := s.fallback.(routeLoadAwareSelector); ok {
		selector.MarkRouteDone(provider, authID, model)
		return
	}
	if selector, ok := s.fallback.(loadAwareSelector); ok {
		selector.MarkDone(authID, model)
	}
}

func (s *SessionAffinitySelector) MarkRouteResult(provider, authID, model string, success bool, ttft time.Duration, release bool) {
	if s == nil || s.fallback == nil {
		return
	}
	if selector, ok := s.fallback.(routeResultAwareSelector); ok {
		selector.MarkRouteResult(provider, authID, model, success, ttft, release)
		return
	}
	if selector, ok := s.fallback.(resultAwareSelector); ok {
		selector.MarkResult(authID, model, success, ttft)
		return
	}
	if release {
		s.MarkRouteDone(provider, authID, model)
	}
}

func selectorLogEntry(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

// truncateSessionID shortens session ID for logging (first 8 chars + "...")
func truncateSessionID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return id[:8] + "..."
}

// Stop releases resources held by the selector.
func (s *SessionAffinitySelector) Stop() {
	if s.cache != nil {
		s.cache.Stop()
	}
}

// InvalidateAuth removes all session bindings for a specific auth.
// Called when an auth becomes rate-limited or unavailable.
func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s.cache != nil {
		s.cache.InvalidateAuth(authID)
	}
}

// ExtractSessionID extracts session identifier from multiple sources.
// Priority order:
//  1. metadata.user_id (Claude Code format with _session_{uuid}) - highest priority for Claude Code clients
//  2. X-Session-ID header
//  3. Session_id header (Codex)
//  4. X-Client-Request-Id header (PI)
//  5. metadata.user_id (non-Claude Code format)
//  6. conversation_id field in request body
//  7. Stable hash from first few messages content (fallback)
func ExtractSessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	primary, _ := extractSessionIDs(headers, payload, metadata)
	return primary
}

// extractSessionIDs returns (primaryID, fallbackID) for session affinity.
// primaryID: full hash including assistant response (stable after first turn)
// fallbackID: short hash without assistant (used to inherit binding from first turn)
func extractSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (string, string) {
	// 1. metadata.user_id with Claude Code session format (highest priority)
	if len(payload) > 0 {
		userID := gjson.GetBytes(payload, "metadata.user_id").String()
		if userID != "" {
			// Old format: user_{hash}_account__session_{uuid}
			if matches := sessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
				id := "claude:" + matches[1]
				return id, ""
			}
			// New format: JSON object with session_id field
			// e.g. {"device_id":"...","account_uuid":"...","session_id":"uuid"}
			if len(userID) > 0 && userID[0] == '{' {
				if sid := gjson.Get(userID, "session_id").String(); sid != "" {
					return "claude:" + sid, ""
				}
			}
		}
	}

	// 2. X-Session-ID header
	if headers != nil {
		if sid := headers.Get("X-Session-ID"); sid != "" {
			return "header:" + sid, ""
		}
	}

	// 3. Session_id header (Codex)
	if headers != nil {
		if sid := headers.Get("Session-Id"); sid != "" {
			return "codex:" + sid, ""
		}
		if sid := headers.Get("Session_id"); sid != "" {
			return "codex:" + sid, ""
		}
	}

	// 4. X-Client-Request-Id header (PI)
	if headers != nil {
		if rid := headers.Get("X-Client-Request-Id"); rid != "" {
			return "clientreq:" + rid, ""
		}
	}

	if len(payload) == 0 {
		return "", ""
	}

	// 6. metadata.user_id (non-Claude Code format)
	userID := gjson.GetBytes(payload, "metadata.user_id").String()
	if userID != "" {
		return "user:" + userID, ""
	}

	// 7. conversation_id field
	if convID := gjson.GetBytes(payload, "conversation_id").String(); convID != "" {
		return "conv:" + convID, ""
	}

	// 8. Hash-based fallback from message content
	return extractMessageHashIDs(payload)
}

func extractMessageHashIDs(payload []byte) (primaryID, fallbackID string) {
	var systemPrompt, firstUserMsg, firstAssistantMsg string

	// OpenAI/Claude messages format
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := extractMessageContent(msg.Get("content"))
			if content == "" {
				return true
			}

			switch role {
			case "system":
				if systemPrompt == "" {
					systemPrompt = truncateString(content, 100)
				}
			case "user":
				if firstUserMsg == "" {
					firstUserMsg = truncateString(content, 100)
				}
			case "assistant":
				if firstAssistantMsg == "" {
					firstAssistantMsg = truncateString(content, 100)
				}
			}

			if systemPrompt != "" && firstUserMsg != "" && firstAssistantMsg != "" {
				return false
			}
			return true
		})
	}

	// Claude API: top-level "system" field (array or string)
	if systemPrompt == "" {
		topSystem := gjson.GetBytes(payload, "system")
		if topSystem.Exists() {
			if topSystem.IsArray() {
				topSystem.ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text").String(); text != "" && systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
						return false
					}
					return true
				})
			} else if topSystem.Type == gjson.String {
				systemPrompt = truncateString(topSystem.String(), 100)
			}
		}
	}

	// Gemini format
	if systemPrompt == "" && firstUserMsg == "" {
		sysInstr := gjson.GetBytes(payload, "systemInstruction.parts")
		if sysInstr.Exists() && sysInstr.IsArray() {
			sysInstr.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" && systemPrompt == "" {
					systemPrompt = truncateString(text, 100)
					return false
				}
				return true
			})
		}

		contents := gjson.GetBytes(payload, "contents")
		if contents.Exists() && contents.IsArray() {
			contents.ForEach(func(_, msg gjson.Result) bool {
				role := msg.Get("role").String()
				msg.Get("parts").ForEach(func(_, part gjson.Result) bool {
					text := part.Get("text").String()
					if text == "" {
						return true
					}
					switch role {
					case "user":
						if firstUserMsg == "" {
							firstUserMsg = truncateString(text, 100)
						}
					case "model":
						if firstAssistantMsg == "" {
							firstAssistantMsg = truncateString(text, 100)
						}
					}
					return false
				})
				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	// OpenAI Responses API format (v1/responses)
	if systemPrompt == "" && firstUserMsg == "" {
		if instr := gjson.GetBytes(payload, "instructions").String(); instr != "" {
			systemPrompt = truncateString(instr, 100)
		}

		input := gjson.GetBytes(payload, "input")
		if input.Exists() && input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				itemType := item.Get("type").String()
				if itemType == "reasoning" {
					return true
				}
				// Skip non-message typed items (function_call, function_call_output, etc.)
				// but allow items with no type that have a role (inline message format).
				if itemType != "" && itemType != "message" {
					return true
				}

				role := item.Get("role").String()
				if itemType == "" && role == "" {
					return true
				}

				// Handle both string content and array content (multimodal).
				content := item.Get("content")
				var text string
				if content.Type == gjson.String {
					text = content.String()
				} else {
					text = extractResponsesAPIContent(content)
				}
				if text == "" {
					return true
				}

				switch role {
				case "developer", "system":
					if systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
					}
				case "user":
					if firstUserMsg == "" {
						firstUserMsg = truncateString(text, 100)
					}
				case "assistant":
					if firstAssistantMsg == "" {
						firstAssistantMsg = truncateString(text, 100)
					}
				}

				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	if systemPrompt == "" && firstUserMsg == "" {
		return "", ""
	}

	shortHash := computeSessionHash(systemPrompt, firstUserMsg, "")
	if firstAssistantMsg == "" {
		return shortHash, ""
	}

	fullHash := computeSessionHash(systemPrompt, firstUserMsg, firstAssistantMsg)
	return fullHash, shortHash
}

func computeSessionHash(systemPrompt, userMsg, assistantMsg string) string {
	h := fnv.New64a()
	if systemPrompt != "" {
		h.Write([]byte("sys:" + systemPrompt + "\n"))
	}
	if userMsg != "" {
		h.Write([]byte("usr:" + userMsg + "\n"))
	}
	if assistantMsg != "" {
		h.Write([]byte("ast:" + assistantMsg + "\n"))
	}
	return fmt.Sprintf("msg:%016x", h.Sum64())
}

func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// extractMessageContent extracts text content from a message content field.
// Handles both string content and array content (multimodal messages).
// For array content, extracts text from all text-type elements.
func extractMessageContent(content gjson.Result) string {
	// String content: "Hello world"
	if content.Type == gjson.String {
		return content.String()
	}

	// Array content: [{"type":"text","text":"Hello"},{"type":"image",...}]
	if content.IsArray() {
		var texts []string
		content.ForEach(func(_, part gjson.Result) bool {
			// Handle Claude format: {"type":"text","text":"content"}
			if part.Get("type").String() == "text" {
				if text := part.Get("text").String(); text != "" {
					texts = append(texts, text)
				}
			}
			// Handle OpenAI format: {"type":"text","text":"content"}
			// Same structure as Claude, already handled above
			return true
		})
		if len(texts) > 0 {
			return strings.Join(texts, " ")
		}
	}

	return ""
}

func extractResponsesAPIContent(content gjson.Result) string {
	if !content.IsArray() {
		return ""
	}
	var texts []string
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "input_text" || partType == "output_text" || partType == "text" {
			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		}
		return true
	})
	if len(texts) > 0 {
		return strings.Join(texts, " ")
	}
	return ""
}

// extractSessionID is kept for backward compatibility.
// Deprecated: Use ExtractSessionID instead.
func extractSessionID(payload []byte) string {
	return ExtractSessionID(nil, payload, nil)
}
