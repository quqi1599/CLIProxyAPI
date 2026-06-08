package openai

import (
	"bytes"
	"context"
	"sync"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	miniMaxHighspeedNarrativeModel = "MiniMax-M2.7-highspeed"

	defaultMiniMaxHighspeedNarrativeMaxConcurrent   = 2
	defaultMiniMaxHighspeedNarrativeMaxOutputTokens = 4096

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
	bodyBytes      int
	maxTokens      int64
	structuralHits int
	narrativeHits  int
	active         int
	queued         int
	maxConcurrent  int
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

func (l *miniMaxHighspeedNarrativeLimiter) acquire(ctx context.Context, maxConcurrent int) (func(), int, int, error) {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMiniMaxHighspeedNarrativeMaxConcurrent
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	l.mu.Lock()
	if l.active < maxConcurrent && len(l.waiters) == 0 {
		l.active++
		active := l.active
		l.mu.Unlock()
		return l.releaseFunc(maxConcurrent), active, 0, nil
	}

	waiter := &miniMaxHighspeedNarrativeWaiter{ready: make(chan struct{})}
	l.waiters = append(l.waiters, waiter)
	queued := len(l.waiters)
	l.mu.Unlock()

	select {
	case <-waiter.ready:
		release := l.releaseFunc(maxConcurrent)
		if err := ctx.Err(); err != nil {
			release()
			return nil, 0, queued, err
		}
		active := l.activeSnapshot()
		return release, active, queued, nil
	case <-ctx.Done():
		l.mu.Lock()
		removed := l.removeWaiterLocked(waiter)
		if !removed {
			l.releaseAssignedSlotLocked(maxConcurrent)
		}
		l.mu.Unlock()
		return nil, 0, queued, ctx.Err()
	}
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

	release, active, queued, err := limiter.acquire(ctx, settings.maxConcurrent)
	decision.active = active
	decision.queued = queued
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
	updated := rawJSON
	capped := false
	for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		tokenValue := gjson.GetBytes(updated, path)
		if !tokenValue.Exists() || tokenValue.Type != gjson.Number || tokenValue.Int() <= int64(maxOutputTokens) {
			continue
		}
		next, err := sjson.SetBytes(updated, path, maxOutputTokens)
		if err != nil {
			log.WithError(err).WithField("path", path).Warn("minimax highspeed narrative guard failed to cap output tokens")
			continue
		}
		updated = next
		capped = true
	}
	return updated, capped
}
