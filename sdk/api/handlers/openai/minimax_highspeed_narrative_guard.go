package openai

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	miniMaxHighspeedNarrativeModel = "MiniMax-M2.7-highspeed"

	defaultMiniMaxHighspeedNarrativeMaxConcurrent     = 2
	defaultMiniMaxHighspeedNarrativeMaxOutputTokens   = 4096
	defaultMiniMaxHighspeedNarrativeRetryAfterSeconds = 30

	miniMaxHighspeedNarrativeMinBodyBytes      = 100 * 1024
	miniMaxHighspeedNarrativeMinOutputTokens   = 10000
	miniMaxHighspeedNarrativeMinStructuralHits = 2
	miniMaxHighspeedNarrativeMinNarrativeHits  = 2
	miniMaxHighspeedNarrativeMinTotalHits      = 5
)

var defaultMiniMaxHighspeedNarrativeLimiter = &miniMaxHighspeedNarrativeLimiter{}

var miniMaxHighspeedNarrativeStructuralMarkers = []string{
	"lastRules",
	"FICTIONAL CONTEXT LOCK",
	"OUTPUT FORMAT",
	"statebar",
	"plot_choices",
	"content_key_summary",
	"Mandatory language requirement",
}

var miniMaxHighspeedNarrativeMarkers = []string{
	"<scenario>",
	"<narration>",
	"<action>",
	"<message>",
	"novel-style storytelling",
	"roleplay",
	"relationship_identity",
	"story_phase_goal",
	"output_tone_constraint",
	"censy",
}

type miniMaxHighspeedNarrativeGuardSettings struct {
	enabled           bool
	maxConcurrent     int
	maxOutputTokens   int
	retryAfterSeconds int
}

type miniMaxHighspeedNarrativeMatch struct {
	matched        bool
	bodyBytes      int
	maxTokens      int64
	structuralHits int
	narrativeHits  int
}

type miniMaxHighspeedNarrativeGuardDecision struct {
	rawJSON           []byte
	release           func()
	blocked           bool
	retryAfterSeconds int
	bodyBytes         int
	maxTokens         int64
	structuralHits    int
	narrativeHits     int
	active            int
	maxConcurrent     int
	cappedOutput      bool
}

type miniMaxHighspeedNarrativeLimiter struct {
	mu     sync.Mutex
	active int
}

func (l *miniMaxHighspeedNarrativeLimiter) acquire(maxConcurrent int) (func(), int, bool) {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMiniMaxHighspeedNarrativeMaxConcurrent
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= maxConcurrent {
		return nil, l.active, false
	}
	l.active++
	active := l.active
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.active > 0 {
				l.active--
			}
		})
	}, active, true
}

func (h *OpenAIAPIHandler) prepareMiniMaxHighspeedNarrativeGuard(rawJSON []byte) miniMaxHighspeedNarrativeGuardDecision {
	return prepareMiniMaxHighspeedNarrativeGuard(rawJSON, h.Cfg, defaultMiniMaxHighspeedNarrativeLimiter)
}

func prepareMiniMaxHighspeedNarrativeGuard(rawJSON []byte, cfg *sdkconfig.SDKConfig, limiter *miniMaxHighspeedNarrativeLimiter) miniMaxHighspeedNarrativeGuardDecision {
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
	decision.retryAfterSeconds = settings.retryAfterSeconds
	decision.maxConcurrent = settings.maxConcurrent

	updated, capped := capMiniMaxHighspeedNarrativeOutput(rawJSON, settings.maxOutputTokens)
	decision.rawJSON = updated
	decision.cappedOutput = capped

	release, active, ok := limiter.acquire(settings.maxConcurrent)
	decision.active = active
	if !ok {
		decision.blocked = true
		log.WithFields(log.Fields{
			"model":             miniMaxHighspeedNarrativeModel,
			"body_bytes":        decision.bodyBytes,
			"max_tokens":        decision.maxTokens,
			"structural_hits":   decision.structuralHits,
			"narrative_hits":    decision.narrativeHits,
			"active":            decision.active,
			"max_concurrent":    decision.maxConcurrent,
			"retry_after_secs":  decision.retryAfterSeconds,
			"max_output_tokens": settings.maxOutputTokens,
		}).Warn("minimax highspeed narrative guard rejected request")
		return decision
	}
	decision.release = release
	log.WithFields(log.Fields{
		"model":             miniMaxHighspeedNarrativeModel,
		"body_bytes":        decision.bodyBytes,
		"max_tokens":        decision.maxTokens,
		"structural_hits":   decision.structuralHits,
		"narrative_hits":    decision.narrativeHits,
		"active":            decision.active,
		"max_concurrent":    decision.maxConcurrent,
		"capped_output":     decision.cappedOutput,
		"max_output_tokens": settings.maxOutputTokens,
	}).Info("minimax highspeed narrative guard admitted request")
	return decision
}

func miniMaxHighspeedNarrativeSettings(cfg *sdkconfig.SDKConfig) miniMaxHighspeedNarrativeGuardSettings {
	settings := miniMaxHighspeedNarrativeGuardSettings{
		maxConcurrent:     defaultMiniMaxHighspeedNarrativeMaxConcurrent,
		maxOutputTokens:   defaultMiniMaxHighspeedNarrativeMaxOutputTokens,
		retryAfterSeconds: defaultMiniMaxHighspeedNarrativeRetryAfterSeconds,
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
	if guard.RetryAfterSeconds > 0 {
		settings.retryAfterSeconds = guard.RetryAfterSeconds
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
	body := string(rawJSON)
	match.structuralHits = countContainedMarkers(body, miniMaxHighspeedNarrativeStructuralMarkers)
	match.narrativeHits = countContainedMarkers(body, miniMaxHighspeedNarrativeMarkers)
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

func countContainedMarkers(body string, markers []string) int {
	count := 0
	for _, marker := range markers {
		if strings.Contains(body, marker) {
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

func writeMiniMaxHighspeedNarrativeGuardRateLimit(c *gin.Context, decision miniMaxHighspeedNarrativeGuardDecision) {
	if decision.retryAfterSeconds <= 0 {
		decision.retryAfterSeconds = defaultMiniMaxHighspeedNarrativeRetryAfterSeconds
	}
	c.Header("Retry-After", strconv.Itoa(decision.retryAfterSeconds))
	c.JSON(http.StatusTooManyRequests, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: "Heavy MiniMax-M2.7-highspeed narrative workloads are temporarily limited. Please retry later or reduce max_tokens/context size.",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
		},
	})
}
