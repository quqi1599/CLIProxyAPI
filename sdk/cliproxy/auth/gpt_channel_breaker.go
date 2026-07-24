package auth

import (
	"net/http"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const (
	codexChannelBreakerWindow          = 30 * time.Second
	codexChannelBreakerSampleLimit     = 20
	codexChannelBreakerMinimumSamples  = 10
	codexChannelBreakerFailurePercent  = 80
	codexChannelBreakerOpen5xxFailures = 3
	codexChannelBreakerMaxBackoffLevel = 3
)

type codexChannelOutcome struct {
	atUnixNano int64
	failed     bool
}

type codexChannelBreakerState struct {
	Health          HealthState
	BackoffLevel    uint8
	ProbeRequestID  string
	ProbeLeaseUntil time.Time

	consecutive5xx uint8
	recent         [codexChannelBreakerSampleLimit]codexChannelOutcome
	recentNext     uint8
	recentCount    uint8
}

func applyCodexChannelBreakerResult(state *codexChannelBreakerState, result Result, now time.Time, requestID string) {
	if state == nil {
		return
	}

	switch state.Health.BreakerState {
	case HealthBreakerOpen:
		return
	case HealthBreakerHalfOpen:
		if state.ProbeRequestID == "" || requestID != state.ProbeRequestID {
			return
		}
		state.ProbeRequestID = ""
		state.ProbeLeaseUntil = time.Time{}
		if result.Success {
			applyHealthSuccess(&state.Health, now)
			if state.Health.BreakerState == HealthBreakerClosed {
				state.BackoffLevel = 0
				state.consecutive5xx = 0
				state.recentNext = 0
				state.recentCount = 0
				state.recent = [codexChannelBreakerSampleLimit]codexChannelOutcome{}
			}
			return
		}
		if !shouldCountCodexChannelBreakerFailure(result) {
			return
		}
		applyHealthFailure(&state.Health, now, statusCodeFromResult(result.Error))
		openCodexChannelBreaker(state, now)
		return
	}

	if result.Success {
		recordCodexChannelOutcome(state, now, false)
		state.consecutive5xx = 0
		applyHealthSuccess(&state.Health, now)
		return
	}
	if !shouldCountCodexChannelBreakerFailure(result) {
		return
	}

	statusCode := statusCodeFromResult(result.Error)
	recordCodexChannelOutcome(state, now, true)
	if statusCode >= http.StatusInternalServerError && statusCode < 600 {
		if state.consecutive5xx < ^uint8(0) {
			state.consecutive5xx++
		}
	} else {
		state.consecutive5xx = 0
	}
	applyHealthFailure(&state.Health, now, statusCode)
	if state.consecutive5xx >= codexChannelBreakerOpen5xxFailures || codexChannelBreakerWindowExceeded(state, now) {
		openCodexChannelBreaker(state, now)
		return
	}
	state.Health.BreakerState = HealthBreakerClosed
	state.Health.OpenUntil = time.Time{}
	state.Health.HalfOpenSuccesses = 0
}

func shouldCountCodexChannelBreakerFailure(result Result) bool {
	if scope, ok := failureScopeFromResult(result); ok && scope == failurecontract.ScopeCredential {
		return result.Error != nil &&
			result.Error.Retryable &&
			statusCodeFromResult(result.Error) == http.StatusTooManyRequests
	}
	return shouldCountChannelBreakerFailure(result)
}

func reserveCodexChannelProbe(state *codexChannelBreakerState, requestID string, now time.Time) bool {
	if state == nil {
		return true
	}
	switch state.Health.BreakerState {
	case HealthBreakerOpen:
		if state.Health.OpenUntil.After(now) || requestID == "" {
			return false
		}
		state.Health.BreakerState = HealthBreakerHalfOpen
		state.Health.OpenUntil = time.Time{}
	case HealthBreakerHalfOpen:
		if requestID == "" {
			return false
		}
	default:
		return true
	}
	if state.ProbeRequestID != "" {
		if state.ProbeLeaseUntil.After(now) {
			return false
		}
		state.ProbeRequestID = ""
	}
	state.ProbeRequestID = requestID
	state.ProbeLeaseUntil = now.Add(gptChannelProbeLease)
	return true
}

func releaseCodexChannelProbe(state *codexChannelBreakerState, requestID string) {
	if state == nil || requestID == "" || state.ProbeRequestID != requestID {
		return
	}
	state.ProbeRequestID = ""
	state.ProbeLeaseUntil = time.Time{}
}

func recordCodexChannelOutcome(state *codexChannelBreakerState, now time.Time, failed bool) {
	index := int(state.recentNext) % len(state.recent)
	state.recent[index] = codexChannelOutcome{
		atUnixNano: now.UnixNano(),
		failed:     failed,
	}
	state.recentNext = uint8((index + 1) % len(state.recent))
	if state.recentCount < uint8(len(state.recent)) {
		state.recentCount++
	}
}

func codexChannelBreakerWindowExceeded(state *codexChannelBreakerState, now time.Time) bool {
	if state == nil {
		return false
	}
	cutoff := now.Add(-codexChannelBreakerWindow).UnixNano()
	total := 0
	failures := 0
	for i := 0; i < int(state.recentCount); i++ {
		outcome := state.recent[i]
		if outcome.atUnixNano < cutoff {
			continue
		}
		total++
		if outcome.failed {
			failures++
		}
	}
	return total >= codexChannelBreakerMinimumSamples &&
		failures*100 >= total*codexChannelBreakerFailurePercent
}

func openCodexChannelBreaker(state *codexChannelBreakerState, now time.Time) {
	if state.BackoffLevel < codexChannelBreakerMaxBackoffLevel {
		state.BackoffLevel++
	}
	state.Health.BreakerState = HealthBreakerOpen
	state.Health.OpenUntil = now.Add(codexChannelBreakerCooldown(state.BackoffLevel))
	state.Health.HalfOpenSuccesses = 0
	state.ProbeRequestID = ""
	state.ProbeLeaseUntil = time.Time{}
}

func codexChannelBreakerCooldown(level uint8) time.Duration {
	switch level {
	case 1:
		return 30 * time.Second
	case 2:
		return 60 * time.Second
	default:
		return 120 * time.Second
	}
}
