package openai

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

const (
	miniMaxHighspeedNarrativeModel = "MiniMax-M2.7-highspeed"

	defaultMiniMaxHighspeedNarrativeMaxConcurrent   = 4
	defaultMiniMaxHighspeedNarrativeMaxOutputTokens = 8192

	miniMaxHighspeedNarrativeMinBodyBytes      = 100 * 1024
	miniMaxHighspeedNarrativeMinOutputTokens   = 10000
	miniMaxHighspeedNarrativeMinStructuralHits = 2
	miniMaxHighspeedNarrativeMinNarrativeHits  = 2
	miniMaxHighspeedNarrativeMinTotalHits      = 5
)

var defaultMiniMaxHighspeedNarrativeLimiter = &miniMaxHighspeedNarrativeLimiter{}

var miniMaxHighspeedNarrativeStructuralMarkers = [][]byte{
	[]byte("lastRules"),
	[]byte("FICTIONAL CONTEXT LOCK"),
	[]byte("OUTPUT FORMAT"),
	[]byte("statebar"),
	[]byte("plot_choices"),
	[]byte("content_key_summary"),
	[]byte("Mandatory language requirement"),
}

var miniMaxHighspeedNarrativeMarkers = [][]byte{
	[]byte("<scenario>"),
	[]byte("<narration>"),
	[]byte("<action>"),
	[]byte("<message>"),
	[]byte("novel-style storytelling"),
	[]byte("roleplay"),
	[]byte("relationship_identity"),
	[]byte("story_phase_goal"),
	[]byte("output_tone_constraint"),
	[]byte("censy"),
}

type miniMaxHighspeedNarrativeGuardSettings struct {
	enabled         bool
	maxConcurrent   int
	maxQueue        int
	maxWaitSeconds  int
	maxOutputTokens int
}

type miniMaxHighspeedNarrativeMatch struct {
	matched        bool
	bodyBytes      int
	maxTokens      int64
	structuralHits int
	narrativeHits  int
}

type miniMaxHighspeedNarrativeGuardDecision struct {
	rawJSON        []byte
	release        func()
	waitErr        error
	queueFull      bool
	waitTimedOut   bool
	bodyBytes      int
	maxTokens      int64
	structuralHits int
	narrativeHits  int
	active         int
	queued         int
	maxConcurrent  int
	maxQueue       int
	maxWaitSeconds int
	cappedOutput   bool
}

type miniMaxHighspeedNarrativeLimiter struct {
	mu      sync.Mutex
	active  int
	waiters []*miniMaxHighspeedNarrativeWaiter
}

type miniMaxHighspeedNarrativeWaiter struct {
	ready chan struct{}
}

func (l *miniMaxHighspeedNarrativeLimiter) acquire(ctx context.Context, maxConcurrent, maxQueue, maxWaitSeconds int) (func(), int, int, bool, bool, error) {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMiniMaxHighspeedNarrativeMaxConcurrent
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, false, false, err
	}
	l.mu.Lock()
	if l.active < maxConcurrent && len(l.waiters) == 0 {
		l.active++
		active := l.active
		l.mu.Unlock()
		return l.releaseFunc(maxConcurrent), active, 0, false, false, nil
	}
	if maxQueue > 0 && len(l.waiters) >= maxQueue {
		active := l.active
		queued := len(l.waiters)
		l.mu.Unlock()
		return nil, active, queued, true, false, nil
	}

	waiter := &miniMaxHighspeedNarrativeWaiter{ready: make(chan struct{})}
	l.waiters = append(l.waiters, waiter)
	queued := len(l.waiters)
	l.mu.Unlock()

	waitCtx := ctx
	cancelWait := func() {}
	if maxWaitSeconds > 0 {
		waitCtx, cancelWait = context.WithTimeout(ctx, time.Duration(maxWaitSeconds)*time.Second)
	}
	defer cancelWait()

	select {
	case <-waiter.ready:
		release := l.releaseFunc(maxConcurrent)
		if err := waitCtx.Err(); err != nil {
			release()
			return nil, 0, queued, false, miniMaxHighspeedNarrativeWaitTimedOut(ctx, err), err
		}
		active := l.activeSnapshot()
		return release, active, queued, false, false, nil
	case <-waitCtx.Done():
		l.mu.Lock()
		removed := l.removeWaiterLocked(waiter)
		if !removed {
			l.releaseAssignedSlotLocked(maxConcurrent)
		}
		l.mu.Unlock()
		err := waitCtx.Err()
		return nil, 0, queued, false, miniMaxHighspeedNarrativeWaitTimedOut(ctx, err), err
	}
}

func miniMaxHighspeedNarrativeWaitTimedOut(parent context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil
}

func (l *miniMaxHighspeedNarrativeLimiter) activeSnapshot() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

func (l *miniMaxHighspeedNarrativeLimiter) releaseFunc(maxConcurrent int) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.releaseAssignedSlotLocked(maxConcurrent)
		})
	}
}

func (l *miniMaxHighspeedNarrativeLimiter) releaseAssignedSlotLocked(maxConcurrent int) {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMiniMaxHighspeedNarrativeMaxConcurrent
	}
	if l.active > 0 {
		l.active--
	}
	for l.active < maxConcurrent && len(l.waiters) > 0 {
		next := l.waiters[0]
		copy(l.waiters, l.waiters[1:])
		l.waiters[len(l.waiters)-1] = nil
		l.waiters = l.waiters[:len(l.waiters)-1]
		l.active++
		close(next.ready)
	}
}

func (l *miniMaxHighspeedNarrativeLimiter) removeWaiterLocked(waiter *miniMaxHighspeedNarrativeWaiter) bool {
	for i, existing := range l.waiters {
		if existing != waiter {
			continue
		}
		copy(l.waiters[i:], l.waiters[i+1:])
		l.waiters[len(l.waiters)-1] = nil
		l.waiters = l.waiters[:len(l.waiters)-1]
		return true
	}
	return false
}

func (h *OpenAIAPIHandler) prepareMiniMaxHighspeedNarrativeGuard(ctx context.Context, rawJSON []byte) miniMaxHighspeedNarrativeGuardDecision {
	return prepareMiniMaxHighspeedNarrativeGuard(ctx, rawJSON, h.Cfg, defaultMiniMaxHighspeedNarrativeLimiter)
}

func prepareMiniMaxHighspeedNarrativeGuard(ctx context.Context, rawJSON []byte, cfg *sdkconfig.SDKConfig, limiter *miniMaxHighspeedNarrativeLimiter) miniMaxHighspeedNarrativeGuardDecision {
	decision := miniMaxHighspeedNarrativeGuardDecision{rawJSON: rawJSON}
	settings := miniMaxHighspeedNarrativeSettings(cfg)
	if !settings.enabled {
		return decision
	}
	match := matchMiniMaxHighspeedNarrative(rawJSON)
	if !match.matched {
		return decision
	}

	decision.bodyBytes = match.bodyBytes
	decision.maxTokens = match.maxTokens
	decision.structuralHits = match.structuralHits
	decision.narrativeHits = match.narrativeHits
	decision.maxConcurrent = settings.maxConcurrent
	decision.maxQueue = settings.maxQueue
	decision.maxWaitSeconds = settings.maxWaitSeconds

	release, active, queued, queueFull, waitTimedOut, err := limiter.acquire(ctx, settings.maxConcurrent, settings.maxQueue, settings.maxWaitSeconds)
	decision.active = active
	decision.queued = queued
	decision.queueFull = queueFull
	decision.waitTimedOut = waitTimedOut
	if err != nil {
		decision.waitErr = err
		return decision
	}

	updated, capped := capMiniMaxHighspeedNarrativeOutput(rawJSON, settings.maxOutputTokens)
	decision.rawJSON = updated
	decision.cappedOutput = capped
	decision.release = release
	return decision
}

func miniMaxHighspeedNarrativeSettings(cfg *sdkconfig.SDKConfig) miniMaxHighspeedNarrativeGuardSettings {
	settings := miniMaxHighspeedNarrativeGuardSettings{
		maxConcurrent:   defaultMiniMaxHighspeedNarrativeMaxConcurrent,
		maxOutputTokens: defaultMiniMaxHighspeedNarrativeMaxOutputTokens,
	}
	if cfg == nil {
		return settings
	}
	guard := cfg.RequestGuards.MiniMaxHighspeedNarrative
	settings.enabled = guard.Enabled
	if guard.MaxConcurrent > 0 {
		settings.maxConcurrent = guard.MaxConcurrent
	}
	if guard.MaxQueue > 0 {
		settings.maxQueue = guard.MaxQueue
	}
	if guard.MaxWaitSeconds > 0 {
		settings.maxWaitSeconds = guard.MaxWaitSeconds
	}
	if guard.MaxOutputTokens > 0 {
		settings.maxOutputTokens = guard.MaxOutputTokens
	}
	return settings
}

func matchMiniMaxHighspeedNarrative(rawJSON []byte) miniMaxHighspeedNarrativeMatch {
	match := miniMaxHighspeedNarrativeMatch{bodyBytes: len(rawJSON)}
	if gjson.GetBytes(rawJSON, "model").String() != miniMaxHighspeedNarrativeModel {
		return match
	}
	if len(rawJSON) < miniMaxHighspeedNarrativeMinBodyBytes {
		return match
	}
	match.maxTokens = maxRequestedOutputTokens(rawJSON)
	if match.maxTokens < miniMaxHighspeedNarrativeMinOutputTokens {
		return match
	}
	match.structuralHits = countContainedMarkers(rawJSON, miniMaxHighspeedNarrativeStructuralMarkers)
	match.narrativeHits = countContainedMarkers(rawJSON, miniMaxHighspeedNarrativeMarkers)
	totalHits := match.structuralHits + match.narrativeHits
	match.matched = match.structuralHits >= miniMaxHighspeedNarrativeMinStructuralHits &&
		match.narrativeHits >= miniMaxHighspeedNarrativeMinNarrativeHits &&
		totalHits >= miniMaxHighspeedNarrativeMinTotalHits
	return match
}

func maxRequestedOutputTokens(rawJSON []byte) int64 {
	var maxTokens int64
	for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		tokenValue := gjson.GetBytes(rawJSON, path)
		if !tokenValue.Exists() || tokenValue.Type != gjson.Number {
			continue
		}
		if value := tokenValue.Int(); value > maxTokens {
			maxTokens = value
		}
	}
	return maxTokens
}

func countContainedMarkers(body []byte, markers [][]byte) int {
	count := 0
	for _, marker := range markers {
		if bytes.Contains(body, marker) {
			count++
		}
	}
	return count
}

func capMiniMaxHighspeedNarrativeOutput(rawJSON []byte, maxOutputTokens int) ([]byte, bool) {
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultMiniMaxHighspeedNarrativeMaxOutputTokens
	}
	replacement := strconv.Itoa(maxOutputTokens)
	type edit struct {
		start int
		end   int
	}
	edits := make([]edit, 0, 3)
	for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		tokenValue := gjson.GetBytes(rawJSON, path)
		if !tokenValue.Exists() || tokenValue.Type != gjson.Number || tokenValue.Int() <= int64(maxOutputTokens) {
			continue
		}
		if tokenValue.Index < 0 || tokenValue.Index+len(tokenValue.Raw) > len(rawJSON) {
			continue
		}
		edits = append(edits, edit{start: tokenValue.Index, end: tokenValue.Index + len(tokenValue.Raw)})
	}
	if len(edits) == 0 {
		return rawJSON, false
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	size := len(rawJSON)
	for _, item := range edits {
		size += len(replacement) - (item.end - item.start)
	}
	updated := make([]byte, 0, size)
	cursor := 0
	for _, item := range edits {
		updated = append(updated, rawJSON[cursor:item.start]...)
		updated = append(updated, replacement...)
		cursor = item.end
	}
	updated = append(updated, rawJSON[cursor:]...)
	return updated, true
}

func writeMiniMaxHighspeedNarrativeGuardQueueFull(c *gin.Context, decision miniMaxHighspeedNarrativeGuardDecision) {
	c.JSON(http.StatusServiceUnavailable, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: "MiniMax-M2.7-highspeed narrative queue is temporarily busy. Please retry later or reduce max_tokens/context size.",
			Type:    "server_error",
			Code:    "temporarily_busy",
		},
	})
}
