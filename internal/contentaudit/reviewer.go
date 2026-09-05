package contentaudit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	ModelReviewModeOff     = "off"
	ModelReviewModeShadow  = "shadow"
	ModelReviewModeEnforce = "enforce"

	ModelReviewBlock     = "block"
	ModelReviewAllow     = "allow"
	ModelReviewUncertain = "uncertain"
)

var errModelReviewSaturated = errors.New("content audit model review capacity is saturated")
var errModelReviewCircuitOpen = errors.New("content audit model review circuit is open")
var errModelReviewInvalidResult = errors.New("content audit model review returned an invalid result")
var errModelReviewContextIncomplete = errors.New("content audit model review context is incomplete")
var errModelReviewDailyBudget = errors.New("content audit model review daily budget is exhausted")
var errModelReviewMinuteLimit = errors.New("content audit model review minute rate is limited")
var errModelReviewBudgetStorage = errors.New("content audit model review budget storage is unavailable")

// ModelReviewRequest contains only the current user enforcement scope and non-sensitive policy metadata.
type ModelReviewRequest struct {
	Model             string
	PromptVersion     string
	PolicyVersion     string `json:"-"`
	TenantScope       string `json:"-"`
	Text              string
	ReferenceText     string
	ContextIncomplete bool
	MatchedTerm       string
	RuleID            string
	Category          string
	Severity          string
}

// ModelReviewResult is a bounded, non-sensitive semantic classification.
type ModelReviewResult struct {
	Decision         string           `json:"decision"`
	Category         string           `json:"category,omitempty"`
	Confidence       float64          `json:"confidence"`
	ReasonCodes      []string         `json:"reason_codes,omitempty"`
	StageLatenciesMS map[string]int64 `json:"-"`
	ResolvedModel    string           `json:"-"`
}

// Reviewer performs one non-streaming semantic review without entering the public HTTP middleware chain.
type Reviewer interface {
	Review(context.Context, ModelReviewRequest) (ModelReviewResult, error)
}

type modelReviewOutcome struct {
	ModelReviewResult
	Model    string
	Latency  time.Duration
	CacheHit bool
	Fallback string
	Reviewed bool
}

type modelReviewCacheEntry struct {
	result    ModelReviewResult
	expiresAt time.Time
}

// A flight owns its budget independently of any waiter. Flights, including
// providers that are slow to honor cancellation, occupy a bounded admission slot.
type modelReviewFlight struct {
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
	waiters           int
	started           time.Time
	providerStarted   time.Time
	admissionStarted  time.Time
	admissionFinished time.Time
	trace             *coreexecutor.ContentAuditReviewTrace
	result            ModelReviewResult
	err               error
}

type modelReviewController struct {
	cfg      config.ContentAuditModelReviewConfig
	reviewer Reviewer
	// admit is initialized before publishing the controller and reserves durable
	// quota only for a unique external attempt after cache and capacity gates.
	admit     func(context.Context) (bool, string, error)
	rules     map[string]struct{}
	ruleGate  bool
	semaphore chan struct{}
	cacheKey  [32]byte
	cacheMu   sync.Mutex
	cache     map[string]modelReviewCacheEntry
	flightsMu sync.Mutex
	flights   map[string]*modelReviewFlight
	circuitMu sync.Mutex
	failures  int
	openUntil time.Time
	halfOpen  bool
}

func newModelReviewController(cfg config.ContentAuditModelReviewConfig, reviewer Reviewer) *modelReviewController {
	if reviewer == nil || cfg.Mode == ModelReviewModeOff {
		return nil
	}
	if cfg.CircuitFailureThreshold <= 0 {
		cfg.CircuitFailureThreshold = 5
	}
	if cfg.CircuitOpenSeconds <= 0 {
		cfg.CircuitOpenSeconds = 30
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 16
	}
	if cfg.TimeoutMilliseconds <= 0 {
		cfg.TimeoutMilliseconds = 4000
	}
	controller := &modelReviewController{
		cfg:       cfg,
		reviewer:  reviewer,
		rules:     makeRuleSelection(cfg.Rules),
		ruleGate:  cfg.Rules != nil,
		semaphore: make(chan struct{}, cfg.MaxConcurrent),
		cache:     make(map[string]modelReviewCacheEntry),
		flights:   make(map[string]*modelReviewFlight),
	}
	if _, err := rand.Read(controller.cacheKey[:]); err != nil {
		controller.cacheKey = sha256.Sum256([]byte(time.Now().UTC().String()))
	}
	return controller
}

func (c *modelReviewController) review(ctx context.Context, request ModelReviewRequest) modelReviewOutcome {
	if c == nil || c.reviewer == nil {
		return modelReviewOutcome{ModelReviewResult: ModelReviewResult{Decision: ModelReviewUncertain}, Fallback: "reviewer_disabled"}
	}
	if c.ruleGate {
		if _, ok := c.rules[strings.TrimSpace(request.RuleID)]; !ok {
			return modelReviewOutcome{
				ModelReviewResult: ModelReviewResult{Decision: ModelReviewUncertain},
				Fallback:          "rule_not_selected",
			}
		}
	}
	started := time.Now()
	deadline := started.Add(time.Duration(c.cfg.TimeoutMilliseconds) * time.Millisecond)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	request.Model = c.cfg.Model
	request.PromptVersion = c.cfg.PromptVersion
	// Fingerprint the complete task before compaction; identical excerpts are not
	// interchangeable when their original context or decision versions differ.
	cacheKey := c.fingerprint(request)
	if err := waitCtx.Err(); err != nil {
		return finishModelReview(request.Model, started, ModelReviewResult{}, err, false)
	}
	if result, ok := c.cached(cacheKey); ok {
		result.StageLatenciesMS = nil
		return finishModelReview(request.Model, started, result, nil, true)
	}
	c.flightsMu.Lock()
	flight := c.flights[cacheKey]
	if flight != nil && flight.ctx.Err() != nil {
		// Do not replace an abandoned, cancellation-resistant provider with another
		// goroutine. Its admission slot is released only when it actually returns.
		c.flightsMu.Unlock()
		return finishModelReview(request.Model, started, ModelReviewResult{}, errModelReviewSaturated, false)
	}
	if flight == nil {
		if result, ok := c.cached(cacheKey); ok {
			c.flightsMu.Unlock()
			result.StageLatenciesMS = nil
			return finishModelReview(request.Model, started, result, nil, true)
		}
		if len(c.flights) >= 2*cap(c.semaphore) {
			c.flightsMu.Unlock()
			return finishModelReview(request.Model, started, ModelReviewResult{}, errModelReviewSaturated, false)
		}
		flightCtx, flightCancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		flightCtx, trace := coreexecutor.WithContentAuditReviewTrace(flightCtx)
		flight = &modelReviewFlight{ctx: flightCtx, cancel: flightCancel, done: make(chan struct{}), started: started, trace: trace}
		c.flights[cacheKey] = flight
		go c.runFlight(cacheKey, flight, request)
	}
	flight.waiters++
	c.flightsMu.Unlock()
	defer c.leaveFlight(flight)
	select {
	case <-waitCtx.Done():
		return finishModelReview(request.Model, started, c.flightTimings(flight), waitCtx.Err(), false)
	case <-flight.done:
		if err := waitCtx.Err(); err != nil {
			return finishModelReview(request.Model, started, flight.result, err, false)
		}
		return finishModelReview(request.Model, started, flight.result, flight.err, false)
	}
}

func finishModelReview(model string, started time.Time, result ModelReviewResult, err error, cacheHit bool) modelReviewOutcome {
	result = cloneModelReviewResult(result)
	latency := time.Since(started)
	if result.StageLatenciesMS == nil {
		result.StageLatenciesMS = make(map[string]int64)
	}
	result.StageLatenciesMS["total"] = latency.Milliseconds()
	outcome := modelReviewOutcome{ModelReviewResult: result, Model: model, Latency: latency, Reviewed: true, CacheHit: cacheHit}
	if err != nil {
		outcome.ModelReviewResult = ModelReviewResult{Decision: ModelReviewUncertain, StageLatenciesMS: result.StageLatenciesMS, ResolvedModel: result.ResolvedModel}
		outcome.Fallback = modelReviewFallbackReason(err)
	}
	return outcome
}

func (c *modelReviewController) leaveFlight(flight *modelReviewFlight) {
	c.flightsMu.Lock()
	defer c.flightsMu.Unlock()
	flight.waiters--
	if flight.waiters == 0 {
		flight.cancel()
	}
}

func (c *modelReviewController) flightTimings(flight *modelReviewFlight) ModelReviewResult {
	c.flightsMu.Lock()
	defer c.flightsMu.Unlock()
	stages := flight.trace.Snapshot()
	if stages == nil {
		stages = make(map[string]int64, 2)
	}
	if flight.providerStarted.IsZero() {
		stages["queue"] = time.Since(flight.started).Milliseconds()
	} else {
		stages["queue"] = flight.providerStarted.Sub(flight.started).Milliseconds()
		stages["provider"] = time.Since(flight.providerStarted).Milliseconds()
	}
	if !flight.admissionStarted.IsZero() {
		finished := flight.admissionFinished
		if finished.IsZero() {
			finished = time.Now()
		}
		stages["admission"] = finished.Sub(flight.admissionStarted).Milliseconds()
	}
	return ModelReviewResult{StageLatenciesMS: stages}
}

func (c *modelReviewController) runFlight(key string, flight *modelReviewFlight, request ModelReviewRequest) {
	result, err := c.executeFlight(key, flight, request)
	c.flightsMu.Lock()
	flight.result, flight.err = result, err
	delete(c.flights, key)
	close(flight.done)
	flight.cancel()
	c.flightsMu.Unlock()
}

func (c *modelReviewController) executeFlight(key string, flight *modelReviewFlight, request ModelReviewRequest) (ModelReviewResult, error) {
	queueTimer := time.NewTimer(time.Duration(c.cfg.QueueTimeoutMilliseconds) * time.Millisecond)
	defer queueTimer.Stop()
	// Prefer an immediately free slot even with an intentionally zero queue wait.
	select {
	case c.semaphore <- struct{}{}:
	default:
		select {
		case c.semaphore <- struct{}{}:
		case <-queueTimer.C:
			return c.flightTimings(flight), errModelReviewSaturated
		case <-flight.ctx.Done():
			return c.flightTimings(flight), flight.ctx.Err()
		}
	}
	defer func() { <-c.semaphore }()
	if err := flight.ctx.Err(); err != nil {
		return c.flightTimings(flight), err
	}
	request = compactModelReviewRequest(request, c.cfg.MaxInputBytes)
	if err := flight.ctx.Err(); err != nil {
		return c.flightTimings(flight), err
	}
	if request.ContextIncomplete {
		// Incomplete context cannot yield a usable decision, so avoid consuming
		// supplier quota merely to downgrade the response unconditionally.
		return c.flightTimings(flight), errModelReviewContextIncomplete
	}
	if !c.beginCircuitAttempt() {
		return c.flightTimings(flight), errModelReviewCircuitOpen
	}
	if err := flight.ctx.Err(); err != nil {
		c.recordCircuitCanceled()
		return c.flightTimings(flight), err
	}
	if c.admit != nil {
		c.flightsMu.Lock()
		flight.admissionStarted = time.Now()
		c.flightsMu.Unlock()
		approved, reason, err := c.admit(flight.ctx)
		c.flightsMu.Lock()
		flight.admissionFinished = time.Now()
		c.flightsMu.Unlock()
		if ctxErr := flight.ctx.Err(); ctxErr != nil {
			c.recordCircuitCanceled()
			return c.flightTimings(flight), ctxErr
		}
		if err != nil || !approved {
			c.recordCircuitCanceled()
			failure := errModelReviewBudgetStorage
			if err == nil {
				switch reason {
				case "daily_budget_exhausted":
					failure = errModelReviewDailyBudget
				case "minute_rate_limited":
					failure = errModelReviewMinuteLimit
				}
			}
			return c.flightTimings(flight), failure
		}
	}
	// Local identity and version partitioning must never be sent to a provider.
	request.TenantScope, request.PolicyVersion = "", ""
	c.flightsMu.Lock()
	flight.providerStarted = time.Now()
	c.flightsMu.Unlock()
	result, err := c.reviewer.Review(flight.ctx, request)
	result = cloneModelReviewResult(result)
	if err != nil {
		var trace interface{ AuditReviewStageLatenciesMS() map[string]int64 }
		if errors.As(err, &trace) {
			result.StageLatenciesMS = cloneModelReviewStages(trace.AuditReviewStageLatenciesMS())
		}
	}
	if result.StageLatenciesMS == nil {
		result.StageLatenciesMS = make(map[string]int64)
	}
	for stage, millis := range c.flightTimings(flight).StageLatenciesMS {
		result.StageLatenciesMS[stage] = millis
	}
	if ctxErr := flight.ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	if err == nil {
		result = normalizeModelReviewResult(result)
		if result.Decision == "" {
			err = errModelReviewInvalidResult
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.recordCircuitCanceled()
		} else {
			c.recordCircuitFailure()
		}
		return result, err
	}
	c.recordCircuitSuccess()
	c.storeCache(key, result)
	return result, nil
}

func makeRuleSelection(rules []string) map[string]struct{} {
	selected := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if rule = strings.TrimSpace(rule); rule != "" {
			selected[rule] = struct{}{}
		}
	}
	return selected
}

func (c *modelReviewController) beginCircuitAttempt() bool {
	c.circuitMu.Lock()
	defer c.circuitMu.Unlock()
	now := time.Now()
	if now.Before(c.openUntil) {
		return false
	}
	if !c.openUntil.IsZero() {
		if c.halfOpen {
			return false
		}
		c.halfOpen = true
	}
	return true
}

func (c *modelReviewController) recordCircuitSuccess() {
	c.circuitMu.Lock()
	defer c.circuitMu.Unlock()
	c.failures = 0
	c.openUntil = time.Time{}
	c.halfOpen = false
}

func (c *modelReviewController) recordCircuitFailure() {
	c.circuitMu.Lock()
	defer c.circuitMu.Unlock()
	c.halfOpen = false
	c.failures++
	if c.failures >= c.cfg.CircuitFailureThreshold {
		c.openUntil = time.Now().Add(time.Duration(c.cfg.CircuitOpenSeconds) * time.Second)
	}
}

func (c *modelReviewController) recordCircuitCanceled() {
	c.circuitMu.Lock()
	defer c.circuitMu.Unlock()
	c.halfOpen = false
}

func normalizeModelReviewResult(result ModelReviewResult) ModelReviewResult {
	result = cloneModelReviewResult(result)
	result.Decision = strings.ToLower(strings.TrimSpace(result.Decision))
	switch result.Decision {
	case ModelReviewBlock, ModelReviewAllow, ModelReviewUncertain:
	default:
		result.Decision = ""
	}
	result.Category = strings.ToLower(strings.TrimSpace(result.Category))
	switch result.Category {
	case "", "jailbreak", "csam", "weapons", "extremism", "drugs", "criminal", "fraud", "cyber", "piracy", "gambling", "sexual", "self_harm", "violence", "none", "unknown":
	default:
		result.Decision = ""
	}
	if result.Decision == ModelReviewBlock && (result.Category == "" || result.Category == "none" || result.Category == "unknown") {
		result.Decision = ""
	}
	if math.IsNaN(result.Confidence) || math.IsInf(result.Confidence, 0) || result.Confidence < 0 || result.Confidence > 1 {
		result.Decision = ""
	}
	if len(result.ReasonCodes) > 8 {
		result.Decision = ""
		result.ReasonCodes = nil
	}
	for index := range result.ReasonCodes {
		result.ReasonCodes[index] = strings.ToUpper(strings.TrimSpace(result.ReasonCodes[index]))
		code := result.ReasonCodes[index]
		if len(code) == 0 || len(code) > 64 {
			result.Decision = ""
			break
		}
		for _, character := range code {
			if !(character >= 'A' && character <= 'Z') && !(character >= '0' && character <= '9') && character != '_' {
				result.Decision = ""
				break
			}
		}
	}
	return result
}

func (c *modelReviewController) fingerprint(request ModelReviewRequest) string {
	mac := hmac.New(sha256.New, c.cacheKey[:])
	var size [8]byte
	for _, value := range []string{request.TenantScope, request.PolicyVersion, request.PromptVersion, request.Model, request.RuleID, request.Category, request.Severity, request.MatchedTerm, request.Text, request.ReferenceText} {
		// Length prefixes prevent delimiter injection from merging distinct fields.
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(value))
	}
	if request.ContextIncomplete {
		_, _ = mac.Write([]byte{1})
	} else {
		_, _ = mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *modelReviewController) cached(key string) (ModelReviewResult, bool) {
	if c.cfg.CacheSeconds <= 0 {
		return ModelReviewResult{}, false
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	entry, ok := c.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(c.cache, key)
		return ModelReviewResult{}, false
	}
	return cloneModelReviewResult(entry.result), true
}

func (c *modelReviewController) storeCache(key string, result ModelReviewResult) {
	if c.cfg.CacheSeconds <= 0 {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if len(c.cache) >= 10_000 {
		now := time.Now()
		for existingKey, entry := range c.cache {
			if now.After(entry.expiresAt) {
				delete(c.cache, existingKey)
			}
		}
		if len(c.cache) >= 10_000 {
			for existingKey := range c.cache {
				delete(c.cache, existingKey)
				break
			}
		}
	}
	c.cache[key] = modelReviewCacheEntry{result: cloneModelReviewResult(result), expiresAt: time.Now().Add(time.Duration(c.cfg.CacheSeconds) * time.Second)}
}

func cloneModelReviewResult(result ModelReviewResult) ModelReviewResult {
	result.ReasonCodes = append([]string(nil), result.ReasonCodes...)
	result.StageLatenciesMS = cloneModelReviewStages(result.StageLatenciesMS)
	return result
}

func cloneModelReviewStages(stages map[string]int64) map[string]int64 {
	if stages == nil {
		return nil
	}
	cloned := make(map[string]int64, 10)
	for _, name := range []string{"queue", "admission", "provider", "total", "auth_select", "connect", "request_write", "ttfb", "transport", "read", "parse"} {
		if value, ok := stages[name]; ok && value >= 0 {
			cloned[name] = value
		}
	}
	return cloned
}

func compactModelReviewRequest(request ModelReviewRequest, maxBytes int) ModelReviewRequest {
	if maxBytes <= 0 || len(request.Text)+len(request.ReferenceText) <= maxBytes {
		return request
	}
	request.ContextIncomplete = true
	currentBudget := maxBytes
	if request.ReferenceText != "" {
		currentBudget = max(1, maxBytes*3/4)
	}
	request.Text = compactReviewText(request.Text, request.MatchedTerm, currentBudget)
	referenceBudget := maxBytes - len(request.Text)
	if referenceBudget > 0 {
		request.ReferenceText = compactReviewText(request.ReferenceText, request.MatchedTerm, referenceBudget)
	} else {
		request.ReferenceText = ""
	}
	return request
}

func truncateReviewText(text string, maxBytes int) string {
	text = strings.TrimSpace(text)
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	text = text[:maxBytes]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func compactReviewText(text, matchedTerm string, maxBytes int) string {
	text = strings.TrimSpace(text)
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	term := strings.TrimSpace(matchedTerm)
	if term == "" {
		return truncateReviewText(text, maxBytes)
	}
	matchIndex := findReviewMatchIndex(text, term)
	if matchIndex < 0 {
		return truncateReviewText(text, maxBytes)
	}
	const marker = "[...content omitted...]\n"
	available := maxBytes - len(marker)*2
	if available <= 0 {
		return truncateReviewText(text, maxBytes)
	}
	beforeBytes := available / 3
	start := max(0, matchIndex-beforeBytes)
	end := min(len(text), start+available)
	if end == len(text) {
		start = max(0, end-available)
	}
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	if end < len(text) {
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
	}
	var builder strings.Builder
	if start > 0 {
		builder.WriteString(marker)
	}
	builder.WriteString(text[start:end])
	if end < len(text) {
		builder.WriteByte('\n')
		builder.WriteString(strings.TrimSpace(marker))
	}
	return truncateReviewText(builder.String(), maxBytes)
}

func findReviewMatchIndex(text, term string) int {
	if index := strings.Index(strings.ToLower(text), strings.ToLower(term)); index >= 0 {
		return index
	}
	needle := []rune(moderationCandidateText(term))
	if len(needle) == 0 {
		return -1
	}
	failure := make([]int, len(needle))
	for index, matched := 1, 0; index < len(needle); index++ {
		for matched > 0 && needle[index] != needle[matched] {
			matched = failure[matched-1]
		}
		if needle[index] == needle[matched] {
			matched++
		}
		failure[index] = matched
	}
	positions := make([]int, len(needle))
	matched := 0
	processed := 0
	for byteIndex, character := range text {
		if isModerationInvisible(character) || (!unicode.IsLetter(character) && !unicode.IsNumber(character)) {
			continue
		}
		character = unicode.ToLower(character)
		positions[processed%len(positions)] = byteIndex
		processed++
		for matched > 0 && character != needle[matched] {
			matched = failure[matched-1]
		}
		if character == needle[matched] {
			matched++
		}
		if matched == len(needle) {
			return positions[(processed-len(needle))%len(positions)]
		}
	}
	return -1
}

func modelReviewFallbackReason(err error) string {
	switch {
	case errors.Is(err, errModelReviewSaturated):
		return "saturated"
	case errors.Is(err, errModelReviewCircuitOpen):
		return "circuit_open"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, errModelReviewInvalidResult):
		return "invalid_result"
	case errors.Is(err, errModelReviewContextIncomplete):
		return "context_incomplete"
	case errors.Is(err, errModelReviewDailyBudget):
		return "daily_budget_exhausted"
	case errors.Is(err, errModelReviewMinuteLimit):
		return "minute_rate_limited"
	case errors.Is(err, errModelReviewBudgetStorage):
		return "budget_storage_error"
	}
	var failure interface{ AuditReviewFailureCode() string }
	if errors.As(err, &failure) {
		switch code := failure.AuditReviewFailureCode(); code {
		case "review_upstream_http_3xx":
			return code
		case "review_upstream_http_400", "review_upstream_http_401", "review_upstream_http_403", "review_upstream_http_404", "review_upstream_http_408", "review_upstream_http_429", "review_upstream_http_5xx", "review_upstream_http_other", "review_upstream_error", "review_transport_error", "review_response_empty", "review_response_refusal", "review_response_incomplete", "review_response_schema_invalid", "review_response_json_invalid", "review_response_too_large", "review_response_read_error", "review_auth_unavailable", "review_request_invalid", "review_route_invalid":
			return code
		}
	}
	return "review_error"
}
