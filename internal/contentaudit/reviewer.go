package contentaudit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"golang.org/x/sync/singleflight"
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

// ModelReviewRequest contains only the current user enforcement scope and non-sensitive policy metadata.
type ModelReviewRequest struct {
	Model         string
	PromptVersion string
	Text          string
	RuleID        string
	Category      string
	Severity      string
}

// ModelReviewResult is a bounded, non-sensitive semantic classification.
type ModelReviewResult struct {
	Decision    string   `json:"decision"`
	Category    string   `json:"category,omitempty"`
	Confidence  float64  `json:"confidence"`
	ReasonCodes []string `json:"reason_codes,omitempty"`
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

type modelReviewController struct {
	cfg       config.ContentAuditModelReviewConfig
	reviewer  Reviewer
	semaphore chan struct{}
	cacheKey  [32]byte
	cacheMu   sync.Mutex
	cache     map[string]modelReviewCacheEntry
	group     singleflight.Group
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
	controller := &modelReviewController{
		cfg:       cfg,
		reviewer:  reviewer,
		semaphore: make(chan struct{}, cfg.MaxConcurrent),
		cache:     make(map[string]modelReviewCacheEntry),
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
	request.Model = c.cfg.Model
	request.PromptVersion = c.cfg.PromptVersion
	request.Text = truncateReviewText(request.Text, c.cfg.MaxInputBytes)
	cacheKey := c.fingerprint(request)
	if result, ok := c.cached(cacheKey); ok {
		return modelReviewOutcome{ModelReviewResult: result, Model: request.Model, CacheHit: true, Reviewed: true}
	}

	started := time.Now()
	value, err, _ := c.group.Do(cacheKey, func() (any, error) {
		if result, ok := c.cached(cacheKey); ok {
			return result, nil
		}
		queueTimer := time.NewTimer(time.Duration(c.cfg.QueueTimeoutMilliseconds) * time.Millisecond)
		defer queueTimer.Stop()
		select {
		case c.semaphore <- struct{}{}:
			defer func() { <-c.semaphore }()
		case <-queueTimer.C:
			return ModelReviewResult{}, errModelReviewSaturated
		case <-ctx.Done():
			return ModelReviewResult{}, ctx.Err()
		}
		if !c.beginCircuitAttempt() {
			return ModelReviewResult{}, errModelReviewCircuitOpen
		}

		reviewCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.TimeoutMilliseconds)*time.Millisecond)
		defer cancel()
		result, errReview := c.reviewer.Review(reviewCtx, request)
		if errReview != nil {
			c.recordCircuitFailure()
			return ModelReviewResult{}, errReview
		}
		result = normalizeModelReviewResult(result)
		if result.Decision == "" {
			c.recordCircuitFailure()
			return ModelReviewResult{}, fmt.Errorf("content audit model review returned an invalid decision")
		}
		c.recordCircuitSuccess()
		c.storeCache(cacheKey, result)
		return result, nil
	})
	latency := time.Since(started)
	if err != nil {
		return modelReviewOutcome{
			ModelReviewResult: ModelReviewResult{Decision: ModelReviewUncertain},
			Model:             request.Model,
			Latency:           latency,
			Fallback:          modelReviewFallbackReason(err),
			Reviewed:          true,
		}
	}
	result, ok := value.(ModelReviewResult)
	if !ok {
		return modelReviewOutcome{
			ModelReviewResult: ModelReviewResult{Decision: ModelReviewUncertain},
			Model:             request.Model,
			Latency:           latency,
			Fallback:          "invalid_result",
			Reviewed:          true,
		}
	}
	return modelReviewOutcome{ModelReviewResult: result, Model: request.Model, Latency: latency, Reviewed: true}
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

func normalizeModelReviewResult(result ModelReviewResult) ModelReviewResult {
	result.Decision = strings.ToLower(strings.TrimSpace(result.Decision))
	switch result.Decision {
	case ModelReviewBlock, ModelReviewAllow, ModelReviewUncertain:
	default:
		result.Decision = ""
	}
	result.Category = strings.ToLower(strings.TrimSpace(result.Category))
	if result.Confidence < 0 {
		result.Confidence = 0
	} else if result.Confidence > 1 {
		result.Confidence = 1
	}
	if len(result.ReasonCodes) > 8 {
		result.ReasonCodes = result.ReasonCodes[:8]
	}
	for index := range result.ReasonCodes {
		result.ReasonCodes[index] = strings.ToUpper(strings.TrimSpace(result.ReasonCodes[index]))
	}
	return result
}

func (c *modelReviewController) fingerprint(request ModelReviewRequest) string {
	mac := hmac.New(sha256.New, c.cacheKey[:])
	_, _ = mac.Write([]byte(request.PromptVersion))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(request.Model))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(request.RuleID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(request.Text))
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
	return entry.result, true
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
	c.cache[key] = modelReviewCacheEntry{result: result, expiresAt: time.Now().Add(time.Duration(c.cfg.CacheSeconds) * time.Second)}
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
	default:
		return "review_error"
	}
}
