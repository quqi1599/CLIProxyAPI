package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/provideridentity"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	internalusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// RequestAuthPreparer lets an executor update missing auth metadata immediately
// before a request. Manager serializes and persists returned updates.
type RequestAuthPreparer interface {
	ShouldPrepareRequestAuth(auth *Auth) bool
	PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error)
}

// RawEndpointExecutor executes provider-owned endpoints without protocol translation.
type RawEndpointExecutor interface {
	ExecuteRawEndpoint(ctx context.Context, auth *Auth, req cliproxyexecutor.RawEndpointRequest) (cliproxyexecutor.RawEndpointResponse, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

const (
	homeAuthCountMetadataKey = "__cliproxy_home_auth_count"
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
	// schedulerHotPathSyncMinInterval caps how often request-path pick failures
	// may trigger a full scheduler rebuild. Direct auth mutations still sync
	// immediately.
	schedulerHotPathSyncMinInterval = 100 * time.Millisecond
)

type requestAttemptTraceContextKey struct{}

type pendingSessionBinding struct {
	cache      *SessionCache
	key        string
	authID     string
	channelKey string
}

type selectorRouteSelection struct {
	selector Selector
	provider string
	model    string
	authID   string
}

type selectionAvailabilitySummary struct {
	total              int
	ready              int
	skippedDisabled    int
	skippedCooldown    int
	skippedBreaker     int
	skippedUnavailable int
	healthDownweighted int
}

type requestAttemptTrace struct {
	mu                           sync.Mutex
	requestID                    string
	attempts                     int
	fallbacks                    int
	maxAttempts                  int
	maxFallbacks                 int
	translatorRuns               int
	finalProvider                string
	finalModel                   string
	finalExecutor                string
	finalStatus                  int
	emptyResponses               int
	emptyRetries                 int
	emptyUpstreams               map[string]struct{}
	sessionBinding               pendingSessionBinding
	gptChannels                  map[string]struct{}
	failedChannels               map[string]struct{}
	gptModels                    map[string]string
	gptRound                     int
	gptThirdRound                map[string]struct{}
	gptRoute                     bool
	gptRouteSet                  bool
	selection                    selectorRouteSelection
	selectionReason              string
	selectionAvailability        selectionAvailabilitySummary
	gptFirstEventObserved        bool
	gptFirstEventTimeouts        int
	gptFirstEventWait            time.Duration
	gptFirstEventPolicySet       bool
	gptFirstEventPolicy          GPTFirstEventPolicySnapshot
	gptFirstEventBudgetExhausted bool
	gptRetryPressureState        string
	gptRetryPressureReason       string
	gptRetryPressureWait         time.Duration
	gptRetryPressurePermitLimit  int
	gptRetryPressureEligible     int
	gptRetryPressureDegraded     int
	gptRetryPressureThrottled    bool
	zeroEligibleProbeKey         string
}

type requestExecutionSummary struct {
	RequestID                    string
	AttemptCount                 int
	FallbackCount                int
	MaxAttempts                  int
	MaxFallbacks                 int
	TranslatorRuns               int
	FinalProvider                string
	FinalModel                   string
	FinalExecutor                string
	FinalStatus                  int
	GPTRoundCount                int
	EmptyResponses               int
	EmptyRetries                 int
	EmptyUpstreams               int
	GPTFirstEventTimeouts        int
	GPTFirstEventWait            time.Duration
	GPTFirstEventPolicy          GPTFirstEventPolicySnapshot
	GPTFirstEventBudgetExhausted bool
	GPTRetryPressureState        string
	GPTRetryPressureReason       string
	GPTRetryPressureWait         time.Duration
	GPTRetryPressurePermitLimit  int
	GPTRetryPressureEligible     int
	GPTRetryPressureDegraded     int
	GPTRetryPressureThrottled    bool
}

type routePlanSummary struct {
	RequestedModel               string `json:"requested_model,omitempty"`
	ResolvedModel                string `json:"resolved_model,omitempty"`
	UpstreamModel                string `json:"upstream_model,omitempty"`
	AuthIndex                    string `json:"auth_index,omitempty"`
	Provider                     string `json:"provider,omitempty"`
	Protocol                     string `json:"protocol,omitempty"`
	Executor                     string `json:"executor,omitempty"`
	UpstreamPath                 string `json:"upstream_path,omitempty"`
	Translator                   string `json:"translator,omitempty"`
	RoutingGroup                 string `json:"routing_group,omitempty"`
	FallbackFrom                 string `json:"fallback_from,omitempty"`
	FallbackReason               string `json:"fallback_reason,omitempty"`
	CompatKind                   string `json:"compat_kind,omitempty"`
	CompatKindSource             string `json:"compat_kind_source,omitempty"`
	CompatMapping                string `json:"compat_mapping,omitempty"`
	CompatBaseHost               string `json:"compat_base_host,omitempty"`
	ClientProfile                string `json:"client_profile,omitempty"`
	ContextHint                  string `json:"model_context_hint,omitempty"`
	EffortSource                 string `json:"reasoning_effort_source,omitempty"`
	EffortOriginal               string `json:"reasoning_effort_original,omitempty"`
	EffortNormalized             string `json:"reasoning_effort_normalized,omitempty"`
	CompactionIntent             string `json:"compaction_intent,omitempty"`
	CompactionTriggerMode        string `json:"compaction_trigger_mode,omitempty"`
	CompactionCompatibilityGroup string `json:"compaction_compatibility_group,omitempty"`
}

func ensureRequestAttemptTrace(ctx context.Context) (context.Context, *requestAttemptTrace) {
	if ctx == nil {
		ctx = context.Background()
	}
	if trace, ok := ctx.Value(requestAttemptTraceContextKey{}).(*requestAttemptTrace); ok && trace != nil {
		return ctx, trace
	}
	requestID := strings.TrimSpace(logging.GetRequestID(ctx))
	if requestID == "" {
		requestID = logging.GenerateRequestID()
		ctx = logging.WithRequestID(ctx, requestID)
	}
	trace := &requestAttemptTrace{requestID: requestID}
	return context.WithValue(ctx, requestAttemptTraceContextKey{}, trace), trace
}

func requestAttemptTraceFromContext(ctx context.Context) *requestAttemptTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(requestAttemptTraceContextKey{}).(*requestAttemptTrace)
	return trace
}

func (t *requestAttemptTrace) nextAttempt(retryReason string) coreusage.RequestAttempt {
	if t == nil {
		return coreusage.RequestAttempt{}
	}
	retryReason = strings.TrimSpace(retryReason)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempts++
	if strings.EqualFold(retryReason, emptyUpstreamResponseErrorCode) {
		t.emptyRetries++
	}
	return coreusage.RequestAttempt{
		RequestID:   t.requestID,
		AttemptNo:   t.attempts,
		RetryReason: retryReason,
	}
}

func (t *requestAttemptTrace) recordEmptyResponse(upstream string) {
	if t == nil {
		return
	}
	upstream = strings.TrimSpace(upstream)
	t.mu.Lock()
	t.emptyResponses++
	if upstream != "" {
		if t.emptyUpstreams == nil {
			t.emptyUpstreams = make(map[string]struct{})
		}
		t.emptyUpstreams[upstream] = struct{}{}
	}
	t.mu.Unlock()
}

func (t *requestAttemptTrace) recordEmptyResponseRetry() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.emptyRetries++
	t.mu.Unlock()
}

func (t *requestAttemptTrace) attemptCount() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attempts
}

func (t *requestAttemptTrace) requestIDValue() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requestID
}

func (t *requestAttemptTrace) swapZeroEligibleProbeKey(key string) string {
	if t == nil {
		return ""
	}
	key = strings.TrimSpace(key)
	t.mu.Lock()
	previous := t.zeroEligibleProbeKey
	t.zeroEligibleProbeKey = key
	t.mu.Unlock()
	return previous
}

func (t *requestAttemptTrace) zeroEligibleProbeKeyValue() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.zeroEligibleProbeKey
}

func (t *requestAttemptTrace) recordFallback() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.fallbacks++
	t.mu.Unlock()
}

func (t *requestAttemptTrace) claimGPTFirstEventObservation() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gptFirstEventObserved {
		return false
	}
	t.gptFirstEventObserved = true
	return true
}

func (t *requestAttemptTrace) recordGPTFirstEventAttempt(wait time.Duration, timedOut bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if wait > 0 {
		t.gptFirstEventWait += wait
	}
	if timedOut {
		t.gptFirstEventTimeouts++
	}
	t.mu.Unlock()
}

func (t *requestAttemptTrace) configureGPTFirstEventPolicy(snapshot GPTFirstEventPolicySnapshot) GPTFirstEventPolicySnapshot {
	if t == nil {
		return snapshot
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.gptFirstEventPolicySet {
		t.gptFirstEventPolicy = snapshot
		t.gptFirstEventPolicySet = true
	}
	return t.gptFirstEventPolicy
}

func (t *requestAttemptTrace) gptFirstEventPolicyValue() (GPTFirstEventPolicySnapshot, bool) {
	if t == nil {
		return GPTFirstEventPolicySnapshot{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gptFirstEventPolicy, t.gptFirstEventPolicySet
}

func (t *requestAttemptTrace) gptFirstEventRetryLimits() (maxChannels, maxRounds int) {
	policy, configured := t.gptFirstEventPolicyValue()
	if !configured {
		maxChannels, maxRounds = gptImmediateFailoverMaxChannels, gptImmediateFailoverMaxRounds
	} else {
		if policy.MaxChannels <= 0 {
			policy.MaxChannels = gptImmediateFailoverMaxChannels
		}
		if policy.MaxRounds <= 0 {
			policy.MaxRounds = gptImmediateFailoverMaxRounds
		}
		maxChannels, maxRounds = policy.MaxChannels, policy.MaxRounds
	}
	if t != nil {
		t.mu.Lock()
		pressureEvaluated := t.gptRetryPressureState != ""
		t.mu.Unlock()
		if pressureEvaluated && maxRounds > 2 {
			maxRounds = 2
		}
	}
	return maxChannels, maxRounds
}

func (t *requestAttemptTrace) gptFirstEventRemainingBudget() (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.gptFirstEventPolicySet || t.gptFirstEventPolicy.WaitBudgetMs <= 0 {
		return 0, false
	}
	remaining := time.Duration(t.gptFirstEventPolicy.WaitBudgetMs)*time.Millisecond - t.gptFirstEventWait
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

func (t *requestAttemptTrace) markGPTFirstEventBudgetExhausted() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.gptFirstEventBudgetExhausted = true
	t.mu.Unlock()
}

func (t *requestAttemptTrace) recordGPTRetryPressure(snapshot gptRetryPressureSnapshot, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.gptRetryPressureState = snapshot.State
	t.gptRetryPressureReason = snapshot.Reason
	t.gptRetryPressureWait += snapshot.Wait
	t.gptRetryPressurePermitLimit = snapshot.PermitLimit
	t.gptRetryPressureEligible = snapshot.EligibleRoutes
	t.gptRetryPressureDegraded = snapshot.DegradedRoutes
	if err != nil || snapshot.Rejected || snapshot.State == gptRetryPressureStateCongested || snapshot.Wait >= time.Millisecond {
		t.gptRetryPressureThrottled = true
	}
	t.mu.Unlock()
}

func (t *requestAttemptTrace) configureBudget(maxAttempts, maxFallbacks int) {
	if t == nil {
		return
	}
	if maxAttempts < 0 {
		maxAttempts = 0
	}
	if maxFallbacks < 0 {
		maxFallbacks = 0
	}
	t.mu.Lock()
	t.maxAttempts = maxAttempts
	t.maxFallbacks = maxFallbacks
	t.mu.Unlock()
}

func (t *requestAttemptTrace) configureGPTRoute(enabled bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.gptRoute = enabled
	t.gptRouteSet = true
	t.mu.Unlock()
}

func (t *requestAttemptTrace) gptRouteValue() (bool, bool) {
	if t == nil {
		return false, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gptRoute, t.gptRouteSet
}

func (t *requestAttemptTrace) beginGPTRound(round int) {
	if t == nil || round <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.gptRoute {
		return
	}
	t.gptRound = round
	t.gptChannels = make(map[string]struct{})
	t.failedChannels = make(map[string]struct{})
	if round == 2 {
		t.gptThirdRound = make(map[string]struct{})
	}
}

func (t *requestAttemptTrace) gptRoundValue() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gptRound
}

func (t *requestAttemptTrace) recordExecution(provider, model, executor string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.translatorRuns++
	if provider = strings.TrimSpace(provider); provider != "" {
		t.finalProvider = provider
	}
	if model = strings.TrimSpace(model); model != "" {
		t.finalModel = model
	}
	if executor = strings.TrimSpace(executor); executor != "" {
		t.finalExecutor = executor
	}
	t.mu.Unlock()
}

func (t *requestAttemptTrace) recordFinalStatus(status int) {
	if t == nil || status <= 0 {
		return
	}
	t.mu.Lock()
	t.finalStatus = status
	t.mu.Unlock()
}

func (t *requestAttemptTrace) stageSessionBinding(cache *SessionCache, key, authID, channelKey string) {
	if t == nil || cache == nil || key == "" || authID == "" {
		return
	}
	t.mu.Lock()
	t.sessionBinding = pendingSessionBinding{cache: cache, key: key, authID: authID, channelKey: channelKey}
	t.mu.Unlock()
}

func (t *requestAttemptTrace) commitSessionBinding(authID string) {
	if t == nil || authID == "" {
		return
	}
	t.mu.Lock()
	pending := t.sessionBinding
	if pending.authID == authID {
		t.sessionBinding = pendingSessionBinding{}
	} else {
		pending = pendingSessionBinding{}
	}
	t.mu.Unlock()
	if pending.cache != nil {
		pending.cache.SetBinding(pending.key, pending.authID, pending.channelKey)
	}
}

func (t *requestAttemptTrace) stageSelectorSelection(selector Selector, provider, model, authID string) {
	if t == nil || selector == nil || strings.TrimSpace(authID) == "" {
		return
	}
	t.mu.Lock()
	t.selection = selectorRouteSelection{
		selector: selector,
		provider: strings.TrimSpace(provider),
		model:    strings.TrimSpace(model),
		authID:   strings.TrimSpace(authID),
	}
	t.mu.Unlock()
}

func (t *requestAttemptTrace) selectorSelection(authID string, release bool) (selectorRouteSelection, bool) {
	if t == nil || strings.TrimSpace(authID) == "" {
		return selectorRouteSelection{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.selection.selector == nil || t.selection.authID != strings.TrimSpace(authID) {
		return selectorRouteSelection{}, false
	}
	selection := t.selection
	if release {
		t.selection = selectorRouteSelection{}
	}
	return selection, true
}

func (t *requestAttemptTrace) recordSelectionReason(reason string) {
	if t == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	t.mu.Lock()
	t.selectionReason = reason
	t.mu.Unlock()
}

func (t *requestAttemptTrace) recordSelectionAvailability(summary selectionAvailabilitySummary) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.selectionAvailability = summary
	t.mu.Unlock()
}

func (t *requestAttemptTrace) selectionLogFields() log.Fields {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fields := log.Fields{}
	if t.selectionReason != "" {
		fields["selection_reason"] = t.selectionReason
	}
	summary := t.selectionAvailability
	if summary.total > 0 {
		fields["candidate_count"] = summary.total
		fields["candidate_ready_count"] = summary.ready
	}
	if summary.skippedDisabled > 0 {
		fields["candidate_skipped_disabled"] = summary.skippedDisabled
	}
	if summary.skippedCooldown > 0 {
		fields["candidate_skipped_cooldown"] = summary.skippedCooldown
	}
	if summary.skippedBreaker > 0 {
		fields["candidate_skipped_breaker"] = summary.skippedBreaker
	}
	if summary.skippedUnavailable > 0 {
		fields["candidate_skipped_unavailable"] = summary.skippedUnavailable
	}
	if summary.healthDownweighted > 0 {
		fields["candidate_health_downweighted"] = summary.healthDownweighted
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func recordSelectorReason(ctx context.Context, reason string) {
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		trace.recordSelectionReason(reason)
	}
}

func (t *requestAttemptTrace) reserveGPTChannel(key string, limit int) (newChannel, allowed bool) {
	if t == nil || key == "" {
		return true, true
	}
	if limit <= 0 {
		limit = gptImmediateFailoverMaxChannels
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gptChannels == nil {
		t.gptChannels = make(map[string]struct{})
	}
	if _, exists := t.gptChannels[key]; exists {
		return false, true
	}
	if len(t.gptChannels) >= limit {
		return false, false
	}
	t.gptChannels[key] = struct{}{}
	return true, true
}

func (t *requestAttemptTrace) markFailedChannel(key string, err error) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	if t.failedChannels == nil {
		t.failedChannels = make(map[string]struct{})
	}
	t.failedChannels[key] = struct{}{}
	if t.gptRound == 2 && isGPTThirdRoundFailure(err) {
		if t.gptThirdRound == nil {
			t.gptThirdRound = make(map[string]struct{})
		}
		t.gptThirdRound[key] = struct{}{}
	}
	t.mu.Unlock()
}

func (t *requestAttemptTrace) failedChannelKeys() map[string]struct{} {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.failedChannels) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(t.failedChannels))
	for key := range t.failedChannels {
		out[key] = struct{}{}
	}
	return out
}

func (t *requestAttemptTrace) failedGPTChannel(key string) bool {
	if t == nil || key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, failed := t.failedChannels[key]
	return failed
}

func (t *requestAttemptTrace) pinGPTChannelModel(key string, models []string) []string {
	if t == nil || key == "" || len(models) == 0 {
		return models
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gptModels == nil {
		t.gptModels = make(map[string]string)
	}
	pinned := t.gptModels[key]
	if pinned == "" {
		t.gptModels[key] = models[0]
		return models
	}
	for _, model := range models {
		if model == pinned {
			return []string{pinned}
		}
	}
	return models
}

func (t *requestAttemptTrace) hasGPTThirdRoundChannels() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.gptThirdRound) > 0
}

func (t *requestAttemptTrace) gptThirdRoundChannelKeys() (map[string]struct{}, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gptRound != 3 || len(t.gptThirdRound) == 0 {
		return nil, false
	}
	out := make(map[string]struct{}, len(t.gptThirdRound))
	for key := range t.gptThirdRound {
		out[key] = struct{}{}
	}
	return out, true
}

func (t *requestAttemptTrace) summary() requestExecutionSummary {
	if t == nil {
		return requestExecutionSummary{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return requestExecutionSummary{
		RequestID:                    t.requestID,
		AttemptCount:                 t.attempts,
		FallbackCount:                t.fallbacks,
		MaxAttempts:                  t.maxAttempts,
		MaxFallbacks:                 t.maxFallbacks,
		TranslatorRuns:               t.translatorRuns,
		FinalProvider:                t.finalProvider,
		FinalModel:                   t.finalModel,
		FinalExecutor:                t.finalExecutor,
		FinalStatus:                  t.finalStatus,
		GPTRoundCount:                t.gptRound,
		EmptyResponses:               t.emptyResponses,
		EmptyRetries:                 t.emptyRetries,
		EmptyUpstreams:               len(t.emptyUpstreams),
		GPTFirstEventTimeouts:        t.gptFirstEventTimeouts,
		GPTFirstEventWait:            t.gptFirstEventWait,
		GPTFirstEventPolicy:          t.gptFirstEventPolicy,
		GPTFirstEventBudgetExhausted: t.gptFirstEventBudgetExhausted,
		GPTRetryPressureState:        t.gptRetryPressureState,
		GPTRetryPressureReason:       t.gptRetryPressureReason,
		GPTRetryPressureWait:         t.gptRetryPressureWait,
		GPTRetryPressurePermitLimit:  t.gptRetryPressurePermitLimit,
		GPTRetryPressureEligible:     t.gptRetryPressureEligible,
		GPTRetryPressureDegraded:     t.gptRetryPressureDegraded,
		GPTRetryPressureThrottled:    t.gptRetryPressureThrottled,
	}
}

func retryReasonFromError(err error) string {
	if err == nil {
		return ""
	}
	if isTransientRoutingError(err) {
		return "transient_routing_error"
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		code := strings.TrimSpace(authErr.Code)
		if code != "" {
			return code
		}
		if authErr.Retryable {
			return "retryable_error"
		}
	}
	if code := strings.TrimSpace(errorCodeFromError(err)); code != "" {
		return code
	}
	if status := statusCodeFromError(err); status > 0 {
		return "status_" + strconv.Itoa(status)
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) && cooldownErr != nil {
		return "model_cooldown"
	}
	return "upstream_error"
}

func addRequestAttemptLogFields(ctx context.Context, fields log.Fields) {
	if len(fields) == 0 {
		return
	}
	attempt := coreusage.RequestAttemptFromContext(ctx)
	if attempt.RequestID != "" {
		fields["request_id"] = attempt.RequestID
	}
	if attempt.AttemptNo > 0 {
		fields["attempt_no"] = attempt.AttemptNo
	}
	if attempt.RetryReason != "" {
		fields["retry_reason"] = attempt.RetryReason
	}
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		if round := trace.gptRoundValue(); round > 0 {
			fields["round_no"] = round
		}
		for key, value := range trace.selectionLogFields() {
			fields[key] = value
		}
	}
}

func providerExecutorName(executor ProviderExecutor) string {
	if executor == nil {
		return ""
	}
	typed := reflect.TypeOf(executor)
	for typed != nil && typed.Kind() == reflect.Ptr {
		typed = typed.Elem()
	}
	if typed != nil {
		if name := strings.TrimSpace(typed.Name()); name != "" {
			return name
		}
	}
	return strings.TrimSpace(executor.Identifier())
}

func logRequestExecutionSummary(ctx context.Context, trace *requestAttemptTrace, finalSuccess bool, finalErr error, commercialMode bool) {
	if trace == nil {
		return
	}
	summary := trace.summary()
	if summary.RequestID == "" && ctx != nil {
		summary.RequestID = strings.TrimSpace(logging.GetRequestID(ctx))
	}
	if summary.FinalStatus == 0 {
		if finalSuccess {
			summary.FinalStatus = http.StatusOK
		} else if status := statusCodeFromError(finalErr); status > 0 {
			summary.FinalStatus = status
		}
	}
	summary.FinalStatus = normalizeRequestExecutionFinalStatus(summary.FinalStatus, finalErr)
	logFinalSuccess := finalSuccess && (summary.FinalStatus == 0 || summary.FinalStatus < http.StatusBadRequest)
	if !shouldLogRequestExecutionSummary(summary, logFinalSuccess, commercialMode) {
		return
	}
	finalErrorType, finalErrorCode := requestExecutionSummaryErrorFields(summary.FinalStatus, finalErr)

	fields := log.Fields{
		"event":                "request_execution_summary",
		"final_success":        logFinalSuccess,
		"attempt_count":        summary.AttemptCount,
		"fallback_count":       summary.FallbackCount,
		"max_attempts":         summary.MaxAttempts,
		"max_fallbacks":        summary.MaxFallbacks,
		"translator_run_count": summary.TranslatorRuns,
	}
	if summary.RequestID != "" {
		fields["request_id"] = summary.RequestID
	}
	if summary.FinalStatus > 0 {
		fields["final_status"] = summary.FinalStatus
	}
	if finalErrorType != "" {
		fields["final_error_type"] = finalErrorType
	}
	if finalErrorCode != "" {
		fields["final_error_code"] = finalErrorCode
	}
	if summary.FinalProvider != "" {
		fields["final_provider"] = summary.FinalProvider
	}
	if summary.FinalModel != "" {
		fields["final_model"] = summary.FinalModel
	}
	if summary.FinalExecutor != "" {
		fields["final_executor"] = summary.FinalExecutor
	}
	if summary.GPTRoundCount > 0 {
		fields["gpt_round_count"] = summary.GPTRoundCount
	}
	if summary.EmptyResponses > 0 {
		finalEmptyResponse := strings.EqualFold(errorCodeFromError(finalErr), emptyUpstreamResponseErrorCode)
		fields["empty_response_count"] = summary.EmptyResponses
		fields["empty_response_retry_count"] = summary.EmptyRetries
		fields["empty_response_upstream_count"] = summary.EmptyUpstreams
		fields["empty_response_recovered"] = logFinalSuccess
		fields["empty_response_exhausted"] = !logFinalSuccess && finalEmptyResponse
		fields["final_empty_response"] = finalEmptyResponse
	}
	if summary.GPTFirstEventWait > 0 {
		fields["first_event_wait_ms"] = summary.GPTFirstEventWait.Milliseconds()
	}
	if summary.GPTFirstEventTimeouts > 0 {
		fields["first_event_timeout_count"] = summary.GPTFirstEventTimeouts
	}
	if summary.GPTFirstEventPolicy.PolicyState != "" {
		fields["first_event_policy_state"] = summary.GPTFirstEventPolicy.PolicyState
		fields["first_event_policy_reason"] = summary.GPTFirstEventPolicy.DecisionReason
		fields["first_event_policy_source"] = summary.GPTFirstEventPolicy.DecisionSource
		fields["first_event_timeout_ms"] = summary.GPTFirstEventPolicy.EnforcedTimeoutMs
		fields["first_event_max_channels"] = summary.GPTFirstEventPolicy.MaxChannels
		fields["first_event_max_rounds"] = summary.GPTFirstEventPolicy.MaxRounds
		fields["first_event_wait_budget_ms"] = summary.GPTFirstEventPolicy.WaitBudgetMs
	}
	if summary.GPTFirstEventBudgetExhausted {
		fields["first_event_wait_budget_exhausted"] = true
	}
	if summary.GPTRetryPressureState != "" {
		fields["retry_pressure_state"] = summary.GPTRetryPressureState
		fields["retry_pressure_reason"] = summary.GPTRetryPressureReason
		fields["retry_pressure_wait_ms"] = summary.GPTRetryPressureWait.Milliseconds()
		fields["retry_pressure_permit_limit"] = summary.GPTRetryPressurePermitLimit
		fields["retry_pressure_eligible_routes"] = summary.GPTRetryPressureEligible
		fields["retry_pressure_degraded_routes"] = summary.GPTRetryPressureDegraded
		fields["retry_pressure_throttled"] = summary.GPTRetryPressureThrottled
	}
	logEntryWithRequestID(ctx).WithFields(fields).Info("request_execution_summary")
}

func shouldLogRequestExecutionSummary(summary requestExecutionSummary, finalSuccess, commercialMode bool) bool {
	if !commercialMode || !finalSuccess {
		return true
	}
	if summary.FinalStatus != 0 && summary.FinalStatus != http.StatusOK {
		return true
	}
	if summary.AttemptCount > 1 || summary.FallbackCount > 0 || summary.TranslatorRuns > 1 || summary.GPTRoundCount > 1 {
		return true
	}
	if summary.EmptyResponses > 0 || summary.GPTFirstEventTimeouts > 0 || summary.GPTFirstEventBudgetExhausted {
		return true
	}
	if summary.GPTRetryPressureThrottled || summary.GPTRetryPressureWait >= time.Millisecond || summary.GPTRetryPressureReason != "" {
		return true
	}
	return summary.GPTRetryPressureState != "" && summary.GPTRetryPressureState != gptRetryPressureStateNormal
}

func normalizeRequestExecutionFinalStatus(status int, err error) int {
	if isRequestScopedContentSafetyError(err) {
		return http.StatusBadRequest
	}
	// The trace status describes the latest upstream attempt, while err is the
	// terminal result returned to the caller. Prefer a structured terminal
	// error so a caller cancellation after a 503/probe wait is not logged as a
	// stale upstream 503.
	if err != nil {
		if errStatus := statusCodeFromError(err); errStatus > 0 {
			return errStatus
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 499
		}
	}
	if status > 0 {
		return status
	}
	if err == nil {
		return 0
	}
	return http.StatusInternalServerError
}

func requestExecutionSummaryErrorFields(status int, err error) (string, string) {
	if isRequestScopedContentSafetyError(err) {
		return "invalid_request_error", "content_policy_violation"
	}
	code := strings.TrimSpace(errorCodeFromError(err))
	if status <= 0 || status < http.StatusBadRequest {
		return "", ""
	}
	switch status {
	case 499:
		return "cancelled", firstNonEmpty(code, "request_canceled")
	case http.StatusUnauthorized:
		return "authentication_error", firstNonEmpty(code, "invalid_api_key")
	case http.StatusForbidden:
		return "permission_error", firstNonEmpty(code, "insufficient_quota")
	case http.StatusTooManyRequests:
		return "rate_limit_error", firstNonEmpty(code, "rate_limit_exceeded")
	case http.StatusNotFound:
		return "invalid_request_error", firstNonEmpty(code, "model_not_found")
	default:
		if status >= http.StatusInternalServerError {
			return "server_error", firstNonEmpty(code, "internal_server_error")
		}
		return "invalid_request_error", firstNonEmpty(code, "status_"+strconv.Itoa(status))
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func logRoutePlan(ctx context.Context, auth *Auth, provider, routeModel, resolvedModel, upstreamModel string, opts cliproxyexecutor.Options, executor ProviderExecutor, operation string) {
	trace := requestAttemptTraceFromContext(ctx)
	if trace == nil {
		return
	}
	plan := buildRoutePlanSummary(trace.summary(), auth, provider, routeModel, resolvedModel, upstreamModel, opts, executor, operation, coreusage.RequestAttemptFromContext(ctx))
	fields := log.Fields{
		"event":                          "route_plan",
		"route_plan":                     plan,
		"compaction_intent":              metadataString(opts.Metadata, cliproxyexecutor.CompactionIntentMetadataKey),
		"compaction_trigger_mode":        metadataString(opts.Metadata, cliproxyexecutor.CompactionTriggerModeMetadataKey),
		"compaction_compatibility_group": metadataString(opts.Metadata, cliproxyexecutor.CompactionCompatibilityGroupMetadataKey),
	}
	addRequestAttemptLogFields(ctx, fields)
	logEntryWithRequestID(ctx).WithFields(fields).Info("route_plan")
}

func buildRoutePlanSummary(previous requestExecutionSummary, auth *Auth, provider, routeModel, resolvedModel, upstreamModel string, opts cliproxyexecutor.Options, executor ProviderExecutor, operation string, attempt coreusage.RequestAttempt) routePlanSummary {
	requestPath := metadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)
	sourceFormat := strings.TrimSpace(opts.SourceFormat.String())
	executorName := providerExecutorName(executor)
	protocol := routePlanProtocol(sourceFormat, requestPath)
	routingGroup := ""
	if auth != nil {
		routingGroup = explicitAuthRoutingGroup(auth)
	}
	effortOriginal := metadataString(opts.Metadata, cliproxyexecutor.ReasoningEffortOriginalMetadataKey)
	requestedModel := routePlanRequestedModel(opts, routeModel)
	clientProfile := metadataString(opts.Metadata, cliproxyexecutor.ClientProfileMetadataKey)
	identity := routePlanProviderIdentity(auth, provider)
	compatKindSource := ""
	if identity.Kind != "" {
		compatKindSource = string(identity.Source)
	}
	compactionIntent := cliproxyexecutor.CompactionIntentFromOptions(cliproxyexecutor.Request{}, opts)
	compactionTriggerMode := metadataString(opts.Metadata, cliproxyexecutor.CompactionTriggerModeMetadataKey)
	upstreamPath := routePlanUpstreamPath(protocol, requestPath, executorName, operation)
	translator := routePlanTranslator(protocol, requestPath, executorName)
	if cliproxyexecutor.IsRemoteCompactionIntent(compactionIntent) {
		upstreamPath, translator = routePlanRemoteCompactionTarget(compactionIntent, compactionTriggerMode, executorName, upstreamPath, translator)
	}
	return routePlanSummary{
		RequestedModel:               requestedModel,
		ResolvedModel:                strings.TrimSpace(resolvedModel),
		UpstreamModel:                strings.TrimSpace(upstreamModel),
		AuthIndex:                    authMetricIndex(auth),
		Provider:                     strings.TrimSpace(provider),
		Protocol:                     protocol,
		Executor:                     executorName,
		UpstreamPath:                 upstreamPath,
		Translator:                   translator,
		RoutingGroup:                 routingGroup,
		FallbackFrom:                 routePlanFallbackFrom(previous),
		FallbackReason:               strings.TrimSpace(attempt.RetryReason),
		CompatKind:                   identity.Kind,
		CompatKindSource:             compatKindSource,
		CompatMapping:                routePlanCompatMapping(requestedModel, resolvedModel, identity.Kind),
		CompatBaseHost:               identity.BaseHost,
		ClientProfile:                clientProfile,
		ContextHint:                  metadataString(opts.Metadata, cliproxyexecutor.ModelContextHintMetadataKey),
		EffortSource:                 metadataString(opts.Metadata, cliproxyexecutor.ReasoningEffortSourceMetadataKey),
		EffortOriginal:               effortOriginal,
		EffortNormalized:             routePlanNormalizedReasoningEffort(auth, provider, requestedModel, clientProfile, resolvedModel, effortOriginal),
		CompactionIntent:             string(compactionIntent),
		CompactionTriggerMode:        compactionTriggerMode,
		CompactionCompatibilityGroup: metadataString(opts.Metadata, cliproxyexecutor.CompactionCompatibilityGroupMetadataKey),
	}
}

func routePlanRemoteCompactionTarget(intent cliproxyexecutor.CompactionIntent, triggerMode, executorName, fallbackPath, fallbackTranslator string) (string, string) {
	upstreamPath := fallbackPath
	translator := fallbackTranslator
	switch intent {
	case cliproxyexecutor.CompactionIntentLegacyEndpoint:
		upstreamPath = "/responses/compact"
	case cliproxyexecutor.CompactionIntentV2Trigger:
		if triggerMode == ResponsesCompactionTriggerBridgeLegacy {
			upstreamPath = "/responses/compact"
		} else {
			upstreamPath = "/responses"
		}
	case cliproxyexecutor.CompactionIntentContextManagement:
		upstreamPath = "/responses"
	}
	if executorName == "OpenAICompatExecutor" {
		if intent == cliproxyexecutor.CompactionIntentV2Trigger && triggerMode == ResponsesCompactionTriggerBridgeLegacy {
			translator = "OpenAIResponsesCompactionBridgeLegacy"
		} else {
			translator = "OpenAIResponsesPassthrough"
		}
	}
	return upstreamPath, translator
}

func routePlanProviderIdentity(auth *Auth, provider string) provideridentity.Identity {
	var attributes map[string]string
	if auth != nil {
		if strings.TrimSpace(provider) == "" {
			provider = auth.Provider
		}
		attributes = auth.Attributes
	}
	return provideridentity.Resolve(provideridentity.InputFromAttributes(provider, attributes))
}

func routePlanCompatKindWithSource(auth *Auth) (string, string) {
	identity := routePlanProviderIdentity(auth, "")
	if identity.Kind == "" {
		return "", ""
	}
	return identity.Kind, string(identity.Source)
}

func routePlanCompatMapping(requestedModel, resolvedModel, compatKind string) string {
	if internalconfig.NormalizeOpenAICompatibilityKind(compatKind) != "doubao" {
		return ""
	}
	if isDeepSeekV4RouteModel(requestedModel) || isDeepSeekV4RouteModel(resolvedModel) {
		return "deepseek_v4_via_doubao_volcengine"
	}
	return ""
}

func isDeepSeekV4RouteModel(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	return strings.HasPrefix(modelName, "deepseek-v4-pro") || strings.HasPrefix(modelName, "deepseek-v4-flash")
}

func routePlanCompatBaseHost(auth *Auth) string {
	return routePlanProviderIdentity(auth, "").BaseHost
}

func routePlanNormalizedReasoningEffort(auth *Auth, provider string, requestedModel string, clientProfile string, resolvedModel string, effortOriginal string) string {
	effortOriginal = strings.TrimSpace(effortOriginal)
	if effortOriginal == "" {
		return ""
	}

	deepSeekOfficial := isDeepSeekOfficialRoute(auth, resolvedModel)
	if !deepSeekOfficial && !thinking.ShouldNormalizeStrongestReasoningIntent(requestedModel, clientProfile, effortOriginal) {
		return ""
	}

	providerKey := routePlanThinkingProviderKey(auth, provider)
	modelInfo := registry.LookupModelInfo(strings.TrimSpace(thinking.ParseSuffix(resolvedModel).ModelName), providerKey)
	var support *registry.ThinkingSupport
	if modelInfo != nil {
		support = modelInfo.Thinking
	}
	normalized := thinking.NormalizeReasoningEffortForTarget(effortOriginal, support, deepSeekOfficial)
	return normalized.Normalized
}

func isDeepSeekOfficialRoute(auth *Auth, resolvedModel string) bool {
	if auth == nil {
		return false
	}
	if !isDeepSeekV4RouteModel(resolvedModel) {
		return false
	}
	return routePlanProviderIdentity(auth, "").CanonicalProvider == "deepseek"
}

func routePlanThinkingProviderKey(auth *Auth, provider string) string {
	identity := routePlanProviderIdentity(auth, provider)
	if identity.ExecutorKey != "" {
		return identity.ExecutorKey
	}
	if identity.Kind != "" {
		return identity.Kind
	}
	return identity.CanonicalProvider
}

func routePlanRequestedModel(opts cliproxyexecutor.Options, routeModel string) string {
	if requested := strings.TrimSpace(requestedModelAliasFromOptions(opts, routeModel)); requested != "" {
		return requested
	}
	return strings.TrimSpace(routeModel)
}

func routePlanProtocol(sourceFormat, requestPath string) string {
	path := strings.ToLower(strings.TrimSpace(requestPath))
	sourceFormat = strings.ToLower(strings.TrimSpace(sourceFormat))
	switch {
	case strings.HasSuffix(path, "/v1/images/generations"), strings.HasSuffix(path, "/v1/images/edits"), sourceFormat == "openai-image":
		return "openai_image"
	case strings.HasSuffix(path, "/v1/videos"), sourceFormat == "openai-video":
		return "openai_video"
	case isResponsesEndpointPath(path), sourceFormat == "openai-response":
		return "openai_responses"
	case sourceFormat == "claude":
		return "claude_messages"
	case sourceFormat == "gemini":
		return "gemini_generate_content"
	case sourceFormat == "openai":
		return "openai_chat"
	case sourceFormat != "":
		return sourceFormat
	default:
		return "unknown"
	}
}

func routePlanUpstreamPath(protocol, requestPath, executorName, operation string) string {
	switch executorName {
	case "CodexExecutor", "CodexAutoExecutor":
		if protocol == "openai_image" {
			if strings.HasSuffix(strings.ToLower(strings.TrimSpace(requestPath)), "/v1/images/edits") {
				return "/v1/images/edits"
			}
			return "/v1/images/generations"
		}
		return "/responses"
	case "ClaudeExecutor":
		if operation == "count" {
			return "/v1/messages/count_tokens?beta=true"
		}
		return "/v1/messages?beta=true"
	case "KimiExecutor":
		return "/v1/chat/completions"
	case "OpenAICompatExecutor":
		switch protocol {
		case "openai_image":
			if strings.HasSuffix(strings.ToLower(strings.TrimSpace(requestPath)), "/v1/images/edits") {
				return "/images/edits"
			}
			return "/images/generations"
		case "openai_responses":
			if operation == "stream" {
				return "/chat/completions"
			}
			return "/responses/compact"
		default:
			return "/chat/completions"
		}
	case "GeminiExecutor", "GeminiVertexExecutor", "GeminiCLIExecutor", "AIStudioExecutor":
		return "generateContent"
	default:
		if protocol == "openai_image" {
			if strings.HasSuffix(strings.ToLower(strings.TrimSpace(requestPath)), "/v1/images/edits") {
				return "/v1/images/edits"
			}
			return "/v1/images/generations"
		}
		if protocol == "openai_responses" {
			return "/v1/responses"
		}
		if operation == "count" {
			return "count_tokens"
		}
		return requestPath
	}
}

func routePlanTranslator(protocol, requestPath, executorName string) string {
	switch executorName {
	case "CodexExecutor", "CodexAutoExecutor":
		switch protocol {
		case "openai_responses":
			return "OpenAIResponsesToCodex"
		case "openai_image":
			return "OpenAIImageToCodex"
		default:
			return "OpenAIToCodex"
		}
	case "KimiExecutor":
		if protocol == "claude_messages" {
			return "ClaudeToKimiOpenAICompat"
		}
		return "OpenAIToKimiOpenAICompat"
	case "ClaudeExecutor":
		switch protocol {
		case "claude_messages":
			return "ClaudePassthrough"
		case "openai_responses":
			return "OpenAIResponsesToClaude"
		default:
			return "OpenAIToClaude"
		}
	case "OpenAICompatExecutor":
		switch protocol {
		case "openai_responses":
			return "OpenAIResponsesToOpenAICompat"
		case "openai_image":
			return "OpenAIImageToOpenAICompat"
		case "claude_messages":
			return "ClaudeToOpenAICompat"
		default:
			return "OpenAIChatCompatible"
		}
	case "GeminiExecutor", "GeminiVertexExecutor", "GeminiCLIExecutor", "AIStudioExecutor":
		switch protocol {
		case "openai_responses":
			return "OpenAIResponsesToGemini"
		case "openai_image":
			return "OpenAIImageToGemini"
		case "claude_messages":
			return "ClaudeToGemini"
		default:
			return "OpenAIToGemini"
		}
	default:
		if protocol == "openai_responses" {
			return "OpenAIResponses"
		}
		if protocol == "claude_messages" {
			return "ClaudeCompatible"
		}
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(requestPath)), "/v1/images/edits") || strings.HasSuffix(strings.ToLower(strings.TrimSpace(requestPath)), "/v1/images/generations") {
			return "OpenAIImageCompatible"
		}
		return "GenericTranslator"
	}
}

func routePlanFallbackFrom(previous requestExecutionSummary) string {
	parts := make([]string, 0, 3)
	if previous.FinalProvider != "" {
		parts = append(parts, previous.FinalProvider)
	}
	if previous.FinalModel != "" {
		parts = append(parts, previous.FinalModel)
	}
	if previous.FinalExecutor != "" {
		parts = append(parts, previous.FinalExecutor)
	}
	return strings.Join(parts, "/")
}

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshMaxConcurrency = 16
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	// refreshIneffectiveBackoff throttles refresh attempts when an executor returns
	// success but the auth still evaluates as needing refresh (e.g. token expiry
	// wasn't updated). Without this guard, the auto-refresh loop can tight-loop and
	// burn CPU at idle.
	refreshIneffectiveBackoff        = 30 * time.Second
	quotaBackoffBase                 = time.Second
	quotaBackoffMax                  = 30 * time.Minute
	transientErrorCooldown           = time.Minute
	quotaHardCooldownFailures        = health429OpenFailures
	quotaImmediateCooldownRetryAfter = 15 * time.Minute
	accountQuotaCooldown             = 24 * time.Hour
	emptyUpstreamResponseErrorCode   = "empty_upstream_response"
	halfOpenProbeStateLimit          = 4096
	transientNetworkRetryAttempts    = 3
	transientNetworkRetryMaxDelay    = 5 * time.Second
	slowRequestSoftThreshold         = 30 * time.Second
	slowRequestHardThreshold         = time.Minute
	slowRequestSoftPenalty           = 10
	slowRequestHardPenalty           = 25
	slowRequestMinHealthScore        = 10
	gptImmediateFailoverMaxChannels  = 8
	gptImmediateFailoverMaxRounds    = 3
	gptChannelProbeLease             = 10 * time.Minute
	deepSeekProtocolAffinityMinBytes = 3 * 1024 * 1024
	deepSeekProtocolAffinityMinTools = 120
	defaultGPTFirstEventTimeout      = 25 * time.Second
	gptZeroEligibleProbeActiveTTL    = 6 * time.Minute
	gptZeroEligibleProbeMinWait      = 15 * time.Second
	gptZeroEligibleProbeWaitGrace    = 2 * time.Second
	gptZeroEligibleProbeMaxWait      = 55 * time.Second
	gptZeroEligibleProbeMaxWaiters   = 8
	gptZeroEligibleProbeStateLimit   = 4096
)

var quotaCooldownDisabled atomic.Bool
var deleteUnauthorizedAuthEnabled atomic.Bool
var transientErrorCooldownSeconds atomic.Int64

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

// SetDeleteUnauthorizedAuth toggles whether a 401 response should evict the auth
// from memory and delete it from the underlying store. When false (default), a
// 401 only marks the auth as unauthorized and cools it down (see MarkResult),
// but the auth record is preserved.
func SetDeleteUnauthorizedAuth(enable bool) {
	deleteUnauthorizedAuthEnabled.Store(enable)
}

// SetTransientErrorCooldownSeconds configures cooldowns for 408/500/502/503/504.
// 0 keeps the legacy default; negative values disable transient error cooldowns.
func SetTransientErrorCooldownSeconds(seconds int) {
	transientErrorCooldownSeconds.Store(int64(seconds))
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	return quotaCooldownDisabledForAuthWithConfig(auth, nil)
}

func quotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
		if providerCoolingDisabledForAuth(auth, cfg) {
			return true
		}
	}
	if cfg != nil && cfg.DisableCooling {
		return true
	}
	return quotaCooldownDisabled.Load()
}

func providerCoolingDisabledForAuth(auth *Auth, cfg *internalconfig.Config) bool {
	if auth == nil || cfg == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		return false
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if providerKey == "" && compatName == "" && provider != "openai-compatibility" {
		return false
	}
	if providerKey == "" {
		providerKey = provider
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, provider)
	return entry != nil && entry.DisableCooling
}

func nextTransientErrorRetryAfter(now time.Time) time.Time {
	seconds := transientErrorCooldownSeconds.Load()
	if seconds < 0 {
		return time.Time{}
	}
	if seconds == 0 {
		return now.Add(transientErrorCooldown)
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Duration records the upstream attempt duration for health-weight adjustment.
	Duration time.Duration
	// TTFT records time to first response data. Non-streaming calls may use
	// Duration as a conservative approximation when this value is unavailable.
	TTFT time.Duration
	// Error describes the failure when Success is false.
	Error *Error
	// Cause keeps the original executor error for typed infrastructure failures.
	Cause             error
	keepSelectorLease bool
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

type loadAwareSelector interface {
	MarkDone(authID, model string)
}

type resultAwareSelector interface {
	MarkResult(authID, model string, success bool, ttft time.Duration)
}

type routeResultAwareSelector interface {
	MarkRouteResult(provider, authID, model string, success bool, ttft time.Duration, release bool)
}

type routeLoadAwareSelector interface {
	MarkRouteDone(provider, authID, model string)
}

type PluginScheduler interface {
	PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
}

type pluginSchedulerState interface {
	HasScheduler() bool
}

// StoppableSelector is an optional interface for selectors that hold resources.
// Selectors that implement this interface will have Stop called during shutdown.
type StoppableSelector interface {
	Selector
	Stop()
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store            Store
	cooldownStore    CooldownStateStore
	executors        map[string]ProviderExecutor
	selector         Selector
	hook             Hook
	mu               sync.RWMutex
	selectorUpdateMu sync.Mutex
	auths            map[string]*Auth
	scheduler        *authScheduler
	// pluginScheduler runs outside m.mu before falling back to native selection.
	pluginScheduler PluginScheduler
	// homeRuntimeAuths caches auths returned by Home so websocket sessions can
	// reuse an established upstream credential without dispatching every turn.
	homeRuntimeAuths map[string]map[string]*Auth
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int

	// Retry controls request retry behavior.
	requestRetry         atomic.Int32
	maxRetryCredentials  atomic.Int32
	maxRetryInterval     atomic.Int64
	retryQueueDelay      atomic.Int64
	gptFirstEventTimeout atomic.Int64
	schedulerHotSyncDue  atomic.Int64
	translatorRegistry   atomic.Pointer[sdktranslator.Registry]

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// modelPoolOffsets tracks per-auth alias pool rotation state.
	modelPoolOffsets map[string]int

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// dynamicSelectors caches per-routing-group selector instances when routing
	// group strategy overrides are enabled.
	dynamicSelectorsMu sync.Mutex
	dynamicSelectors   map[string]Selector

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel context.CancelFunc
	refreshLoop   *authAutoRefreshLoop

	// halfOpenProbeNext tracks the earliest time another half-open probe may be
	// sent for one auth/model combination.
	halfOpenProbeMu          sync.Mutex
	halfOpenProbeNext        map[string]time.Time
	halfOpenProbeActiveUntil map[string]time.Time
	zeroEligibleProbeMu      sync.Mutex
	zeroEligibleProbes       map[string]zeroEligibleProbeLease
	channelBreakers          map[string]HealthState
	gptChannelBreakers       map[string]*codexChannelBreakerState
	compactionBreakerMu      sync.Mutex
	compactionBreakers       map[string]*compactionBreakerState

	codexModelLoadMu sync.Mutex
	codexModelLoads  map[string]int

	requestPrepareLocks   sync.Map
	refreshLocks          sync.Map
	activeStreams         *activeStreamTracker
	gptFirstEventObserver *gptFirstEventObserver
	gptRetryPressure      *gptRetryPressureController
	gptPolicyPersistMu    sync.Mutex
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:                    store,
		executors:                make(map[string]ProviderExecutor),
		selector:                 selector,
		hook:                     hook,
		auths:                    make(map[string]*Auth),
		homeRuntimeAuths:         make(map[string]map[string]*Auth),
		providerOffsets:          make(map[string]int),
		modelPoolOffsets:         make(map[string]int),
		dynamicSelectors:         make(map[string]Selector),
		halfOpenProbeNext:        make(map[string]time.Time),
		halfOpenProbeActiveUntil: make(map[string]time.Time),
		zeroEligibleProbes:       make(map[string]zeroEligibleProbeLease),
		channelBreakers:          make(map[string]HealthState),
		gptChannelBreakers:       make(map[string]*codexChannelBreakerState),
		compactionBreakers:       make(map[string]*compactionBreakerState),
		codexModelLoads:          make(map[string]int),
		activeStreams:            newActiveStreamTracker(),
		gptFirstEventObserver:    newGPTFirstEventObserver(),
		gptRetryPressure:         newGPTRetryPressureController(),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.gptFirstEventTimeout.Store(defaultGPTFirstEventTimeout.Nanoseconds())
	manager.scheduler = newAuthScheduler(selector)
	return manager
}

func (m *Manager) SetPluginScheduler(scheduler PluginScheduler) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pluginScheduler = scheduler
	m.mu.Unlock()
}

// SetTranslatorRegistry binds request execution to an explicit translator
// registry. Passing nil clears the manager-level override.
func (m *Manager) SetTranslatorRegistry(registry *sdktranslator.Registry) {
	if m == nil {
		return
	}
	m.translatorRegistry.Store(registry)
}

func (m *Manager) translatorContext(ctx context.Context) context.Context {
	if m == nil {
		return sdktranslator.ContextWithRegistry(ctx, nil)
	}
	return sdktranslator.ContextWithRegistry(ctx, m.translatorRegistry.Load())
}

func (m *Manager) hasPluginScheduler() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	scheduler := m.pluginScheduler
	m.mu.RUnlock()
	if scheduler == nil {
		return false
	}
	if state, ok := scheduler.(pluginSchedulerState); ok {
		return state.HasScheduler()
	}
	return true
}

func isBuiltInSelector(selector Selector) bool {
	switch selector.(type) {
	case *RoundRobinSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

func selectorUsesSpread(selector Selector) bool {
	switch s := selector.(type) {
	case *SpreadSelector:
		return true
	case *SessionAffinitySelector:
		return selectorUsesSpread(s.fallback)
	default:
		return false
	}
}

func authRoutingGroup(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		for _, key := range []string{"routing_group", "routing-group"} {
			if value := normalizeRoutingGroupKey(auth.Attributes[key]); value != "" {
				return value
			}
		}
		if value := normalizeRoutingGroupKey(auth.Attributes["compat_kind"]); value != "" {
			return value
		}
		if value := normalizeRoutingGroupKey(auth.Attributes["compat_name"]); value != "" {
			return value
		}
		if value := normalizeRoutingGroupKey(auth.Attributes["provider_key"]); value != "" {
			return value
		}
	}
	if value := normalizeRoutingGroupKey(auth.Prefix); value != "" {
		return value
	}
	return normalizeRoutingGroupKey(auth.Provider)
}

func commonRoutingGroup(auths []*Auth) string {
	group := ""
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		current := authRoutingGroup(auth)
		if current == "" {
			return ""
		}
		if group == "" {
			group = current
			continue
		}
		if group != current {
			return ""
		}
	}
	return group
}

func (m *Manager) routingGroupStrategies() map[string]string {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return nil
	}
	return NormalizeRoutingGroupStrategies(cfg.Routing.GroupStrategies)
}

func (m *Manager) routingProviderStrategies() map[string]string {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return nil
	}
	return NormalizeRoutingProviderStrategies(cfg.Routing.ProviderStrategies)
}

func (m *Manager) hasRoutingGroupStrategies() bool {
	return len(m.routingGroupStrategies()) > 0
}

func (m *Manager) hasRoutingProviderStrategies() bool {
	return len(m.routingProviderStrategies()) > 0
}

func (m *Manager) hasRoutingStrategyOverrides() bool {
	return m.hasRoutingGroupStrategies() || m.hasRoutingProviderStrategies()
}

func commonProviderKey(auths []*Auth) string {
	providerKey := ""
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		current := normalizeRoutingGroupKey(auth.Provider)
		if current == "" {
			return ""
		}
		if providerKey == "" {
			providerKey = current
			continue
		}
		if providerKey != current {
			return ""
		}
	}
	return providerKey
}

func authProviderFamilyKey(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	if value := normalizeRoutingGroupKey(auth.Attributes["provider_family"]); value != "" {
		return value
	}
	for _, key := range []string{"provider_type", "provider-type"} {
		if value := normalizeRoutingGroupKey(auth.Attributes[key]); value != "" {
			return value
		}
	}
	if normalizeRoutingGroupKey(auth.Attributes["compat_name"]) != "" || normalizeRoutingGroupKey(auth.Attributes["provider_key"]) != "" {
		return "openai-compatibility"
	}
	return ""
}

func commonProviderFamilyKey(auths []*Auth) string {
	providerKey := ""
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		current := authProviderFamilyKey(auth)
		if current == "" {
			return ""
		}
		if providerKey == "" {
			providerKey = current
			continue
		}
		if providerKey != current {
			return ""
		}
	}
	return providerKey
}

func commonProviderStrategyKeys(auths []*Auth) []string {
	exact := commonProviderKey(auths)
	family := commonProviderFamilyKey(auths)
	keys := make([]string, 0, 2)
	if exact != "" {
		keys = append(keys, exact)
	}
	if family != "" && family != exact {
		keys = append(keys, family)
	}
	return keys
}

func (m *Manager) routingStrategyForAuths(auths []*Auth) (string, string, bool) {
	if overrides := m.routingGroupStrategies(); len(overrides) > 0 {
		group := commonRoutingGroup(auths)
		if group != "" {
			if strategy, ok := overrides[group]; ok {
				return "group:" + group, strategy, true
			}
		}
	}
	if overrides := m.routingProviderStrategies(); len(overrides) > 0 {
		for _, providerKey := range commonProviderStrategyKeys(auths) {
			if strategy, ok := overrides[providerKey]; ok {
				return "provider:" + providerKey, strategy, true
			}
		}
	}
	return "", "", false
}

func (m *Manager) selectorForStrategyGroup(group, strategy string) Selector {
	if m == nil {
		return SelectorForRoutingStrategy(strategy)
	}
	normalizedStrategy, ok := NormalizeRoutingStrategy(strategy)
	if !ok {
		return m.selector
	}
	group = normalizeRoutingGroupKey(group)
	if group == "" {
		return SelectorForRoutingStrategy(normalizedStrategy)
	}

	selector := SelectorForRoutingStrategy(normalizedStrategy)
	if normalizedStrategy == RoutingStrategySpread && strings.HasPrefix(group, "group:") {
		selector = &SpreadSelector{channelAware: true}
	}
	affinityEnabled := false
	affinityTTL := time.Hour
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg != nil {
		if parsedTTL, errParse := time.ParseDuration(strings.TrimSpace(cfg.Routing.SessionAffinityTTL)); errParse == nil && parsedTTL > 0 {
			affinityTTL = parsedTTL
		}
	}
	if sessionSelector, ok := m.selector.(*SessionAffinitySelector); ok && sessionSelector != nil {
		affinityEnabled = true
		if sessionSelector.cache != nil && sessionSelector.cache.ttl > 0 {
			affinityTTL = sessionSelector.cache.ttl
		}
	}
	if strings.HasPrefix(group, "group:") {
		groupName := strings.TrimPrefix(group, "group:")
		if cfg != nil {
			for configuredGroup, enabled := range cfg.Routing.GroupSessionAffinity {
				if normalizeRoutingGroupKey(configuredGroup) == groupName {
					affinityEnabled = enabled
					break
				}
			}
		}
	}
	cacheKey := strings.Join([]string{
		group,
		normalizedStrategy,
		strconv.FormatBool(affinityEnabled),
		affinityTTL.String(),
	}, "\x00")

	m.dynamicSelectorsMu.Lock()
	defer m.dynamicSelectorsMu.Unlock()
	if selector, ok := m.dynamicSelectors[cacheKey]; ok && selector != nil {
		return selector
	}

	if affinityEnabled {
		selector = NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
			Fallback: selector,
			TTL:      affinityTTL,
		})
	}
	m.dynamicSelectors[cacheKey] = selector
	return selector
}

func (m *Manager) selectorForAuths(auths []*Auth) Selector {
	group, strategy, ok := m.routingStrategyForAuths(auths)
	if !ok {
		return m.selector
	}
	return m.selectorForStrategyGroup(group, strategy)
}

func (m *Manager) stopDynamicSelectors() {
	if m == nil {
		return
	}
	m.dynamicSelectorsMu.Lock()
	selectors := make([]Selector, 0, len(m.dynamicSelectors))
	for key, selector := range m.dynamicSelectors {
		if selector == nil {
			delete(m.dynamicSelectors, key)
			continue
		}
		selectors = append(selectors, selector)
	}
	m.dynamicSelectors = make(map[string]Selector)
	m.dynamicSelectorsMu.Unlock()

	for _, selector := range selectors {
		if stoppable, ok := selector.(StoppableSelector); ok {
			stoppable.Stop()
		}
	}
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
}

func (m *Manager) syncSchedulerOnPickFailure(now time.Time) bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	nowUnix := now.UnixNano()
	nextAllowed := now.Add(schedulerHotPathSyncMinInterval).UnixNano()
	for {
		dueAt := m.schedulerHotSyncDue.Load()
		if dueAt > nowUnix {
			return false
		}
		if m.schedulerHotSyncDue.CompareAndSwap(dueAt, nextAllowed) {
			m.syncScheduler()
			return true
		}
	}
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	m.scheduler.upsertAuth(snapshot)
}

// RefreshSchedulerAll rebuilds scheduler entries for every known auth.
func (m *Manager) RefreshSchedulerAll() {
	if m == nil {
		return
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.auths))
	for id := range m.auths {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.RefreshSchedulerEntry(id)
	}
}

// ReconcileRegistryModelStates aligns per-model runtime state with the current
// registry snapshot for one auth.
//
// Supported models are reset to a clean state because re-registration already
// cleared the registry-side cooldown/suspension snapshot. ModelStates for
// models that are no longer present in the registry are pruned entirely so
// renamed/removed models cannot keep auth-level status stale.
func (m *Manager) ReconcileRegistryModelStates(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}

	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	supported := make(map[string]struct{}, len(supportedModels))
	for _, model := range supportedModels {
		if model == nil {
			continue
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey == "" {
			continue
		}
		supported[modelKey] = struct{}{}
	}

	var snapshot *Auth
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil && len(auth.ModelStates) > 0 {
		changed := false
		for modelKey, state := range auth.ModelStates {
			baseModel := canonicalModelKey(modelKey)
			if baseModel == "" {
				baseModel = strings.TrimSpace(modelKey)
			}
			if _, supportedModel := supported[baseModel]; !supportedModel {
				// Drop state for models that disappeared from the current registry
				// snapshot. Keeping them around leaks stale errors into auth-level
				// status, management output, and websocket fallback checks.
				delete(auth.ModelStates, modelKey)
				changed = true
				continue
			}
			if state == nil {
				continue
			}
			if isPersistedModelSupportState(state) {
				registry.GetGlobalRegistry().SuspendClientModel(authID, baseModel, "model_not_supported")
				continue
			}
			if modelStateIsClean(state) {
				continue
			}
			resetModelState(state, now)
			changed = true
		}
		if len(auth.ModelStates) == 0 {
			auth.ModelStates = nil
		}
		if changed {
			updateAggregatedAvailability(auth, now)
			if !hasModelError(auth, now) {
				auth.LastError = nil
				auth.StatusMessage = ""
				auth.Status = StatusActive
			}
			auth.UpdatedAt = now
			if errPersist := m.persist(ctx, auth); errPersist != nil {
				logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth changes during model state reconciliation: %v", errPersist)
			}
			snapshot = auth.Clone()
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	m.selectorUpdateMu.Lock()
	defer m.selectorUpdateMu.Unlock()
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	previousSessionSelector, _ := m.selector.(*SessionAffinitySelector)
	nextSessionSelector, _ := selector.(*SessionAffinitySelector)
	m.selector = selector
	m.mu.Unlock()
	m.stopDynamicSelectors()
	if previousSessionSelector != nil && previousSessionSelector != nextSessionSelector {
		previousSessionSelector.Stop()
	}
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// SetCooldownStateStore swaps the independent runtime cooldown state store.
func (m *Manager) SetCooldownStateStore(store CooldownStateStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cooldownStore = store
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	previous, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	routingPolicyChanged := previous == nil ||
		!reflect.DeepEqual(previous.Routing.GroupStrategies, cfg.Routing.GroupStrategies) ||
		!reflect.DeepEqual(previous.Routing.ProviderStrategies, cfg.Routing.ProviderStrategies) ||
		!reflect.DeepEqual(previous.Routing.GroupSessionAffinity, cfg.Routing.GroupSessionAffinity)
	m.runtimeConfig.Store(cfg)
	if routingPolicyChanged {
		m.stopDynamicSelectors()
	}
	clearedCooldowns := m.clearDisabledCooldownStates(cfg)
	if !cfg.Home.Enabled {
		m.clearHomeRuntimeAuths()
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if clearedCooldowns {
		m.persistCooldownStates(context.Background())
	}
}

func (m *Manager) commercialModeEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.CommercialMode
}

func (m *Manager) cooldownDisabledForAuth(auth *Auth) bool {
	if m == nil {
		return quotaCooldownDisabledForAuth(auth)
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return quotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

func (m *Manager) clearDisabledCooldownStates(cfg *internalconfig.Config) bool {
	if m == nil {
		return false
	}
	now := time.Now()
	snapshots := make([]*Auth, 0)
	m.mu.Lock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if !quotaCooldownDisabledForAuthWithConfig(auth, cfg) && !auth.Disabled && auth.Status != StatusDisabled {
			continue
		}
		if clearCooldownStateForAuth(auth, now) {
			snapshots = append(snapshots, auth.Clone())
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		for _, snapshot := range snapshots {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	return len(snapshots) > 0
}

// RestoreCooldownStates restores unexpired persisted cooldown records into registered auths.
func (m *Manager) RestoreCooldownStates(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	records, errLoad := store.Load(ctx)
	if errLoad != nil {
		return errLoad
	}
	var policyRecords []GPTFirstEventPolicyStateRecord
	if policyStore, ok := store.(GPTFirstEventPolicyStateStore); ok {
		var errPolicyLoad error
		policyRecords, errPolicyLoad = policyStore.LoadGPTFirstEventPolicyStates(ctx)
		if errPolicyLoad != nil {
			return errPolicyLoad
		}
	}
	if m.gptFirstEventObserver != nil {
		m.gptFirstEventObserver.restorePolicyStates(policyRecords)
	}
	if len(records) == 0 {
		return nil
	}

	now := time.Now()
	authLevelRecords := make([]CooldownStateRecord, 0)
	snapshotsByID := make(map[string]*Auth)

	m.mu.Lock()
	for _, record := range records {
		if strings.TrimSpace(record.Model) == "" {
			authLevelRecords = append(authLevelRecords, record)
			continue
		}
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				snapshotsByID[auth.ID] = auth.Clone()
			}
		}
	}
	for _, record := range authLevelRecords {
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				snapshotsByID[auth.ID] = auth.Clone()
			}
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		for _, snapshot := range snapshotsByID {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	m.persistCooldownStates(ctx)
	return nil
}

func (m *Manager) restoreCooldownRecordLocked(record CooldownStateRecord, now time.Time) bool {
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" || record.NextRetryAfter.IsZero() || !record.NextRetryAfter.After(now) {
		return false
	}
	auth := m.auths[authID]
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || m.cooldownDisabledForAuth(auth) {
		return false
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	reason := strings.TrimSpace(record.Reason)
	model := strings.TrimSpace(record.Model)
	quota := record.Quota
	if quota.Exceeded && quota.NextRecoverAt.IsZero() {
		quota.NextRecoverAt = record.NextRetryAfter
	}

	if model == "" {
		// Older builds persisted generic upstream 429s as credential-wide
		// cooldowns. They cannot prove that the whole account is exhausted and
		// would incorrectly block sibling models after a restart.
		if isLegacyGenericRateLimitError(record.LastError) {
			return clearCredentialCooldownState(auth, now)
		}
		auth.Unavailable = true
		auth.Status = StatusError
		auth.NextRetryAfter = record.NextRetryAfter
		auth.Quota = quota
		auth.UpdatedAt = updatedAt
		if reason != "" {
			auth.StatusMessage = reason
		}
		auth.LastError = cloneError(record.LastError)
		return true
	}

	state := ensureModelState(auth, model)
	state.Unavailable = true
	state.Status = StatusError
	state.NextRetryAfter = record.NextRetryAfter
	state.Quota = quota
	state.UpdatedAt = updatedAt
	if reason != "" {
		state.StatusMessage = reason
	}
	state.LastError = cloneError(record.LastError)
	// A model record is intentionally independent from credential
	// availability. Credential-wide state is restored only from its own record.
	clearAggregatedAvailabilityUnlessExplicitCredentialCooldown(auth, now)
	return true
}

func clearCredentialCooldownState(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	changed := auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || auth.LastError != nil
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
	auth.LastError = nil
	auth.StatusMessage = ""
	if auth.Status != StatusDisabled {
		auth.Status = StatusActive
	}
	if changed {
		auth.UpdatedAt = now
	}
	return changed
}

func isLegacyGenericRateLimitError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusTooManyRequests {
		return false
	}
	if isAccountQuotaExhaustedResultError(err) || isBalanceExhaustedResultError(err) {
		return false
	}
	kind := failurecontract.Kind(strings.ToLower(strings.TrimSpace(err.Kind)))
	scope, _ := controlledFailureScope(err.Scope)
	return (kind == "" || kind == failurecontract.RateLimited) &&
		(scope == "" || scope == failurecontract.ScopeCredential)
}

func clearCooldownStateForAuth(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	changed := false
	if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero() {
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		auth.Quota = QuotaState{}
		auth.UpdatedAt = now
		changed = true
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() {
			state.Unavailable = false
			state.NextRetryAfter = time.Time{}
			state.Quota = QuotaState{}
			state.UpdatedAt = now
			changed = true
		}
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	return changed
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// ResetQuota clears quota/cooldown state for an auth and resumes registry routing.
func (m *Manager) ResetQuota(ctx context.Context, authID string) (*Auth, []string, error) {
	if m == nil {
		return nil, nil, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil, fmt.Errorf("auth id is required")
	}

	now := time.Now()
	var snapshot *Auth
	models := make([]string, 0)
	registeredModels := modelsForRegisteredAuth(authID)
	cooldownStateChanged := false

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return nil, nil, nil
	}

	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}

	for modelKey, state := range auth.ModelStates {
		if strings.TrimSpace(modelKey) == "" {
			continue
		}
		models = append(models, modelKey)
		if state != nil {
			resetModelState(state, now)
		}
	}
	if clearCooldownStateForAuth(auth, now) {
		if len(models) == 0 {
			models = append(models, registeredModels...)
		}
	} else if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}

	if len(models) == 0 {
		models = append(models, registeredModels...)
	}
	models = dedupeStrings(models)

	if !auth.Disabled && auth.Status != StatusDisabled && !hasModelError(auth, now) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.UpdatedAt = now
	if errPersist := m.persist(ctx, auth); errPersist != nil {
		m.mu.Unlock()
		return nil, nil, errPersist
	}
	snapshot = auth.Clone()
	if trackCooldownState {
		cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
	}
	m.mu.Unlock()

	for _, modelKey := range models {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(authID, modelKey)
		registry.GetGlobalRegistry().ResumeClientModel(authID, modelKey)
	}
	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if snapshot != nil && cooldownStateChanged {
		m.persistCooldownStates(ctx)
	}
	return snapshot, models, nil
}

func modelsForRegisteredAuth(authID string) []string {
	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	models := make([]string, 0, len(supportedModels))
	for _, supportedModel := range supportedModels {
		if supportedModel == nil || strings.TrimSpace(supportedModel.ID) == "" {
			continue
		}
		models = append(models, supportedModel.ID)
	}
	return models
}

func (m *Manager) persistCooldownStates(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	records, store := m.cooldownStateSnapshot()
	if store == nil {
		return
	}
	if errSave := store.Save(ctx, records); errSave != nil {
		logEntryWithRequestID(ctx).Warnf("failed to persist cooldown state: %v", errSave)
	}
}

func (m *Manager) persistGPTFirstEventPolicyStates(ctx context.Context) {
	if m == nil || m.gptFirstEventObserver == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.gptPolicyPersistMu.Lock()
	defer m.gptPolicyPersistMu.Unlock()
	if errCtx := ctx.Err(); errCtx != nil {
		return
	}
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	policyStore, ok := store.(GPTFirstEventPolicyStateStore)
	if !ok {
		return
	}
	records := m.gptFirstEventObserver.exportPolicyStates(time.Now())
	if errSave := policyStore.SaveGPTFirstEventPolicyStates(ctx, records); errSave != nil {
		logEntryWithRequestID(ctx).Warnf("failed to persist GPT first-event policy state: %v", errSave)
	}
}

func (m *Manager) cooldownStateSnapshot() ([]CooldownStateRecord, CooldownStateStore) {
	now := time.Now()
	records := make([]CooldownStateRecord, 0)

	m.mu.RLock()
	store := m.cooldownStore
	if store == nil {
		m.mu.RUnlock()
		return nil, nil
	}
	for _, auth := range m.auths {
		records = append(records, m.cooldownStateRecordsForAuthLocked(auth, now)...)
	}
	m.mu.RUnlock()

	sort.Slice(records, func(i, j int) bool {
		if records[i].Provider != records[j].Provider {
			return records[i].Provider < records[j].Provider
		}
		if records[i].AuthID != records[j].AuthID {
			return records[i].AuthID < records[j].AuthID
		}
		return records[i].Model < records[j].Model
	})
	return records, store
}

func (m *Manager) cooldownStateRecordsForAuthLocked(auth *Auth, now time.Time) []CooldownStateRecord {
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == StatusDisabled || m.cooldownDisabledForAuth(auth) {
		return nil
	}
	records := make([]CooldownStateRecord, 0, 1+len(auth.ModelStates))
	if record, ok := authCooldownStateRecord(auth, now); ok {
		records = append(records, record)
	}
	for model, state := range auth.ModelStates {
		if record, ok := modelCooldownStateRecord(auth, model, state, now); ok {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Model < records[j].Model
	})
	return records
}

func cooldownStateRecordsEqual(a, b []CooldownStateRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !cooldownStateRecordEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func cooldownStateRecordEqual(a, b CooldownStateRecord) bool {
	if a.Provider != b.Provider ||
		a.AuthID != b.AuthID ||
		a.AuthFile != b.AuthFile ||
		a.Model != b.Model ||
		a.Status != b.Status ||
		a.Reason != b.Reason ||
		!a.NextRetryAfter.Equal(b.NextRetryAfter) ||
		!a.UpdatedAt.Equal(b.UpdatedAt) ||
		!cooldownQuotaEqual(a.Quota, b.Quota) {
		return false
	}
	return cooldownErrorEqual(a.LastError, b.LastError)
}

func cooldownQuotaEqual(a, b QuotaState) bool {
	return a.Exceeded == b.Exceeded &&
		a.Reason == b.Reason &&
		a.BackoffLevel == b.BackoffLevel &&
		a.NextRecoverAt.Equal(b.NextRecoverAt)
}

func cooldownErrorEqual(a, b *Error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind &&
		a.Scope == b.Scope &&
		a.Code == b.Code &&
		a.Message == b.Message &&
		a.Retryable == b.Retryable &&
		a.HTTPStatus == b.HTTPStatus
}

func authCooldownStateRecord(auth *Auth, now time.Time) (CooldownStateRecord, bool) {
	if auth == nil || !auth.Unavailable || auth.NextRetryAfter.IsZero() || !auth.NextRetryAfter.After(now) {
		return CooldownStateRecord{}, false
	}
	return CooldownStateRecord{
		Provider:       strings.TrimSpace(auth.Provider),
		AuthID:         auth.ID,
		AuthFile:       cooldownAuthFile(auth),
		Status:         "cooling",
		NextRetryAfter: auth.NextRetryAfter,
		Reason:         cooldownReason(auth.StatusMessage, auth.Quota, auth.LastError),
		Quota:          auth.Quota,
		LastError:      cloneError(auth.LastError),
		UpdatedAt:      auth.UpdatedAt,
	}, true
}

func modelCooldownStateRecord(auth *Auth, model string, state *ModelState, now time.Time) (CooldownStateRecord, bool) {
	model = strings.TrimSpace(model)
	if auth == nil || state == nil || model == "" || !state.Unavailable || state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(now) {
		return CooldownStateRecord{}, false
	}
	return CooldownStateRecord{
		Provider:       strings.TrimSpace(auth.Provider),
		AuthID:         auth.ID,
		AuthFile:       cooldownAuthFile(auth),
		Model:          model,
		Status:         "cooling",
		NextRetryAfter: state.NextRetryAfter,
		Reason:         cooldownReason(state.StatusMessage, state.Quota, state.LastError),
		Quota:          state.Quota,
		LastError:      cloneError(state.LastError),
		UpdatedAt:      state.UpdatedAt,
	}, true
}

func cooldownReason(statusMessage string, quota QuotaState, lastErr *Error) string {
	if reason := strings.TrimSpace(quota.Reason); reason != "" {
		return reason
	}
	if statusMessage = strings.TrimSpace(statusMessage); statusMessage != "" {
		return statusMessage
	}
	if lastErr != nil {
		if code := strings.TrimSpace(lastErr.Code); code != "" {
			return code
		}
		if message := strings.TrimSpace(lastErr.Message); message != "" {
			return message
		}
	}
	return ""
}

// HomeEnabled reports whether the home control plane integration is enabled in the runtime config.
func (m *Manager) HomeEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.Home.Enabled
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	return preserveRequestedModelSuffix(requestedModel, resolved)
}

func isAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func isCodexProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
}

func isCodexAuth(auth *Auth) bool {
	return auth != nil && isCodexProviderName(auth.Provider)
}

func isCodexAPIKeyAuth(auth *Auth) bool {
	return isCodexAuth(auth) && isAPIKeyAuth(auth)
}

func isCodexOAuthCredential(auth *Auth) bool {
	return isCodexAuth(auth) &&
		!isCodexAPIKeyAuth(auth) &&
		(authAccessToken(auth) != "" || authHasRefreshCredential(auth))
}

func isOpenAICompatAPIKeyAuth(auth *Auth) bool {
	if !isAPIKeyAuth(auth) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

func openAICompatProviderKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" {
			return util.OpenAICompatibleProviderKey(providerKey)
		}
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			return util.OpenAICompatibleProviderKey(compatName)
		}
	}
	return util.OpenAICompatibleProviderKey(auth.Provider)
}

func openAICompatModelPoolKey(auth *Auth, requestedModel string) string {
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + openAICompatProviderKey(auth) + "|" + strings.ToLower(base)
}

func apiKeyModelPoolKey(auth *Auth, requestedModel string) string {
	if auth == nil {
		return ""
	}
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + strings.ToLower(strings.TrimSpace(auth.Provider)) + "|" + strings.ToLower(base)
}

func oauthModelAliasPoolKey(auth *Auth, requestedModel string) string {
	if auth == nil {
		return ""
	}
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + modelAliasChannel(auth) + "|" + strings.ToLower(base)
}

func (m *Manager) nextModelPoolOffset(key string, size int) int {
	if m == nil || size <= 1 {
		return 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelPoolOffsets == nil {
		m.modelPoolOffsets = make(map[string]int)
	}
	offset := m.modelPoolOffsets[key]
	if offset >= 2_147_483_640 {
		offset = 0
	}
	m.modelPoolOffsets[key] = offset + 1
	if size <= 0 {
		return 0
	}
	return offset % size
}

func rotateStrings(values []string, offset int) []string {
	if len(values) <= 1 {
		return values
	}
	if offset <= 0 {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}
	offset = offset % len(values)
	out := make([]string, 0, len(values))
	out = append(out, values[offset:]...)
	out = append(out, values[:offset]...)
	return out
}

func (m *Manager) resolveOpenAICompatUpstreamModelPool(auth *Auth, requestedModel string) []string {
	if m == nil || !isOpenAICompatAPIKeyAuth(auth) {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func preserveRequestedModelSuffix(requestedModel, resolved string) string {
	return preserveResolvedModelSuffix(resolved, thinking.ParseSuffix(requestedModel))
}

func (m *Manager) executionModelCandidates(auth *Auth, routeModel string) []string {
	if auth != nil && auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			return rewriteMiniMaxM3StandardRouteCandidates(routeModel, []string{homeModel})
		}
	}
	requestedModel := rewriteModelForAuth(routeModel, auth)
	if pool := m.resolveOAuthUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			requestedModel = pool[0]
		} else {
			offset := m.nextModelPoolOffset(oauthModelAliasPoolKey(auth, requestedModel), len(pool))
			return rewriteMiniMaxM3StandardRouteCandidates(routeModel, rotateStrings(pool, offset))
		}
	} else {
		requestedModel = m.applyOAuthModelAlias(auth, requestedModel)
	}
	if pool := m.resolveAPIKeyUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			return rewriteMiniMaxM3StandardRouteCandidates(routeModel, pool)
		}
		offset := m.nextModelPoolOffset(apiKeyModelPoolKey(auth, requestedModel), len(pool))
		return rewriteMiniMaxM3StandardRouteCandidates(routeModel, rotateStrings(pool, offset))
	}
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			return rewriteMiniMaxM3StandardRouteCandidates(routeModel, pool)
		}
		offset := m.nextModelPoolOffset(openAICompatModelPoolKey(auth, requestedModel), len(pool))
		return rewriteMiniMaxM3StandardRouteCandidates(routeModel, rotateStrings(pool, offset))
	}
	resolved := m.applyAPIKeyModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolved) == "" {
		resolved = requestedModel
	}
	return rewriteMiniMaxM3StandardRouteCandidates(routeModel, []string{resolved})
}

func (m *Manager) selectionModelForAuth(auth *Auth, routeModel string) string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	resolvedModel := m.applyOAuthModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolvedModel) == "" {
		resolvedModel = requestedModel
	}
	return resolvedModel
}

func (m *Manager) selectionModelKeyForAuth(auth *Auth, routeModel string) string {
	return canonicalModelKey(m.selectionModelForAuth(auth, routeModel))
}

func (m *Manager) stateModelForExecution(auth *Auth, routeModel, upstreamModel string, pooled bool) string {
	if auth != nil && auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" {
			if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
				return resolved
			}
			return homeModel
		}
	}
	stateModel := executionResultModel(routeModel, upstreamModel, pooled)
	selectionModel := m.selectionModelForAuth(auth, routeModel)
	if canonicalModelKey(selectionModel) == canonicalModelKey(upstreamModel) && strings.TrimSpace(selectionModel) != "" {
		return strings.TrimSpace(upstreamModel)
	}
	return stateModel
}

func executionResultModel(routeModel, upstreamModel string, pooled bool) string {
	if pooled {
		if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
			return resolved
		}
	}
	if requested := strings.TrimSpace(routeModel); requested != "" {
		return requested
	}
	return strings.TrimSpace(upstreamModel)
}

func (m *Manager) filterExecutionModels(auth *Auth, routeModel string, candidates []string, pooled bool) []string {
	if len(candidates) == 0 {
		return nil
	}
	if isCodexAuth(auth) &&
		(isCodexAPIKeyAuth(auth) || !hasUnauthorizedAuthFailure(auth)) {
		return append([]string(nil), candidates...)
	}
	now := time.Now()
	out := make([]string, 0, len(candidates))
	for _, upstreamModel := range candidates {
		stateModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
		blocked, _, _ := isAuthBlockedForModel(auth, stateModel, now)
		probeActive := auth != nil && m.halfOpenProbeActive(auth.ID, stateModel, now)
		if blocked && !probeActive {
			continue
		}
		out = append(out, upstreamModel)
	}
	return out
}

type cooldownFallbackCandidate struct {
	auth     *Auth
	model    string
	next     time.Time
	priority int
	quota    bool
}

type zeroEligibleProbeLease struct {
	requestID   string
	next        time.Time
	activeUntil time.Time
	done        chan struct{}
	waiters     int
	probeAuthID string
	probeModel  string
}

type zeroEligibleProbeWaitState uint8

const (
	zeroEligibleProbeWaitNone zeroEligibleProbeWaitState = iota
	zeroEligibleProbeWaitCompleted
	zeroEligibleProbeWaitTimedOut
	zeroEligibleProbeWaitRejected
)

func (m *Manager) preparedExecutionModels(auth *Auth, routeModel string) ([]string, bool) {
	candidates := m.executionModelCandidates(auth, routeModel)
	pooled := len(candidates) > 1
	return m.filterExecutionModels(auth, routeModel, candidates, pooled), pooled
}

func (m *Manager) preparedExecutionModelsForRequest(auth *Auth, routeModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]string, bool) {
	candidates := m.executionModelCandidates(auth, routeModel)
	pooled := len(candidates) > 1
	models := m.filterExecutionModels(auth, routeModel, candidates, pooled)
	models = filterImageInputUnsupportedExecutionModels(req, opts, models)
	models = filterMiniMaxM3RequiredExecutionModels(routeModel, req, opts, models)
	return models, pooled
}

func (m *Manager) prepareExecutionModels(auth *Auth, routeModel string) []string {
	models, _ := m.preparedExecutionModels(auth, routeModel)
	return models
}

func (m *Manager) availableAuthsForRouteModel(auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	return m.availableAuthsForRouteModelContext(context.Background(), auths, provider, routeModel, now)
}

func (m *Manager) availableAuthsForRouteModelContext(ctx context.Context, auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}
	availabilitySummary := selectionAvailabilitySummary{total: len(auths)}
	defer func() {
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			trace.recordSelectionAvailability(availabilitySummary)
		}
	}()

	gptRoute := isGPTRequestRoute(ctx, []string{provider}, routeModel)
	if gptRoute {
		m.configureZeroEligibleProbeScope(ctx, zeroEligibleProbeScopeKey(routeModel, auths))
	}
	spreadAcrossPriorities := selectorUsesSpread(m.selectorForAuths(auths))
	availableAll := make([]*Auth, 0, len(auths))
	availableByPriority := make(map[int][]*Auth)
	fallbackCandidates := make([]cooldownFallbackCandidate, 0, len(auths))
	cooldownCount := 0
	activeFallbackAvailable := false
	var earliest time.Time
	recordAvailable := func(candidate *Auth, checkModel string, includeHealth bool) {
		availableAll = append(availableAll, candidate)
		availabilitySummary.ready++
		if includeHealth && spreadAcrossPriorities {
			state := resolveHealthState(candidate, checkModel)
			if healthStateKnown(state) && recoveredHealthScore(state, now) < healthScoreDefault {
				availabilitySummary.healthDownweighted++
			}
		}
		if spreadAcrossPriorities {
			return
		}
		priority := effectiveSelectionPriorityForRoute(candidate, checkModel, now, includeHealth)
		availableByPriority[priority] = append(availableByPriority[priority], candidate)
	}
	recordTemporalBlock := func(candidate *Auth, checkModel string, next time.Time, quota, includeHealth bool) {
		cooldownCount++
		if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
			earliest = next
		}
		fallbackCandidates = append(fallbackCandidates, cooldownFallbackCandidate{
			auth:     candidate,
			model:    checkModel,
			next:     next,
			priority: effectiveSelectionPriorityForRoute(candidate, checkModel, now, includeHealth),
			quota:    quota,
		})
	}
	for _, candidate := range auths {
		checkModel := m.selectionModelForAuth(candidate, routeModel)
		includeHealth := gptRoute || !isGPTRetryRoute([]string{candidate.Provider}, checkModel)
		blocked, reason, next := isAuthBlockedForModelRoute(candidate, checkModel, now, includeHealth)
		if !blocked {
			if m.halfOpenProbeActive(candidate.ID, checkModel, now) &&
				(!gptRoute || !m.zeroEligibleProbeBlocksRequest(ctx, routeModel, now)) {
				activeFallbackAvailable = true
				recordAvailable(candidate, checkModel, includeHealth)
				continue
			}
			healthBlocked, healthNext := false, time.Time{}
			if includeHealth {
				healthBlocked, healthNext = m.healthSelectionBlocked(candidate, checkModel, now)
			}
			if healthBlocked {
				availabilitySummary.skippedBreaker++
				recordTemporalBlock(candidate, checkModel, healthNext, quotaCooldownForModel(candidate, checkModel), includeHealth)
				continue
			}
			recordAvailable(candidate, checkModel, includeHealth)
			continue
		}
		if (reason == blockReasonCooldown || reason == blockReasonOther) && !next.IsZero() {
			if m.halfOpenProbeActive(candidate.ID, checkModel, now) &&
				(!gptRoute || !m.zeroEligibleProbeBlocksRequest(ctx, routeModel, now)) {
				activeFallbackAvailable = true
				recordAvailable(candidate, checkModel, includeHealth)
				continue
			}
			recordTemporalBlock(candidate, checkModel, next, reason == blockReasonCooldown, includeHealth)
		}
		switch reason {
		case blockReasonDisabled:
			availabilitySummary.skippedDisabled++
		case blockReasonCooldown:
			availabilitySummary.skippedCooldown++
		case blockReasonOther:
			state := resolveHealthState(candidate, checkModel)
			if state.BreakerState == HealthBreakerOpen || state.BreakerState == HealthBreakerHalfOpen {
				availabilitySummary.skippedBreaker++
			} else {
				availabilitySummary.skippedUnavailable++
			}
		}
	}

	if activeFallbackAvailable && len(fallbackCandidates) > 0 {
		if fallback, _ := m.cooldownFallbackProbe(fallbackCandidates, now); fallback != nil {
			includeHealth := gptRoute || !isGPTRetryRoute([]string{fallback.auth.Provider}, fallback.model)
			recordAvailable(fallback.auth, fallback.model, includeHealth)
		}
	}

	if spreadAcrossPriorities {
		if len(availableAll) == 0 {
			if cooldownCount == len(auths) && !earliest.IsZero() {
				if fallback, probeNext := m.zeroEligibleFallbackProbe(ctx, routeModel, fallbackCandidates, now, gptRoute); fallback != nil {
					return []*Auth{fallback.auth}, nil
				} else if !probeNext.IsZero() && probeNext.Before(earliest) {
					earliest = probeNext
				}
				providerForError := provider
				if providerForError == "mixed" {
					providerForError = ""
				}
				resetIn := earliest.Sub(now)
				if resetIn < 0 {
					resetIn = 0
				}
				return nil, newModelCooldownError(routeModel, providerForError, resetIn)
			}
			return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
		}
		if len(availableAll) > 1 {
			sort.Slice(availableAll, func(i, j int) bool { return availableAll[i].ID < availableAll[j].ID })
		}
		return availableAll, nil
	}

	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			if fallback, probeNext := m.zeroEligibleFallbackProbe(ctx, routeModel, fallbackCandidates, now, gptRoute); fallback != nil {
				return []*Auth{fallback.auth}, nil
			} else if !probeNext.IsZero() && probeNext.Before(earliest) {
				earliest = probeNext
			}
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(routeModel, providerForError, resetIn)
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

func (m *Manager) cooldownFallbackProbe(candidates []cooldownFallbackCandidate, now time.Time) (*cooldownFallbackCandidate, time.Time) {
	if len(candidates) == 0 {
		return nil, time.Time{}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.next.IsZero() != right.next.IsZero() {
			return !left.next.IsZero()
		}
		if !left.next.Equal(right.next) {
			return left.next.Before(right.next)
		}
		if left.priority != right.priority {
			return left.priority > right.priority
		}
		leftID, rightID := "", ""
		if left.auth != nil {
			leftID = left.auth.ID
		}
		if right.auth != nil {
			rightID = right.auth.ID
		}
		return leftID < rightID
	})

	var probeNext time.Time
	for _, candidate := range candidates {
		if candidate.auth == nil {
			continue
		}
		interval, activeTTL := healthHalfOpenInterval, healthHalfOpenActiveTTL
		if candidate.quota {
			interval, activeTTL = quotaHalfOpenInterval, quotaHalfOpenActiveTTL
		}
		ok, next := m.reserveHalfOpenProbeWithWindow(candidate.auth.ID, candidate.model, now, interval, activeTTL)
		if ok {
			fallback := candidate
			return &fallback, time.Time{}
		}
		if !next.IsZero() && (probeNext.IsZero() || next.Before(probeNext)) {
			probeNext = next
		}
	}
	return nil, probeNext
}

func (m *Manager) zeroEligibleFallbackProbe(ctx context.Context, model string, candidates []cooldownFallbackCandidate, now time.Time, singleFlight bool) (*cooldownFallbackCandidate, time.Time) {
	if !singleFlight {
		return m.cooldownFallbackProbe(candidates, now)
	}
	ok, next := m.reserveZeroEligibleProbe(ctx, model, now)
	if !ok {
		return nil, next
	}
	fallback, probeNext := m.cooldownFallbackProbe(candidates, now)
	if fallback == nil {
		m.releaseZeroEligibleProbe(ctx, model)
	} else {
		m.bindZeroEligibleProbeRoute(ctx, model, fallback.auth.ID, fallback.model)
	}
	return fallback, probeNext
}

func quotaCooldownForModel(auth *Auth, model string) bool {
	if auth == nil {
		return false
	}
	if model != "" && len(auth.ModelStates) > 0 {
		state, ok := auth.ModelStates[model]
		if (!ok || state == nil) && model != "" {
			baseModel := canonicalModelKey(model)
			if baseModel != "" && baseModel != model {
				state, ok = auth.ModelStates[baseModel]
			}
		}
		if ok && state != nil {
			return state.Quota.Exceeded
		}
	}
	return auth.Quota.Exceeded
}

func copyTriedMap(src map[string]struct{}) map[string]struct{} {
	if len(src) == 0 {
		return make(map[string]struct{})
	}
	out := make(map[string]struct{}, len(src))
	for key := range src {
		out[key] = struct{}{}
	}
	return out
}

func halfOpenProbeKey(authID, model string) string {
	return strings.TrimSpace(authID) + "\x00" + canonicalModelKey(model)
}

func zeroEligibleProbeKey(model string) string {
	return canonicalModelKey(model)
}

func zeroEligibleProbeScopeKey(model string, auths []*Auth) string {
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		return ""
	}
	routeSet := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if routeKey := routingChannelBaseKey(auth); routeKey != "" {
			routeSet[routeKey] = struct{}{}
		}
	}
	if len(routeSet) == 0 {
		return modelKey
	}
	routes := make([]string, 0, len(routeSet))
	for routeKey := range routeSet {
		routes = append(routes, routeKey)
	}
	sort.Strings(routes)
	return modelKey + "\x00" + strings.Join(routes, "\x1f")
}

func requestIDFromAttemptTrace(ctx context.Context) string {
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		return trace.requestIDValue()
	}
	return ""
}

func zeroEligibleProbeKeyFromContext(ctx context.Context, model string) string {
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		if key := trace.zeroEligibleProbeKeyValue(); key != "" {
			return key
		}
	}
	return zeroEligibleProbeKey(model)
}

func (m *Manager) configureZeroEligibleProbeScope(ctx context.Context, key string) {
	trace := requestAttemptTraceFromContext(ctx)
	if m == nil || trace == nil || strings.TrimSpace(key) == "" {
		return
	}
	previous := trace.swapZeroEligibleProbeKey(key)
	if previous != "" && previous != key {
		m.releaseZeroEligibleProbeKey(ctx, previous)
	}
}

func (m *Manager) reserveZeroEligibleProbe(ctx context.Context, model string, now time.Time) (bool, time.Time) {
	if m == nil {
		return true, time.Time{}
	}
	key := zeroEligibleProbeKeyFromContext(ctx, model)
	if key == "" {
		return true, time.Time{}
	}
	requestID := requestIDFromAttemptTrace(ctx)
	m.zeroEligibleProbeMu.Lock()
	defer m.zeroEligibleProbeMu.Unlock()
	m.pruneZeroEligibleProbesLocked(now)
	lease := m.zeroEligibleProbes[key]
	if lease.activeUntil.After(now) {
		if requestID != "" && lease.requestID == requestID {
			return true, lease.next
		}
		return false, lease.activeUntil
	}
	if lease.done != nil {
		close(lease.done)
		lease.done = nil
		lease.waiters = 0
	}
	if lease.next.After(now) {
		m.zeroEligibleProbes[key] = lease
		return false, lease.next
	}
	lease = zeroEligibleProbeLease{
		requestID:   requestID,
		next:        now.Add(healthHalfOpenInterval),
		activeUntil: now.Add(gptZeroEligibleProbeActiveTTL),
		done:        make(chan struct{}),
	}
	m.zeroEligibleProbes[key] = lease
	return true, lease.next
}

func (m *Manager) waitForZeroEligibleProbe(ctx context.Context, model string, maxWait time.Duration) (zeroEligibleProbeWaitState, error) {
	if m == nil || maxWait <= 0 {
		return zeroEligibleProbeWaitNone, nil
	}
	key := zeroEligibleProbeKeyFromContext(ctx, model)
	if key == "" {
		return zeroEligibleProbeWaitNone, nil
	}
	now := time.Now()
	requestID := requestIDFromAttemptTrace(ctx)
	m.zeroEligibleProbeMu.Lock()
	lease, ok := m.zeroEligibleProbes[key]
	if !ok || lease.done == nil || !lease.activeUntil.After(now) ||
		(requestID != "" && lease.requestID == requestID) {
		m.zeroEligibleProbeMu.Unlock()
		return zeroEligibleProbeWaitNone, nil
	}
	if lease.waiters >= gptZeroEligibleProbeMaxWaiters {
		m.zeroEligibleProbeMu.Unlock()
		return zeroEligibleProbeWaitRejected, nil
	}
	wait := maxWait
	if remaining := lease.activeUntil.Sub(now); remaining < wait {
		wait = remaining
	}
	if wait <= 0 {
		m.zeroEligibleProbeMu.Unlock()
		return zeroEligibleProbeWaitTimedOut, nil
	}
	done := lease.done
	lease.waiters++
	m.zeroEligibleProbes[key] = lease
	m.zeroEligibleProbeMu.Unlock()

	defer func() {
		m.zeroEligibleProbeMu.Lock()
		current, exists := m.zeroEligibleProbes[key]
		if exists && current.done == done && current.waiters > 0 {
			current.waiters--
			m.zeroEligibleProbes[key] = current
		}
		m.zeroEligibleProbeMu.Unlock()
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return zeroEligibleProbeWaitCompleted, nil
	case <-timer.C:
		return zeroEligibleProbeWaitTimedOut, nil
	case <-ctx.Done():
		return zeroEligibleProbeWaitTimedOut, ctx.Err()
	}
}

func (m *Manager) zeroEligibleProbeBlocksRequest(ctx context.Context, model string, now time.Time) bool {
	if m == nil {
		return false
	}
	key := zeroEligibleProbeKeyFromContext(ctx, model)
	if key == "" {
		return false
	}
	requestID := requestIDFromAttemptTrace(ctx)
	m.zeroEligibleProbeMu.Lock()
	defer m.zeroEligibleProbeMu.Unlock()
	lease := m.zeroEligibleProbes[key]
	if !lease.activeUntil.After(now) {
		return false
	}
	return requestID == "" || lease.requestID == "" || lease.requestID != requestID
}

func (m *Manager) releaseZeroEligibleProbe(ctx context.Context, model string) {
	if m == nil {
		return
	}
	key := zeroEligibleProbeKeyFromContext(ctx, model)
	m.releaseZeroEligibleProbeKey(ctx, key)
}

func (m *Manager) bindZeroEligibleProbeRoute(ctx context.Context, model, authID, probeModel string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	key := zeroEligibleProbeKeyFromContext(ctx, model)
	requestID := requestIDFromAttemptTrace(ctx)
	if key == "" || requestID == "" {
		return
	}
	m.zeroEligibleProbeMu.Lock()
	lease, ok := m.zeroEligibleProbes[key]
	if ok && lease.requestID == requestID && lease.done != nil {
		lease.probeAuthID = strings.TrimSpace(authID)
		lease.probeModel = strings.TrimSpace(probeModel)
		m.zeroEligibleProbes[key] = lease
	}
	m.zeroEligibleProbeMu.Unlock()
}

func (m *Manager) releaseZeroEligibleProbeKey(ctx context.Context, key string) {
	if m == nil {
		return
	}
	requestID := requestIDFromAttemptTrace(ctx)
	if key == "" || requestID == "" {
		return
	}
	var done chan struct{}
	probeAuthID, probeModel := "", ""
	m.zeroEligibleProbeMu.Lock()
	lease, ok := m.zeroEligibleProbes[key]
	if ok && lease.requestID == requestID {
		done = lease.done
		probeAuthID = lease.probeAuthID
		probeModel = lease.probeModel
		lease.done = nil
		lease.requestID = ""
		lease.activeUntil = time.Time{}
		lease.waiters = 0
		lease.probeAuthID = ""
		lease.probeModel = ""
		m.zeroEligibleProbes[key] = lease
	}
	m.zeroEligibleProbeMu.Unlock()
	// The half-open bypass must disappear before waiters are released. This
	// also covers owner cancellation/early-return paths that never call
	// MarkResult.
	if probeAuthID != "" {
		m.releaseHalfOpenProbe(probeAuthID, probeModel)
	}
	if done != nil {
		close(done)
	}
}

func (m *Manager) pruneZeroEligibleProbesLocked(now time.Time) {
	if m == nil || len(m.zeroEligibleProbes) <= gptZeroEligibleProbeStateLimit {
		return
	}
	for key, lease := range m.zeroEligibleProbes {
		if lease.done == nil && !lease.activeUntil.After(now) && !lease.next.After(now) {
			delete(m.zeroEligibleProbes, key)
		}
	}
	for len(m.zeroEligibleProbes) > gptZeroEligibleProbeStateLimit {
		for key, lease := range m.zeroEligibleProbes {
			if lease.done == nil {
				delete(m.zeroEligibleProbes, key)
				break
			}
		}
		if len(m.zeroEligibleProbes) > gptZeroEligibleProbeStateLimit {
			break
		}
	}
}

func (m *Manager) nextHalfOpenProbeAt(authID, model string) time.Time {
	if m == nil {
		return time.Time{}
	}
	key := halfOpenProbeKey(authID, model)
	if key == "\x00" {
		return time.Time{}
	}
	m.halfOpenProbeMu.Lock()
	defer m.halfOpenProbeMu.Unlock()
	nowTime := time.Now()
	m.pruneHalfOpenProbeStateLocked(nowTime)
	if activeUntil := m.halfOpenProbeActiveUntil[key]; !activeUntil.IsZero() && !activeUntil.After(nowTime) {
		delete(m.halfOpenProbeActiveUntil, key)
	}
	next := m.halfOpenProbeNext[key]
	if !next.IsZero() && !next.After(nowTime) {
		delete(m.halfOpenProbeNext, key)
		return time.Time{}
	}
	return next
}

func (m *Manager) reserveHalfOpenProbe(authID, model string, now time.Time) (bool, time.Time) {
	return m.reserveHalfOpenProbeWithWindow(authID, model, now, healthHalfOpenInterval, healthHalfOpenActiveTTL)
}

func (m *Manager) reserveHalfOpenProbeWithWindow(authID, model string, now time.Time, interval, activeTTL time.Duration) (bool, time.Time) {
	if m == nil {
		return true, time.Time{}
	}
	if interval <= 0 {
		interval = healthHalfOpenInterval
	}
	if activeTTL <= 0 {
		activeTTL = healthHalfOpenActiveTTL
	}
	key := halfOpenProbeKey(authID, model)
	if key == "\x00" {
		return true, time.Time{}
	}
	m.halfOpenProbeMu.Lock()
	defer m.halfOpenProbeMu.Unlock()
	m.pruneHalfOpenProbeStateLocked(now)
	if next := m.halfOpenProbeNext[key]; !next.IsZero() && next.After(now) {
		return false, next
	}
	next := now.Add(interval)
	m.halfOpenProbeNext[key] = next
	if m.halfOpenProbeActiveUntil == nil {
		m.halfOpenProbeActiveUntil = make(map[string]time.Time)
	}
	m.halfOpenProbeActiveUntil[key] = now.Add(activeTTL)
	return true, next
}

func (m *Manager) halfOpenProbeActive(authID, model string, now time.Time) bool {
	if m == nil {
		return false
	}
	key := halfOpenProbeKey(authID, model)
	if key == "\x00" {
		return false
	}
	m.halfOpenProbeMu.Lock()
	defer m.halfOpenProbeMu.Unlock()
	m.pruneHalfOpenProbeStateLocked(now)
	activeUntil := m.halfOpenProbeActiveUntil[key]
	if activeUntil.IsZero() {
		return false
	}
	if !activeUntil.After(now) {
		delete(m.halfOpenProbeActiveUntil, key)
		return false
	}
	return true
}

func (m *Manager) releaseHalfOpenProbe(authID, model string) {
	if m == nil {
		return
	}
	key := halfOpenProbeKey(authID, model)
	if key == "\x00" {
		return
	}
	m.halfOpenProbeMu.Lock()
	delete(m.halfOpenProbeActiveUntil, key)
	m.halfOpenProbeMu.Unlock()
}

func (m *Manager) pruneHalfOpenProbeStateLocked(now time.Time) {
	if m == nil {
		return
	}
	if len(m.halfOpenProbeNext)+len(m.halfOpenProbeActiveUntil) <= halfOpenProbeStateLimit {
		return
	}
	for key, next := range m.halfOpenProbeNext {
		if next.IsZero() || !next.After(now) {
			delete(m.halfOpenProbeNext, key)
		}
	}
	for key, activeUntil := range m.halfOpenProbeActiveUntil {
		if activeUntil.IsZero() || !activeUntil.After(now) {
			delete(m.halfOpenProbeActiveUntil, key)
		}
	}
	for len(m.halfOpenProbeNext) > halfOpenProbeStateLimit {
		for key := range m.halfOpenProbeNext {
			delete(m.halfOpenProbeNext, key)
			break
		}
	}
	for len(m.halfOpenProbeActiveUntil) > halfOpenProbeStateLimit {
		for key := range m.halfOpenProbeActiveUntil {
			delete(m.halfOpenProbeActiveUntil, key)
			break
		}
	}
}

func (m *Manager) healthSelectionBlocked(auth *Auth, model string, now time.Time) (bool, time.Time) {
	if isCodexAuth(auth) && !isCodexAPIKeyAuth(auth) {
		return false, time.Time{}
	}
	state := resolveHealthState(auth, model)
	switch state.BreakerState {
	case HealthBreakerOpen:
		if !state.OpenUntil.IsZero() && state.OpenUntil.After(now) {
			return true, state.OpenUntil
		}
		fallthrough
	case HealthBreakerHalfOpen:
		if next := m.nextHalfOpenProbeAt(auth.ID, model); !next.IsZero() && next.After(now) {
			return true, next
		}
	}
	return false, time.Time{}
}

func (m *Manager) reserveGPTChannelAttempt(ctx context.Context, auth *Auth, provider, model string, now time.Time) bool {
	if m == nil || auth == nil || !isGPTRequestRoute(ctx, []string{provider, auth.Provider}, model) {
		return true
	}
	if isCodexAuth(auth) && !isCodexAPIKeyAuth(auth) {
		return true
	}
	key := gptChannelBreakerKey(auth, model)
	requestID := ""
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		requestID = trace.requestIDValue()
	}
	m.mu.Lock()
	state := m.gptChannelBreakers[key]
	allowed := state == nil || reserveCodexChannelProbe(state, requestID, now)
	m.mu.Unlock()
	return allowed
}

func (m *Manager) releaseGPTChannelAttempt(ctx context.Context, auth *Auth) {
	if m == nil || auth == nil {
		return
	}
	trace := requestAttemptTraceFromContext(ctx)
	requestID := trace.requestIDValue()
	if requestID == "" {
		return
	}
	baseKey := routingChannelBaseKey(auth)
	m.mu.Lock()
	for key, state := range m.gptChannelBreakers {
		if strings.HasPrefix(key, baseKey+"\x00model=") {
			releaseCodexChannelProbe(state, requestID)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) gptChannelBreakerOpen(auth *Auth, model string, now time.Time) bool {
	if m == nil || auth == nil {
		return false
	}
	m.mu.RLock()
	state := m.gptChannelBreakers[gptChannelBreakerKey(auth, model)]
	open := state != nil &&
		state.Health.BreakerState == HealthBreakerOpen &&
		state.Health.OpenUntil.After(now)
	m.mu.RUnlock()
	return open
}

func selectionArgForSelector(selector Selector, routeModel string) string {
	if isBuiltInSelector(selector) {
		return ""
	}
	return routeModel
}

func schedulerAttributeSensitive(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	normalized := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(key)
	compact := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(key)
	for _, fragment := range []string{
		"api_key",
		"apikey",
		"token",
		"secret",
		"cookie",
		"credential",
		"password",
		"storage",
		"authorization",
		"auth_header",
		"proxy_url",
	} {
		if strings.Contains(key, fragment) || strings.Contains(normalized, fragment) || strings.Contains(compact, fragment) {
			return true
		}
	}
	return false
}

func schedulerSafeAttributes(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		if schedulerAttributeSensitive(key) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneSchedulerAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func cloneAuthSlice(auths []*Auth) []*Auth {
	if len(auths) == 0 {
		return nil
	}
	out := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, auth.Clone())
	}
	return out
}

func schedulerAuthCandidates(auths []*Auth) []pluginapi.SchedulerAuthCandidate {
	if len(auths) == 0 {
		return nil
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, pluginapi.SchedulerAuthCandidate{
			ID:         auth.ID,
			Provider:   strings.ToLower(strings.TrimSpace(auth.Provider)),
			Priority:   authPriority(auth),
			Status:     string(auth.Status),
			Attributes: schedulerSafeAttributes(auth.Attributes),
		})
	}
	return out
}

func schedulerProviders(provider string, providers []string) []string {
	out := make([]string, 0, len(providers)+1)
	seen := make(map[string]struct{}, len(providers)+1)
	addProvider := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "mixed" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	addProvider(provider)
	for _, value := range providers {
		addProvider(value)
	}
	return out
}

func schedulerOptions(opts cliproxyexecutor.Options) pluginapi.SchedulerOptions {
	return pluginapi.SchedulerOptions{
		Headers:  cloneHTTPHeader(opts.Headers),
		Metadata: cloneSchedulerAnyMap(opts.Metadata),
	}
}

func pickSchedulerAuthByID(candidates []*Auth, authID string) *Auth {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID == authID {
			return candidate
		}
	}
	return nil
}

func builtinSchedulerStrategy(delegate string) (schedulerStrategy, bool) {
	switch strings.TrimSpace(delegate) {
	case pluginapi.SchedulerBuiltinRoundRobin:
		return schedulerStrategyRoundRobin, true
	case pluginapi.SchedulerBuiltinFillFirst:
		return schedulerStrategyFillFirst, true
	default:
		return schedulerStrategyCustom, false
	}
}

func (m *Manager) pickViaBuiltinScheduler(ctx context.Context, strategy schedulerStrategy, provider string, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, bool, error) {
	if m == nil || m.scheduler == nil {
		return nil, false, nil
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		var selected *Auth
		var errPick error
		if providerKey == "mixed" {
			selected, _, errPick = m.scheduler.pickMixedWithStrategy(ctx, providers, model, opts, tried, strategy)
			if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
				m.syncSchedulerOnPickFailure(time.Now())
				selected, _, errPick = m.scheduler.pickMixedWithStrategy(ctx, providers, model, opts, tried, strategy)
			}
		} else {
			selected, errPick = m.scheduler.pickSingleWithStrategy(ctx, providerKey, model, opts, tried, strategy)
			if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
				m.syncSchedulerOnPickFailure(time.Now())
				selected, errPick = m.scheduler.pickSingleWithStrategy(ctx, providerKey, model, opts, tried, strategy)
			}
		}
		if errPick != nil {
			return nil, true, errPick
		}
		if selected == nil {
			return nil, true, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		switch strategy {
		case schedulerStrategyFillFirst:
			recordSelectorReason(ctx, "builtin_scheduler_fill_first")
		default:
			recordSelectorReason(ctx, "builtin_scheduler_round_robin")
		}
		return selected, true, nil
	}
}

func (m *Manager) pickViaPluginScheduler(ctx context.Context, scheduler PluginScheduler, provider string, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, candidates []*Auth) (*Auth, bool, error) {
	if scheduler == nil || len(candidates) == 0 {
		return nil, false, nil
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	requestProvider := providerKey
	if providerKey == "mixed" {
		requestProvider = ""
	}
	req := pluginapi.SchedulerPickRequest{
		Provider:   requestProvider,
		Providers:  schedulerProviders(providerKey, providers),
		Model:      model,
		Stream:     opts.Stream,
		Options:    schedulerOptions(opts),
		Candidates: schedulerAuthCandidates(candidates),
	}
	resp, handled, errPick := scheduler.PickAuth(ctx, req)
	if errPick != nil {
		return nil, true, errPick
	}
	if !handled || !resp.Handled {
		return nil, false, nil
	}
	if selected := pickSchedulerAuthByID(candidates, resp.AuthID); selected != nil {
		recordSelectorReason(ctx, "plugin_scheduler")
		return selected, true, nil
	}

	strategy, okStrategy := builtinSchedulerStrategy(resp.DelegateBuiltin)
	if !okStrategy {
		return nil, false, nil
	}
	return m.pickViaBuiltinScheduler(ctx, strategy, providerKey, providers, model, opts, tried)
}

func (m *Manager) authSupportsRouteModel(registryRef *registry.ModelRegistry, auth *Auth, routeModel string) bool {
	if registryRef == nil || auth == nil {
		return true
	}
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return true
	}
	if registeredModels := registryRef.GetModelsForClient(auth.ID); len(registeredModels) == 0 {
		return !authRequiresRegisteredModels(auth)
	}
	if registryRef.ClientSupportsModel(auth.ID, routeKey) {
		return true
	}
	selectionKey := m.selectionModelKeyForAuth(auth, routeModel)
	return selectionKey != "" && selectionKey != routeKey && registryRef.ClientSupportsModel(auth.ID, selectionKey)
}

func authRequiresRegisteredModels(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "apikey") {
			return true
		}
	}
	accountKind, _ := auth.AccountInfo()
	return strings.EqualFold(accountKind, "api_key")
}

func closeStreamResult(result *cliproxyexecutor.StreamResult) {
	if result == nil {
		return
	}
	result.Close()
}

func streamResultHeaders(result *cliproxyexecutor.StreamResult) http.Header {
	if result == nil {
		return nil
	}
	return result.Headers
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

type streamBootstrapReadResult struct {
	buffered           []cliproxyexecutor.StreamChunk
	closed             bool
	firstPayloadDelay  time.Duration
	err                error
	emptyResponse      bool
	bufferLimitReached bool
}

type streamFirstEventDeadline struct {
	timeout time.Duration
	timer   *time.Timer
	state   atomic.Int32
}

func newStreamFirstEventDeadline(timeout time.Duration, cancel context.CancelCauseFunc) *streamFirstEventDeadline {
	if timeout <= 0 || cancel == nil {
		return nil
	}
	deadline := &streamFirstEventDeadline{timeout: timeout}
	deadline.timer = time.AfterFunc(timeout, func() {
		if deadline.state.CompareAndSwap(0, 2) {
			cancel(logging.NewCancellationCause(logging.CancelOriginUpstreamTimeout, context.DeadlineExceeded))
		}
	})
	return deadline
}

func (d *streamFirstEventDeadline) complete() bool {
	if d == nil {
		return true
	}
	if d.state.CompareAndSwap(0, 1) {
		d.timer.Stop()
		return true
	}
	return d.state.Load() != 2
}

func (d *streamFirstEventDeadline) timeoutError(err error) error {
	if d == nil || d.state.Load() != 2 {
		return err
	}
	if err == nil {
		err = context.DeadlineExceeded
	}
	return &failurecontract.Failure{
		Kind:          failurecontract.TransportError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusGatewayTimeout,
		ProviderCode:  "gpt_first_event_timeout",
		SemanticCode:  "gpt_first_event_timeout",
		SemanticType:  "gateway_timeout",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		Retryable:     true,
		Cause:         err,
		PublicMessage: fmt.Sprintf("GPT upstream produced no deliverable event within %s", d.timeout),
	}
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk, startedAt time.Time) ([]cliproxyexecutor.StreamChunk, bool, time.Duration, error) {
	result := readStreamBootstrapWithDelivery(ctx, ch, startedAt, nil, emptyResponsePolicy{})
	return result.buffered, result.closed, result.firstPayloadDelay, result.err
}

func readStreamBootstrapWithDelivery(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk, startedAt time.Time, tracker *deliverableOutputTracker, policy emptyResponsePolicy) streamBootstrapReadResult {
	if ch == nil {
		if tracker != nil {
			tracker.Finish()
		}
		return streamBootstrapReadResult{
			closed:        true,
			emptyResponse: tracker != nil && !tracker.deliverable,
		}
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return streamBootstrapReadResult{err: ctx.Err()}
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			if tracker != nil {
				tracker.Finish()
			}
			return streamBootstrapReadResult{
				buffered:          buffered,
				closed:            true,
				firstPayloadDelay: time.Since(startedAt),
				emptyResponse:     tracker != nil && !tracker.deliverable,
			}
		}
		if chunk.Err != nil {
			return streamBootstrapReadResult{err: chunk.Err}
		}
		buffered = append(buffered, chunk)
		if tracker != nil {
			tracker.Observe(chunk.Payload)
			if tracker.deliverable {
				return streamBootstrapReadResult{
					buffered:          buffered,
					firstPayloadDelay: time.Since(startedAt),
				}
			}
			if tracker.bytesReceived >= policy.maxBytes || tracker.chunksCount >= policy.maxEvents {
				return streamBootstrapReadResult{
					buffered:           buffered,
					firstPayloadDelay:  time.Since(startedAt),
					bufferLimitReached: true,
				}
			}
			continue
		}
		if len(chunk.Payload) > 0 && !streamBootstrapPayloadIsMetadataOnly(chunk.Payload) {
			return streamBootstrapReadResult{
				buffered:          buffered,
				firstPayloadDelay: time.Since(startedAt),
			}
		}
	}
}

type streamExecutionLogMeta struct {
	requestedModel   string
	upstreamModel    string
	provider         string
	executor         string
	requestPath      string
	compatKind       string
	compatKindSource string
	compatMapping    string
	toolShape        coreusage.ToolShape
	compactionIntent cliproxyexecutor.CompactionIntent
}

type streamRuntimeStats struct {
	summaryFields          streamSummaryFields
	chunksCount            int
	bytesOut               int
	upstreamChunkWait      time.Duration
	upstreamChunkWaitCount int
}

func (s *streamRuntimeStats) observe(payload []byte) {
	if s == nil || len(payload) == 0 {
		return
	}
	s.chunksCount++
	s.bytesOut += len(payload)
	s.summaryFields.observePayload(payload)
}

type streamRequestRuntime struct {
	meta               streamExecutionLogMeta
	responseModelAlias string
	logger             *log.Entry
	trace              *requestAttemptTrace
	attempt            coreusage.RequestAttempt
	done               <-chan struct{}
	trackerID          uint64
	stats              streamRuntimeStats
}

func newStreamRequestRuntime(ctx context.Context, meta streamExecutionLogMeta, responseModelAlias string, trackerID uint64) streamRequestRuntime {
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	return streamRequestRuntime{
		meta:               meta,
		responseModelAlias: responseModelAlias,
		logger:             logEntryWithRequestID(ctx),
		trace:              requestAttemptTraceFromContext(ctx),
		attempt:            coreusage.RequestAttemptFromContext(ctx),
		done:               done,
		trackerID:          trackerID,
	}
}

func (r *streamRequestRuntime) rewritePayload(payload []byte) []byte {
	if r == nil || len(payload) == 0 || r.responseModelAlias == "" {
		return payload
	}
	return rewriteResponsePayloadModelAlias(payload, r.responseModelAlias)
}

func (r *streamRequestRuntime) recordFinalStatus(status int) {
	if r == nil || r.trace == nil {
		return
	}
	r.trace.recordFinalStatus(status)
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, meta streamExecutionLogMeta, responseModelAlias string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, cancelUpstream func(), startedAt time.Time, firstPayloadDelay time.Duration, releaseSlot func(), deliveryAudit *emptyResponseAudit) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			if cancelUpstream != nil {
				cancelUpstream()
			}
			if releaseSlot != nil {
				releaseSlot()
			}
		})
	}
	trackerID := uint64(0)
	if m != nil && m.activeStreams != nil {
		trackerID = m.activeStreams.start(meta.provider, meta.upstreamModel, meta.requestPath, startedAt)
	}
	runtime := newStreamRequestRuntime(ctx, meta, responseModelAlias, trackerID)
	go func() {
		defer close(out)
		defer cancel()
		selectorModel := meta.upstreamModel
		if requestedModel := coreusage.RequestedModelAliasFromContext(ctx); requestedModel != "" {
			selectorModel = requestedModel
		}
		resultRecorded := false
		defer func() {
			if !resultRecorded {
				m.markSelectorLoadDone(ctx, auth.ID, selectorModel)
			}
			m.releaseGPTChannelAttempt(ctx, auth)
		}()
		if m != nil && m.activeStreams != nil {
			defer m.activeStreams.stop(runtime.trackerID)
		}
		var failed bool
		var clientGone bool
		defer func() {
			if deliveryAudit != nil && deliveryAudit.tracker != nil {
				deliveryAudit.tracker.Finish()
				if !failed && !clientGone && !deliveryAudit.tracker.deliverable {
					m.logEmptyResponseDetected(
						ctx,
						auth,
						meta.provider,
						deliveryAudit.routeModel,
						deliveryAudit.opts,
						deliveryAudit.tracker,
						true,
						false,
						int64(runtime.stats.bytesOut),
					)
				}
			}
			totalDuration := time.Since(startedAt)
			streamDuration := totalDuration - firstPayloadDelay
			if streamDuration < 0 {
				streamDuration = 0
			}
			finishReason := runtime.stats.summaryFields.finishReason
			if finishReason == "" {
				finishReason = "done"
				if failed {
					finishReason = "error"
				} else if clientGone {
					finishReason = "client_gone"
				}
			}
			record := internalusage.StreamSummaryRecord{
				TimeToFirstChunkMs:         firstPayloadDelay.Milliseconds(),
				UpstreamChunkWaitMs:        runtime.stats.upstreamChunkWait.Milliseconds(),
				UpstreamChunkWaitCount:     runtime.stats.upstreamChunkWaitCount,
				StreamDurationMs:           streamDuration.Milliseconds(),
				TotalDurationMs:            totalDuration.Milliseconds(),
				ChunksCount:                runtime.stats.chunksCount,
				BytesOut:                   int64(runtime.stats.bytesOut),
				StreamOutputTokens:         runtime.stats.summaryFields.outputTokens,
				StreamOutputTokensObserved: runtime.stats.summaryFields.outputTokensObserved,
				ClientGone:                 clientGone && !failed,
				FinishReason:               finishReason,
			}
			if completeStreamSummaryUpstream(ctx, meta, runtime.attempt, record) {
				return
			}
			logAndPersistStreamSummary(ctx, meta, runtime.attempt, record)
		}()
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				chunk.Err = normalizeOpaqueUpstream500Failure(chunk.Err, failurecontract.StreamPhaseAfterOutput, true)
				if !cliproxyexecutor.IsRemoteCompactionIntent(meta.compactionIntent) && isGPTRetryRoute([]string{meta.provider}, meta.requestedModel) {
					chunk.Err = normalizeOpaqueGPTAttemptFailure(chunk.Err, failurecontract.StreamPhaseAfterOutput, true)
				}
				rerr := resultErrorFromCause(chunk.Err)
				m.markExecutionResult(ctx, auth, selectorModel, Result{AuthID: auth.ID, Provider: meta.provider, Model: meta.upstreamModel, Success: false, Duration: time.Since(startedAt), TTFT: firstPayloadDelay, Error: rerr, Cause: chunk.Err}, meta.compactionIntent)
				resultRecorded = true
				runtime.recordFinalStatus(statusCodeFromError(chunk.Err))
				if shouldEvictUnauthorizedError(chunk.Err) {
					if errEvict := m.evictUnauthorizedAuth(ctx, auth, meta.provider, meta.upstreamModel); errEvict != nil {
						runtime.logger.Warnf("evict unauthorized auth %s failed: %v", auth.ID, errEvict)
					}
				}
			}
			if !forward {
				return false
			}
			if len(chunk.Payload) > 0 {
				if deliveryAudit != nil && deliveryAudit.tracker != nil {
					deliveryAudit.tracker.Observe(chunk.Payload)
				}
				chunk.Payload = runtime.rewritePayload(chunk.Payload)
				runtime.stats.observe(chunk.Payload)
			}
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-runtime.done:
				if !failed {
					clientGone = true
				}
				forward = false
				cancel()
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				cancel()
				return
			}
		}
		for {
			var (
				chunk cliproxyexecutor.StreamChunk
				ok    bool
			)
			waitStartedAt := time.Now()
			if ctx == nil {
				chunk, ok = <-remaining
			} else {
				select {
				case <-ctx.Done():
					if !failed {
						clientGone = true
					}
					cancel()
					return
				case chunk, ok = <-remaining:
				}
			}
			if !ok {
				break
			}
			runtime.stats.upstreamChunkWait += time.Since(waitStartedAt)
			runtime.stats.upstreamChunkWaitCount++
			if ok := emit(chunk); !ok {
				cancel()
				return
			}
		}
		if !failed {
			m.markExecutionResult(ctx, auth, selectorModel, Result{AuthID: auth.ID, Provider: meta.provider, Model: meta.upstreamModel, Success: true, Duration: time.Since(startedAt), TTFT: firstPayloadDelay}, meta.compactionIntent)
			resultRecorded = true
			if trace := requestAttemptTraceFromContext(ctx); trace != nil {
				trace.recordFinalStatus(http.StatusOK)
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out, Cancel: cancel}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, routeProviders []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel string, execModels []string, pooled bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	compactionIntent := compactionIntentFromRequest(req, opts)
	remoteCompaction := cliproxyexecutor.IsRemoteCompactionIntent(compactionIntent)
	gptRoute := isGPTRetryRoute(routeProviders, routeModel) && !remoteCompaction
	if remoteCompaction && len(execModels) > 1 {
		execModels = execModels[:1]
	}
	var lastErr error
	didRefreshOnUnauthorized := false
	for idx, execModel := range execModels {
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			if remaining, tracked := trace.gptFirstEventRemainingBudget(); tracked && remaining <= 0 {
				trace.markGPTFirstEventBudgetExhausted()
				return nil, newGPTFirstEventWaitBudgetError()
			}
		}
		if idx > 0 && gptRoute &&
			!m.reserveGPTChannelAttempt(ctx, auth, provider, routeModel, time.Now()) {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			m.releaseGPTChannelAttempt(ctx, auth)
			if lastErr != nil {
				return nil, markGPTChannelFailoverError(lastErr)
			}
			return nil, &Error{Code: "auth_not_found", Message: "GPT channel unavailable"}
		}
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		execOpts := withSelectedCompactionCapability(execReq, opts, auth)
		execReq, execOpts = applyRequestAfterAuthInterceptor(ctx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		releaseSlot, errReserve := m.reserveCodexModelSlot(provider, resultModel)
		if errReserve != nil {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			m.releaseGPTChannelAttempt(ctx, auth)
			return nil, errReserve
		}
		logRoutePlan(ctx, auth, provider, routeModel, resultModel, execModel, execOpts, executor, "stream")
		startedAt := time.Now()
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			trace.recordExecution(provider, resultModel, providerExecutorName(executor))
		}
		attemptCtx, attemptCancel := context.WithCancelCause(ctx)
		firstEventTimeout := m.firstEventTimeoutForRoute(ctx, routeProviders, routeModel)
		firstEventDeadline := newStreamFirstEventDeadline(firstEventTimeout, attemptCancel)
		var streamResult *cliproxyexecutor.StreamResult
		var cleanupOnce sync.Once
		cleanupAttempt := func() {
			cleanupOnce.Do(func() {
				firstEventDeadline.complete()
				attemptCancel(logging.NewCancellationCause(logging.CancelOriginInternalAbort, context.Canceled))
				closeStreamResult(streamResult)
				releaseSlot()
			})
		}
		streamResult, errStream := executor.ExecuteStream(attemptCtx, auth, execReq, execOpts)
		errStream = firstEventDeadline.timeoutError(errStream)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				cleanupAttempt()
				m.markSelectorLoadDone(ctx, auth.ID, routeModel)
				m.releaseGPTChannelAttempt(ctx, auth)
				return nil, newCallerRequestFailure(errCtx)
			}
			if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(ctx, auth, errStream, didRefreshOnUnauthorized); okRefresh {
				closeStreamResult(streamResult)
				auth = refreshed
				didRefreshOnUnauthorized = true
				streamResult, errStream = executor.ExecuteStream(attemptCtx, auth, execReq, execOpts)
				errStream = firstEventDeadline.timeoutError(errStream)
				if errStream != nil {
					if errCtx := ctx.Err(); errCtx != nil {
						cleanupAttempt()
						m.markSelectorLoadDone(ctx, auth.ID, routeModel)
						m.releaseGPTChannelAttempt(ctx, auth)
						return nil, newCallerRequestFailure(errCtx)
					}
				}
			}
		}
		if errStream == nil && (streamResult == nil || streamResult.Chunks == nil) {
			errStream = &Error{
				Code:      "invalid_stream_result",
				Message:   "upstream returned an invalid stream result",
				Retryable: true,
			}
		}
		if errStream != nil {
			errStream = normalizeOpaqueUpstream500Failure(errStream, failurecontract.StreamPhaseBeforeOutput, false)
			if gptRoute {
				errStream = normalizeOpaqueGPTAttemptFailure(errStream, failurecontract.StreamPhaseBeforeOutput, false)
			}
		}
		if errStream != nil {
			cleanupAttempt()
			rerr := resultErrorFromCause(errStream)
			elapsed := time.Since(startedAt)
			if gptRoute {
				m.recordGPTFirstEventAttempt(ctx, routeModel, routingChannelBaseKey(auth), firstEventTimeout, elapsed, false, errStream)
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Duration: elapsed, TTFT: elapsed, Error: rerr, Cause: errStream}
			result.RetryAfter = retryAfterFromError(errStream)
			channelFailover := shouldFailoverGPTChannel(errStream, routeProviders, routeModel)
			directFailover := channelFailover
			unauthorized := shouldEvictUnauthorizedError(errStream)
			requestInvalid := isRequestInvalidError(errStream)
			routeFallback := requestInvalid && shouldFallbackRequestScopedRouteErrorForRequest(routeModel, opts, errStream)
			result.keepSelectorLease = idx < len(execModels)-1 &&
				!channelFailover &&
				!unauthorized &&
				(!requestInvalid || routeFallback)
			m.markExecutionResult(ctx, auth, routeModel, result, compactionIntent)
			channelFailover = channelFailover ||
				(gptRoute && m.gptChannelBreakerOpen(auth, routeModel, time.Now()))
			if channelFailover && result.keepSelectorLease {
				m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			}
			if trace := requestAttemptTraceFromContext(ctx); trace != nil {
				trace.recordFinalStatus(statusCodeFromError(errStream))
			}
			m.recordContentSafetyRequest(ctx, auth, provider, routeModel, execModel, opts, req.Payload, errStream)
			if channelFailover {
				if !directFailover {
					return nil, markGPTChannelFailoverError(errStream)
				}
				return nil, errStream
			}
			if unauthorized {
				return nil, errStream
			}
			if requestInvalid {
				if routeFallback {
					lastErr = errStream
					if idx < len(execModels)-1 {
						if trace := requestAttemptTraceFromContext(ctx); trace != nil {
							trace.recordFallback()
						}
					}
					continue
				}
				return nil, errStream
			}
			lastErr = errStream
			if idx < len(execModels)-1 {
				if trace := requestAttemptTraceFromContext(ctx); trace != nil {
					trace.recordFallback()
				}
			}
			continue
		}

		emptyPolicy := m.emptyResponsePolicy(routeModel, opts)
		var deliveryTracker *deliverableOutputTracker
		if emptyPolicy.enabled && !emptyPolicy.auditOnly {
			deliveryTracker = newDeliverableOutputTracker(emptyPolicy.format)
		}
		bootstrapRead := readStreamBootstrapWithDelivery(attemptCtx, streamResult.Chunks, startedAt, deliveryTracker, emptyPolicy)
		buffered := bootstrapRead.buffered
		closed := bootstrapRead.closed
		firstPayloadDelay := bootstrapRead.firstPayloadDelay
		bootstrapErr := firstEventDeadline.timeoutError(bootstrapRead.err)
		if bootstrapErr != nil && firstPayloadDelay <= 0 {
			firstPayloadDelay = time.Since(startedAt)
		}
		if bootstrapRead.emptyResponse {
			m.logEmptyResponseDetected(attemptCtx, auth, provider, routeModel, opts, deliveryTracker, false, false, 0)
			bootstrapErr = newEmptyUpstreamResponseFailure()
		}
		if bootstrapRead.bufferLimitReached {
			m.logEmptyResponseBufferLimit(attemptCtx, auth, provider, routeModel, opts, deliveryTracker, emptyPolicy)
			bootstrapErr = newEmptyUpstreamResponseFailure()
		}
		if bootstrapErr != nil {
			if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(ctx, auth, bootstrapErr, didRefreshOnUnauthorized); okRefresh {
				closeStreamResult(streamResult)
				auth = refreshed
				didRefreshOnUnauthorized = true
				streamResult, bootstrapErr = executor.ExecuteStream(attemptCtx, auth, execReq, execOpts)
				bootstrapErr = firstEventDeadline.timeoutError(bootstrapErr)
				buffered = nil
				closed = false
				firstPayloadDelay = time.Since(startedAt)
				if bootstrapErr == nil {
					if streamResult == nil || streamResult.Chunks == nil {
						bootstrapErr = &Error{
							Code:      "invalid_stream_result",
							Message:   "upstream returned an invalid stream result",
							Retryable: true,
						}
					} else {
						deliveryTracker = nil
						if emptyPolicy.enabled && !emptyPolicy.auditOnly {
							deliveryTracker = newDeliverableOutputTracker(emptyPolicy.format)
						}
						bootstrapRead = readStreamBootstrapWithDelivery(attemptCtx, streamResult.Chunks, startedAt, deliveryTracker, emptyPolicy)
						buffered = bootstrapRead.buffered
						closed = bootstrapRead.closed
						firstPayloadDelay = bootstrapRead.firstPayloadDelay
						bootstrapErr = firstEventDeadline.timeoutError(bootstrapRead.err)
						if bootstrapErr != nil && firstPayloadDelay <= 0 {
							firstPayloadDelay = time.Since(startedAt)
						}
						if bootstrapRead.emptyResponse {
							m.logEmptyResponseDetected(attemptCtx, auth, provider, routeModel, opts, deliveryTracker, false, false, 0)
							bootstrapErr = newEmptyUpstreamResponseFailure()
						}
						if bootstrapRead.bufferLimitReached {
							m.logEmptyResponseBufferLimit(attemptCtx, auth, provider, routeModel, opts, deliveryTracker, emptyPolicy)
							bootstrapErr = newEmptyUpstreamResponseFailure()
						}
					}
				}
			}
		}
		if bootstrapErr == nil && !firstEventDeadline.complete() {
			bootstrapErr = firstEventDeadline.timeoutError(nil)
			if firstPayloadDelay <= 0 {
				firstPayloadDelay = time.Since(startedAt)
			}
		}
		if bootstrapErr != nil {
			bootstrapErr = normalizeOpaqueUpstream500Failure(bootstrapErr, failurecontract.StreamPhaseBeforeOutput, false)
			if gptRoute {
				bootstrapErr = normalizeOpaqueGPTAttemptFailure(bootstrapErr, failurecontract.StreamPhaseBeforeOutput, false)
			}
		}
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				cleanupAttempt()
				m.markSelectorLoadDone(ctx, auth.ID, routeModel)
				m.releaseGPTChannelAttempt(ctx, auth)
				return nil, newCallerRequestFailure(errCtx)
			}
			if gptRoute {
				m.recordGPTFirstEventAttempt(ctx, routeModel, routingChannelBaseKey(auth), firstEventTimeout, firstPayloadDelay, false, bootstrapErr)
			}
			rerr := resultErrorFromCause(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Duration: time.Since(startedAt), TTFT: firstPayloadDelay, Error: rerr, Cause: bootstrapErr}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			channelFailover := shouldFailoverGPTChannel(bootstrapErr, routeProviders, routeModel)
			directFailover := channelFailover
			unauthorized := shouldEvictUnauthorizedError(bootstrapErr)
			requestInvalid := isRequestInvalidError(bootstrapErr)
			routeFallback := requestInvalid && shouldFallbackRequestScopedRouteErrorForRequest(routeModel, opts, bootstrapErr)
			result.keepSelectorLease = idx < len(execModels)-1 &&
				!channelFailover &&
				!unauthorized &&
				(!requestInvalid || routeFallback)
			m.markExecutionResult(ctx, auth, routeModel, result, compactionIntent)
			channelFailover = channelFailover ||
				(gptRoute && m.gptChannelBreakerOpen(auth, routeModel, time.Now()))
			if channelFailover && result.keepSelectorLease {
				m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			}
			if trace := requestAttemptTraceFromContext(ctx); trace != nil {
				trace.recordFinalStatus(statusCodeFromError(bootstrapErr))
			}
			if requestInvalid {
				m.recordContentSafetyRequest(ctx, auth, provider, routeModel, execModel, opts, req.Payload, bootstrapErr)
			}
			cleanupAttempt()
			if channelFailover || unauthorized {
				if channelFailover && !directFailover {
					bootstrapErr = markGPTChannelFailoverError(bootstrapErr)
				}
				return nil, newStreamBootstrapError(bootstrapErr, streamResultHeaders(streamResult))
			}
			if requestInvalid {
				if routeFallback {
					lastErr = bootstrapErr
					if idx < len(execModels)-1 {
						if trace := requestAttemptTraceFromContext(ctx); trace != nil {
							trace.recordFallback()
						}
					}
					continue
				}
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				lastErr = bootstrapErr
				if trace := requestAttemptTraceFromContext(ctx); trace != nil {
					trace.recordFallback()
				}
				continue
			}
			return nil, newStreamBootstrapError(bootstrapErr, streamResultHeaders(streamResult))
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			if gptRoute {
				m.recordGPTFirstEventAttempt(ctx, routeModel, routingChannelBaseKey(auth), firstEventTimeout, firstPayloadDelay, false, emptyErr)
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Duration: time.Since(startedAt), TTFT: firstPayloadDelay, Error: emptyErr}
			result.keepSelectorLease = idx < len(execModels)-1
			m.markExecutionResult(ctx, auth, routeModel, result, compactionIntent)
			channelFailover := gptRoute && m.gptChannelBreakerOpen(auth, routeModel, time.Now())
			if channelFailover && result.keepSelectorLease {
				m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			}
			if trace := requestAttemptTraceFromContext(ctx); trace != nil {
				trace.recordFinalStatus(statusCodeFromError(emptyErr))
			}
			cleanupAttempt()
			if channelFailover {
				return nil, newStreamBootstrapError(markGPTChannelFailoverError(emptyErr), streamResultHeaders(streamResult))
			}
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				if trace := requestAttemptTraceFromContext(ctx); trace != nil {
					trace.recordFallback()
				}
				continue
			}
			return nil, newStreamBootstrapError(emptyErr, streamResultHeaders(streamResult))
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		responseModelAlias := m.requestedResponseModelAlias(auth, opts, routeModel, execModel)
		requestedModel := metadataString(opts.Metadata, cliproxyexecutor.RequestedModelMetadataKey)
		if requestedModel == "" {
			requestedModel = routeModel
		}
		compatKind, compatKindSource := routePlanCompatKindWithSource(auth)
		streamMeta := streamExecutionLogMeta{
			requestedModel:   requestedModel,
			upstreamModel:    execModel,
			provider:         provider,
			executor:         providerExecutorName(executor),
			requestPath:      metadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey),
			compatKind:       compatKind,
			compatKindSource: compatKindSource,
			compatMapping:    routePlanCompatMapping(requestedModel, execModel, compatKind),
			toolShape:        toolShapeFromOptions(opts),
			compactionIntent: compactionIntent,
		}
		if gptRoute {
			m.recordGPTFirstEventAttempt(ctx, routeModel, routingChannelBaseKey(auth), firstEventTimeout, firstPayloadDelay, true, nil)
		}
		return m.wrapStreamResult(
			attemptCtx,
			auth.Clone(),
			streamMeta,
			responseModelAlias,
			streamResult.Headers,
			buffered,
			remaining,
			cleanupAttempt,
			startedAt,
			firstPayloadDelay,
			nil,
			newEmptyResponseAudit(emptyPolicy, routeModel, opts),
		), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, lastErr
}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

// RefreshAPIKeyModelAlias rebuilds the API-key model alias table from the current runtime config.
func (m *Manager) RefreshAPIKeyModelAlias() {
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "gemini":
			if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "vertex":
			if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// SetGPTFirstEventTimeout updates the GPT stream bootstrap deadline.
// Zero uses the safe default and a negative value disables the deadline.
func (m *Manager) SetGPTFirstEventTimeout(timeout time.Duration) {
	if m == nil {
		return
	}
	if timeout == 0 {
		timeout = defaultGPTFirstEventTimeout
	} else if timeout < 0 {
		timeout = 0
	}
	m.gptFirstEventTimeout.Store(timeout.Nanoseconds())
}

func (m *Manager) firstEventTimeoutForRoute(ctx context.Context, providers []string, model string) time.Duration {
	if m == nil || !isGPTRequestRoute(ctx, providers, model) {
		return 0
	}
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		if policy, configured := trace.gptFirstEventPolicyValue(); configured {
			timeout := time.Duration(policy.EnforcedTimeoutMs) * time.Millisecond
			if remaining, tracked := trace.gptFirstEventRemainingBudget(); tracked && remaining < timeout {
				timeout = remaining
			}
			return timeout
		}
	}
	timeout := time.Duration(m.gptFirstEventTimeout.Load())
	if timeout < 0 {
		return 0
	}
	return timeout
}

// SetRetryQueueDelay updates the delay inserted before fallback credential retries.
func (m *Manager) SetRetryQueueDelay(delay time.Duration) {
	if m == nil {
		return
	}
	if delay < 0 {
		delay = 0
	}
	m.retryQueueDelay.Store(delay.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	now := time.Now()
	clearedCooldown := false
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		clearedCooldown = clearCooldownStateForAuth(auth, now)
	}
	auth.EnsureIndex()
	if err := m.persist(ctx, auth); err != nil {
		return nil, err
	}
	authClone := auth.Clone()
	m.mu.Lock()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	if clearedCooldown {
		m.persistCooldownStates(ctx)
	}
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return nil, nil
	}
	if !auth.indexAssigned && auth.Index == "" {
		auth.Index = existing.Index
		auth.indexAssigned = existing.indexAssigned
	}
	auth.Success = existing.Success
	auth.Failed = existing.Failed
	auth.recentRequests = existing.recentRequests
	if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
		if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
			auth.ModelStates = existing.ModelStates
		}
	}
	now := time.Now()
	clearedCooldown := false
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		clearedCooldown = clearCooldownStateForAuth(auth, now)
	}
	auth.EnsureIndex()
	m.mu.Unlock()
	if err := m.persist(ctx, auth); err != nil {
		return nil, err
	}
	authClone := auth.Clone()
	m.mu.Lock()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	if clearedCooldown {
		m.persistCooldownStates(ctx)
	}
	return auth.Clone(), nil
}

// Remove deletes an auth from runtime state without persisting.
// Disk and token-store deletion must be handled by the caller.
func (m *Manager) Remove(ctx context.Context, id string) {
	if m == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	_ = ctx

	m.mu.Lock()
	existing := m.auths[id]
	if existing == nil {
		m.mu.Unlock()
		return
	}
	provider := strings.TrimSpace(existing.Provider)
	delete(m.auths, id)
	if m.modelPoolOffsets != nil {
		delete(m.modelPoolOffsets, id)
	}
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	m.mu.Unlock()

	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	m.queueRefreshUnschedule(id)
	m.invalidateSessionAffinity(id)

	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.persistCooldownStates(ctx)
}

func (m *Manager) invalidateSessionAffinity(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	if invalidator, ok := selector.(interface{ InvalidateAuth(string) }); ok && invalidator != nil {
		invalidator.InvalidateAuth(authID)
	}
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return m.runExecuteAttempts(ctx, providers, req, opts)
}

// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return m.runCountAttempts(ctx, providers, req, opts)
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return m.runStreamAttempts(ctx, providers, req, opts)
}

type requestToFormatResolver interface {
	RequestToFormat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format
}

type requestToFormatContextResolver interface {
	RequestToFormatContext(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format
}

func applyRequestAfterAuthInterceptor(ctx context.Context, executor ProviderExecutor, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, requestedModel string) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if opts.RequestAfterAuthInterceptor == nil {
		return req, opts
	}
	toFormat := requestToFormat(ctx, provider, executor, req, opts)
	resp := opts.RequestAfterAuthInterceptor(ctx, cliproxyexecutor.RequestAfterAuthInterceptRequest{
		SourceFormat:   opts.SourceFormat,
		ToFormat:       toFormat,
		Model:          req.Model,
		RequestedModel: requestedModel,
		Stream:         opts.Stream,
		Headers:        cloneHTTPHeader(opts.Headers),
		Body:           internalpayload.CloneBytes(req.Payload),
		Metadata:       opts.Metadata,
	})
	opts.Headers = mergeRequestHeaders(opts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		req.Payload = internalpayload.CloneBytes(resp.Body)
		opts.OriginalRequest = internalpayload.CloneBytes(resp.Body)
	}
	return req, opts
}

func requestToFormat(ctx context.Context, provider string, executor ProviderExecutor, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	contextResolver, okContext := executor.(requestToFormatContextResolver)
	if okContext && contextResolver != nil {
		formatRequestTo := contextResolver.RequestToFormatContext(ctx, req, opts)
		if formatRequestTo != "" {
			return formatRequestTo
		}
	}
	resolver, ok := executor.(requestToFormatResolver)
	if ok && resolver != nil {
		formatRequestTo := resolver.RequestToFormat(req, opts)
		if formatRequestTo != "" {
			return formatRequestTo
		}
	}
	source := opts.SourceFormat.String()
	if source == "openai-image" || source == "openai-video" {
		return opts.SourceFormat
	}
	if opts.Alt == "responses/compact" && !opts.Stream {
		return sdktranslator.FormatOpenAIResponse
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return sdktranslator.FormatCodex
	case "xai":
		return sdktranslator.FormatCodex
	case "claude":
		return sdktranslator.FormatClaude
	case "gemini", "vertex", "aistudio":
		return sdktranslator.FormatGemini
	case "kimi":
		return sdktranslator.FormatOpenAI
	case "antigravity":
		return sdktranslator.FormatAntigravity
	default:
		return sdktranslator.FormatOpenAI
	}
}

func mergeRequestHeaders(current, updates http.Header, clear []string) http.Header {
	if updates == nil && len(clear) == 0 {
		return current
	}
	out := cloneHTTPHeader(current)
	if out == nil && (len(updates) > 0 || len(clear) > 0) {
		out = make(http.Header)
	}
	for _, key := range clear {
		out.Del(key)
	}
	for key, values := range updates {
		out.Del(key)
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

type providerResponseOperation func(ProviderExecutor, context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	return m.executeResponseMixedOnce(ctx, providers, req, opts, maxRetryCredentials, "execute", ProviderExecutor.Execute)
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	return m.executeResponseMixedOnce(ctx, providers, req, opts, maxRetryCredentials, "count", ProviderExecutor.CountTokens)
}

func (m *Manager) executeResponseMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, operation string, execute providerResponseOperation) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	compactionIntent := compactionIntentFromRequest(req, opts)
	remoteCompaction := cliproxyexecutor.IsRemoteCompactionIntent(compactionIntent)
	gptRoute := isGPTRetryRoute(providers, routeModel) && !remoteCompaction
	var fallbackGuard *gptLargeToolHistoryFallbackGuard
	if !remoteCompaction {
		fallbackGuard = newGPTLargeToolHistoryFallbackGuard(providers, routeModel, opts)
	}
	compactionFallback := newRemoteCompactionFallbackGuard(compactionIntent)
	if gptRoute {
		maxRetryCredentials = gptImmediateFailoverMaxChannels - 1
	}
	maxRetryCredentials = fallbackGuard.effectiveMaxRetryCredentials(maxRetryCredentials)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	trace := requestAttemptTraceFromContext(ctx)
	m.markPreviouslyFailedGPTChannels(ctx, tried)
	nextRetryReason := ""
	var lastErr error
	var retryPermitRelease func()
	defer func() {
		if retryPermitRelease != nil {
			retryPermitRelease()
		}
	}()
	for {
		if operation == "execute" && gptRoute && retryPermitRelease == nil && shouldAcquireGPTRetryPermit(trace) {
			release, pressure, errPermit := m.acquireGPTRetryPermit(ctx, providers, routeModel)
			trace.recordGPTRetryPressure(pressure, errPermit)
			if errPermit != nil {
				return cliproxyexecutor.Response{}, errPermit
			}
			retryPermitRelease = release
		}
		if !homeMode && maxRetryCredentials > 0 && len(attempted) > maxRetryCredentials &&
			!shouldBypassCredentialRetryLimitForRequest(routeModel, opts, lastErr) {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			m.logAuthSelectionFailureMetric(ctx, providers, routeModel, opts, errPick)
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}
		tried[auth.ID] = struct{}{}
		if compactionFallback.shouldSkipAuth(auth) {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			continue
		}
		compactionFallback.markAuth(auth)
		if fallbackGuard.shouldSkipAuth(auth) {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			continue
		}
		fallbackGuard.markAuth(auth)

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		m.logAuthSelectionMetric(ctx, auth, provider, routeModel)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)
		execCtx = contextWithSelectedAuthRoutingGroup(execCtx, auth)
		if trace != nil {
			execCtx = coreusage.WithRequestAttempt(execCtx, trace.nextAttempt(nextRetryReason))
			nextRetryReason = ""
		}

		models, pooled := m.preparedExecutionModelsForRequest(auth, routeModel, req, opts)
		if gptRoute {
			models = trace.pinGPTChannelModel(routingChannelBaseKey(auth), models)
		}
		if len(models) == 0 {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			continue
		}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			attempted[auth.ID] = struct{}{}
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: resultErrorFromCause(errPrepare), Cause: errPrepare}
			m.MarkResult(execCtx, result)
			if remoteCompaction {
				if m.prepareRemoteCompactionFallback(ctx, providers, routeModel, compactionIntent, compactionFallback, auth, tried, errPrepare) {
					lastErr = errPrepare
					nextRetryReason = retryReasonFromError(errPrepare)
					trace.recordFinalStatus(statusCodeFromError(errPrepare))
					trace.recordFallback()
					m.logRemoteCompactionFallback(ctx, routeModel, compactionFallback, errPrepare)
					continue
				}
				return cliproxyexecutor.Response{}, errPrepare
			}
			lastErr = errPrepare
			nextRetryReason = retryReasonFromError(errPrepare)
			trace.recordFinalStatus(statusCodeFromError(errPrepare))
			trace.recordFallback()
			continue
		}
		if gptRoute {
			channelKey := routingChannelBaseKey(auth)
			if !m.reserveGPTChannelAttempt(execCtx, auth, provider, routeModel, time.Now()) {
				m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				m.markRetryChannelTried(ctx, tried, auth, nil)
				continue
			}
			newChannel, allowed := trace.reserveGPTChannel(channelKey, gptChannelAttemptLimit(maxRetryCredentials))
			if !allowed {
				m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				m.releaseGPTChannelAttempt(execCtx, auth)
				if lastErr != nil {
					return cliproxyexecutor.Response{}, lastErr
				}
				return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "GPT channel attempt limit reached"}
			}
			if !newChannel && trace.failedGPTChannel(channelKey) {
				m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				m.releaseGPTChannelAttempt(execCtx, auth)
				m.markRetryChannelTried(ctx, tried, auth, nil)
				continue
			}
		}
		attempted[auth.ID] = struct{}{}
		var authErr error
		countAttempt := false
		didRefreshOnUnauthorized := false
	modelLoop:
		for idx, upstreamModel := range models {
			if idx > 0 && gptRoute &&
				!m.reserveGPTChannelAttempt(execCtx, auth, provider, routeModel, time.Now()) {
				m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				authErr = markGPTChannelFailoverError(authErr)
				break modelLoop
			}
			resultModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			execOpts := withSelectedCompactionCapability(execReq, opts, auth)
			execReq, execOpts = applyRequestAfterAuthInterceptor(execCtx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
			logRoutePlan(execCtx, auth, provider, routeModel, resultModel, upstreamModel, execOpts, executor, operation)
			if trace != nil {
				trace.recordExecution(provider, resultModel, providerExecutorName(executor))
			}
			startedAt := time.Now()
			resp, errExec := execute(executor, execCtx, auth, execReq, execOpts)
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
					m.releaseGPTChannelAttempt(execCtx, auth)
					return cliproxyexecutor.Response{}, newCallerRequestFailure(errCtx)
				}
				if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(execCtx, auth, errExec, didRefreshOnUnauthorized); okRefresh {
					auth = refreshed
					didRefreshOnUnauthorized = true
					resp, errExec = execute(executor, execCtx, auth, execReq, execOpts)
				}
			}
			if errExec == nil && operation == "execute" {
				emptyPolicy := m.emptyResponsePolicy(routeModel, opts)
				if emptyPolicy.enabled {
					deliveryTracker := newDeliverableOutputTracker(emptyPolicy.format)
					deliveryTracker.Observe(resp.Payload)
					deliveryTracker.Finish()
					if !deliveryTracker.deliverable {
						downstreamBytes := int64(0)
						if emptyPolicy.auditOnly {
							downstreamBytes = int64(len(resp.Payload))
						}
						m.logEmptyResponseDetected(execCtx, auth, provider, routeModel, opts, deliveryTracker, emptyPolicy.auditOnly, false, downstreamBytes)
						if !emptyPolicy.auditOnly {
							errExec = newEmptyUpstreamResponseFailure()
						}
					}
				}
			}
			if errExec != nil {
				errExec = normalizeOpaqueUpstream500Failure(errExec, failurecontract.StreamPhaseBeforeOutput, false)
				if gptRoute {
					errExec = normalizeOpaqueGPTAttemptFailure(errExec, failurecontract.StreamPhaseBeforeOutput, false)
				}
			}
			elapsed := time.Since(startedAt)
			if operation == "execute" && gptRoute {
				m.recordGPTRetryPressureAttempt(execCtx, routeModel, routingChannelBaseKey(auth), errExec == nil, errExec)
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil, Duration: elapsed, TTFT: elapsed}
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
					m.releaseGPTChannelAttempt(execCtx, auth)
					return cliproxyexecutor.Response{}, newCallerRequestFailure(errCtx)
				}
				result.Error = resultErrorFromCause(errExec)
				result.Cause = errExec
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				channelFailover := shouldFailoverGPTChannel(errExec, providers, routeModel)
				unauthorized := shouldEvictUnauthorizedError(errExec)
				requestInvalid := isRequestInvalidError(errExec)
				routeFallback := requestInvalid && shouldFallbackRequestScopedRouteErrorForRequest(routeModel, opts, errExec)
				result.keepSelectorLease = idx < len(models)-1 &&
					!channelFailover &&
					!unauthorized &&
					(!requestInvalid || routeFallback)
				m.markExecutionResult(execCtx, auth, routeModel, result, compactionIntent)
				channelFailover = channelFailover ||
					(gptRoute && m.gptChannelBreakerOpen(auth, routeModel, time.Now()))
				if channelFailover && result.keepSelectorLease {
					m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				}
				trace.recordFinalStatus(statusCodeFromError(errExec))
				m.recordContentSafetyRequest(execCtx, auth, provider, routeModel, upstreamModel, opts, req.Payload, errExec)
				authErr = errExec
				countAttempt = true
				if remoteCompaction {
					break modelLoop
				}
				if channelFailover {
					if !shouldFailoverGPTChannel(errExec, providers, routeModel) {
						authErr = markGPTChannelFailoverError(errExec)
					}
					break modelLoop
				}
				if unauthorized {
					if errEvict := m.evictUnauthorizedAuth(execCtx, auth, provider, resultModel); errEvict != nil {
						logEntryWithRequestID(execCtx).Warnf("evict unauthorized auth %s failed: %v", auth.ID, errEvict)
					}
					countAttempt = false
					break modelLoop
				}
				if requestInvalid {
					if routeFallback {
						if isDeepSeekCompatibilityFallbackError(errExec) {
							m.markCompatibilityFallbackRouteTried(tried, auth)
						}
						if idx < len(models)-1 {
							trace.recordFallback()
						}
						continue modelLoop
					}
					return cliproxyexecutor.Response{}, errExec
				}
				if idx < len(models)-1 {
					trace.recordFallback()
				}
				continue modelLoop
			}
			m.markExecutionResult(execCtx, auth, routeModel, result, compactionIntent)
			trace.recordFinalStatus(http.StatusOK)
			if responseModelAlias := m.requestedResponseModelAlias(auth, opts, routeModel, upstreamModel); responseModelAlias != "" {
				resp.Payload = rewriteResponsePayloadModelAlias(resp.Payload, responseModelAlias)
			}
			return resp, nil
		}
		if authErr != nil {
			if remoteCompaction {
				if !m.prepareRemoteCompactionFallback(ctx, providers, routeModel, compactionIntent, compactionFallback, auth, tried, authErr) {
					return cliproxyexecutor.Response{}, authErr
				}
				if countAttempt {
					attempted[auth.ID] = struct{}{}
				}
				lastErr = authErr
				nextRetryReason = retryReasonFromError(authErr)
				trace.recordFallback()
				m.logRemoteCompactionFallback(ctx, routeModel, compactionFallback, authErr)
				continue
			}
			channelFailover := shouldFailoverGPTChannel(authErr, providers, routeModel) ||
				(gptRoute && m.gptChannelBreakerOpen(auth, routeModel, time.Now()))
			if channelFailover || isGPTNetworkRoundFailure(authErr) {
				m.markRetryChannelTried(ctx, tried, auth, authErr)
			}
			routeFallback := shouldFallbackRequestScopedRouteErrorForRequest(routeModel, opts, authErr)
			transientNetworkFallback := isTransientRoutingError(authErr)
			emptyUpstreamFallback := isRetryableEmptyUpstreamResponseError(authErr)
			if isRequestInvalidError(authErr) {
				if !routeFallback {
					return cliproxyexecutor.Response{}, authErr
				}
			}
			if countAttempt {
				attempted[auth.ID] = struct{}{}
			}
			lastErr = authErr
			nextRetryReason = retryReasonFromError(authErr)
			trace.recordFallback()
			if homeMode {
				homeAuthCount++
			} else if !channelFailover && !routeFallback && !transientNetworkFallback && !emptyUpstreamFallback && !typedFailureRequestsImmediateRetry(authErr) {
				if errWait := m.waitForRetryQueue(ctx); errWait != nil {
					return cliproxyexecutor.Response{}, errWait
				}
			}
			continue
		}
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (*cliproxyexecutor.StreamResult, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	compactionIntent := compactionIntentFromRequest(req, opts)
	remoteCompaction := cliproxyexecutor.IsRemoteCompactionIntent(compactionIntent)
	gptRoute := isGPTRetryRoute(providers, routeModel) && !remoteCompaction
	var fallbackGuard *gptLargeToolHistoryFallbackGuard
	if !remoteCompaction {
		fallbackGuard = newGPTLargeToolHistoryFallbackGuard(providers, routeModel, opts)
	}
	compactionFallback := newRemoteCompactionFallbackGuard(compactionIntent)
	trace := requestAttemptTraceFromContext(ctx)
	if gptRoute {
		maxChannels, _ := trace.gptFirstEventRetryLimits()
		maxRetryCredentials = maxChannels - 1
	}
	maxRetryCredentials = fallbackGuard.effectiveMaxRetryCredentials(maxRetryCredentials)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	m.markPreviouslyFailedGPTChannels(ctx, tried)
	nextRetryReason := ""
	var lastErr error
	var retryPermitRelease func()
	defer func() {
		if retryPermitRelease != nil {
			retryPermitRelease()
		}
	}()
	for {
		if gptRoute && retryPermitRelease == nil && shouldAcquireGPTRetryPermit(trace) {
			release, pressure, errPermit := m.acquireGPTRetryPermit(ctx, providers, routeModel)
			trace.recordGPTRetryPressure(pressure, errPermit)
			if errPermit != nil {
				return nil, errPermit
			}
			retryPermitRelease = release
		}
		if gptRoute {
			if remaining, tracked := trace.gptFirstEventRemainingBudget(); tracked && remaining <= 0 {
				trace.markGPTFirstEventBudgetExhausted()
				return nil, newGPTFirstEventWaitBudgetError()
			}
		}
		if !homeMode && maxRetryCredentials > 0 && len(attempted) > maxRetryCredentials &&
			!shouldBypassCredentialRetryLimitForRequest(routeModel, opts, lastErr) {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			m.logAuthSelectionFailureMetric(ctx, providers, routeModel, opts, errPick)
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return nil, lastErr
			}
			return nil, errPick
		}
		tried[auth.ID] = struct{}{}
		if compactionFallback.shouldSkipAuth(auth) {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			continue
		}
		compactionFallback.markAuth(auth)
		if fallbackGuard.shouldSkipAuth(auth) {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			continue
		}
		fallbackGuard.markAuth(auth)

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		m.logAuthSelectionMetric(ctx, auth, provider, routeModel)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)
		execCtx = contextWithSelectedAuthRoutingGroup(execCtx, auth)
		if trace != nil {
			execCtx = coreusage.WithRequestAttempt(execCtx, trace.nextAttempt(nextRetryReason))
			nextRetryReason = ""
		}
		models, pooled := m.preparedExecutionModelsForRequest(auth, routeModel, req, opts)
		if gptRoute {
			models = trace.pinGPTChannelModel(routingChannelBaseKey(auth), models)
		}
		if len(models) == 0 {
			m.markSelectorLoadDone(ctx, auth.ID, routeModel)
			continue
		}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			attempted[auth.ID] = struct{}{}
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: resultErrorFromCause(errPrepare), Cause: errPrepare}
			m.MarkResult(execCtx, result)
			if remoteCompaction {
				if m.prepareRemoteCompactionFallback(ctx, providers, routeModel, compactionIntent, compactionFallback, auth, tried, errPrepare) {
					lastErr = errPrepare
					nextRetryReason = retryReasonFromError(errPrepare)
					trace.recordFinalStatus(statusCodeFromError(errPrepare))
					trace.recordFallback()
					m.logRemoteCompactionFallback(ctx, routeModel, compactionFallback, errPrepare)
					continue
				}
				return nil, errPrepare
			}
			lastErr = errPrepare
			nextRetryReason = retryReasonFromError(errPrepare)
			trace.recordFinalStatus(statusCodeFromError(errPrepare))
			trace.recordFallback()
			continue
		}
		if gptRoute {
			channelKey := routingChannelBaseKey(auth)
			if !m.reserveGPTChannelAttempt(execCtx, auth, provider, routeModel, time.Now()) {
				m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				m.markRetryChannelTried(ctx, tried, auth, nil)
				continue
			}
			newChannel, allowed := trace.reserveGPTChannel(channelKey, gptChannelAttemptLimit(maxRetryCredentials))
			if !allowed {
				m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				m.releaseGPTChannelAttempt(execCtx, auth)
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, &Error{Code: "auth_not_found", Message: "GPT channel attempt limit reached"}
			}
			if !newChannel && trace.failedGPTChannel(channelKey) {
				m.markSelectorLoadDone(execCtx, auth.ID, routeModel)
				m.releaseGPTChannelAttempt(execCtx, auth)
				m.markRetryChannelTried(ctx, tried, auth, nil)
				continue
			}
		}
		attempted[auth.ID] = struct{}{}
		execReq := sanitizeDownstreamWebsocketFallbackRequest(execCtx, auth, req)
		streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, providers, execReq, opts, routeModel, models, pooled)
		if errStream != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return nil, newCallerRequestFailure(errCtx)
			}
			channelFailover := shouldFailoverGPTChannel(errStream, providers, routeModel) ||
				(gptRoute && m.gptChannelBreakerOpen(auth, routeModel, time.Now()))
			if remoteCompaction {
				if m.prepareRemoteCompactionFallback(ctx, providers, routeModel, compactionIntent, compactionFallback, auth, tried, errStream) {
					lastErr = errStream
					nextRetryReason = retryReasonFromError(errStream)
					trace.recordFinalStatus(statusCodeFromError(errStream))
					trace.recordFallback()
					m.logRemoteCompactionFallback(ctx, routeModel, compactionFallback, errStream)
					continue
				}
				return nil, errStream
			}
			if channelFailover || isGPTNetworkRoundFailure(errStream) {
				m.markRetryChannelTried(ctx, tried, auth, errStream)
			}
			if shouldEvictUnauthorizedError(errStream) {
				if errEvict := m.evictUnauthorizedAuth(execCtx, auth, provider, routeModel); errEvict != nil {
					logEntryWithRequestID(execCtx).Warnf("evict unauthorized auth %s failed: %v", auth.ID, errEvict)
				}
				lastErr = errStream
				nextRetryReason = retryReasonFromError(errStream)
				trace.recordFinalStatus(statusCodeFromError(errStream))
				trace.recordFallback()
				if errWait := m.waitForRetryQueue(ctx); errWait != nil {
					return nil, errWait
				}
				continue
			}
			routeFallback := shouldFallbackRequestScopedRouteErrorForRequest(routeModel, opts, errStream)
			if routeFallback && isDeepSeekCompatibilityFallbackError(errStream) {
				m.markCompatibilityFallbackRouteTried(tried, auth)
			}
			transientNetworkFallback := isTransientRoutingError(errStream)
			emptyUpstreamFallback := isRetryableEmptyUpstreamResponseError(errStream)
			if isRequestInvalidError(errStream) {
				if !routeFallback {
					return nil, errStream
				}
			}
			attempted[auth.ID] = struct{}{}
			lastErr = errStream
			nextRetryReason = retryReasonFromError(errStream)
			trace.recordFinalStatus(statusCodeFromError(errStream))
			trace.recordFallback()
			if homeMode {
				homeAuthCount++
			} else if !channelFailover && !routeFallback && !transientNetworkFallback && !emptyUpstreamFallback && !typedFailureRequestsImmediateRetry(errStream) {
				if errWait := m.waitForRetryQueue(ctx); errWait != nil {
					return nil, errWait
				}
			}
			continue
		}
		return streamResult, nil
	}
}

func sanitizeDownstreamWebsocketFallbackRequest(ctx context.Context, auth *Auth, req cliproxyexecutor.Request) cliproxyexecutor.Request {
	if !cliproxyexecutor.DownstreamWebsocket(ctx) || authWebsocketsEnabled(auth) || len(req.Payload) == 0 {
		return req
	}
	updated, errDelete := sjson.DeleteBytes(req.Payload, "generate")
	if errDelete != nil {
		return req
	}
	req.Payload = updated
	return req
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func withHomeAuthCount(opts cliproxyexecutor.Options, count int) cliproxyexecutor.Options {
	if count <= 0 {
		count = 1
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[homeAuthCountMetadataKey] = count
	opts.Metadata = meta
	return opts
}

func homeAuthCountFromMetadata(meta map[string]any) int {
	if len(meta) == 0 {
		return 1
	}
	switch value := meta[homeAuthCountMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	}
	return 1
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

type requestAuthPrepareLock struct {
	mu sync.Mutex
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok || preparer == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lockValue, _ := m.requestPrepareLocks.LoadOrStore(id, &requestAuthPrepareLock{})
	lock, ok := lockValue.(*requestAuthPrepareLock)
	if !ok || lock == nil {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lock.mu.Lock()
	defer lock.mu.Unlock()

	target := auth.Clone()
	m.mu.RLock()
	if current := m.auths[id]; current != nil {
		target = current.Clone()
	}
	m.mu.RUnlock()

	if !preparer.ShouldPrepareRequestAuth(target) {
		return target, nil
	}

	updated, errPrepare := preparer.PrepareRequestAuth(ctx, target)
	if errPrepare != nil {
		return auth, errPrepare
	}
	if updated == nil {
		return target, nil
	}

	saved, errUpdate := m.Update(ctx, updated)
	if errUpdate != nil {
		return updated, errUpdate
	}
	if saved != nil {
		return saved, nil
	}
	return updated, nil
}

func contextWithRequestedModelAlias(ctx context.Context, opts cliproxyexecutor.Options, fallback string) context.Context {
	alias := requestedModelAliasFromOptions(opts, fallback)
	ctx = coreusage.WithRequestedModelAlias(ctx, alias)
	effort := reasoningEffortFromOptions(opts)
	if effort != "" {
		ctx = coreusage.WithReasoningEffort(ctx, effort)
	}
	serviceTier := serviceTierFromOptions(opts)
	if serviceTier != "" {
		ctx = coreusage.WithServiceTier(ctx, serviceTier)
	}
	ctx = coreusage.WithRequestShape(ctx, requestShapeFromOptions(opts))
	ctx = coreusage.WithToolShape(ctx, toolShapeFromOptions(opts))
	return ctx
}

func requestShapeFromOptions(opts cliproxyexecutor.Options) coreusage.RequestShape {
	if len(opts.Metadata) == 0 {
		return coreusage.RequestShape{}
	}
	return coreusage.RequestShape{
		MessageCount: intMetadataValue(opts.Metadata[cliproxyexecutor.MessageCountMetadataKey]),
		ToolCount:    intMetadataValue(opts.Metadata[cliproxyexecutor.ToolCountMetadataKey]),
	}
}

func toolShapeFromOptions(opts cliproxyexecutor.Options) coreusage.ToolShape {
	if len(opts.Metadata) == 0 {
		return coreusage.ToolShape{}
	}
	return coreusage.ToolShape{
		ToolTypes:         metadataString(opts.Metadata, cliproxyexecutor.ToolShapeTypesMetadataKey),
		ToolNameHashes:    metadataString(opts.Metadata, cliproxyexecutor.ToolNameHashesMetadataKey),
		DeclaredToolCount: intMetadataValue(opts.Metadata[cliproxyexecutor.DeclaredToolCountMetadataKey]),
		InteractionCount:  intMetadataValue(opts.Metadata[cliproxyexecutor.ToolInteractionCountMetadataKey]),
		MCPToolCount:      intMetadataValue(opts.Metadata[cliproxyexecutor.MCPToolCountMetadataKey]),
		BuiltinToolCount:  intMetadataValue(opts.Metadata[cliproxyexecutor.BuiltinToolCountMetadataKey]),
	}
}

func intMetadataValue(raw any) int {
	switch value := raw.(type) {
	case int:
		if value > 0 {
			return value
		}
	case int32:
		if value > 0 {
			return int(value)
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float32:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	case string:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(value))
		if errParse == nil && parsed > 0 {
			return parsed
		}
	case []byte:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(string(value)))
		if errParse == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

func requestedModelAliasFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		return strings.TrimSpace(value)
	case []byte:
		if len(value) == 0 {
			return fallback
		}
		return strings.TrimSpace(string(value))
	default:
		return fallback
	}
}

func reasoningEffortFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ReasoningEffortMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func serviceTierFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ServiceTierMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func pinnedAuthFallbackFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthFallbackMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		return errParse == nil && parsed
	case []byte:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(string(value)))
		return errParse == nil && parsed
	default:
		return false
	}
}

func (m *Manager) relaxPinnedAuthForFallback(ctx context.Context, opts cliproxyexecutor.Options, model string, tried map[string]struct{}) cliproxyexecutor.Options {
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	if pinnedAuthID == "" || !pinnedAuthFallbackFromMetadata(opts.Metadata) {
		return opts
	}
	_, alreadyTried := tried[pinnedAuthID]
	if !alreadyTried && m.IsAuthSchedulableForModel(pinnedAuthID, model) {
		return opts
	}

	metadata := make(map[string]any, len(opts.Metadata))
	for key, value := range opts.Metadata {
		if key == cliproxyexecutor.PinnedAuthMetadataKey {
			continue
		}
		metadata[key] = value
	}
	opts.Metadata = metadata
	reason := "pinned_auth_ineligible"
	if alreadyTried {
		reason = "pinned_auth_already_tried"
	}
	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"event":  "auth_affinity_escape",
		"model":  canonicalModelKey(model),
		"reason": reason,
	}).Info("auth selection: relaxed stale pinned auth")
	return opts
}

func disallowFreeAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowFreeAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && parsed
	case []byte:
		parsed, err := strconv.ParseBool(strings.TrimSpace(string(val)))
		return err == nil && parsed
	default:
		return false
	}
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelPoolForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) []string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelPoolForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) []string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelPoolForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) []string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelPoolForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) []string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelPoolForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) []string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return nil
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func (m *Manager) resolveAPIKeyUpstreamModelPool(auth *Auth, requestedModel string) []string {
	if m == nil || auth == nil {
		return nil
	}
	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}

	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return resolveUpstreamModelPoolForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		return resolveUpstreamModelPoolForCodexAPIKey(cfg, auth, requestedModel)
	case "gemini":
		return resolveUpstreamModelPoolForGeminiAPIKey(cfg, auth, requestedModel)
	case "vertex":
		return resolveUpstreamModelPoolForVertexAPIKey(cfg, auth, requestedModel)
	default:
		return resolveUpstreamModelPoolForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

// AvailableProviders returns the set of provider keys that currently have at least one
// registered auth record that is not disabled. It is a best-effort snapshot for routing
// decisions and does not account for per-model cooldowns or transient runtime availability.
// Disabled auths (Disabled flag or StatusDisabled) are excluded so routing does not target
// providers that auth selection would refuse to use, which would otherwise cause execution
// failures instead of falling back to lower-priority routers.
func (m *Manager) AvailableProviders() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]struct{}, len(m.auths))
	out := make([]string, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider == "" {
			continue
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		seen[provider] = struct{}{}
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}

// HasProviderAuth reports whether at least one non-disabled auth record is registered for
// the provider. Disabled auths (Disabled flag or StatusDisabled) are excluded to match the
// behavior of auth selection, which refuses to pick disabled credentials.
func (m *Manager) HasProviderAuth(provider string) bool {
	if m == nil {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if strings.ToLower(strings.TrimSpace(auth.Provider)) == provider {
			return true
		}
	}
	return false
}

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func (m *Manager) retryQueueWait() time.Duration {
	if m == nil {
		return 0
	}
	base := time.Duration(m.retryQueueDelay.Load())
	if base <= 0 {
		return 0
	}
	jitterLimit := int64(base)
	if jitterLimit <= 1 {
		return base
	}
	return base + time.Duration(time.Now().UnixNano()%jitterLimit)
}

func (m *Manager) effectiveRetryWait(err error, wait time.Duration) time.Duration {
	if wait > 0 {
		return wait
	}
	if typedFailureRequestsImmediateRetry(err) {
		return 0
	}
	return m.retryQueueWait()
}

func typedFailureRequestsImmediateRetry(err error) bool {
	typed, ok := failurecontract.As(err)
	if !ok || typed.RetryAfter == nil || *typed.RetryAfter != 0 {
		return false
	}
	_, controlled := controlledFailureScope(string(typed.Scope))
	return controlled
}

func (m *Manager) waitForRetryQueue(ctx context.Context) error {
	return waitForCooldown(ctx, m.retryQueueWait())
}

func codexModelLoadKey(provider, model string) string {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return ""
	}
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		return ""
	}
	return "codex:" + modelKey
}

func (m *Manager) reserveCodexModelSlot(provider, model string) (func(), error) {
	key := codexModelLoadKey(provider, model)
	if key == "" || m == nil {
		return func() {}, nil
	}
	m.codexModelLoadMu.Lock()
	if m.codexModelLoads == nil {
		m.codexModelLoads = make(map[string]int)
	}
	// Track Codex model pressure without rejecting requests. Hard model-level
	// 429s are too disruptive for long-running streaming workloads.
	m.codexModelLoads[key]++
	m.codexModelLoadMu.Unlock()

	return func() {
		m.codexModelLoadMu.Lock()
		defer m.codexModelLoadMu.Unlock()
		current := m.codexModelLoads[key]
		if current <= 1 {
			delete(m.codexModelLoads, key)
			return
		}
		m.codexModelLoads[key] = current - 1
	}, nil
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	gptRoute := isGPTRetryRoute(providers, model)
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := executorKeyFromAuth(auth)
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		includeHealth := gptRoute || !isGPTRetryRoute([]string{auth.Provider}, checkModel)
		blocked, reason, next := isAuthBlockedForModelRoute(auth, checkModel, now, includeHealth)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) retryAllowed(attempt int, providers []string) bool {
	if m == nil || attempt < 0 || len(providers) == 0 {
		return false
	}
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := executorKeyFromAuth(auth)
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt < effectiveRetry {
			return true
		}
	}
	return false
}

func shouldRetryGPTRound(err error, completedRound int, providers []string, model string, trace *requestAttemptTrace) (time.Duration, bool) {
	_, maxRounds := trace.gptFirstEventRetryLimits()
	if err == nil || completedRound < 0 || completedRound >= maxRounds-1 {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	if failure, ok := failurecontract.As(err); ok {
		if failure.OutputCommitted || failure.Scope == failurecontract.ScopeRequest {
			return 0, false
		}
	}
	if completedRound == 0 {
		return 0, shouldFailoverGPTChannel(err, providers, model) ||
			isGPTAuthUnavailableError(err) ||
			isGPTNetworkRoundFailure(err)
	}
	return 0, (trace != nil && trace.hasGPTThirdRoundChannels()) ||
		isGPTThirdRoundFailure(err)
}

func isGPTAuthUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	var authErr *Error
	return errors.As(err, &authErr) && authErr != nil &&
		strings.EqualFold(strings.TrimSpace(authErr.Code), "auth_unavailable")
}

func isGPTNetworkRoundFailure(err error) bool {
	if err == nil {
		return false
	}
	if isTransientNetworkError(err) || isRetryableEmptyUpstreamResponseError(err) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(errorCodeFromError(err)), "empty_stream") {
		return true
	}
	switch statusCodeFromError(err) {
	case http.StatusRequestTimeout, http.StatusGatewayTimeout, 520, 521, 522, 523, 524:
		return true
	default:
		return false
	}
}

func isGPTThirdRoundFailure(err error) bool {
	if err == nil || isRequestInvalidError(err) {
		return false
	}
	if failure, ok := failurecontract.As(err); ok {
		if failure.OutputCommitted || failure.Scope == failurecontract.ScopeRequest {
			return false
		}
	}
	return statusCodeFromError(err) == http.StatusServiceUnavailable ||
		isGPTAuthUnavailableError(err) ||
		isGPTNetworkRoundFailure(err)
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if shouldFailoverGPTChannel(err, providers, model) {
		return 0, false
	}
	if typed, ok := failurecontract.As(err); ok {
		if _, controlled := controlledFailureScope(string(typed.Scope)); controlled {
			return m.shouldRetryTypedFailure(typed, attempt, providers, model, maxWait)
		}
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	if isTransientRoutingError(err) {
		return transientNetworkRetryDelay(attempt, maxWait)
	}
	if isRetryableEmptyUpstreamResponseError(err) {
		if !m.retryAllowed(attempt, providers) {
			return 0, false
		}
		return transientNetworkRetryDelay(attempt, maxWait)
	}
	if maxWait <= 0 {
		return 0, false
	}
	if status == 0 && isRetryableAuthError(err) {
		if !m.retryAllowed(attempt, providers) {
			return 0, false
		}
		return 0, true
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil || *retryAfter <= 0 || *retryAfter > maxWait {
		return 0, false
	}
	return *retryAfter, true
}

func (m *Manager) shouldRetryTypedFailure(failure *failurecontract.Failure, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if failure == nil || failure.OutputCommitted || failure.Scope == failurecontract.ScopeRequest ||
		!failure.Retryable || maxWait <= 0 || !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	if failure.RetryAfter != nil {
		if *failure.RetryAfter < 0 || *failure.RetryAfter > maxWait {
			return 0, false
		}
		return *failure.RetryAfter, true
	}
	if wait, found := m.closestCooldownWait(providers, model, attempt); found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	return transientNetworkRetryDelay(attempt, maxWait)
}

func isGPTRetryRoute(providers []string, model string) bool {
	modelKey := strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	if strings.HasPrefix(modelKey, "gpt-") {
		return true
	}
	hasProvider := false
	for _, provider := range providers {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			continue
		}
		hasProvider = true
		if !strings.EqualFold(provider, "codex") {
			return false
		}
	}
	return hasProvider
}

func isGPTRequestRoute(ctx context.Context, providers []string, model string) bool {
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		if enabled, configured := trace.gptRouteValue(); configured {
			return enabled
		}
	}
	return isGPTRetryRoute(providers, model)
}

type gptChannelFailoverError struct {
	cause error
}

func (e *gptChannelFailoverError) Error() string {
	if e == nil || e.cause == nil {
		return "GPT channel unavailable"
	}
	return e.cause.Error()
}

func (e *gptChannelFailoverError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func markGPTChannelFailoverError(err error) error {
	if err == nil {
		err = &Error{Code: "gpt_channel_unavailable", Message: "GPT channel unavailable", Retryable: true}
	}
	var marked *gptChannelFailoverError
	if errors.As(err, &marked) {
		return err
	}
	return &gptChannelFailoverError{cause: err}
}

func shouldFailoverGPTChannel(err error, providers []string, model string) bool {
	if err == nil || !isGPTRetryRoute(providers, model) {
		return false
	}
	var marked *gptChannelFailoverError
	if errors.As(err, &marked) {
		return true
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return false
	}
	if failure, ok := failurecontract.As(err); ok {
		if failure.OutputCommitted || failure.Scope == failurecontract.ScopeRequest {
			return false
		}
		if failure.Scope == failurecontract.ScopeProvider && failure.Retryable {
			return true
		}
	}
	switch statusCodeFromError(err) {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func gptChannelAttemptLimit(maxRetryCredentials int) int {
	limit := gptImmediateFailoverMaxChannels
	if maxRetryCredentials > 0 && maxRetryCredentials+1 < limit {
		limit = maxRetryCredentials + 1
	}
	return limit
}

func (m *Manager) markRetryChannelTried(ctx context.Context, tried map[string]struct{}, auth *Auth, err error) {
	if m == nil || auth == nil {
		return
	}
	key := routingChannelBaseKey(auth)
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		trace.markFailedChannel(key, err)
	}
	m.mu.RLock()
	for _, peer := range m.auths {
		if peer != nil && routingChannelBaseKey(peer) == key {
			tried[peer.ID] = struct{}{}
		}
	}
	m.mu.RUnlock()
}

func (m *Manager) markPreviouslyFailedGPTChannels(ctx context.Context, tried map[string]struct{}) {
	trace := requestAttemptTraceFromContext(ctx)
	failed := trace.failedChannelKeys()
	thirdRound, restrictThirdRound := trace.gptThirdRoundChannelKeys()
	if len(failed) == 0 && !restrictThirdRound {
		return
	}
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		channelKey := routingChannelBaseKey(auth)
		_, failedChannel := failed[channelKey]
		_, thirdRoundAllowed := thirdRound[channelKey]
		if failedChannel || (restrictThirdRound && !thirdRoundAllowed) {
			tried[auth.ID] = struct{}{}
		}
	}
	m.mu.RUnlock()
}

func transientNetworkRetryDelay(attempt int, maxWait time.Duration) (time.Duration, bool) {
	if attempt < 0 || attempt >= transientNetworkRetryAttempts {
		return 0, false
	}
	wait := time.Duration(attempt+1) * time.Second
	if wait > transientNetworkRetryMaxDelay {
		wait = transientNetworkRetryMaxDelay
	}
	if maxWait > 0 && wait > maxWait {
		return 0, false
	}
	return wait, true
}

func isRetryableAuthError(err error) bool {
	if err == nil {
		return false
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Retryable
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTransientUpstreamStatus(statusCode int) bool {
	switch statusCode {
	case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524:
		return true
	default:
		return false
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	result = normalizeResultFailureContract(result)
	result = normalizeModelRateLimitScope(result)
	if isGPTRequestRoute(ctx, []string{result.Provider}, result.Model) {
		result = normalizeOpaqueGPTResultFailure(result)
	}
	probeModel := coreusage.RequestedModelAliasFromContext(ctx)
	if probeModel == "" {
		probeModel = result.Model
	}
	defer func() {
		// Remove the temporary "probe in flight" bypass before waking the
		// route-scope waiters. The next-probe interval remains intact, so a
		// failed probe cannot release a herd back onto the same blocked route.
		m.releaseHalfOpenProbe(result.AuthID, probeModel)
		if canonicalModelKey(result.Model) != canonicalModelKey(probeModel) {
			m.releaseHalfOpenProbe(result.AuthID, result.Model)
		}
		m.releaseZeroEligibleProbe(ctx, probeModel)
	}()
	selectorResult := result
	if requestedModel := coreusage.RequestedModelAliasFromContext(ctx); requestedModel != "" {
		selectorResult.Model = requestedModel
	}
	m.markSelectorResult(ctx, selectorResult)

	shouldResumeModel := false
	shouldSuspendModel := false
	shouldUnregisterAuth := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	var modelQuotaRecoverAt time.Time
	registryModel := ""
	var authSnapshot *Auth
	var schedulerSnapshots []*Auth
	cooldownStateChanged := false

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		requestedModelAlias := coreusage.RequestedModelAliasFromContext(ctx)
		aliasAvailabilityModel := openAICompatAvailabilityAliasForResult(auth, requestedModelAlias, result)
		managedModel := strings.TrimSpace(result.Model)
		if aliasAvailabilityModel != "" {
			managedModel = aliasAvailabilityModel
		}
		managedModel = canonicalModelKey(managedModel)
		registryModel = managedModel
		if result.Success && aliasAvailabilityModel == "" {
			registryModel = strings.TrimSpace(result.Model)
		}
		codexAPIKeyHealthOnly := isCodexAPIKeyAuth(auth)
		codexOAuthUnauthorized := isCodexOAuthCredential(auth) &&
			isCredentialUnauthorizedResult(result)
		codexBypassCooling := isCodexAuth(auth) &&
			!codexAPIKeyHealthOnly &&
			!codexOAuthUnauthorized
		typedFailure, hasTypedFailure := typedFailureFromResult(result)
		failureScope, hasTypedFailureScope := failureScopeFromResult(result)
		preserveAuthLevelCooldown := shouldPreserveAuthLevelCooldown(auth, now)
		slowPenalty := 0
		if result.Success && m.slowRequestPenaltyEnabledLocked(auth) {
			latency := result.TTFT
			if latency <= 0 {
				latency = result.Duration
			}
			slowPenalty = slowRequestHealthPenalty(latency)
		}
		var cooldownRecordsBefore []CooldownStateRecord
		trackCooldownState := m.cooldownStore != nil
		if trackCooldownState {
			cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
		}
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}

		if shouldDisableAuthForProxyFailure(auth, result) {
			disableAuthForProxyFailure(auth, result, now)
			shouldUnregisterAuth = true
			cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
			if cfg == nil {
				cfg = &internalconfig.Config{}
			}
			m.rebuildAPIKeyModelAliasLocked(cfg)
			logEntryWithRequestID(ctx).WithFields(log.Fields{
				"auth_id":  auth.ID,
				"provider": auth.Provider,
				"model":    result.Model,
			}).Warn("disabled auth because SOCKS5 proxy dialing failed")
		} else if shouldDisableAuthForBalanceExhausted(result) {
			disableAuthForBalanceExhausted(auth, result, now)
			shouldUnregisterAuth = true
			cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
			if cfg == nil {
				cfg = &internalconfig.Config{}
			}
			m.rebuildAPIKeyModelAliasLocked(cfg)
			logEntryWithRequestID(ctx).WithFields(log.Fields{
				"auth_id":  auth.ID,
				"provider": auth.Provider,
				"model":    result.Model,
			}).Warn("disabled auth because upstream reported insufficient balance")
		} else if result.Success {
			if result.Model != "" {
				state := ensureModelState(auth, canonicalModelKey(result.Model))
				resetModelState(state, now)
				applyHealthSuccess(&state.Health, now)
				applySlowRequestHealthPenalty(&state.Health, now, slowPenalty)
				if !preserveAuthLevelCooldown {
					updateAggregatedAvailability(auth, now)
				}
				if aliasAvailabilityModel != "" && aliasAvailabilityModel != strings.TrimSpace(result.Model) {
					aliasState := ensureModelState(auth, aliasAvailabilityModel)
					resetModelState(aliasState, now)
					applyHealthSuccess(&aliasState.Health, now)
					aliasState.UpdatedAt = now
					clearAggregatedAvailability(auth)
				}
				if !preserveAuthLevelCooldown && !hasModelError(auth, now) {
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
				auth.UpdatedAt = now
				shouldResumeModel = true
				clearModelQuota = true
			} else {
				clearAuthStateOnSuccess(auth, now)
				applyHealthSuccess(&auth.Health, now)
				applySlowRequestHealthPenalty(&auth.Health, now, slowPenalty)
			}
		} else {
			if hasTypedFailureScope && failureScope == failurecontract.ScopeRequest {
				// Request failures are terminal for this payload and must not alter
				// credential, model, or provider availability.
			} else if hasTypedFailure && hasTypedFailureScope && failureScope == failurecontract.ScopeCredential {
				disableCooling := m.cooldownDisabledForAuth(auth)
				applyTypedCredentialFailureState(auth, typedFailure, result.Error, now, disableCooling)
			} else if codexOAuthUnauthorized {
				disableCooling := m.cooldownDisabledForAuth(auth)
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now, disableCooling)
			} else if hasTypedFailure && hasTypedFailureScope && failureScope == failurecontract.ScopeModel && managedModel != "" {
				applyTypedModelFailureState(auth, managedModel, typedFailure, result.Error, now, preserveAuthLevelCooldown, quotaCooldownDisabledForAuth(auth))
				if typedFailure.Kind == failurecontract.RateLimited {
					shouldSuspendModel = !ensureModelState(auth, managedModel).NextRetryAfter.IsZero()
					setModelQuota = shouldSuspendModel
					modelQuotaRecoverAt = ensureModelState(auth, managedModel).NextRetryAfter
					suspendReason = "rate_limited"
				}
			} else if codexBypassCooling {
				if result.Model != "" {
					state := ensureModelState(auth, result.Model)
					resetModelState(state, now)
					state.Health = HealthState{}
					updateAggregatedAvailability(auth, now)
					auth.Health = HealthState{}
					auth.LastError = nil
					auth.StatusMessage = ""
					if auth.Status != StatusDisabled {
						auth.Status = StatusActive
					}
					auth.UpdatedAt = now
					shouldResumeModel = true
					clearModelQuota = true
				} else {
					clearAuthStateOnSuccess(auth, now)
				}
			} else if hasTypedFailureScope && failureScope == failurecontract.ScopeProvider {
				// Provider failures feed the provider/channel breaker below. They must
				// not be attributed to a single credential or model.
			} else if codexAPIKeyHealthOnly {
				applyCodexAPIKeyFailureState(auth, result, now)
			} else if managedModel != "" {
				if !isRequestScopedNotFoundResultError(result.Error) &&
					!isRequestScopedFeatureUnsupportedResultError(result.Error) &&
					!isRequestScopedContentSafetyResultError(result.Error) &&
					!isRequestScopedContextLimitResultError(result.Error) &&
					!isRequestScopedInvalidParameterResultError(result.Error) &&
					!isRequestScopedParameterRangeResultError(result.Error) &&
					!isTransientRoutingResultError(result.Error) {
					disableCooling := quotaCooldownDisabledForAuth(auth)
					state := ensureModelState(auth, managedModel)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					statusCode := statusCodeFromResult(result.Error)
					accountQuotaFailure := isAccountQuotaExhaustedResultError(result.Error)
					applyHealthFailure(&state.Health, now, statusCode)
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						if !preserveAuthLevelCooldown {
							auth.LastError = cloneError(result.Error)
							auth.StatusMessage = result.Error.Message
						}
					}
					if isModelSupportResultError(result.Error) {
						state.Status = StatusDisabled
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "model_not_supported"
						shouldSuspendModel = true
					} else if accountQuotaFailure {
						applyHealthFailure(&auth.Health, now, statusCode)
						next := applyAccountQuotaFailureState(auth, state, result.Error, result.RetryAfter, now)
						suspendReason = "billing_cycle_quota"
						shouldSuspendModel = true
						setModelQuota = true
						modelQuotaRecoverAt = next
					} else if isCloudflareChallengeResultError(result.Error) {
						next, backoffLevel := nextCloudflareCooldown(state.Quota.BackoffLevel, disableCooling, now)
						state.NextRetryAfter = next
						state.StatusMessage = "cloudflare challenge"
						if auth.LastError != nil {
							auth.StatusMessage = "cloudflare challenge"
						}
						state.Quota = QuotaState{
							Exceeded:      true,
							Reason:        "cloudflare challenge",
							NextRecoverAt: next,
							BackoffLevel:  backoffLevel,
						}
					} else if isInvalidGrantResultError(result.Error) {
						if disableCooling {
							state.NextRetryAfter = time.Time{}
						} else {
							state.NextRetryAfter = now.Add(30 * time.Minute)
							suspendReason = "invalid_grant"
							shouldSuspendModel = true
						}
					} else {
						switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "payment_required"
								shouldSuspendModel = true
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(12 * time.Hour)
								state.NextRetryAfter = next
								suspendReason = "not_found"
								shouldSuspendModel = true
							}
						case 429:
							var next time.Time
							backoffLevel := state.Quota.BackoffLevel
							hardCooldown := !disableCooling && shouldHardCooldownQuotaForAuth(auth, state.Health, result.RetryAfter)
							if hardCooldown {
								if result.RetryAfter != nil {
									next = now.Add(*result.RetryAfter)
								} else {
									cooldown, nextLevel := nextQuotaCooldown(backoffLevel, disableCooling)
									if cooldown > 0 {
										next = now.Add(cooldown)
									}
									backoffLevel = nextLevel
								}
								next = laterTime(next, state.Health.OpenUntil)
							}
							state.NextRetryAfter = next
							state.Quota = QuotaState{
								Exceeded:      true,
								Reason:        "quota",
								NextRecoverAt: next,
								BackoffLevel:  backoffLevel,
							}
							if hardCooldown {
								suspendReason = "quota"
								shouldSuspendModel = true
								setModelQuota = true
								modelQuotaRecoverAt = next
							}
						case 408, 500, 502, 503, 504:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								state.NextRetryAfter = nextTransientErrorRetryAfter(now)
							}
						default:
							if isTransientUpstreamStatus(statusCode) {
								if disableCooling {
									state.NextRetryAfter = time.Time{}
								} else if next := transientHardCooldownUntil(state.Health); !next.IsZero() {
									state.NextRetryAfter = next
								} else {
									state.NextRetryAfter = time.Time{}
								}
							} else {
								state.NextRetryAfter = time.Time{}
							}
						}
					}

					auth.Status = StatusError
					auth.UpdatedAt = now
					if !accountQuotaFailure && !preserveAuthLevelCooldown {
						updateAggregatedAvailability(auth, now)
						if aliasAvailabilityModel != "" {
							clearAggregatedAvailability(auth)
						}
					}
				}
			} else if !hasTypedFailureScope || failureScope != failurecontract.ScopeModel {
				disableCooling := m.cooldownDisabledForAuth(auth)
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now, disableCooling)
			}
		}
		schedulerSnapshots = append(schedulerSnapshots, m.applyChannelBreakerResultLocked(ctx, auth, result, requestedModelAlias, now)...)
		if slowPenalty > 0 {
			schedulerSnapshots = append(schedulerSnapshots, m.applySlowRequestGroupPenaltyLocked(auth, result, now, slowPenalty)...)
		}

		if errPersist := m.persist(ctx, auth); errPersist != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth result state: %v", errPersist)
		}
		authSnapshot = auth.Clone()
		schedulerSnapshots = append(schedulerSnapshots, authSnapshot)
		if trackCooldownState {
			cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
			cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
		}
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		seenSnapshots := make(map[string]struct{}, len(schedulerSnapshots))
		for _, snapshot := range schedulerSnapshots {
			if snapshot == nil || snapshot.ID == "" {
				continue
			}
			if _, seen := seenSnapshots[snapshot.ID]; seen {
				continue
			}
			seenSnapshots[snapshot.ID] = struct{}{}
			m.scheduler.upsertAuth(snapshot)
		}
	}
	if authSnapshot != nil && cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}

	if shouldUnregisterAuth {
		registry.GetGlobalRegistry().UnregisterClient(result.AuthID)
	}
	if registryModel == "" {
		registryModel = strings.TrimSpace(result.Model)
	}
	if registryModel == "" {
		registryModel = coreusage.RequestedModelAliasFromContext(ctx)
	}
	if clearModelQuota && registryModel != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, registryModel)
	}
	if setModelQuota && registryModel != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, registryModel, modelQuotaRecoverAt)
	}
	if shouldResumeModel && registryModel != "" {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, registryModel)
	} else if shouldSuspendModel && registryModel != "" {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, registryModel, suspendReason)
	}

	if authSnapshot != nil {
		m.logAuthResultMetric(ctx, authSnapshot, result)
	}
	if result.Success {
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			trace.commitSessionBinding(result.AuthID)
		}
	}
	m.hook.OnResult(ctx, result)
	m.publishErrorEvent(result, authSnapshot)
}

func applyCodexAPIKeyFailureState(auth *Auth, result Result, now time.Time) {
	if auth == nil {
		return
	}
	if isCodexAPIKeyRequestScopedResultError(result.Error) || isLocalGPTFirstEventTimeoutResult(result) {
		return
	}
	statusCode := statusCodeFromResult(result.Error)
	shouldLowerHealth := shouldCountCodexAPIKeyHealthFailure(result)
	var resultErr *Error
	if result.Error != nil {
		resultErr = cloneError(result.Error)
	}
	if result.Model != "" {
		state := ensureModelState(auth, result.Model)
		if state == nil {
			return
		}
		state.Unavailable = false
		state.NextRetryAfter = time.Time{}
		state.Quota = QuotaState{}
		state.Status = StatusError
		state.UpdatedAt = now
		if resultErr != nil {
			state.LastError = cloneError(resultErr)
			state.StatusMessage = resultErr.Message
			auth.LastError = cloneError(resultErr)
			auth.StatusMessage = resultErr.Message
		}
		if shouldLowerHealth {
			applyCodexAPIKeyHealthFailure(&state.Health, now, statusCode)
		}
		updateAggregatedAvailability(auth, now)
	} else {
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		auth.Quota = QuotaState{}
		if shouldLowerHealth {
			applyCodexAPIKeyHealthFailure(&auth.Health, now, statusCode)
		}
	}
	if auth.Status != StatusDisabled {
		auth.Status = StatusError
	}
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		auth.StatusMessage = resultErr.Message
	}
	auth.UpdatedAt = now
}

func shouldCountCodexAPIKeyHealthFailure(result Result) bool {
	if result.Success || result.Error == nil {
		return false
	}
	if isLocalGPTFirstEventTimeoutResult(result) {
		return false
	}
	if isCodexAPIKeyRequestScopedResultError(result.Error) {
		return false
	}
	statusCode := statusCodeFromResult(result.Error)
	if statusCode == 0 {
		return true
	}
	if isTransientNetworkResultError(result.Error) ||
		isModelSupportResultError(result.Error) ||
		isAccountQuotaExhaustedResultError(result.Error) {
		return true
	}
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusNotFound, http.StatusTooManyRequests:
		return true
	default:
		return isTransientUpstreamStatus(statusCode) ||
			isRetryableAvailabilityErrorMessage(result.Error.Code+" "+result.Error.Message)
	}
}

func isCodexAPIKeyRequestScopedResultError(err *Error) bool {
	return isRequestScopedNotFoundResultError(err) ||
		isRequestScopedFeatureUnsupportedResultError(err) ||
		isRequestScopedContentSafetyResultError(err) ||
		isRequestScopedContextLimitResultError(err) ||
		isRequestScopedInvalidParameterResultError(err) ||
		isRequestScopedParameterRangeResultError(err)
}

func applyCodexAPIKeyHealthFailure(health *HealthState, now time.Time, statusCode int) {
	if health == nil {
		return
	}
	applyHealthFailure(health, now, statusCode)
}

func openAICompatAvailabilityAliasForResult(auth *Auth, requestedModelAlias string, result Result) string {
	if authProviderFamilyKey(auth) != "openai-compatibility" {
		return ""
	}
	requestedModelAlias = strings.TrimSpace(requestedModelAlias)
	if requestedModelAlias == "" {
		return ""
	}
	if canonicalModelKey(requestedModelAlias) == canonicalModelKey(result.Model) {
		return ""
	}
	if result.Success {
		if auth == nil || len(auth.ModelStates) == 0 {
			return ""
		}
		if state := auth.ModelStates[requestedModelAlias]; state != nil {
			return requestedModelAlias
		}
		aliasKey := canonicalModelKey(requestedModelAlias)
		if aliasKey != "" && aliasKey != requestedModelAlias {
			if state := auth.ModelStates[aliasKey]; state != nil {
				return requestedModelAlias
			}
		}
		return ""
	}
	if result.Error == nil {
		return ""
	}
	if isRequestScopedNotFoundResultError(result.Error) ||
		isRequestScopedFeatureUnsupportedResultError(result.Error) ||
		isRequestScopedContentSafetyResultError(result.Error) ||
		isRequestScopedContextLimitResultError(result.Error) ||
		isRequestScopedInvalidParameterResultError(result.Error) ||
		isRequestScopedParameterRangeResultError(result.Error) ||
		isTransientRoutingResultError(result.Error) ||
		isModelSupportResultError(result.Error) ||
		isAccountQuotaExhaustedResultError(result.Error) ||
		isBalanceExhaustedResultError(result.Error) {
		return ""
	}
	statusCode := statusCodeFromResult(result.Error)
	if statusCode == http.StatusTooManyRequests || isTransientUpstreamStatus(statusCode) {
		return requestedModelAlias
	}
	if statusCode == 0 && isTransientNetworkResultError(result.Error) {
		return requestedModelAlias
	}
	if isRetryableAvailabilityErrorMessage(result.Error.Code + " " + result.Error.Message) {
		return requestedModelAlias
	}
	return ""
}

func channelBreakerModelKeyForResult(auth *Auth, result Result, requestedModelAlias string) string {
	modelKey := strings.TrimSpace(result.Model)
	if aliasModel := openAICompatAvailabilityAliasForResult(auth, requestedModelAlias, result); aliasModel != "" {
		return aliasModel
	}
	return modelKey
}

func (m *Manager) applyChannelBreakerResultLocked(ctx context.Context, auth *Auth, result Result, requestedModelAlias string, now time.Time) []*Auth {
	if m == nil || auth == nil || quotaCooldownDisabledForAuth(auth) {
		return nil
	}
	routeModel := strings.TrimSpace(requestedModelAlias)
	if routeModel == "" {
		routeModel = result.Model
	}
	if isGPTRequestRoute(ctx, []string{result.Provider, auth.Provider}, routeModel) {
		if isCodexAuth(auth) && !isCodexAPIKeyAuth(auth) {
			return nil
		}
		return m.applyGPTChannelBreakerResultLocked(ctx, auth, result, routeModel, now)
	}
	aliasScoped := openAICompatAvailabilityAliasForResult(auth, requestedModelAlias, result) != ""
	if failure, ok := typedFailureFromResult(result); ok && failure.Kind == failurecontract.RateLimited {
		return nil
	}
	breakerModel := channelBreakerModelKeyForResult(auth, result, requestedModelAlias)
	key := channelBreakerKey(auth, breakerModel)
	if key == "" {
		return nil
	}
	m.pruneChannelBreakersLocked(now)
	if result.Success {
		m.recordChannelBreakerSuccessLocked(key, now)
		return nil
	}
	if !shouldCountChannelBreakerFailure(result) {
		return nil
	}

	statusCode := statusCodeFromResult(result.Error)
	health := m.channelBreakers[key]
	applyHealthFailure(&health, now, statusCode)
	if health.ConsecutiveFailures >= channelBreakerOpenFailures {
		cooldown := healthOpenCooldown(statusCode, health.ConsecutiveFailures)
		if result.RetryAfter != nil && *result.RetryAfter > cooldown {
			cooldown = *result.RetryAfter
		}
		if cooldown > quotaBackoffMax {
			cooldown = quotaBackoffMax
		}
		if cooldown <= 0 {
			cooldown = healthOpenCooldown(0, health.ConsecutiveFailures)
		}
		health.BreakerState = HealthBreakerOpen
		health.OpenUntil = now.Add(cooldown)
	}
	if health.BreakerState == HealthBreakerClosed && health.ConsecutiveFailures == 0 {
		delete(m.channelBreakers, key)
		return nil
	}
	if m.channelBreakers == nil {
		m.channelBreakers = make(map[string]HealthState)
	}
	m.channelBreakers[key] = health
	if health.BreakerState != HealthBreakerOpen || health.OpenUntil.IsZero() || !health.OpenUntil.After(now) {
		return nil
	}
	return m.applyChannelBreakerCooldownLocked(auth, result, breakerModel, aliasScoped, health, now)
}

func (m *Manager) recordChannelBreakerSuccessLocked(key string, now time.Time) {
	if m == nil || key == "" || len(m.channelBreakers) == 0 {
		return
	}
	health, ok := m.channelBreakers[key]
	if !ok {
		return
	}
	applyHealthSuccess(&health, now)
	if health.BreakerState == HealthBreakerClosed {
		delete(m.channelBreakers, key)
		return
	}
	m.channelBreakers[key] = health
}

func (m *Manager) applyGPTChannelBreakerResultLocked(ctx context.Context, auth *Auth, result Result, routeModel string, now time.Time) []*Auth {
	key := gptChannelBreakerKey(auth, routeModel)
	if key == "" {
		return nil
	}
	counted := result.Success || shouldCountCodexChannelBreakerFailure(result)
	if m.gptChannelBreakers == nil {
		if !counted {
			return nil
		}
		m.gptChannelBreakers = make(map[string]*codexChannelBreakerState)
	}
	state := m.gptChannelBreakers[key]
	if state == nil {
		if !counted {
			return nil
		}
		state = &codexChannelBreakerState{}
		m.gptChannelBreakers[key] = state
	}
	requestID := ""
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		requestID = trace.requestIDValue()
	}
	previousHealth := state.Health
	applyCodexChannelBreakerResult(state, result, now, requestID)
	if !counted {
		return nil
	}
	ordinaryClosedSuccess := result.Success &&
		(previousHealth.BreakerState == "" || previousHealth.BreakerState == HealthBreakerClosed) &&
		previousHealth.ConsecutiveFailures == 0 &&
		recoveredHealthScore(previousHealth, now) >= healthScoreDefault &&
		state.Health.BreakerState == HealthBreakerClosed
	if ordinaryClosedSuccess {
		m.pruneGPTChannelBreakersLocked()
		return nil
	}

	snapshots := make([]*Auth, 0)
	baseKey := routingChannelBaseKey(auth)
	for _, peer := range m.auths {
		if peer == nil || peer.Disabled || peer.Status == StatusDisabled || routingChannelBaseKey(peer) != baseKey {
			continue
		}
		if routeModel != "" {
			if modelState := ensureModelState(peer, routeModel); modelState != nil && modelState.Status != StatusDisabled {
				modelState.Health = state.Health
				modelState.UpdatedAt = now
			}
		}
		peer.UpdatedAt = now
		snapshots = append(snapshots, peer.Clone())
	}
	m.pruneGPTChannelBreakersLocked()
	return snapshots
}

func (m *Manager) pruneGPTChannelBreakersLocked() {
	if m == nil || len(m.gptChannelBreakers) <= channelBreakerStateLimit {
		return
	}
	for key, state := range m.gptChannelBreakers {
		if state == nil || state.Health.BreakerState == HealthBreakerClosed {
			delete(m.gptChannelBreakers, key)
		}
	}
	for len(m.gptChannelBreakers) > channelBreakerStateLimit {
		for key := range m.gptChannelBreakers {
			delete(m.gptChannelBreakers, key)
			break
		}
	}
}

func (m *Manager) applyChannelBreakerCooldownLocked(auth *Auth, result Result, breakerModel string, aliasScoped bool, health HealthState, now time.Time) []*Auth {
	baseKey := channelBreakerBaseKey(auth)
	if m == nil || baseKey == "" || strings.TrimSpace(breakerModel) == "" {
		return nil
	}
	statusCode := statusCodeFromResult(result.Error)
	message := channelBreakerStatusMessage
	if result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
		message = channelBreakerStatusMessage + ": " + result.Error.Message
	}
	snapshots := make([]*Auth, 0)
	for _, peer := range m.auths {
		if peer == nil || peer.Disabled || peer.Status == StatusDisabled {
			continue
		}
		if channelBreakerBaseKey(peer) != baseKey {
			continue
		}
		state := ensureModelState(peer, breakerModel)
		if state == nil || state.Status == StatusDisabled {
			continue
		}
		state.Unavailable = true
		state.Status = StatusError
		state.StatusMessage = message
		state.LastError = &Error{
			Code:       channelBreakerErrorCode,
			Message:    message,
			Retryable:  true,
			HTTPStatus: statusCode,
		}
		state.NextRetryAfter = laterTime(state.NextRetryAfter, health.OpenUntil)
		state.Health = health
		state.UpdatedAt = now
		if peer.Status != StatusDisabled {
			peer.Status = StatusError
		}
		peer.StatusMessage = message
		peer.LastError = cloneError(state.LastError)
		peer.UpdatedAt = now
		updateAggregatedAvailability(peer, now)
		if aliasScoped {
			clearAggregatedAvailability(peer)
		}
		snapshots = append(snapshots, peer.Clone())
	}
	return snapshots
}

func shouldCountChannelBreakerFailure(result Result) bool {
	if result.Success || result.Error == nil {
		return false
	}
	if failure, ok := typedFailureFromResult(result); ok {
		return !failure.OutputCommitted && failure.Scope == failurecontract.ScopeProvider && failure.Retryable
	}
	if isRequestScopedNotFoundResultError(result.Error) ||
		isRequestScopedFeatureUnsupportedResultError(result.Error) ||
		isRequestScopedContentSafetyResultError(result.Error) ||
		isRequestScopedContextLimitResultError(result.Error) ||
		isRequestScopedInvalidParameterResultError(result.Error) ||
		isRequestScopedParameterRangeResultError(result.Error) {
		return false
	}
	if isTransientRoutingResultError(result.Error) {
		return false
	}
	if isModelSupportResultError(result.Error) || isBalanceExhaustedResultError(result.Error) {
		return false
	}
	statusCode := statusCodeFromResult(result.Error)
	if statusCode == 0 {
		return true
	}
	if statusCode == http.StatusTooManyRequests || isTransientUpstreamStatus(statusCode) {
		return true
	}
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusNotFound {
		return false
	}
	return isRetryableAvailabilityErrorMessage(result.Error.Code + " " + result.Error.Message)
}

func slowRequestHealthPenalty(duration time.Duration) int {
	if duration >= slowRequestHardThreshold {
		return slowRequestHardPenalty
	}
	if duration >= slowRequestSoftThreshold {
		return slowRequestSoftPenalty
	}
	return 0
}

func applySlowRequestHealthPenalty(health *HealthState, now time.Time, penalty int) bool {
	if health == nil || penalty <= 0 {
		return false
	}
	score := recoveredHealthScore(*health, now)
	score -= penalty
	if score < slowRequestMinHealthScore {
		score = slowRequestMinHealthScore
	}
	if score > healthScoreDefault {
		score = healthScoreDefault
	}
	health.Observed = true
	health.Score = score
	health.LastUpdatedAt = now
	health.LastStatusCode = http.StatusOK
	if health.BreakerState == "" {
		health.BreakerState = HealthBreakerClosed
	}
	return true
}

func (m *Manager) slowRequestPenaltyEnabledLocked(auth *Auth) bool {
	if m == nil || auth == nil {
		return false
	}
	return selectorUsesSpread(m.selectorForAuths([]*Auth{auth}))
}

func slowRequestPenaltyBaseKey(auth *Auth) string {
	if isCodexAPIKeyAuth(auth) {
		return codexAPIKeyChannelBaseKey(auth)
	}
	return channelBreakerBaseKey(auth)
}

func (m *Manager) applySlowRequestGroupPenaltyLocked(auth *Auth, result Result, now time.Time, penalty int) []*Auth {
	if m == nil || auth == nil || penalty <= 0 {
		return nil
	}
	baseKey := slowRequestPenaltyBaseKey(auth)
	if baseKey == "" {
		return nil
	}
	snapshots := make([]*Auth, 0)
	for _, peer := range m.auths {
		if peer == nil || peer.ID == auth.ID || peer.Disabled || peer.Status == StatusDisabled {
			continue
		}
		if slowRequestPenaltyBaseKey(peer) != baseKey {
			continue
		}
		if !m.slowRequestPenaltyEnabledLocked(peer) {
			continue
		}
		changed := false
		if result.Model != "" {
			state := ensureModelState(peer, result.Model)
			if state == nil || state.Status == StatusDisabled {
				continue
			}
			changed = applySlowRequestHealthPenalty(&state.Health, now, penalty)
			if changed {
				state.UpdatedAt = now
			}
		} else {
			changed = applySlowRequestHealthPenalty(&peer.Health, now, penalty)
		}
		if !changed {
			continue
		}
		peer.UpdatedAt = now
		snapshots = append(snapshots, peer.Clone())
	}
	return snapshots
}

func codexAPIKeyChannelBaseKey(auth *Auth) string {
	if !isCodexAPIKeyAuth(auth) || auth.Attributes == nil {
		return ""
	}
	baseURL := normalizeChannelBreakerURL(auth.Attributes["base_url"])
	proxyURL := normalizeChannelBreakerURL(auth.ProxyURL)
	prefix := strings.ToLower(strings.TrimSpace(auth.Prefix))
	routingGroup := normalizeRoutingGroupKey(auth.Attributes["routing_group"])
	return strings.Join([]string{
		"codex-api-key",
		baseURL,
		proxyURL,
		prefix,
		routingGroup,
	}, "\x00")
}

func channelBreakerBaseKey(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	if authProviderFamilyKey(auth) != "openai-compatibility" &&
		strings.TrimSpace(auth.Attributes["provider_key"]) == "" &&
		strings.TrimSpace(auth.Attributes["compat_name"]) == "" {
		return ""
	}
	providerKey := normalizeRoutingGroupKey(auth.Attributes["provider_key"])
	if providerKey == "" {
		providerKey = normalizeRoutingGroupKey(auth.Provider)
	}
	compatName := normalizeRoutingGroupKey(auth.Attributes["compat_name"])
	baseURL := normalizeChannelBreakerURL(auth.Attributes["base_url"])
	proxyURL := normalizeChannelBreakerURL(auth.ProxyURL)
	prefix := strings.ToLower(strings.TrimSpace(auth.Prefix))
	routingGroup := normalizeRoutingGroupKey(auth.Attributes["routing_group"])
	return strings.Join([]string{
		"openai-compatibility",
		providerKey,
		compatName,
		baseURL,
		proxyURL,
		prefix,
		routingGroup,
	}, "\x00")
}

func routingChannelBaseKey(auth *Auth) string {
	if key := codexAPIKeyChannelBaseKey(auth); key != "" {
		return key
	}
	if key := channelBreakerBaseKey(auth); key != "" {
		return key
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return ""
	}
	return "auth\x00" + strings.TrimSpace(auth.ID)
}

func gptChannelBreakerKey(auth *Auth, model string) string {
	baseKey := routingChannelBaseKey(auth)
	modelKey := canonicalModelKey(model)
	if baseKey == "" || modelKey == "" {
		return ""
	}
	return baseKey + "\x00model=" + modelKey
}

func channelBreakerKey(auth *Auth, model string) string {
	baseKey := channelBreakerBaseKey(auth)
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		modelKey = strings.ToLower(strings.TrimSpace(model))
	}
	if baseKey == "" || modelKey == "" {
		return ""
	}
	return baseKey + "\x00model=" + modelKey
}

func normalizeChannelBreakerURL(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	for strings.HasSuffix(raw, "/") {
		raw = strings.TrimSuffix(raw, "/")
	}
	return raw
}

func (m *Manager) pruneChannelBreakersLocked(now time.Time) {
	if m == nil || len(m.channelBreakers) <= channelBreakerStateLimit {
		return
	}
	for key, health := range m.channelBreakers {
		if health.BreakerState == HealthBreakerOpen && !health.OpenUntil.IsZero() && health.OpenUntil.After(now) {
			continue
		}
		delete(m.channelBreakers, key)
	}
	for len(m.channelBreakers) > channelBreakerStateLimit {
		for key := range m.channelBreakers {
			delete(m.channelBreakers, key)
			break
		}
	}
}

func (m *Manager) markSelectorLoadDone(ctx context.Context, authID, model string) {
	if m == nil || strings.TrimSpace(authID) == "" || strings.TrimSpace(model) == "" {
		return
	}
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		if selection, ok := trace.selectorSelection(authID, true); ok {
			if selector, routeAware := selection.selector.(routeLoadAwareSelector); routeAware {
				selector.MarkRouteDone(selection.provider, selection.authID, selection.model)
			} else if selector, loadAware := selection.selector.(loadAwareSelector); loadAware {
				selector.MarkDone(selection.authID, selection.model)
			}
			return
		}
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	if selector, ok := selector.(loadAwareSelector); ok {
		selector.MarkDone(authID, model)
	}

	m.dynamicSelectorsMu.Lock()
	selectors := make([]Selector, 0, len(m.dynamicSelectors))
	for _, selector := range m.dynamicSelectors {
		if selector != nil {
			selectors = append(selectors, selector)
		}
	}
	m.dynamicSelectorsMu.Unlock()

	for _, selector := range selectors {
		if loadAware, ok := selector.(loadAwareSelector); ok {
			loadAware.MarkDone(authID, model)
		}
	}
}

func (m *Manager) markSelectorResult(ctx context.Context, result Result) {
	if m == nil || strings.TrimSpace(result.AuthID) == "" || strings.TrimSpace(result.Model) == "" {
		return
	}
	ttft := result.TTFT
	if ttft <= 0 {
		ttft = result.Duration
	}
	recordOutcome := result.Success || shouldCountCodexChannelBreakerFailure(result)
	softRouteOutcome := !result.Success && isLocalGPTFirstEventTimeoutResult(result)
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		release := !result.keepSelectorLease
		if selection, ok := trace.selectorSelection(result.AuthID, release); ok {
			softSpreadOutcome := softRouteOutcome && selectorUsesSpread(selection.selector)
			if recordOutcome || softSpreadOutcome {
				if selector, routeAware := selection.selector.(routeResultAwareSelector); routeAware {
					selector.MarkRouteResult(selection.provider, selection.authID, selection.model, result.Success, ttft, release)
				} else if selector, resultAware := selection.selector.(resultAwareSelector); resultAware && recordOutcome {
					selector.MarkResult(selection.authID, selection.model, result.Success, ttft)
				} else if release {
					if selector, loadAware := selection.selector.(loadAwareSelector); loadAware {
						selector.MarkDone(selection.authID, selection.model)
					}
				}
			} else if release {
				if selector, routeAware := selection.selector.(routeLoadAwareSelector); routeAware {
					selector.MarkRouteDone(selection.provider, selection.authID, selection.model)
				} else if selector, loadAware := selection.selector.(loadAwareSelector); loadAware {
					selector.MarkDone(selection.authID, selection.model)
				}
			}
			return
		}
	}
	record := func(selector Selector) {
		if softRouteOutcome && selectorUsesSpread(selector) {
			if routeAware, ok := selector.(routeResultAwareSelector); ok && strings.TrimSpace(result.Provider) != "" {
				routeAware.MarkRouteResult(result.Provider, result.AuthID, result.Model, false, ttft, !result.keepSelectorLease)
				return
			}
		}
		if resultAware, ok := selector.(resultAwareSelector); ok && recordOutcome {
			resultAware.MarkResult(result.AuthID, result.Model, result.Success, ttft)
			return
		}
		if loadAware, ok := selector.(loadAwareSelector); ok {
			loadAware.MarkDone(result.AuthID, result.Model)
		}
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	record(selector)

	m.dynamicSelectorsMu.Lock()
	selectors := make([]Selector, 0, len(m.dynamicSelectors))
	for _, selector := range m.dynamicSelectors {
		if selector != nil {
			selectors = append(selectors, selector)
		}
	}
	m.dynamicSelectorsMu.Unlock()
	for _, selector := range selectors {
		record(selector)
	}
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func modelStateIsClean(state *ModelState) bool {
	if state == nil {
		return true
	}
	if state.Status != StatusActive {
		return false
	}
	if state.Unavailable || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if len(auth.ModelStates) == 0 {
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		hasState = true
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func applyHealthSuccess(health *HealthState, now time.Time) {
	if health == nil {
		return
	}
	score := recoveredHealthScore(*health, now)
	health.Observed = true
	health.SuccessCount++
	health.LastSuccessAt = now
	health.LastUpdatedAt = now
	health.LastStatusCode = http.StatusOK
	switch health.BreakerState {
	case HealthBreakerOpen:
		if !health.OpenUntil.IsZero() && health.OpenUntil.After(now) {
			return
		}
		health.BreakerState = HealthBreakerHalfOpen
		health.HalfOpenSuccesses = 1
		health.ConsecutiveFailures = 0
		health.OpenUntil = time.Time{}
		if score < healthBreakerThreshold {
			score = healthBreakerThreshold
		}
		health.Score = score
		return
	case HealthBreakerHalfOpen:
		health.HalfOpenSuccesses++
		health.ConsecutiveFailures = 0
		if health.HalfOpenSuccesses >= healthHalfOpenSuccesses {
			score += healthScoreStepSuccess
			if score > healthScoreDefault {
				score = healthScoreDefault
			}
			health.Score = score
			health.BreakerState = HealthBreakerClosed
			health.OpenUntil = time.Time{}
			health.HalfOpenSuccesses = 0
			return
		}
		if score < healthBreakerThreshold {
			score = healthBreakerThreshold
		}
		health.Score = score
		return
	default:
		score += healthScoreStepSuccess
		if score > healthScoreDefault {
			score = healthScoreDefault
		}
		health.Score = score
		health.ConsecutiveFailures = 0
		health.BreakerState = HealthBreakerClosed
		health.OpenUntil = time.Time{}
		health.HalfOpenSuccesses = 0
	}
}

func applyHealthFailure(health *HealthState, now time.Time, statusCode int) {
	if health == nil {
		return
	}
	score := recoveredHealthScore(*health, now)
	nextConsecutive := health.ConsecutiveFailures + 1
	score -= healthFailurePenalty(statusCode, nextConsecutive)
	if score < 0 {
		score = 0
	}
	health.Observed = true
	health.Score = score
	health.ConsecutiveFailures = nextConsecutive
	health.FailureCount++
	health.LastFailureAt = now
	health.LastUpdatedAt = now
	health.LastStatusCode = statusCode
	health.HalfOpenSuccesses = 0
	if shouldOpenHealthCircuit(*health, statusCode) {
		health.BreakerState = HealthBreakerOpen
		health.OpenUntil = now.Add(healthOpenCooldown(statusCode, nextConsecutive))
	} else if health.BreakerState == HealthBreakerHalfOpen {
		health.BreakerState = HealthBreakerOpen
		health.OpenUntil = now.Add(healthOpenCooldown(statusCode, nextConsecutive))
	} else if health.BreakerState == HealthBreakerOpen && health.OpenUntil.Before(now) {
		health.OpenUntil = now.Add(healthOpenCooldown(statusCode, nextConsecutive))
	} else {
		health.BreakerState = HealthBreakerClosed
		health.OpenUntil = time.Time{}
	}
}

func healthFailurePenalty(statusCode, consecutiveFailures int) int {
	penalty := 10
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound:
		penalty = 35
	case http.StatusTooManyRequests:
		penalty = 20
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 521, 522, 523, 524:
		penalty = 20
	default:
		if statusCode >= 500 {
			penalty = 20
		}
	}
	if consecutiveFailures > 1 {
		penalty += minInt(20, (consecutiveFailures-1)*5)
	}
	return penalty
}

func shouldOpenHealthCircuit(health HealthState, statusCode int) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound:
		return true
	case http.StatusTooManyRequests:
		return health.ConsecutiveFailures >= health429OpenFailures
	}
	if health.ConsecutiveFailures >= 3 {
		return true
	}
	return health.ConsecutiveFailures >= 2 && health.Score <= healthBreakerThreshold
}

func shouldHardCooldownQuota(health HealthState, retryAfter *time.Duration) bool {
	if retryAfter != nil && *retryAfter >= quotaImmediateCooldownRetryAfter {
		return true
	}
	if health.BreakerState == HealthBreakerOpen {
		return true
	}
	return health.ConsecutiveFailures >= quotaHardCooldownFailures
}

func shouldHardCooldownQuotaForAuth(auth *Auth, health HealthState, retryAfter *time.Duration) bool {
	if executorKeyFromAuth(auth) == "kimi" || authRoutingGroup(auth) == "kimi" {
		return true
	}
	return shouldHardCooldownQuota(health, retryAfter)
}

func transientHardCooldownUntil(health HealthState) time.Time {
	if health.BreakerState != HealthBreakerOpen {
		return time.Time{}
	}
	return health.OpenUntil
}

func laterTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.After(b) {
		return a
	}
	return b
}

func healthOpenCooldown(statusCode, consecutiveFailures int) time.Duration {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound:
		return 10 * time.Minute
	case http.StatusTooManyRequests:
		return time.Duration(minInt(3, consecutiveFailures)) * 15 * time.Second
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 521, 522, 523, 524:
		return time.Duration(minInt(4, consecutiveFailures)) * 30 * time.Second
	default:
		return time.Duration(minInt(3, consecutiveFailures)) * 30 * time.Second
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Kind:       err.Kind,
		Scope:      err.Scope,
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func errorCodeFromError(err error) string {
	if err == nil {
		return ""
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return strings.TrimSpace(authErr.Code)
	}
	type errorCoder interface {
		ErrorCode() string
	}
	var ec errorCoder
	if errors.As(err, &ec) && ec != nil {
		return strings.TrimSpace(ec.ErrorCode())
	}
	return ""
}

func resultErrorFromCause(err error) *Error {
	if err == nil {
		return nil
	}
	resultErr := &Error{
		Code:       errorCodeFromError(err),
		Message:    err.Error(),
		HTTPStatus: statusCodeFromError(err),
	}
	if typed, ok := failurecontract.As(err); ok {
		resultErr.Kind = string(typed.Kind)
		resultErr.Scope = string(typed.Scope)
		resultErr.Retryable = typed.Retryable
	} else if isExecutorRequestScopedError(err) {
		resultErr.Kind = string(failurecontract.InvalidRequest)
		resultErr.Scope = string(failurecontract.ScopeRequest)
		resultErr.Retryable = false
	} else if resultErr.HTTPStatus == http.StatusRequestEntityTooLarge {
		resultErr.Kind = string(failurecontract.RequestTooLarge)
		resultErr.Scope = string(failurecontract.ScopeRequest)
		resultErr.Retryable = false
	}
	return resultErr
}

func normalizeResultFailureContract(result Result) Result {
	if result.Success || result.Cause == nil {
		return result
	}
	typed, ok := failurecontract.As(result.Cause)
	if !ok || typed == nil {
		return result
	}
	classified := failurecontract.Classify(result.Cause)
	if classified == nil {
		return result
	}
	result.Error = resultErrorFromCause(classified)
	if classified.RetryAfter == nil {
		result.RetryAfter = nil
	} else {
		retryAfter := *classified.RetryAfter
		result.RetryAfter = &retryAfter
	}
	return result
}

func normalizeModelRateLimitScope(result Result) Result {
	if result.Success || strings.TrimSpace(result.Model) == "" {
		return result
	}
	var classified *failurecontract.Failure
	if result.Cause != nil {
		classified = failurecontract.Classify(result.Cause)
	}
	if classified == nil || classified.Kind == "" {
		if result.Error == nil || statusCodeFromResult(result.Error) != http.StatusTooManyRequests {
			return result
		}
		classified = &failurecontract.Failure{
			Kind:          failurecontract.RateLimited,
			Scope:         failurecontract.ScopeCredential,
			HTTPStatus:    http.StatusTooManyRequests,
			OuterStatus:   http.StatusTooManyRequests,
			SemanticCode:  strings.TrimSpace(result.Error.Code),
			StreamPhase:   failurecontract.StreamPhaseUnknown,
			RetryAfter:    result.RetryAfter,
			Retryable:     result.Error.Retryable,
			Cause:         result.Cause,
			PublicMessage: result.Error.Message,
		}
	}
	if classified.HTTPStatus != http.StatusTooManyRequests || classified.Kind == failurecontract.QuotaExceeded {
		return result
	}
	if isExplicitAccountWideRateLimitFailure(classified, result.Error) {
		return result
	}
	if classified.Kind != failurecontract.RateLimited && classified.Kind != "" {
		return result
	}
	downscoped := *classified
	downscoped.Kind = failurecontract.RateLimited
	downscoped.Scope = failurecontract.ScopeModel
	if downscoped.HTTPStatus <= 0 {
		downscoped.HTTPStatus = http.StatusTooManyRequests
	}
	if downscoped.OuterStatus <= 0 {
		downscoped.OuterStatus = http.StatusTooManyRequests
	}
	if downscoped.RetryAfter == nil {
		downscoped.RetryAfter = result.RetryAfter
	}
	downscoped.Retryable = true
	if strings.TrimSpace(downscoped.PublicMessage) == "" && result.Error != nil {
		downscoped.PublicMessage = result.Error.Message
	}
	result.Cause = &downscoped
	result.Error = resultErrorFromCause(&downscoped)
	result.RetryAfter = downscoped.RetryAfter
	return result
}

func isExplicitAccountWideRateLimitFailure(failure *failurecontract.Failure, resultErr *Error) bool {
	if failure != nil {
		if failure.Kind == failurecontract.QuotaExceeded && failure.Scope == failurecontract.ScopeCredential {
			return true
		}
		identifiers := strings.ToLower(strings.TrimSpace(failure.SemanticCode + " " + failure.SemanticType + " " + failure.ProviderCode))
		for _, identifier := range []string{
			"usage_limit_reached", "billing_cycle_quota", "insufficient_quota", "quota_exhausted",
			"quota_exceeded", "insufficient_balance", "balance_insufficient", "account_rpm_limit_exceeded",
		} {
			if strings.Contains(identifiers, identifier) {
				return true
			}
		}
	}
	return isAccountQuotaExhaustedResultError(resultErr) || isBalanceExhaustedResultError(resultErr)
}

func shouldPreserveAuthLevelCooldown(auth *Auth, now time.Time) bool {
	if auth == nil || !auth.Unavailable || auth.LastError == nil {
		return false
	}
	active := auth.NextRetryAfter.After(now) || auth.Quota.NextRecoverAt.After(now)
	if !active {
		return false
	}
	err := auth.LastError
	scope, controlled := controlledFailureScope(err.Scope)
	kind := failurecontract.Kind(strings.ToLower(strings.TrimSpace(err.Kind)))
	if controlled && scope == failurecontract.ScopeCredential {
		if kind == failurecontract.AuthenticationFailed || kind == failurecontract.QuotaExceeded {
			return true
		}
	}
	if isInvalidGrantResultError(err) || isAccountQuotaExhaustedResultError(err) || isBalanceExhaustedResultError(err) {
		return true
	}
	return statusCodeFromResult(err) == http.StatusUnauthorized && !isModelSupportResultError(err)
}

func clearAggregatedAvailabilityUnlessExplicitCredentialCooldown(auth *Auth, now time.Time) {
	if !shouldPreserveAuthLevelCooldown(auth, now) {
		clearAggregatedAvailability(auth)
	}
}

func applyTypedModelFailureState(auth *Auth, model string, failure *failurecontract.Failure, resultErr *Error, now time.Time, preserveCredential, disableCooling bool) {
	if auth == nil || failure == nil || strings.TrimSpace(model) == "" {
		return
	}
	model = canonicalModelKey(model)
	state := ensureModelState(auth, model)
	status := failure.HTTPStatus
	if status <= 0 {
		status = statusCodeFromResult(resultErr)
	}
	state.Status = StatusError
	state.Unavailable = true
	state.UpdatedAt = now
	applyHealthFailure(&state.Health, now, status)
	if resultErr != nil {
		state.LastError = cloneError(resultErr)
		state.StatusMessage = resultErr.Message
		if !preserveCredential {
			auth.LastError = cloneError(resultErr)
			auth.StatusMessage = resultErr.Message
		}
	}
	next := time.Time{}
	if !disableCooling && failure.RetryAfter != nil && *failure.RetryAfter > 0 {
		next = now.Add(*failure.RetryAfter)
	} else if !disableCooling {
		switch status {
		case http.StatusTooManyRequests:
			if shouldHardCooldownQuotaForAuth(auth, state.Health, nil) {
				cooldown, nextLevel := nextQuotaCooldown(state.Quota.BackoffLevel, false)
				if cooldown > 0 {
					next = now.Add(cooldown)
				}
				state.Quota.BackoffLevel = nextLevel
				next = laterTime(next, state.Health.OpenUntil)
			}
		case http.StatusNotFound:
			next = now.Add(12 * time.Hour)
		case http.StatusForbidden:
			next = now.Add(30 * time.Minute)
		default:
			if isTransientUpstreamStatus(status) {
				next = nextTransientErrorRetryAfter(now)
			}
		}
	}
	state.NextRetryAfter = next
	if failure.Kind == failurecontract.RateLimited {
		state.Quota.Exceeded = true
		state.Quota.Reason = "rate_limit"
		state.Quota.NextRecoverAt = next
	}
	if !preserveCredential {
		clearAggregatedAvailability(auth)
	}
	if auth.Status != StatusDisabled && !preserveCredential {
		auth.Status = StatusError
	}
	auth.UpdatedAt = now
}

func normalizeOpaqueGPTResultFailure(result Result) Result {
	if result.Success || result.Cause == nil {
		return result
	}
	if _, ok := failurecontract.As(result.Cause); ok {
		return result
	}
	status := statusCodeFromError(result.Cause)
	code := errorCodeFromError(result.Cause)
	if result.Error != nil {
		if status <= 0 {
			status = statusCodeFromResult(result.Error)
		}
		if code == "" {
			code = strings.TrimSpace(result.Error.Code)
		}
	}
	// A stable code is already useful structure even when a legacy executor did
	// not attach an HTTP status. Only normalize truly opaque failures and the
	// specific legacy shape "HTTP 500 with no code".
	if code != "" || (status > 0 && status != http.StatusInternalServerError) {
		return result
	}
	provider500 := status == http.StatusInternalServerError && code == ""
	kind := failurecontract.InternalTransformError
	scope := failurecontract.ScopeRequest
	semanticCode := "internal_execution_error"
	retryable := false
	publicMessage := "internal execution error"
	if provider500 {
		kind = failurecontract.ProviderUnavailable
		scope = failurecontract.ScopeProvider
		semanticCode = "upstream_http_500"
		retryable = true
		publicMessage = "upstream request failed"
	} else if message := strings.TrimSpace(result.Cause.Error()); message != "" {
		// Preserve legacy SDK-visible diagnostics while adding the canonical
		// status/kind/scope contract around the error.
		publicMessage = message
	}
	failure := &failurecontract.Failure{
		Kind:          kind,
		Scope:         scope,
		HTTPStatus:    http.StatusInternalServerError,
		OuterStatus:   http.StatusInternalServerError,
		ProviderCode:  semanticCode,
		SemanticCode:  semanticCode,
		SemanticType:  "server_error",
		StreamPhase:   failurecontract.StreamPhaseUnknown,
		Retryable:     retryable,
		Cause:         result.Cause,
		PublicMessage: publicMessage,
	}
	result.Cause = failure
	result.Error = resultErrorFromCause(failure)
	result.RetryAfter = nil
	return result
}

func normalizeOpaqueUpstream500Failure(err error, phase failurecontract.StreamPhase, outputCommitted bool) error {
	if err == nil {
		return nil
	}
	if _, ok := failurecontract.As(err); ok {
		return err
	}
	if isTransientRoutingError(err) || isRetryableEmptyUpstreamResponseError(err) {
		return err
	}
	if statusCodeFromError(err) != http.StatusInternalServerError || errorCodeFromError(err) != "" {
		return err
	}
	return &failurecontract.Failure{
		Kind:            failurecontract.ProviderUnavailable,
		Scope:           failurecontract.ScopeProvider,
		HTTPStatus:      http.StatusInternalServerError,
		OuterStatus:     http.StatusInternalServerError,
		ProviderCode:    "upstream_http_500",
		SemanticCode:    "upstream_http_500",
		SemanticType:    "server_error",
		StreamPhase:     phase,
		OutputCommitted: outputCommitted,
		Retryable:       !outputCommitted,
		Cause:           err,
		PublicMessage:   "upstream request failed",
	}
}

func normalizeOpaqueGPTAttemptFailure(err error, phase failurecontract.StreamPhase, outputCommitted bool) error {
	if err == nil {
		return nil
	}
	if _, ok := failurecontract.As(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status := http.StatusBadGateway
		code := "upstream_transport_error"
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
		}
		return &failurecontract.Failure{
			Kind:            failurecontract.TransportError,
			Scope:           failurecontract.ScopeProvider,
			HTTPStatus:      status,
			OuterStatus:     status,
			ProviderCode:    code,
			SemanticCode:    code,
			SemanticType:    "server_error",
			StreamPhase:     phase,
			OutputCommitted: outputCommitted,
			Retryable:       !outputCommitted,
			Cause:           err,
			PublicMessage:   "upstream transport failed",
		}
	}
	if isTransientNetworkError(err) {
		status := http.StatusBadGateway
		code := "upstream_transport_error"
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "timeout") || strings.Contains(message, "deadline exceeded") {
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
		}
		return &failurecontract.Failure{
			Kind:            failurecontract.TransportError,
			Scope:           failurecontract.ScopeProvider,
			HTTPStatus:      status,
			OuterStatus:     status,
			ProviderCode:    code,
			SemanticCode:    code,
			SemanticType:    "server_error",
			StreamPhase:     phase,
			OutputCommitted: outputCommitted,
			Retryable:       !outputCommitted,
			Cause:           err,
			PublicMessage:   "upstream transport failed",
		}
	}
	result := normalizeOpaqueGPTResultFailure(Result{
		Success: false,
		Error:   resultErrorFromCause(err),
		Cause:   err,
	})
	failure, ok := failurecontract.As(result.Cause)
	if !ok || failure == nil {
		return result.Cause
	}
	failure.StreamPhase = phase
	failure.OutputCommitted = outputCommitted
	return failure
}

func typedFailureFromResult(result Result) (*failurecontract.Failure, bool) {
	if result.Cause == nil {
		return nil, false
	}
	if _, ok := failurecontract.As(result.Cause); !ok {
		return nil, false
	}
	classified := failurecontract.Classify(result.Cause)
	return classified, classified != nil
}

func isExecutorRequestScopedError(err error) bool {
	if err == nil {
		return false
	}
	var requestErr cliproxyexecutor.RequestScopedError
	return errors.As(err, &requestErr) && requestErr != nil && requestErr.IsRequestScoped()
}

func failureScopeFromResult(result Result) (failurecontract.Scope, bool) {
	if typed, ok := typedFailureFromResult(result); ok {
		if scope, valid := controlledFailureScope(string(typed.Scope)); valid {
			return scope, true
		}
	}
	if result.Error == nil {
		return "", false
	}
	return controlledFailureScope(result.Error.Scope)
}

func isCredentialUnauthorizedResult(result Result) bool {
	if result.Cause != nil {
		return shouldEvictUnauthorizedError(result.Cause)
	}
	return result.Error != nil &&
		statusCodeFromResult(result.Error) == http.StatusUnauthorized &&
		!isModelSupportResultError(result.Error)
}

func controlledFailureScope(value string) (failurecontract.Scope, bool) {
	scope := failurecontract.Scope(strings.ToLower(strings.TrimSpace(value)))
	switch scope {
	case failurecontract.ScopeRequest, failurecontract.ScopeModel, failurecontract.ScopeCredential, failurecontract.ScopeProvider:
		return scope, true
	default:
		return "", false
	}
}

func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) == http.StatusUnauthorized {
		return true
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "status 401") || strings.Contains(raw, "401 unauthorized")
}

func shouldEvictUnauthorizedError(err error) bool {
	if typed, ok := failurecontract.As(err); ok {
		if scope, controlled := controlledFailureScope(string(typed.Scope)); controlled {
			return scope == failurecontract.ScopeCredential &&
				typed.Kind == failurecontract.AuthenticationFailed &&
				statusCodeFromError(err) == http.StatusUnauthorized
		}
	}
	return isUnauthorizedError(err) && !isModelSupportError(err)
}

func isAccountQuotaExhaustedResultError(err *Error) bool {
	if err == nil {
		return false
	}
	switch statusCodeFromResult(err) {
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
	default:
		return false
	}
	return isAccountQuotaExhaustedMessage(err.Message)
}

func isAccountQuotaExhaustedMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"usage limit",
		"billing cycle",
		"quota will be refreshed",
		"refreshed in the next cycle",
		"quota-upgrade",
		"monthly quota",
		"用量上限",
		"账期",
		"帳期",
		"下个周期",
		"下一周期",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func shouldDisableAuthForBalanceExhausted(result Result) bool {
	if result.Success {
		return false
	}
	if failure, ok := typedFailureFromResult(result); ok {
		return isTypedBalanceExhaustedFailure(failure)
	}
	return isBalanceExhaustedResultError(result.Error)
}

func typedFailureHasSemanticIdentifier(failure *failurecontract.Failure, identifiers ...string) bool {
	if failure == nil {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(failure.SemanticCode))
	typeID := strings.ToLower(strings.TrimSpace(failure.SemanticType))
	for _, identifier := range identifiers {
		identifier = strings.ToLower(strings.TrimSpace(identifier))
		if identifier != "" && (code == identifier || typeID == identifier) {
			return true
		}
	}
	return false
}

func isTypedBalanceExhaustedFailure(failure *failurecontract.Failure) bool {
	return failure != nil &&
		failure.Kind == failurecontract.QuotaExceeded &&
		failure.Scope == failurecontract.ScopeCredential &&
		typedFailureHasSemanticIdentifier(failure,
			"insufficient_balance",
			"balance_insufficient",
			"balance_not_enough",
		)
}

func isTypedAccountQuotaExhaustedFailure(failure *failurecontract.Failure) bool {
	return failure != nil &&
		failure.Kind == failurecontract.QuotaExceeded &&
		failure.Scope == failurecontract.ScopeCredential &&
		typedFailureHasSemanticIdentifier(failure,
			"usage_limit_reached",
			"billing_cycle_quota",
			"insufficient_quota",
		)
}

func isBalanceExhaustedResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromResult(err) != http.StatusPaymentRequired {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(err.Code + " " + err.Message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"insufficient balance",
		"insufficient_balance",
		"balance insufficient",
		"balance_insufficient",
		"balance is insufficient",
		"account balance insufficient",
		"not enough balance",
		"balance not enough",
		"balance_not_enough",
		"insufficient credit",
		"insufficient credits",
		"credit balance",
		"credits exhausted",
		"no credit",
		"recharge",
		"top up",
		"top-up",
		"充值",
		"余额不足",
		"餘額不足",
		"余额不够",
		"餘額不夠",
		"余额耗尽",
		"餘額耗盡",
		"余额已用完",
		"餘額已用完",
		"账户余额",
		"帳戶餘額",
		"欠费",
		"欠費",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func accountQuotaRetryAfter(retryAfter *time.Duration) time.Duration {
	if retryAfter != nil && *retryAfter > 0 {
		return *retryAfter
	}
	return accountQuotaCooldown
}

func applyAccountQuotaFailureState(auth *Auth, state *ModelState, resultErr *Error, retryAfter *time.Duration, now time.Time) time.Time {
	next := now.Add(accountQuotaRetryAfter(retryAfter))
	statusMessage := "billing cycle quota exhausted"
	quota := QuotaState{
		Exceeded:      true,
		Reason:        "billing_cycle_quota",
		NextRecoverAt: next,
	}

	auth.Unavailable = true
	auth.Status = StatusError
	auth.StatusMessage = statusMessage
	auth.NextRetryAfter = next
	auth.Quota = quota
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
	}
	if state != nil {
		state.Unavailable = true
		state.Status = StatusError
		state.StatusMessage = statusMessage
		state.NextRetryAfter = next
		state.Quota = quota
		state.UpdatedAt = now
		if resultErr != nil {
			state.LastError = cloneError(resultErr)
		}
	}
	return next
}

func shouldDisableAuthForProxyFailure(auth *Auth, result Result) bool {
	if auth == nil || result.Success {
		return false
	}
	if strings.TrimSpace(auth.ProxyURL) == "" || !proxyutil.IsSOCKS5ProxyURL(auth.ProxyURL) {
		return false
	}
	return proxyutil.IsProxyDialError(result.Cause)
}

func disableAuthForProxyFailure(auth *Auth, result Result, now time.Time) {
	if auth == nil {
		return
	}
	auth.Disabled = true
	auth.Unavailable = true
	auth.Status = StatusDisabled
	auth.StatusMessage = "disabled due to SOCKS5 proxy failure"
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
	auth.UpdatedAt = now
	if result.Error != nil {
		auth.LastError = cloneError(result.Error)
	} else if result.Cause != nil {
		auth.LastError = &Error{Code: "proxy_dial_failed", Message: result.Cause.Error(), Retryable: true}
	}
	if result.Model != "" {
		state := ensureModelState(auth, result.Model)
		if state != nil {
			state.Status = StatusDisabled
			state.StatusMessage = auth.StatusMessage
			state.Unavailable = true
			state.NextRetryAfter = time.Time{}
			state.UpdatedAt = now
			if result.Error != nil {
				state.LastError = cloneError(result.Error)
			} else if result.Cause != nil {
				state.LastError = &Error{Code: "proxy_dial_failed", Message: result.Cause.Error(), Retryable: true}
			}
		}
	}
}

func disableAuthForBalanceExhausted(auth *Auth, result Result, now time.Time) {
	if auth == nil {
		return
	}
	auth.Disabled = true
	auth.Unavailable = true
	auth.Status = StatusDisabled
	auth.StatusMessage = "disabled due to insufficient balance"
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
	auth.UpdatedAt = now
	if result.Error != nil {
		auth.LastError = cloneError(result.Error)
	}
	if result.Model != "" {
		state := ensureModelState(auth, result.Model)
		if state != nil {
			state.Status = StatusDisabled
			state.StatusMessage = auth.StatusMessage
			state.Unavailable = true
			state.NextRetryAfter = time.Time{}
			state.Quota = QuotaState{}
			state.UpdatedAt = now
			if result.Error != nil {
				state.LastError = cloneError(result.Error)
			}
		}
	}
}

func hasUnauthorizedAuthFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	return auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(auth.LastError.Code, "unauthorized")
}

func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 && isUnauthorizedError(err) {
		statusCode = http.StatusUnauthorized
	}
	authErr := &Error{Message: err.Error(), HTTPStatus: statusCode}
	if statusCode == http.StatusUnauthorized {
		authErr.Code = "unauthorized"
		authErr.Retryable = false
	}
	return authErr
}

func retryAfterFromError(err error) *time.Duration {
	retryAfter, ok := failurecontract.RetryAfterOf(err)
	if !ok {
		return nil
	}
	return &retryAfter
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"requested model does not exist",
		"requested model is not available",
		"model is not supported",
		"model not supported",
		"model does not exist",
		"model not found",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
		"not available for this account",
		"not enabled for your account",
		"not enabled for this account",
		"does not have access to model",
		"model has been disabled",
		"模型不存在",
		"模型未开通",
		"模型不可用",
		"没有该模型权限",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	if typed, ok := failurecontract.As(err); ok {
		if scope, controlled := controlledFailureScope(string(typed.Scope)); controlled {
			return scope == failurecontract.ScopeModel && typed.Kind == failurecontract.ModelUnavailable
		}
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isInvalidGrantErrorMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "invalid_grant" {
		return true
	}
	normalized = strings.ReplaceAll(normalized, `\"`, `"`)
	normalized = strings.Join(strings.Fields(normalized), "")
	return strings.Contains(normalized, `"error":"invalid_grant"`) ||
		strings.Contains(normalized, `"code":"invalid_grant"`) ||
		strings.Contains(normalized, `"error_code":"invalid_grant"`)
}

func isInvalidGrantError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	return isInvalidGrantErrorMessage(errorCodeFromError(err)) || isInvalidGrantErrorMessage(err.Error())
}

func isInvalidGrantResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	return isInvalidGrantErrorMessage(err.Code) || isInvalidGrantErrorMessage(err.Message)
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if scope, ok := controlledFailureScope(err.Scope); ok {
		return scope == failurecontract.ScopeModel && failurecontract.Kind(err.Kind) == failurecontract.ModelUnavailable
	}
	status := statusCodeFromResult(err)
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

func isPersistedModelSupportState(state *ModelState) bool {
	if state == nil || state.Status != StatusDisabled {
		return false
	}
	if state.LastError != nil && isModelSupportResultError(state.LastError) {
		return true
	}
	return isModelSupportErrorMessage(state.StatusMessage)
}

func isCloudflareChallengeErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "cf-mitigated") ||
		strings.Contains(lower, "cloudflare challenge") ||
		(strings.Contains(lower, "cloudflare") && strings.Contains(lower, "<html"))
}

func isCloudflareChallengeError(err error) bool {
	if err == nil {
		return false
	}
	return isCloudflareChallengeErrorMessage(err.Error())
}

func isCloudflareChallengeResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isCloudflareChallengeErrorMessage(err.Message)
}

func nextCloudflareCooldown(backoffLevel int, disableCooling bool, now time.Time) (time.Time, int) {
	var next time.Time
	if !disableCooling {
		cooldown, nextLevel := nextQuotaCooldown(backoffLevel, disableCooling)
		if cooldown < 10*time.Second {
			cooldown = 10 * time.Second
		}
		if cooldown > 0 {
			next = now.Add(cooldown)
		}
		backoffLevel = nextLevel
	}
	return next, backoffLevel
}

func isRetryableAvailabilityErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	if isAccountQuotaExhaustedMessage(lower) {
		return true
	}
	patterns := [...]string{
		"payment required",
		"insufficient balance",
		"balance insufficient",
		"account balance insufficient",
		"insufficient_quota",
		"quota exhausted",
		"quota_exhausted",
		"rate limit",
		"rate_limit",
		"too many requests",
		"resource exhausted",
		"no available key",
		"no available api key",
		"no available channel",
		"channel unavailable",
		"upstream unavailable",
		"provider unavailable",
		"no healthy upstream",
		"无可用key",
		"无可用 key",
		"无可用渠道",
		"渠道不可用",
		"上游不可用",
		"额度已用尽",
		"额度不足",
		"余额不足",
		"账户余额不足",
		"帐户余额不足",
		"频率限制",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isRequestScopedFeatureUnsupportedMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"request_feature_unsupported",
		"minimax anthropic compatibility does not support output_config.format",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isDeepSeekOfficialImageInputMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "deepseek_official_image_input")
}

func isLargeClaudeCompatToolHistoryMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "large_claude_tool_history")
}

func isRequestScopedInvalidParameterMessage(code, message string) bool {
	combined := strings.ToLower(strings.TrimSpace(code + " " + message))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "invalidparameter") ||
		strings.Contains(combined, "invalid parameter")
}

func isRequestScopedInvalidParameterResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	return (status == 0 || status == http.StatusBadRequest) && isRequestScopedInvalidParameterMessage(err.Code, err.Message)
}

func isRequestScopedParameterRangeMessage(code, message string) bool {
	combined := strings.ToLower(strings.TrimSpace(code + " " + message))
	if !strings.Contains(combined, "out of supported range") {
		return false
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if strings.Contains(combined, field) {
			return true
		}
	}
	return false
}

func isRequestScopedParameterRangeResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	return (status == 0 || status == http.StatusBadRequest) && isRequestScopedParameterRangeMessage(err.Code, err.Message)
}

func isRequestScopedNotFoundMessage(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return isRequestScopedNotFoundMessage(err.Message)
}

func isRequestScopedFeatureUnsupportedResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != 0 && status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isRequestScopedFeatureUnsupportedMessage(err.Code + ": " + err.Message)
}

func isRequestScopedContentSafetySignal(code, message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return isMiniMaxNewSensitiveSignal(code, message)
	}
	return (strings.Contains(lower, "request was rejected") &&
		(strings.Contains(lower, "high risk") || strings.Contains(lower, "high-risk"))) ||
		(strings.Contains(lower, "content") && strings.Contains(lower, "blocked")) ||
		isContentSafety1301Signal(code, message) ||
		isMiniMaxNewSensitiveSignal(code, message) ||
		isGenericContentSafetySignal(code, message)
}

func isGenericContentSafetySignal(code, message string) bool {
	normalizedCode := strings.Trim(strings.ToLower(strings.TrimSpace(code)), `"'(),:;[]{}<>`)
	if normalizedCode == "content_policy_violation" ||
		normalizedCode == "data_inspection_failed" ||
		normalizedCode == "datainspectionfailed" {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "content_policy_violation") ||
		strings.Contains(lower, "data_inspection_failed") ||
		strings.Contains(lower, "datainspectionfailed") ||
		strings.Contains(lower, "data may contain inappropriate content") {
		return true
	}
	if strings.Contains(lower, "有敏感内容") ||
		strings.Contains(lower, "敏感内容，请勿重复") ||
		(strings.Contains(lower, "敏感内容") && strings.Contains(lower, "请勿重复")) ||
		(strings.Contains(lower, "敏感") && strings.Contains(lower, "请勿重复请求")) ||
		(strings.Contains(lower, "敏感") && strings.Contains(lower, "请勿重复尝试")) {
		return true
	}
	if strings.Contains(lower, "内容安全") ||
		(strings.Contains(lower, "安全策略") && strings.Contains(lower, "触发")) ||
		(strings.Contains(lower, "安全策略") && strings.Contains(lower, "拦截")) {
		return true
	}
	return false
}

func isContentSafety1301Signal(code, message string) bool {
	normalizedCode := strings.Trim(strings.ToLower(strings.TrimSpace(code)), `"'(),:;[]{}<>`)
	if normalizedCode == "1301" {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(lower, "1301") {
		return false
	}
	if strings.Contains(lower, "[1301]") || strings.Contains(lower, "(1301)") {
		return true
	}
	for _, marker := range []string{
		"content safety",
		"sensitive",
		"unsafe",
		"policy",
		"blocked",
		"high risk",
		"high-risk",
		"敏感",
		"安全",
		"高风险",
		"不合规",
		"违规",
		"审核",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isMiniMaxInputNewSensitiveSignal(code, message string) bool {
	normalizedCode := strings.Trim(strings.ToLower(strings.TrimSpace(code)), `"'(),:;[]{}<>`)
	if normalizedCode == "1026" {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	if strings.Contains(lower, "input new_sensitive") {
		return true
	}
	return strings.Contains(lower, "new_sensitive") && strings.Contains(lower, "1026")
}

func isMiniMaxOutputNewSensitiveSignal(code, message string) bool {
	normalizedCode := strings.Trim(strings.ToLower(strings.TrimSpace(code)), `"'(),:;[]{}<>`)
	if normalizedCode == "1027" {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	if strings.Contains(lower, "output new_sensitive") {
		return true
	}
	return strings.Contains(lower, "new_sensitive") && strings.Contains(lower, "1027")
}

func isMiniMaxNewSensitiveSignal(code, message string) bool {
	return isMiniMaxInputNewSensitiveSignal(code, message) ||
		isMiniMaxOutputNewSensitiveSignal(code, message)
}

func isMiniMaxUnknown1000Signal(code, message string) bool {
	normalizedCode := strings.Trim(strings.ToLower(strings.TrimSpace(code)), `"'(),:;[]{}<>`)
	lower := strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(lower, "unknown error") {
		return false
	}
	return normalizedCode == "1000" || strings.Contains(lower, "1000")
}

func hasHTTPStatusInMessage(message string, statuses ...int) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	for _, status := range statuses {
		code := strconv.Itoa(status)
		if strings.Contains(lower, "status_code="+code) ||
			strings.Contains(lower, "status_code: "+code) ||
			strings.Contains(lower, "status code="+code) ||
			strings.Contains(lower, "status code: "+code) ||
			strings.Contains(lower, "status="+code) ||
			strings.Contains(lower, "status: "+code) {
			return true
		}
	}
	return false
}

func isRequestScopedContentSafetyStatus(status int, code, message string) bool {
	if isRequestScopedContentSafetySignal(code, message) {
		switch status {
		case http.StatusBadRequest, http.StatusInternalServerError, http.StatusBadGateway, http.StatusUnavailableForLegalReasons:
			return true
		case 0:
			if hasHTTPStatusInMessage(message, http.StatusBadRequest, http.StatusInternalServerError, http.StatusBadGateway, http.StatusUnavailableForLegalReasons) {
				return true
			}
			return !hasHTTPStatusInMessage(message, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests)
		default:
			return false
		}
	}
	return false
}

func isRequestScopedContentSafetyResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isRequestScopedContentSafetyStatus(statusCodeFromResult(err), err.Code, err.Message) &&
		isRequestScopedContentSafetySignal(err.Code, err.Message)
}

func isRequestScopedContentSafetyError(err error) bool {
	if err == nil {
		return false
	}
	code := errorCodeFromError(err)
	message := err.Error()
	return isRequestScopedContentSafetyStatus(statusCodeFromError(err), code, message) &&
		isRequestScopedContentSafetySignal(code, message)
}

func isRequestScopedContextLimitMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "context window exceeds limit") ||
		strings.Contains(lower, "context window exceeded") ||
		strings.Contains(lower, "context length exceeded") ||
		strings.Contains(lower, "context length exceeds") ||
		strings.Contains(lower, "context_length_exceeded") ||
		(strings.Contains(lower, "maximum context") && strings.Contains(lower, "exceed")) ||
		(strings.Contains(lower, "context") && strings.Contains(lower, "too long"))
}

func isRequestScopedContextLimitStatus(status int, message string) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return true
	case 0:
		return hasHTTPStatusInMessage(message, http.StatusBadRequest, http.StatusUnprocessableEntity)
	default:
		return false
	}
}

func isRequestScopedContextLimitResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isRequestScopedContextLimitStatus(statusCodeFromResult(err), err.Message) &&
		isRequestScopedContextLimitMessage(err.Message)
}

func isRequestScopedContextLimitError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return isRequestScopedContextLimitStatus(statusCodeFromError(err), message) &&
		isRequestScopedContextLimitMessage(message)
}

func isTransientNetworkMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := []string{
		"connection reset by peer",
		"broken pipe",
		"unexpected eof",
		"read: eof",
		"write: eof",
		"server closed idle connection",
		"use of closed network connection",
		"i/o timeout",
		"io timeout",
		"tls handshake timeout",
		"timeout awaiting response headers",
		"client timeout exceeded",
		"context deadline exceeded",
		"connection refused",
		"connection aborted",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return lower == "eof" || strings.HasSuffix(lower, ": eof")
}

func isTransientNetworkStatus(status int, message string) bool {
	if status == 0 {
		return !hasHTTPStatusInMessage(message, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity) ||
			hasHTTPStatusInMessage(message, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout)
	}
	return status == http.StatusRequestTimeout || isTransientUpstreamStatus(status)
}

func isTransientNetworkResultError(err *Error) bool {
	if err == nil {
		return false
	}
	message := strings.TrimSpace(err.Code + " " + err.Message)
	return isTransientNetworkMessage(message) && isTransientNetworkStatus(statusCodeFromResult(err), message)
}

func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return isTransientNetworkMessage(message) && isTransientNetworkStatus(statusCodeFromError(err), message)
}

func isMiniMaxTransientUpstreamStatus(status int, code, message string) bool {
	if !isMiniMaxUnknown1000Signal(code, message) {
		return false
	}
	if status == 0 {
		return !hasHTTPStatusInMessage(message, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity)
	}
	return status == http.StatusRequestTimeout || isTransientUpstreamStatus(status)
}

func isMiniMaxTransientUpstreamResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isMiniMaxTransientUpstreamStatus(statusCodeFromResult(err), err.Code, err.Message)
}

func isMiniMaxTransientUpstreamError(err error) bool {
	if err == nil {
		return false
	}
	return isMiniMaxTransientUpstreamStatus(statusCodeFromError(err), errorCodeFromError(err), err.Error())
}

func isTransientRoutingResultError(err *Error) bool {
	return isTransientNetworkResultError(err) || isMiniMaxTransientUpstreamResultError(err)
}

func isTransientRoutingError(err error) bool {
	return isTransientNetworkError(err) || isMiniMaxTransientUpstreamError(err)
}

func isRetryableEmptyUpstreamResponseError(err error) bool {
	if err == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(errorCodeFromError(err)), emptyUpstreamResponseErrorCode) {
		return false
	}
	status := statusCodeFromError(err)
	if status == 0 {
		return true
	}
	return status == http.StatusRequestTimeout || isTransientUpstreamStatus(status)
}

func isDeepSeekCompatibilityFallbackError(err error) bool {
	if err == nil || statusCodeFromError(err) != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "thinking mode does not support this tool_choice") {
		return true
	}
	if strings.Contains(message, "deepseek_fim_requires_openai_compat") {
		return true
	}
	return strings.Contains(message, "invalid schema for function") &&
		strings.Contains(message, "null is not of type") &&
		strings.Contains(message, "array")
}

func isDeepSeekCompatibilityFallbackModel(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	return strings.HasPrefix(modelName, "deepseek-v4")
}

func shouldFallbackRequestScopedRouteErrorForRequest(routeModel string, opts cliproxyexecutor.Options, err error) bool {
	requestedModel := requestedModelAliasFromOptions(opts, routeModel)
	if isDeepSeekCompatibilityFallbackError(err) {
		return isDeepSeekCompatibilityFallbackModel(routeModel) || isDeepSeekCompatibilityFallbackModel(requestedModel)
	}
	if !isRequestScopedContextLimitError(err) {
		return false
	}
	if isRequestScopedContentSafetyError(err) {
		return false
	}
	if isRequestScopedFallbackModel(routeModel) {
		return true
	}
	return isRequestScopedFallbackModel(requestedModel)
}

func shouldBypassCredentialRetryLimitForRequest(routeModel string, opts cliproxyexecutor.Options, err error) bool {
	return isRequestScopedContextLimitError(err) && shouldFallbackRequestScopedRouteErrorForRequest(routeModel, opts, err)
}

func compatibilityFallbackRouteKey(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	baseURL := normalizeChannelBreakerURL(auth.Attributes["base_url"])
	if baseURL == "" {
		return ""
	}
	providerFamily := authProviderFamilyKey(auth)
	if providerFamily == "" {
		providerFamily = executorKeyFromAuth(auth)
	}
	return providerFamily + "\x00" + baseURL
}

func (m *Manager) markCompatibilityFallbackRouteTried(tried map[string]struct{}, selected *Auth) {
	if m == nil || len(tried) == 0 || selected == nil {
		return
	}
	routeKey := compatibilityFallbackRouteKey(selected)
	if routeKey == "" {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for authID, candidate := range m.auths {
		if compatibilityFallbackRouteKey(candidate) == routeKey {
			tried[authID] = struct{}{}
		}
	}
}

func isRequestScopedFallbackModel(model string) bool {
	return isClaudeSonnet46FallbackModel(model) || isGLM47FallbackModel(model)
}

func isClaudeSonnet46FallbackModel(model string) bool {
	return isSpecificFallbackModel(model, "claude-sonnet-4-6")
}

func isGLM47FallbackModel(model string) bool {
	return isSpecificFallbackModel(model, "glm-4.7")
}

func isSpecificFallbackModel(model string, target string) bool {
	model = strings.TrimSpace(model)
	target = strings.TrimSpace(target)
	if model == "" || target == "" {
		return false
	}
	base := strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	if base == "" {
		base = model
	}
	return strings.EqualFold(base, target)
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it treats 400 responses with
// "invalid_request_error"/"InvalidParameter", guarded oversized Claude compat
// tool-history requests, unsupported request features, request-scoped content
// safety/context-window rejections, request-scoped 404 item misses caused by
// `store=false`, all 413 responses, and all 422 responses as request-shape
// failures for the generic retry loop. Model-support errors are excluded so
// routing can fall through to another auth or upstream.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if isExecutorRequestScopedError(err) {
		return true
	}
	if typed, ok := failurecontract.As(err); ok {
		if scope, controlled := controlledFailureScope(string(typed.Scope)); controlled {
			return scope == failurecontract.ScopeRequest && !typed.Retryable
		}
	}
	if isCloudflareChallengeError(err) {
		return false
	}
	if isInvalidGrantError(err) {
		return false
	}
	if isModelSupportError(err) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(errorCodeFromError(err)), "request_feature_unsupported") {
		status := statusCodeFromError(err)
		if status == 0 || status == http.StatusBadRequest {
			return true
		}
	}
	if isLargeClaudeCompatToolHistoryMessage(err.Error()) {
		status := statusCodeFromError(err)
		if status == 0 || status == http.StatusBadRequest {
			return true
		}
	}
	if isDeepSeekOfficialImageInputMessage(err.Error()) {
		status := statusCodeFromError(err)
		if status == 0 || status == http.StatusBadRequest {
			return true
		}
	}
	if isRequestScopedFeatureUnsupportedMessage(err.Error()) {
		status := statusCodeFromError(err)
		if status == 0 || status == http.StatusBadRequest {
			return true
		}
	}
	if isRequestScopedContentSafetyError(err) {
		return true
	}
	if isRequestScopedContextLimitError(err) {
		return true
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest:
		msg := err.Error()
		return (strings.Contains(msg, "invalid_request_error") && !isRetryableAvailabilityErrorMessage(msg)) ||
			isRequestScopedInvalidParameterMessage("", msg) ||
			isRequestScopedParameterRangeMessage("", msg) ||
			strings.Contains(msg, "INVALID_ARGUMENT") ||
			strings.Contains(msg, "FAILED_PRECONDITION")
	case http.StatusUnavailableForLegalReasons:
		return false
	case http.StatusRequestEntityTooLarge:
		return true
	case http.StatusNotFound:
		return isRequestScopedNotFoundMessage(err.Error())
	case http.StatusUnprocessableEntity:
		return true
	case http.StatusInternalServerError:
		msg := err.Error()
		return strings.Contains(msg, "\"status\":\"UNKNOWN\"") ||
			strings.Contains(msg, "\"status\": \"UNKNOWN\"")
	default:
		return false
	}
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time, disableCooling bool) {
	applyAuthFailureStateWithCodexScope(auth, resultErr, retryAfter, now, disableCooling, false)
}

func applyTypedCredentialFailureState(auth *Auth, failure *failurecontract.Failure, resultErr *Error, now time.Time, disableCooling bool) {
	if auth == nil || failure == nil {
		return
	}
	statusCode := failure.HTTPStatus
	if statusCode <= 0 {
		statusCode = statusCodeFromResult(resultErr)
	}
	retryAfter := failure.RetryAfter
	applyHealthFailure(&auth.Health, now, statusCode)
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
	}

	if isTypedAccountQuotaExhaustedFailure(failure) {
		applyAccountQuotaFailureState(auth, nil, resultErr, retryAfter, now)
		return
	}
	if typedFailureHasSemanticIdentifier(failure, "invalid_grant") {
		auth.StatusMessage = "invalid_grant"
		auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, 30*time.Minute, disableCooling)
		return
	}

	switch failure.Kind {
	case failurecontract.AuthenticationFailed:
		auth.StatusMessage = "authentication failed"
		if statusCode == http.StatusUnauthorized {
			auth.StatusMessage = "unauthorized"
		}
		auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, 30*time.Minute, disableCooling)
	case failurecontract.QuotaExceeded, failurecontract.RateLimited:
		applyTypedCredentialQuotaFailureState(auth, retryAfter, now, disableCooling)
	default:
		applyTypedCredentialStatusFailureState(auth, statusCode, retryAfter, now, disableCooling)
	}
}

func typedCredentialRetryAt(now time.Time, retryAfter *time.Duration, fallback time.Duration, disableCooling bool) time.Time {
	if disableCooling {
		return time.Time{}
	}
	if retryAfter != nil {
		if *retryAfter <= 0 {
			return time.Time{}
		}
		return now.Add(*retryAfter)
	}
	if fallback <= 0 {
		return time.Time{}
	}
	return now.Add(fallback)
}

func applyTypedCredentialQuotaFailureState(auth *Auth, retryAfter *time.Duration, now time.Time, disableCooling bool) {
	if auth == nil {
		return
	}
	auth.StatusMessage = "quota exhausted"
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "quota"
	var next time.Time
	if !disableCooling && retryAfter != nil {
		if *retryAfter > 0 {
			next = now.Add(*retryAfter)
		}
		next = laterTime(next, auth.Health.OpenUntil)
	} else if !disableCooling && shouldHardCooldownQuotaForAuth(auth, auth.Health, nil) {
		cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, disableCooling)
		if cooldown > 0 {
			next = now.Add(cooldown)
		}
		auth.Quota.BackoffLevel = nextLevel
		next = laterTime(next, auth.Health.OpenUntil)
	}
	auth.Quota.NextRecoverAt = next
	auth.NextRetryAfter = next
}

func applyTypedCredentialStatusFailureState(auth *Auth, statusCode int, retryAfter *time.Duration, now time.Time, disableCooling bool) {
	if auth == nil {
		return
	}
	switch statusCode {
	case http.StatusUnauthorized:
		auth.StatusMessage = "unauthorized"
		auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, 30*time.Minute, disableCooling)
	case http.StatusPaymentRequired, http.StatusForbidden:
		auth.StatusMessage = "payment_required"
		auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, 30*time.Minute, disableCooling)
	case http.StatusNotFound:
		auth.StatusMessage = "not_found"
		auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, 12*time.Hour, disableCooling)
	case http.StatusTooManyRequests:
		applyTypedCredentialQuotaFailureState(auth, retryAfter, now, disableCooling)
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		auth.StatusMessage = "transient upstream error"
		auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, nextTransientErrorRetryAfter(now).Sub(now), disableCooling)
	default:
		if isTransientUpstreamStatus(statusCode) {
			auth.StatusMessage = "transient upstream error"
			fallback := time.Duration(0)
			if next := transientHardCooldownUntil(auth.Health); !next.IsZero() {
				fallback = next.Sub(now)
			}
			auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, fallback, disableCooling)
			return
		}
		auth.StatusMessage = "request failed"
		auth.NextRetryAfter = typedCredentialRetryAt(now, retryAfter, 0, disableCooling)
	}
}

func applyAuthFailureStateWithCodexScope(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time, disableCooling, typedCredential bool) {
	if auth == nil {
		return
	}
	statusCode := statusCodeFromResult(resultErr)
	if isCodexAuth(auth) && statusCode != http.StatusUnauthorized && !typedCredential {
		clearAuthStateOnSuccess(auth, now)
		return
	}
	if isRequestScopedNotFoundResultError(resultErr) ||
		isRequestScopedFeatureUnsupportedResultError(resultErr) ||
		isRequestScopedContentSafetyResultError(resultErr) ||
		isRequestScopedContextLimitResultError(resultErr) ||
		isTransientRoutingResultError(resultErr) {
		return
	}
	applyHealthFailure(&auth.Health, now, statusCode)
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	if isAccountQuotaExhaustedResultError(resultErr) {
		applyAccountQuotaFailureState(auth, nil, resultErr, retryAfter, now)
		return
	}
	if isCloudflareChallengeResultError(resultErr) {
		auth.StatusMessage = "cloudflare challenge"
		next, backoffLevel := nextCloudflareCooldown(auth.Quota.BackoffLevel, disableCooling, now)
		auth.Quota = QuotaState{
			Exceeded:      true,
			Reason:        "cloudflare challenge",
			NextRecoverAt: next,
			BackoffLevel:  backoffLevel,
		}
		auth.NextRetryAfter = next
		return
	}
	if isInvalidGrantResultError(resultErr) {
		auth.StatusMessage = "invalid_grant"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
		return
	}
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 402, 403:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if !disableCooling && shouldHardCooldownQuotaForAuth(auth, auth.Health, retryAfter) {
			if retryAfter != nil {
				next = now.Add(*retryAfter)
			} else {
				cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, disableCooling)
				if cooldown > 0 {
					next = now.Add(cooldown)
				}
				auth.Quota.BackoffLevel = nextLevel
			}
			next = laterTime(next, auth.Health.OpenUntil)
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = nextTransientErrorRetryAfter(now)
		}
	default:
		if isTransientUpstreamStatus(statusCode) {
			auth.StatusMessage = "transient upstream error"
			if disableCooling {
				auth.NextRetryAfter = time.Time{}
			} else if next := transientHardCooldownUntil(auth.Health); !next.IsZero() {
				auth.NextRetryAfter = next
			} else {
				auth.NextRetryAfter = time.Time{}
			}
			return
		}
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

func (m *Manager) evictAuth(ctx context.Context, authID string) error {
	authID = strings.TrimSpace(authID)
	if m == nil || authID == "" {
		return nil
	}

	var authSnapshot *Auth
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	m.mu.Lock()
	if existing := m.auths[authID]; existing != nil {
		authSnapshot = existing.Clone()
		delete(m.auths, authID)
		m.rebuildAPIKeyModelAliasLocked(cfg)
	}
	m.mu.Unlock()

	if authSnapshot == nil {
		return nil
	}
	if m.scheduler != nil {
		m.scheduler.removeAuth(authID)
	}
	registry.GetGlobalRegistry().UnregisterClient(authID)

	if m.store == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if authSnapshot.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(authSnapshot.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	if authSnapshot.Metadata == nil {
		return nil
	}
	if err := m.store.Delete(ctx, authID); err != nil {
		return err
	}
	return nil
}

func (m *Manager) evictUnauthorizedAuth(ctx context.Context, auth *Auth, provider, model string) error {
	if auth == nil {
		return nil
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = strings.TrimSpace(auth.Provider)
	}
	model = strings.TrimSpace(model)

	entry := logEntryWithRequestID(ctx)
	if !deleteUnauthorizedAuthEnabled.Load() {
		if model != "" {
			entry.Infof("skip evicting unauthorized auth provider=%s auth=%s model=%s (delete-unauthorized-auth=false)", provider, auth.ID, model)
		} else {
			entry.Infof("skip evicting unauthorized auth provider=%s auth=%s (delete-unauthorized-auth=false)", provider, auth.ID)
		}
		return nil
	}
	if model != "" {
		entry.Infof("evicting unauthorized auth provider=%s auth=%s model=%s due to 401", provider, auth.ID, model)
	} else {
		entry.Infof("evicting unauthorized auth provider=%s auth=%s due to 401", provider, auth.ID)
	}

	return m.evictAuth(ctx, auth.ID)
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// ResolveConfiguredProviders infers provider keys for a route model directly from
// the current auth set and runtime config. It is a safety net for moments when
// the shared model registry temporarily lacks a model registration even though
// the active config still contains matching credentials.
func (m *Manager) ResolveConfiguredProviders(routeModel string) []string {
	if m == nil {
		return nil
	}
	routeModel = strings.TrimSpace(routeModel)
	if routeModel == "" {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.auths))
	seen := make(map[string]struct{}, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
		if providerKey == "" {
			continue
		}
		if _, exists := seen[providerKey]; exists {
			continue
		}
		if _, hasExecutor := m.executors[providerKey]; !hasExecutor {
			continue
		}
		if !m.authMatchesConfiguredRouteModel(auth, routeModel) {
			continue
		}
		seen[providerKey] = struct{}{}
		out = append(out, providerKey)
	}
	return out
}

func (m *Manager) authMatchesConfiguredRouteModel(auth *Auth, routeModel string) bool {
	if m == nil || auth == nil {
		return false
	}

	requestedModel := rewriteModelForAuth(routeModel, auth)
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	if requestedModel == "" {
		return false
	}

	if pool := m.resolveOAuthUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		return true
	}
	if pool := m.resolveAPIKeyUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		return true
	}
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		return true
	}
	if auth.Attributes != nil {
		if homeModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); homeModel != "" &&
			canonicalModelKey(homeModel) == canonicalModelKey(requestedModel) {
			return true
		}
	}
	if authSupportsDirectProviderRouteModel(auth, requestedModel) {
		return true
	}
	return false
}

func authSupportsDirectProviderRouteModel(auth *Auth, routeModel string) bool {
	if auth == nil || authRequiresRegisteredModels(auth) {
		return false
	}
	modelKey := canonicalModelKey(routeModel)
	if modelKey == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return strings.HasPrefix(modelKey, "claude-")
	default:
		return false
	}
}

// IsAuthSchedulableForModel reports whether the current manager state would
// admit an auth for a model before any half-open fallback is considered.
// Long-lived transports use this to discard stale affinity pins without
// duplicating the manager's cooldown and breaker rules.
func (m *Manager) IsAuthSchedulableForModel(authID, routeModel string) bool {
	if m == nil || strings.TrimSpace(authID) == "" {
		return false
	}

	m.mu.RLock()
	auth := m.auths[strings.TrimSpace(authID)]
	if auth != nil {
		auth = auth.Clone()
	}
	m.mu.RUnlock()
	if auth == nil {
		return false
	}
	if strings.TrimSpace(routeModel) != "" &&
		!m.authSupportsRouteModel(registry.GetGlobalRegistry(), auth, routeModel) {
		return false
	}

	checkModel := m.selectionModelForAuth(auth, routeModel)
	gptRoute := isGPTRetryRoute([]string{executorKeyFromAuth(auth)}, routeModel)
	includeHealth := gptRoute || !isGPTRetryRoute([]string{auth.Provider}, checkModel)
	now := time.Now()
	if blocked, _, _ := isAuthBlockedForModelRoute(auth, checkModel, now, includeHealth); blocked {
		return false
	}
	if includeHealth {
		if blocked, _ := m.healthSelectionBlocked(auth, checkModel, now); blocked {
			return false
		}
	}
	return true
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// GetExecutionSessionAuthByID retrieves a Home runtime auth scoped to an execution session.
func (m *Manager) GetExecutionSessionAuthByID(sessionID string, authID string) (*Auth, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	if auth == nil {
		return nil, false
	}
	return auth.Clone(), true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	m.mu.RUnlock()

	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.Lock()
	if sessionID == CloseAllExecutionSessionsID {
		m.clearHomeRuntimeAuthsLocked()
	} else {
		m.clearHomeRuntimeAuthsForSessionLocked(sessionID)
	}
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.Unlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	if m.hasRoutingStrategyOverrides() {
		return false
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	return isBuiltInSelector(selector)
}

func shouldRetrySchedulerPick(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
}

func (m *Manager) routeAwareSelectionRequired(auth *Auth, routeModel string) bool {
	if auth == nil || strings.TrimSpace(routeModel) == "" {
		return false
	}
	routeKey := canonicalModelKey(routeModel)
	requestedModel := rewriteModelForAuth(routeModel, auth)
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	for _, pool := range [][]string{
		m.resolveOAuthUpstreamModelPool(auth, requestedModel),
		m.resolveAPIKeyUpstreamModelPool(auth, requestedModel),
		m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel),
	} {
		if len(pool) == 0 {
			continue
		}
		if len(pool) > 1 {
			return true
		}
		if canonicalModelKey(pool[0]) != routeKey {
			return true
		}
	}
	return m.selectionModelKeyForAuth(auth, routeModel) != routeKey
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if m.HomeEnabled() {
		auth, exec, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		if err == nil {
			intent := compactionIntentFromRequest(cliproxyexecutor.Request{}, opts)
			if !remoteCompactionCandidateAllowed(auth, intent) {
				return nil, nil, remoteCompactionSelectionError(intent)
			}
			if !m.remoteCompactionCandidateAllowed(auth, model, intent) {
				return nil, nil, remoteCompactionRouteUnavailableError()
			}
		}
		return auth, exec, err
	}

	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	localTried := copyTriedMap(tried)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	m.mu.RLock()
	selector := m.selector
	pluginScheduler := m.pluginScheduler
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	intent := compactionIntentFromRequest(cliproxyexecutor.Request{}, opts)
	compactionBlocked := false
	compactionUnavailable := false
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || executorKeyFromAuth(candidate) != provider || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		if _, used := localTried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		if !remoteCompactionCandidateAllowed(candidate, intent) {
			compactionBlocked = true
			continue
		}
		if !m.remoteCompactionCandidateAllowed(candidate, model, intent) {
			compactionUnavailable = true
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		if compactionUnavailable && cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			return nil, nil, remoteCompactionRouteUnavailableError()
		}
		if compactionBlocked && cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			return nil, nil, remoteCompactionSelectionError(intent)
		}
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForRouteModelContext(ctx, candidates, provider, model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		if cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			errAvailable = m.remoteCompactionAvailabilityError([]string{provider}, model, intent, errAvailable)
		}
		return nil, nil, errAvailable
	}
	available = cloneAuthSlice(available)
	if pinnedAuthID == "" {
		available = preferDeepSeekProtocolAffinityAuths(available, model, opts)
	}
	selector = m.selectorForAuths(available)
	m.mu.RUnlock()

	selected, handled, errPick := m.pickViaPluginScheduler(ctx, pluginScheduler, provider, []string{provider}, model, opts, tried, available)
	if errPick != nil {
		if cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			errPick = m.remoteCompactionAvailabilityError([]string{provider}, model, intent, errPick)
		}
		return nil, nil, errPick
	}
	if !handled {
		selected, errPick = selector.Pick(ctx, provider, selectionArgForSelector(selector, model), opts, available)
		if errPick != nil {
			if cliproxyexecutor.IsRemoteCompactionIntent(intent) {
				errPick = m.remoteCompactionAvailabilityError([]string{provider}, model, intent, errPick)
			}
			return nil, nil, errPick
		}
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	if !handled && selectorUsesSpread(selector) {
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			trace.stageSelectorSelection(selector, provider, model, selected.ID)
		}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

// SelectAuth selects one credential through the configured scheduling strategy.
func (m *Manager) SelectAuth(ctx context.Context, provider, model string, opts cliproxyexecutor.Options) (*Auth, error) {
	selected, _, errPick := m.pickNext(ctx, provider, model, opts, nil)
	if errPick != nil {
		return nil, errPick
	}
	return selected, nil
}

// SelectAuthByKind selects one credential of the required kind and skips other credential kinds.
func (m *Manager) SelectAuthByKind(ctx context.Context, provider, model, requiredKind string, opts cliproxyexecutor.Options) (*Auth, error) {
	requiredKind = normalizeAuthKind(requiredKind)
	if requiredKind == "" {
		return nil, &Error{Code: "invalid_auth_kind", Message: "required auth kind is invalid", HTTPStatus: http.StatusBadRequest}
	}

	homeMode := m.HomeEnabled()
	homeAuthCount := homeAuthCountFromMetadata(opts.Metadata)
	tried := make(map[string]struct{})
	for {
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		selected, _, errPick := m.pickNext(ctx, provider, model, pickOpts, tried)
		if errPick != nil {
			return nil, errPick
		}
		if selected == nil {
			return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if selected.AuthKind() == requiredKind {
			return selected, nil
		}
		authID := strings.TrimSpace(selected.ID)
		if authID == "" {
			return nil, &Error{Code: "auth_not_found", Message: "selected auth has no ID"}
		}
		if _, alreadyTried := tried[authID]; alreadyTried {
			return nil, &Error{Code: "auth_not_found", Message: "selector repeatedly returned an ineligible auth"}
		}
		tried[authID] = struct{}{}
		if homeMode {
			homeAuthCount++
		}
	}
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if m.HomeEnabled() {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	opts = m.relaxPinnedAuthForFallback(ctx, opts, model, tried)
	if cliproxyexecutor.IsRemoteCompactionIntent(compactionIntentFromRequest(cliproxyexecutor.Request{}, opts)) {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}

	if m.hasPluginScheduler() || !m.useSchedulerFastPath() {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	if strings.TrimSpace(model) != "" {
		providerSet := map[string]struct{}{provider: {}}
		m.mu.RLock()
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Disabled {
				continue
			}
			if executorKeyForProviderSet(candidate, providerSet, m.executors) == "" {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextLegacy(ctx, provider, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, errPick := m.scheduler.pickSingle(ctx, provider, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncSchedulerOnPickFailure(time.Now())
			selected, errPick = m.scheduler.pickSingle(ctx, provider, model, opts, tried)
			if errPick != nil {
				if fallbackAuth, fallbackExecutor, errFallback := m.pickNextLegacy(ctx, provider, model, opts, tried); errFallback == nil {
					return fallbackAuth, fallbackExecutor, nil
				}
			}
		}
		if errPick != nil {
			return nil, nil, errPick
		}
		if selected == nil {
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		authCopy := selected.Clone()
		if !selected.indexAssigned {
			m.mu.Lock()
			if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
				current.EnsureIndex()
				authCopy = current.Clone()
			}
			m.mu.Unlock()
		}
		recordSelectorReason(ctx, "builtin_scheduler_round_robin")
		return authCopy, executor, nil
	}
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m.HomeEnabled() {
		auth, exec, provider, err := m.pickNextViaHome(ctx, model, opts, tried)
		if err == nil {
			intent := compactionIntentFromRequest(cliproxyexecutor.Request{}, opts)
			if !remoteCompactionCandidateAllowed(auth, intent) {
				return nil, nil, "", remoteCompactionSelectionError(intent)
			}
			if !m.remoteCompactionCandidateAllowed(auth, model, intent) {
				return nil, nil, "", remoteCompactionRouteUnavailableError()
			}
		}
		return auth, exec, provider, err
	}

	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	localTried := copyTriedMap(tried)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	selector := m.selector
	pluginScheduler := m.pluginScheduler
	candidates := make([]*Auth, 0, len(m.auths))
	intent := compactionIntentFromRequest(cliproxyexecutor.Request{}, opts)
	compactionBlocked := false
	compactionUnavailable := false
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		providerKey := executorKeyForProviderSet(candidate, providerSet, m.executors)
		if providerKey == "" {
			continue
		}
		if _, used := localTried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		if !remoteCompactionCandidateAllowed(candidate, intent) {
			compactionBlocked = true
			continue
		}
		if !m.remoteCompactionCandidateAllowed(candidate, model, intent) {
			compactionUnavailable = true
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		if compactionUnavailable && cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			return nil, nil, "", remoteCompactionRouteUnavailableError()
		}
		if compactionBlocked && cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			return nil, nil, "", remoteCompactionSelectionError(intent)
		}
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForRouteModelContext(ctx, candidates, "mixed", model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		if cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			errAvailable = m.remoteCompactionAvailabilityError(providers, model, intent, errAvailable)
		}
		return nil, nil, "", errAvailable
	}
	available = cloneAuthSlice(available)
	if pinnedAuthID == "" {
		available = preferDeepSeekProtocolAffinityAuths(available, model, opts)
	}
	selector = m.selectorForAuths(available)
	m.mu.RUnlock()

	selected, handled, errPick := m.pickViaPluginScheduler(ctx, pluginScheduler, "mixed", providers, model, opts, tried, available)
	if errPick != nil {
		if cliproxyexecutor.IsRemoteCompactionIntent(intent) {
			errPick = m.remoteCompactionAvailabilityError(providers, model, intent, errPick)
		}
		return nil, nil, "", errPick
	}
	if !handled {
		selected, errPick = selector.Pick(ctx, "mixed", selectionArgForSelector(selector, model), opts, available)
		if errPick != nil {
			if cliproxyexecutor.IsRemoteCompactionIntent(intent) {
				errPick = m.remoteCompactionAvailabilityError(providers, model, intent, errPick)
			}
			return nil, nil, "", errPick
		}
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	m.mu.RLock()
	providerKey := executorKeyForProviderSet(selected, providerSet, m.executors)
	executor := m.executors[providerKey]
	m.mu.RUnlock()
	if providerKey == "" || executor == nil {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	if !handled && selectorUsesSpread(selector) {
		if trace := requestAttemptTraceFromContext(ctx); trace != nil {
			trace.stageSelectorSelection(selector, "mixed", model, selected.ID)
		}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m.HomeEnabled() {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}
	opts = m.relaxPinnedAuthForFallback(ctx, opts, model, tried)
	if cliproxyexecutor.IsRemoteCompactionIntent(compactionIntentFromRequest(cliproxyexecutor.Request{}, opts)) {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) == "" && shouldPreferDeepSeekProtocolAffinity(model, opts) {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}

	if m.hasPluginScheduler() || !m.useSchedulerFastPath() {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}
	if !isGPTRequestRoute(ctx, providers, model) {
		for _, provider := range providers {
			if strings.EqualFold(strings.TrimSpace(provider), "codex") {
				return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
			}
		}
	}

	eligibleProviders := make([]string, 0, len(providers))
	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		if _, seen := seenProviders[providerKey]; seen {
			continue
		}
		if _, okExecutor := m.Executor(providerKey); !okExecutor {
			continue
		}
		seenProviders[providerKey] = struct{}{}
		eligibleProviders = append(eligibleProviders, providerKey)
	}
	if len(eligibleProviders) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if strings.TrimSpace(model) != "" {
		providerSet := make(map[string]struct{}, len(eligibleProviders))
		for _, providerKey := range eligibleProviders {
			providerSet[providerKey] = struct{}{}
		}
		m.mu.RLock()
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Disabled {
				continue
			}
			if executorKeyForProviderSet(candidate, providerSet, m.executors) == "" {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}

	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, providerKey, errPick := m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncSchedulerOnPickFailure(time.Now())
			selected, providerKey, errPick = m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
			if errPick != nil {
				if fallbackAuth, fallbackExecutor, fallbackProvider, errFallback := m.pickNextMixedLegacy(ctx, providers, model, opts, tried); errFallback == nil {
					return fallbackAuth, fallbackExecutor, fallbackProvider, nil
				}
			}
		}
		if errPick != nil {
			return nil, nil, "", errPick
		}
		if selected == nil {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		executor, okExecutor := m.Executor(providerKey)
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		authCopy := selected.Clone()
		if !selected.indexAssigned {
			m.mu.Lock()
			if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
				current.EnsureIndex()
				authCopy = current.Clone()
			}
			m.mu.Unlock()
		}
		recordSelectorReason(ctx, "builtin_scheduler_round_robin")
		return authCopy, executor, providerKey, nil
	}
}

func shouldPreferDeepSeekProtocolAffinity(model string, opts cliproxyexecutor.Options) bool {
	if !isDeepSeekV4RouteModel(model) {
		return false
	}
	sourceFormat := strings.ToLower(strings.TrimSpace(opts.SourceFormat.String()))
	if sourceFormat != "openai" && sourceFormat != "claude" {
		return false
	}
	toolInteractions := intMetadataValue(opts.Metadata[cliproxyexecutor.ToolInteractionCountMetadataKey])
	if toolInteractions == 0 {
		return false
	}
	requestBytes := intMetadataValue(opts.Metadata[cliproxyexecutor.RequestBodyBytesMetadataKey])
	return toolInteractions >= deepSeekProtocolAffinityMinTools || requestBytes >= deepSeekProtocolAffinityMinBytes
}

func preferDeepSeekProtocolAffinityAuths(auths []*Auth, model string, opts cliproxyexecutor.Options) []*Auth {
	if !shouldPreferDeepSeekProtocolAffinity(model, opts) {
		return auths
	}
	sourceFormat := strings.ToLower(strings.TrimSpace(opts.SourceFormat.String()))
	preferred := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		identity := routePlanProviderIdentity(auth, "")
		if identity.CanonicalProvider != "deepseek" || identity.BaseHost != "api.deepseek.com" {
			continue
		}
		switch sourceFormat {
		case "openai":
			if isOpenAICompatAPIKeyAuth(auth) &&
				!strings.EqualFold(strings.TrimSpace(identity.ExecutorKey), "claude") &&
				!strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
				preferred = append(preferred, auth)
			}
		case "claude":
			if strings.EqualFold(strings.TrimSpace(identity.ExecutorKey), "claude") || strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
				preferred = append(preferred, auth)
			}
		}
	}
	if len(preferred) == 0 {
		return auths
	}
	return preferred
}

type homeErrorEnvelope struct {
	Error *homeErrorDetail `json:"error"`
}

type homeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

const (
	homeUpstreamModelAttributeKey     = "home_upstream_model"
	homeRequestRetryExceededErrorCode = "request_retry_exceeded"
)

func isHomeRequestRetryExceededError(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(authErr.Code), homeRequestRetryExceededErrorCode)
}

func shouldReturnLastErrorOnPickFailure(homeMode bool, lastErr error, errPick error) bool {
	if lastErr == nil {
		return false
	}
	if !homeMode {
		return true
	}
	return isHomeRequestRetryExceededError(errPick)
}

func homeAuthAlreadyTried(tried map[string]struct{}, authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(tried) == 0 {
		return false
	}
	_, ok := tried[authID]
	return ok
}

func repeatedHomeAuthError() *Error {
	return &Error{
		Code:       homeRequestRetryExceededErrorCode,
		Message:    "home returned a previously tried auth",
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

type homeAuthDispatchResponse struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	AuthIndex  string `json:"auth_index"`
	UserAPIKey string `json:"user_api_key"`
	Auth       Auth   `json:"auth"`
}

type homeAuthDispatcher interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) ([]byte, error)
}

var currentHomeDispatcher = func() homeAuthDispatcher {
	return home.Current()
}

func setHomeUserAPIKeyOnGinContext(ctx context.Context, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || ctx == nil {
		return
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Set(string, any) })
	if !ok || ginCtx == nil {
		return
	}
	ginCtx.Set("userApiKey", apiKey)
}

func homeDispatchHeaders(ctx context.Context, headers http.Header) http.Header {
	apiKey, ok := homeQueryCredentialFromContext(ctx)
	if !ok {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = http.Header{}
	}
	if out.Get("Authorization") != "" || out.Get("X-Goog-Api-Key") != "" || out.Get("X-Api-Key") != "" {
		return out
	}
	out.Set("X-Goog-Api-Key", apiKey)
	return out
}

func homeQueryCredentialFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if queryCtx, ok := ctx.Value("gin").(interface{ Query(string) string }); ok && queryCtx != nil {
		if apiKey := strings.TrimSpace(queryCtx.Query("key")); apiKey != "" {
			return apiKey, true
		}
		if apiKey := strings.TrimSpace(queryCtx.Query("auth_token")); apiKey != "" {
			return apiKey, true
		}
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return "", false
	}
	rawMetadata, ok := ginCtx.Get("accessMetadata")
	if !ok {
		return "", false
	}
	source := accessMetadataSource(rawMetadata)
	if source != "query-key" && source != "query-auth-token" {
		return "", false
	}
	rawAPIKey, ok := ginCtx.Get("userApiKey")
	if !ok {
		return "", false
	}
	apiKey := contextStringValue(rawAPIKey)
	if apiKey == "" {
		return "", false
	}
	return apiKey, true
}

func accessMetadataSource(raw any) string {
	switch v := raw.(type) {
	case map[string]string:
		return strings.TrimSpace(v["source"])
	case map[string]any:
		return contextStringValue(v["source"])
	default:
		return ""
	}
}

func contextStringValue(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func homeExecutionSessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func (m *Manager) clearHomeRuntimeAuths() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.clearHomeRuntimeAuthsLocked()
	m.mu.Unlock()
}

func (m *Manager) clearHomeRuntimeAuthsLocked() {
	if m == nil {
		return
	}
	m.homeRuntimeAuths = make(map[string]map[string]*Auth)
}

func (m *Manager) clearHomeRuntimeAuthsForSessionLocked(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}
	delete(m.homeRuntimeAuths, sessionID)
}

func (m *Manager) rememberHomeRuntimeAuth(sessionID string, auth *Auth) {
	sessionID = strings.TrimSpace(sessionID)
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	if m == nil || auth == nil || sessionID == "" || authID == "" || !authWebsocketsEnabled(auth) {
		return
	}
	m.mu.Lock()
	if m.homeRuntimeAuths == nil {
		m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	}
	sessionAuths := m.homeRuntimeAuths[sessionID]
	if sessionAuths == nil {
		sessionAuths = make(map[string]*Auth)
		m.homeRuntimeAuths[sessionID] = sessionAuths
	}
	sessionAuths[authID] = auth.Clone()
	m.mu.Unlock()
}

func (m *Manager) homeRuntimeAuthByID(sessionID string, authID string) (*Auth, ProviderExecutor, string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, nil, "", false
	}
	m.mu.RLock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	m.mu.RUnlock()
	if auth == nil || !authWebsocketsEnabled(auth) {
		return nil, nil, "", false
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, nil, "", false
	}
	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", false
	}
	return auth.Clone(), executor, providerKey, true
}

func (m *Manager) pickNextViaHome(ctx context.Context, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executionSessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	count := homeAuthCountFromMetadata(opts.Metadata)
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && count <= 1 {
		if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" {
			_, alreadyTried := tried[pinnedAuthID]
			if !alreadyTried {
				if auth, executor, providerKey, ok := m.homeRuntimeAuthByID(executionSessionID, pinnedAuthID); ok {
					return auth, executor, providerKey, nil
				}
			}
		}
	}

	client := currentHomeDispatcher()
	if client == nil || !client.HeartbeatOK() {
		return nil, nil, "", &Error{Code: "home_unavailable", Message: "home control center unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}

	requestedModel := requestedModelFromMetadata(opts.Metadata, model)
	sessionID := ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	dispatchHeaders := homeDispatchHeaders(ctx, opts.Headers)

	raw, err := client.RPopAuth(ctx, requestedModel, sessionID, dispatchHeaders, count)
	if err != nil {
		if errors.Is(err, home.ErrAuthNotFound) {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: err.Error(), HTTPStatus: http.StatusServiceUnavailable}
		}
		return nil, nil, "", &Error{Code: "home_unavailable", Message: err.Error(), Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	}

	var env homeErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal == nil && env.Error != nil {
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = "home returned error"
		}
		status := http.StatusBadGateway
		switch strings.ToLower(code) {
		case "model_not_found":
			status = http.StatusNotFound
		case "authentication_error", "unauthorized", "no_credentials", "invalid_credential":
			status = http.StatusUnauthorized
		}
		return nil, nil, "", &Error{Code: code, Message: msg, HTTPStatus: status}
	}

	var dispatch homeAuthDispatchResponse
	if errUnmarshal := json.Unmarshal(raw, &dispatch); errUnmarshal != nil {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
	}
	setHomeUserAPIKeyOnGinContext(ctx, dispatch.UserAPIKey)
	auth := dispatch.Auth
	if strings.TrimSpace(auth.ID) == "" {
		// Backward compatibility: older home instances returned the auth directly.
		if errUnmarshal := json.Unmarshal(raw, &auth); errUnmarshal != nil {
			return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway}
		}
	}
	if upstreamModel := strings.TrimSpace(dispatch.Model); upstreamModel != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 1)
		}
		auth.Attributes[homeUpstreamModelAttributeKey] = upstreamModel
	}
	if strings.TrimSpace(auth.ID) == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without id", HTTPStatus: http.StatusBadGateway}
	}
	if homeAuthAlreadyTried(tried, auth.ID) {
		return nil, nil, "", repeatedHomeAuthError()
	}
	providerKey := executorKeyFromAuth(&auth)
	if providerKey == "" {
		return nil, nil, "", &Error{Code: "invalid_auth", Message: "home returned auth without provider", HTTPStatus: http.StatusBadGateway}
	}

	homeAuthIndex := strings.TrimSpace(dispatch.AuthIndex)
	if homeAuthIndex != "" {
		auth.Index = homeAuthIndex
		auth.indexAssigned = true
	} else {
		auth.EnsureIndex()
	}

	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusBadGateway}
	}

	authCopy := auth.Clone()
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && authWebsocketsEnabled(authCopy) {
		m.rememberHomeRuntimeAuth(executionSessionID, authCopy)
	}
	return authCopy, executor, providerKey, nil
}

func requestedModelFromMetadata(metadata map[string]any, fallback string) string {
	if metadata != nil {
		if v, ok := metadata[cliproxyexecutor.RequestedModelMetadataKey]; ok {
			switch typed := v.(type) {
			case string:
				if trimmed := strings.TrimSpace(typed); trimmed != "" {
					return trimmed
				}
			case []byte:
				if trimmed := strings.TrimSpace(string(typed)); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "unknown"
	}
	return fallback
}

func (m *Manager) findAllAntigravityCreditsCandidateAuths(ctx context.Context, routeModel string, opts cliproxyexecutor.Options) ([]creditsCandidateEntry, error) {
	if m == nil {
		return nil, nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	var candidates []creditsCandidateEntry
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pinnedAuthID != "" && auth.ID != pinnedAuthID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			continue
		}
		if !strings.Contains(strings.ToLower(strings.TrimSpace(routeModel)), "claude") {
			continue
		}
		providerKey := executorKeyFromAuth(auth)
		executor, ok := m.executors[providerKey]
		if !ok {
			continue
		}
		candidates = append(candidates, creditsCandidateEntry{
			auth:     auth.Clone(),
			executor: executor,
			provider: providerKey,
		})
	}
	m.mu.RUnlock()

	var known []creditsCandidateEntry
	var unknown []creditsCandidateEntry
	for _, candidate := range candidates {
		hint, okHint, errHint := GetAntigravityCreditsHintRequired(ctx, candidate.auth.ID)
		if errHint != nil {
			return nil, antigravityCreditsKVUnavailableError(errHint)
		}
		if okHint && hint.Known {
			if !hint.Available {
				continue
			}
			known = append(known, candidate)
			continue
		}
		unknown = append(unknown, candidate)
	}
	sort.Slice(known, func(i, j int) bool {
		return known[i].auth.ID < known[j].auth.ID
	})
	sort.Slice(unknown, func(i, j int) bool {
		return unknown[i].auth.ID < unknown[j].auth.ID
	})
	return append(known, unknown...), nil
}

type creditsCandidateEntry struct {
	auth     *Auth
	executor ProviderExecutor
	provider string
}

func hasAntigravityProvider(providers []string) bool {
	for _, p := range providers {
		if strings.EqualFold(strings.TrimSpace(p), "antigravity") {
			return true
		}
	}
	return false
}

func shouldAttemptAntigravityCreditsFallback(m *Manager, lastErr error, providers []string) bool {
	status := statusCodeFromError(lastErr)
	log.WithFields(log.Fields{
		"lastErr":   errorString(lastErr),
		"status":    status,
		"providers": providers,
	}).Debug("shouldAttemptAntigravityCreditsFallback")
	if m == nil || lastErr == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.QuotaExceeded.AntigravityCredits {
		return false
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case 0:
		var authErr *Error
		if errors.As(lastErr, &authErr) && authErr != nil {
			return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable" || authErr.Code == "model_cooldown"
		}
		var cooldownErr *modelCooldownError
		if errors.As(lastErr, &cooldownErr) {
			return true
		}
		return false
	default:
		return false
	}
}

func (m *Manager) tryAntigravityCreditsExecute(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, bool, error) {
	routeModel := req.Model
	candidates, errCandidates := m.findAllAntigravityCreditsCandidateAuths(ctx, routeModel, opts)
	if errCandidates != nil {
		return cliproxyexecutor.Response{}, false, errCandidates
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return cliproxyexecutor.Response{}, false, nil
		}
		creditsCtx := WithAntigravityCredits(ctx)
		if rt := m.roundTripperFor(c.auth); rt != nil {
			creditsCtx = context.WithValue(creditsCtx, roundTripperContextKey{}, rt)
			creditsCtx = context.WithValue(creditsCtx, "cliproxy.roundtripper", rt)
		}
		creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
		creditsCtx = contextWithRequestedModelAlias(creditsCtx, creditsOpts, routeModel)
		preparedAuth, errPrepare := m.prepareRequestAuth(creditsCtx, c.executor, c.auth)
		if errPrepare != nil {
			continue
		}
		c.auth = preparedAuth
		publishSelectedAuthMetadata(creditsOpts.Metadata, c.auth.ID)
		models := m.executionModelCandidates(c.auth, routeModel)
		if len(models) == 0 {
			continue
		}
		for _, upstreamModel := range models {
			resultModel := m.stateModelForExecution(c.auth, routeModel, upstreamModel, len(models) > 1)
			execReq := req
			execReq.Model = upstreamModel
			logRoutePlan(creditsCtx, c.auth, c.provider, routeModel, resultModel, upstreamModel, creditsOpts, c.executor, "execute")
			if trace := requestAttemptTraceFromContext(creditsCtx); trace != nil {
				trace.recordExecution(c.provider, resultModel, providerExecutorName(c.executor))
			}
			resp, errExec := c.executor.Execute(creditsCtx, c.auth, execReq, creditsOpts)
			result := Result{AuthID: c.auth.ID, Provider: c.provider, Model: resultModel, Success: errExec == nil}
			if errExec != nil {
				result.Error = resultErrorFromCause(errExec)
				result.Cause = errExec
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.MarkResult(creditsCtx, result)
				if trace := requestAttemptTraceFromContext(creditsCtx); trace != nil {
					trace.recordFinalStatus(statusCodeFromError(errExec))
				}
				continue
			}
			m.MarkResult(creditsCtx, result)
			return resp, true, nil
		}
	}
	return cliproxyexecutor.Response{}, false, nil
}

func (m *Manager) tryAntigravityCreditsExecuteStream(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, bool, error) {
	routeModel := req.Model
	candidates, errCandidates := m.findAllAntigravityCreditsCandidateAuths(ctx, routeModel, opts)
	if errCandidates != nil {
		return nil, false, errCandidates
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return nil, false, nil
		}
		creditsCtx := WithAntigravityCredits(ctx)
		if rt := m.roundTripperFor(c.auth); rt != nil {
			creditsCtx = context.WithValue(creditsCtx, roundTripperContextKey{}, rt)
			creditsCtx = context.WithValue(creditsCtx, "cliproxy.roundtripper", rt)
		}
		creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
		preparedAuth, errPrepare := m.prepareRequestAuth(creditsCtx, c.executor, c.auth)
		if errPrepare != nil {
			continue
		}
		c.auth = preparedAuth
		publishSelectedAuthMetadata(creditsOpts.Metadata, c.auth.ID)
		models := m.executionModelCandidates(c.auth, routeModel)
		if len(models) == 0 {
			continue
		}
		result, errStream := m.executeStreamWithModelPool(creditsCtx, c.executor, c.auth, c.provider, []string{c.provider}, req, creditsOpts, routeModel, models, len(models) > 1)
		if errStream != nil {
			continue
		}
		return result, true, nil
	}
	return nil, false, nil
}

func antigravityCreditsKVUnavailableError(cause error) error {
	if cause == nil {
		return &Error{Code: "home_kv_unavailable", Message: "home kv store unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}
	return &Error{Code: "home_kv_unavailable", Message: "home kv store unavailable: " + cause.Error(), HTTPStatus: http.StatusServiceUnavailable}
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if IsConfigAPIKeyAuth(auth) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	if IsPluginVirtualAuth(auth) {
		return nil
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)
	workers := refreshMaxConcurrency
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
		workers = cfg.AuthAutoRefreshWorkers
	}
	loop := newAuthAutoRefreshLoop(m, interval, workers)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.mu.Unlock()

	loop.rebuild(time.Now())
	go loop.run(ctx)
}

// StopAutoRefresh cancels the background refresh loop, if running.
// It also stops the selector if it implements StoppableSelector.
func (m *Manager) StopAutoRefresh() {
	m.mu.Lock()
	cancel := m.refreshCancel
	selector := m.selector
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Stop selector if it implements StoppableSelector (e.g., SessionAffinitySelector)
	if stoppable, ok := selector.(StoppableSelector); ok {
		stoppable.Stop()
	}
	m.stopDynamicSelectors()
}

func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

func (m *Manager) queueRefreshUnschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.remove(authID)
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	if hasUnauthorizedAuthFailure(a) {
		if !authHasRefreshCredential(a) || a.NextRefreshAfter.IsZero() {
			return false
		}
		return !now.Before(a.NextRefreshAfter)
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		m.mu.Unlock()
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		m.mu.Unlock()
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	m.mu.Unlock()

	m.queueRefreshReschedule(id)
	return true
}

type authRefreshLock struct {
	mu          sync.Mutex
	lastFailure *authRefreshFailure
}

type authRefreshFailure struct {
	credentialVersion [sha256.Size]byte
	err               *Error
	retryAt           time.Time
	terminal          bool
}

func authAccessToken(auth *Auth) string {
	if token := authMetadataString(auth, "access_token"); token != "" {
		return token
	}
	return authMetadataString(auth, "accessToken")
}

func authRefreshToken(auth *Auth) string {
	if token := authMetadataString(auth, "refresh_token"); token != "" {
		return token
	}
	return authMetadataString(auth, "refreshToken")
}

func authHasRefreshCredential(auth *Auth) bool {
	return authRefreshToken(auth) != ""
}

func authRefreshCredentialVersion(auth *Auth) [sha256.Size]byte {
	if auth == nil {
		return [sha256.Size]byte{}
	}
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, authAccessToken(auth))
	_, _ = hasher.Write([]byte{0})
	_, _ = io.WriteString(hasher, authRefreshToken(auth))
	_, _ = hasher.Write([]byte{0})
	lastRefreshedAt := auth.LastRefreshedAt
	if lastRefreshedAt.IsZero() {
		if ts, ok := authLastRefreshTimestamp(auth); ok {
			lastRefreshedAt = ts
		}
	}
	if !lastRefreshedAt.IsZero() {
		_, _ = io.WriteString(hasher, lastRefreshedAt.UTC().Format(time.RFC3339Nano))
	}
	var version [sha256.Size]byte
	copy(version[:], hasher.Sum(nil))
	return version
}

func isTerminalAuthRefreshError(err error) bool {
	if err == nil {
		return false
	}
	if isUnauthorizedError(err) || isInvalidGrantError(err) {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(errorCodeFromError(err) + " " + err.Error()))
	return strings.Contains(normalized, "refresh_token_reused")
}

func terminalAuthRefreshStatus(err error) string {
	if isInvalidGrantError(err) {
		return "invalid_grant"
	}
	normalized := strings.ToLower(strings.TrimSpace(errorCodeFromError(err) + " " + err.Error()))
	if strings.Contains(normalized, "refresh_token_reused") {
		return "refresh_token_reused"
	}
	return "unauthorized"
}

func clearUnauthorizedModelStates(auth *Auth, now time.Time) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	var resumed []string
	for model, state := range auth.ModelStates {
		if state == nil || state.LastError == nil {
			continue
		}
		if state.LastError.StatusCode() != http.StatusUnauthorized &&
			!strings.EqualFold(state.LastError.Code, "unauthorized") {
			continue
		}
		resetModelState(state, now)
		resumed = append(resumed, model)
	}
	if len(resumed) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	return resumed
}

func (m *Manager) tryRefreshAfterUnauthorized(ctx context.Context, auth *Auth, execErr error, alreadyTried bool) (*Auth, bool) {
	if m == nil || auth == nil || alreadyTried || execErr == nil {
		return auth, false
	}
	if !isCodexAuth(auth) ||
		isCodexAPIKeyAuth(auth) ||
		!shouldEvictUnauthorizedError(execErr) ||
		!authHasRefreshCredential(auth) {
		return auth, false
	}
	refreshed, errRefresh := m.refreshAuthForRequest(ctx, auth.ID, authAccessToken(auth))
	if errRefresh != nil || refreshed == nil {
		logEntryWithRequestID(ctx).WithFields(log.Fields{
			"provider":   auth.Provider,
			"auth_index": authMetricIndex(auth),
			"status":     statusCodeFromError(errRefresh),
			"error_code": errorCodeFromError(errRefresh),
		}).Warn("credential refresh after unauthorized response failed")
		return auth, false
	}
	return refreshed, true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	_, _ = m.refreshAuthForRequest(ctx, id, "")
}

func (m *Manager) refreshAuthForRequest(ctx context.Context, id, failedAccessToken string) (*Auth, error) {
	if m == nil {
		return nil, errors.New("auth manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("auth id is empty")
	}

	lockValue, _ := m.refreshLocks.LoadOrStore(id, &authRefreshLock{})
	lock, _ := lockValue.(*authRefreshLock)
	if lock == nil {
		lock = &authRefreshLock{}
		m.refreshLocks.Store(id, lock)
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()

	m.mu.RLock()
	storedAuth := m.auths[id]
	var exec ProviderExecutor
	var auth *Auth
	if storedAuth != nil {
		exec = m.executors[storedAuth.Provider]
		auth = storedAuth.Clone()
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return nil, errors.New("auth or executor not found")
	}
	credentialVersion := authRefreshCredentialVersion(auth)
	if failedAccessToken != "" {
		currentToken := authAccessToken(auth)
		if currentToken != "" && currentToken != failedAccessToken {
			return auth.Clone(), nil
		}
		now := time.Now()
		if failure := lock.lastFailure; failure != nil &&
			failure.credentialVersion == credentialVersion &&
			(failure.terminal || now.Before(failure.retryAt)) {
			if failure.err != nil {
				return nil, cloneError(failure.err)
			}
			return nil, &Error{
				Code:      "credential_refresh_backoff",
				Message:   "credential refresh is in backoff",
				Retryable: !failure.terminal,
			}
		}
	}

	cloned := auth.Clone()
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return nil, err
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		terminalFailure := isTerminalAuthRefreshError(err)
		refreshErr := refreshErrorFromError(err)
		retryAt := now.Add(refreshFailureBackoff)
		if terminalFailure {
			refreshErr.Code = "unauthorized"
			retryAt = time.Time{}
		}
		lock.lastFailure = &authRefreshFailure{
			credentialVersion: credentialVersion,
			err:               cloneError(refreshErr),
			retryAt:           retryAt,
			terminal:          terminalFailure,
		}
		shouldReschedule := false
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			current.LastError = cloneError(refreshErr)
			if terminalFailure {
				current.NextRefreshAfter = time.Time{}
				current.Unavailable = true
				current.Status = StatusError
				current.StatusMessage = terminalAuthRefreshStatus(err)
			} else {
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			}
			m.auths[id] = current
			shouldReschedule = true
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		if shouldReschedule {
			m.queueRefreshReschedule(id)
		}
		return nil, err
	}
	if updated == nil {
		updated = cloned
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.StatusMessage = ""
	updated.Unavailable = false
	if updated.Status == StatusError {
		updated.Status = StatusActive
	}
	updated.UpdatedAt = now
	modelsToResume := clearUnauthorizedModelStates(updated, now)
	updatedAccessToken := authAccessToken(updated)
	if failedAccessToken != "" &&
		(updatedAccessToken == "" || updatedAccessToken == failedAccessToken) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	} else if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	saved, errUpdate := m.Update(ctx, updated)
	if errUpdate != nil {
		retryAt := now.Add(refreshFailureBackoff)
		lock.lastFailure = &authRefreshFailure{
			credentialVersion: credentialVersion,
			err: &Error{
				Code:      "credential_refresh_persist_failed",
				Message:   "credential refresh could not be persisted",
				Retryable: true,
			},
			retryAt: retryAt,
		}
		shouldReschedule := false
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			current.LastError = &Error{
				Code:       "unauthorized",
				Message:    "credential refresh could not be persisted",
				HTTPStatus: http.StatusUnauthorized,
			}
			current.NextRefreshAfter = retryAt
			current.Unavailable = true
			current.Status = StatusError
			current.StatusMessage = "refresh persistence failed"
			m.auths[id] = current
			shouldReschedule = true
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		if shouldReschedule {
			m.queueRefreshReschedule(id)
		}
		log.Warnf("failed to persist refreshed auth %s, %s: %v", auth.Provider, auth.ID, errUpdate)
		return nil, errUpdate
	}
	for _, model := range modelsToResume {
		registry.GetGlobalRegistry().ResumeClientModel(id, model)
	}
	if saved != nil {
		if failedAccessToken != "" &&
			(authAccessToken(saved) == "" || authAccessToken(saved) == failedAccessToken) {
			lock.lastFailure = &authRefreshFailure{
				credentialVersion: authRefreshCredentialVersion(saved),
				err: &Error{
					Code:      "credential_refresh_ineffective",
					Message:   "credential refresh did not replace the rejected access token",
					Retryable: true,
				},
				retryAt: saved.NextRefreshAfter,
			}
		} else {
			lock.lastFailure = nil
		}
		return saved, nil
	}
	lock.lastFailure = nil
	return updated.Clone(), nil
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" ||
				strings.EqualFold(providerKey, auth.Provider) ||
				strings.EqualFold(providerKey, "openai-compatible-pool") {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		providerKey := strings.TrimSpace(auth.Label)
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		return util.OpenAICompatibleProviderKey(providerKey)
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func executorKeyForProviderSet(auth *Auth, providerSet map[string]struct{}, executors map[string]ProviderExecutor) string {
	for _, key := range executorKeyCandidatesFromAuth(auth) {
		if key == "" {
			continue
		}
		if len(providerSet) > 0 {
			if _, ok := providerSet[key]; !ok {
				continue
			}
		}
		if len(executors) > 0 {
			if _, ok := executors[key]; !ok {
				continue
			}
		}
		return key
	}
	return ""
}

func executorKeyCandidatesFromAuth(auth *Auth) []string {
	if auth == nil {
		return nil
	}
	candidates := make([]string, 0, 5)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		add(compatName)
		add(providerKey)
		if providerKey != "" {
			add(util.OpenAICompatibleProviderKey(providerKey))
		}
		if compatName != "" {
			add(util.OpenAICompatibleProviderKey(compatName))
		}
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		label := strings.TrimSpace(auth.Label)
		if label == "" {
			label = "openai-compatibility"
		}
		add(util.OpenAICompatibleProviderKey(label))
	}
	add(auth.Provider)
	return candidates
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	entry := log.NewEntry(log.StandardLogger())
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		entry = entry.WithField("request_id", reqID)
	}
	if clientRequestID := logging.GetClientRequestID(ctx); clientRequestID != "" {
		entry = entry.WithField("client_request_id", clientRequestID)
	}
	return entry
}

func authMetricIndex(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if index := strings.TrimSpace(auth.Index); index != "" {
		return index
	}
	return auth.EnsureIndex()
}

func selectorMetricStrategy(selector Selector) string {
	switch s := selector.(type) {
	case *RoundRobinSelector:
		return RoutingStrategyRoundRobin
	case *FillFirstSelector:
		return RoutingStrategyFillFirst
	case *SequentialFillSelector:
		return RoutingStrategySequentialFill
	case *SpreadSelector:
		if s.channelAware {
			return "channel-spread"
		}
		return RoutingStrategySpread
	case *SessionAffinitySelector:
		fallback := selectorMetricStrategy(s.fallback)
		if fallback == "" {
			return "session-affinity"
		}
		return "session-affinity+" + fallback
	default:
		return "custom"
	}
}

func (m *Manager) authMetricRouting(auth *Auth) (string, string) {
	if m == nil {
		return "default", ""
	}
	if group, _, ok := m.routingStrategyForAuths([]*Auth{auth}); ok {
		return group, selectorMetricStrategy(m.selectorForAuths([]*Auth{auth}))
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	return "default", selectorMetricStrategy(selector)
}

func (m *Manager) authMetricFields(auth *Auth, provider, model string) log.Fields {
	fields := log.Fields{
		"provider": provider,
		"model":    canonicalModelKey(model),
	}
	if auth == nil {
		return fields
	}
	fields["auth_index"] = authMetricIndex(auth)
	if prefix := strings.TrimSpace(auth.Prefix); prefix != "" {
		fields["prefix"] = prefix
	}
	if baseURL := authMetricBaseURL(auth); baseURL != "" {
		fields["base_url"] = baseURL
	}
	if tokenHash := authMetricTokenHash(auth); tokenHash != "" {
		fields["token_hash"] = tokenHash
	}
	if group := authRoutingGroup(auth); group != "" {
		fields["routing_group"] = group
	}
	scope, strategy := m.authMetricRouting(auth)
	if scope != "" {
		fields["routing_scope"] = scope
	}
	if strategy != "" {
		fields["routing_strategy"] = strategy
	}
	return fields
}

func authMetricBaseURL(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return sanitizeAuthMetricBaseURL(auth.Attributes["base_url"])
}

func sanitizeAuthMetricBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return strings.TrimRight(parsed.String(), "/")
	}
	if idx := strings.IndexAny(raw, "?#"); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func authMetricTokenHash(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	token := strings.TrimSpace(auth.Attributes["api_key"])
	if token == "" {
		return ""
	}
	return stableAuthIndex("token:" + token)
}

func (m *Manager) logAuthSelectionMetric(ctx context.Context, auth *Auth, provider, model string) {
	if auth == nil {
		return
	}
	fields := m.authMetricFields(auth, provider, model)
	fields["event"] = "auth_selection"
	addRequestAttemptLogFields(ctx, fields)
	logEntryWithRequestID(ctx).WithFields(fields).Info("auth_selection")
}

func (m *Manager) logAuthSelectionFailureMetric(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, err error) {
	if err == nil {
		return
	}
	fields := log.Fields{
		"event":     "auth_selection_failed",
		"providers": strings.Join(normalizeProviderKeys(providers), ","),
		"model":     canonicalModelKey(model),
	}
	addRequestAttemptLogFields(ctx, fields)
	if status := statusCodeFromError(err); status > 0 {
		fields["status"] = status
		fields["status_code"] = status
	}
	terminalCode := strings.TrimSpace(errorCodeFromError(err))
	if terminalCode != "" {
		fields["error_code"] = terminalCode
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		if terminalCode == "" && authErr.Code != "" {
			fields["error_code"] = authErr.Code
		}
		if authErr.Retryable {
			fields["retryable"] = true
		}
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) && cooldownErr != nil {
		if terminalCode == "" {
			fields["error_code"] = "model_cooldown"
			fields["reset_ms"] = cooldownErr.resetIn.Milliseconds()
		} else {
			fields["cause_error_code"] = "model_cooldown"
			fields["cause_reset_ms"] = cooldownErr.resetIn.Milliseconds()
		}
	}
	for key, value := range m.authAvailabilityMetricFieldsForRequest(providers, model, opts, time.Now()) {
		fields[key] = value
	}
	logEntryWithRequestID(ctx).WithFields(fields).Warn("auth_selection_failed")
}

func (m *Manager) authAvailabilityMetricFields(providers []string, model string, now time.Time) log.Fields {
	return m.gptRouteAvailabilitySnapshot(providers, model, now).logFields(now)
}

func (m *Manager) authAvailabilityMetricFieldsForRequest(providers []string, model string, opts cliproxyexecutor.Options, now time.Time) log.Fields {
	ordinaryFields := m.authAvailabilityMetricFields(providers, model, now)
	intent := compactionIntentFromRequest(cliproxyexecutor.Request{}, opts)
	if !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return ordinaryFields
	}

	compactionFields := m.remoteCompactionRouteAvailabilitySnapshot(providers, model, intent, now).logFields(now)
	fields := make(log.Fields, len(ordinaryFields)+len(compactionFields)*2+1)
	for key, value := range ordinaryFields {
		fields["ordinary_"+key] = value
	}
	for key, value := range compactionFields {
		fields[key] = value
		fields["compaction_"+key] = value
	}
	fields["compaction_intent"] = string(intent)
	return fields
}

func blockReasonLabel(reason blockReason) string {
	switch reason {
	case blockReasonDisabled:
		return "disabled"
	case blockReasonCooldown:
		return "cooldown"
	case blockReasonOther:
		return "health_or_unavailable"
	default:
		return "unknown"
	}
}

func formatReasonCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+":"+strconv.Itoa(counts[key]))
	}
	return strings.Join(parts, ",")
}

func (m *Manager) logAuthResultMetric(ctx context.Context, auth *Auth, result Result) {
	result = normalizeResultFailureContract(result)
	fields := m.authMetricFields(auth, result.Provider, result.Model)
	fields["event"] = "auth_result"
	fields["success"] = result.Success
	addRequestAttemptLogFields(ctx, fields)
	if result.Duration > 0 {
		fields["duration_ms"] = result.Duration.Milliseconds()
		if penalty := slowRequestHealthPenalty(result.Duration); result.Success && penalty > 0 {
			fields["slow_penalty"] = penalty
		}
	}
	status := statusCodeFromResult(result.Error)
	if result.Success && status == 0 {
		status = http.StatusOK
	}
	if status > 0 {
		fields["status"] = status
		fields["status_code"] = status
	}
	if failure, ok := typedFailureFromResult(result); ok {
		fields["retryable"] = failure.Retryable
		fields["output_committed"] = failure.OutputCommitted
		if failure.HTTPStatus > 0 {
			fields["normalized_status"] = failure.HTTPStatus
		}
		if failure.Kind != "" {
			fields["failure_kind"] = string(failure.Kind)
		}
		if failure.Scope != "" {
			fields["failure_scope"] = string(failure.Scope)
			fields["scope"] = string(failure.Scope)
		}
		if failure.OuterStatus > 0 {
			fields["outer_status"] = failure.OuterStatus
			fields["upstream_status"] = failure.OuterStatus
		}
		if semanticCode := strings.TrimSpace(failure.SemanticCode); semanticCode != "" {
			fields["semantic_code"] = semanticCode
			fields["error_code"] = semanticCode
		}
		if semanticType := strings.TrimSpace(failure.SemanticType); semanticType != "" {
			fields["semantic_type"] = semanticType
		}
		if failure.StreamPhase != "" {
			fields["stream_phase"] = string(failure.StreamPhase)
		}
		if upstreamRequestID := strings.TrimSpace(failure.UpstreamRequestID); upstreamRequestID != "" {
			fields["upstream_request_id"] = upstreamRequestID
		}
	} else if result.Error != nil {
		if result.Error.Kind != "" {
			fields["failure_kind"] = result.Error.Kind
		}
		if result.Error.Scope != "" {
			fields["failure_scope"] = result.Error.Scope
		}
		if result.Error.Code != "" {
			fields["error_code"] = result.Error.Code
		}
		fields["retryable"] = result.Error.Retryable
	}
	if result.RetryAfter != nil {
		fields["retry_after_ms"] = result.RetryAfter.Milliseconds()
	}
	logEntryWithRequestID(ctx).WithFields(fields).Info("auth_result")
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[executorKeyFromAuth(a)]
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}

// ExecuteRawEndpoint delegates an untranslated endpoint exchange to its provider executor.
func (m *Manager) ExecuteRawEndpoint(ctx context.Context, auth *Auth, req cliproxyexecutor.RawEndpointRequest) (cliproxyexecutor.RawEndpointResponse, error) {
	if m == nil {
		return cliproxyexecutor.RawEndpointResponse{}, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return cliproxyexecutor.RawEndpointResponse{}, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return cliproxyexecutor.RawEndpointResponse{}, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return cliproxyexecutor.RawEndpointResponse{}, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	rawExecutor, ok := exec.(RawEndpointExecutor)
	if !ok || rawExecutor == nil {
		return cliproxyexecutor.RawEndpointResponse{}, &Error{Code: "not_supported", Message: "executor does not support raw endpoints"}
	}
	return rawExecutor.ExecuteRawEndpoint(ctx, auth, req)
}
