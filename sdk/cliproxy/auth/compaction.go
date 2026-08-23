package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	compactionBreakerFailureThreshold = 3
	compactionBreakerTransientOpen    = time.Minute
	compactionBreakerProtocolOpen     = 10 * time.Minute
	remoteCompactionMaxAttempts       = 2
)

type compactionBreakerState struct {
	ConsecutiveFailures int
	OpenUntil           time.Time
	LastErrorCode       string
}

type remoteCompactionFallbackGuard struct {
	enabled            bool
	compatibilityGroup string
	attempts           int
}

func newRemoteCompactionFallbackGuard(intent cliproxyexecutor.CompactionIntent) *remoteCompactionFallbackGuard {
	return &remoteCompactionFallbackGuard{enabled: cliproxyexecutor.IsRemoteCompactionIntent(intent)}
}

func (g *remoteCompactionFallbackGuard) shouldSkipAuth(auth *Auth) bool {
	if g == nil || !g.enabled || g.attempts == 0 {
		return false
	}
	if g.attempts >= remoteCompactionMaxAttempts {
		return true
	}
	if g.compatibilityGroup == "" {
		return true
	}
	capability := ResolveResponsesCompactionCapability(auth)
	return strings.TrimSpace(capability.CompatibilityGroup) != g.compatibilityGroup
}

func (g *remoteCompactionFallbackGuard) markAuth(auth *Auth) {
	if g == nil || !g.enabled || auth == nil {
		return
	}
	if g.attempts == 0 {
		capability := ResolveResponsesCompactionCapability(auth)
		g.compatibilityGroup = strings.TrimSpace(capability.CompatibilityGroup)
	}
	g.attempts++
}

func (g *remoteCompactionFallbackGuard) canFallback(err error) bool {
	if g == nil || !g.enabled || g.attempts != 1 || g.compatibilityGroup == "" || err == nil {
		return false
	}
	if failure, ok := failurecontract.As(err); ok {
		if failure.OutputCommitted || failure.Scope == failurecontract.ScopeRequest {
			return false
		}
		if failure.Retryable {
			return true
		}
	}
	if isRequestInvalidError(err) {
		return false
	}
	switch statusCodeFromError(err) {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return isTransientRoutingError(err)
	}
}

func (m *Manager) prepareRemoteCompactionFallback(ctx context.Context, providers []string, model string, intent cliproxyexecutor.CompactionIntent, guard *remoteCompactionFallbackGuard, auth *Auth, tried map[string]struct{}, err error) bool {
	if m == nil || !guard.canFallback(err) {
		return false
	}
	m.markRetryChannelTried(ctx, tried, auth, err)
	return m.hasRemoteCompactionFallback(providers, model, intent, guard.compatibilityGroup, tried, time.Now())
}

func (m *Manager) hasRemoteCompactionFallback(providers []string, model string, intent cliproxyexecutor.CompactionIntent, compatibilityGroup string, tried map[string]struct{}, now time.Time) bool {
	compatibilityGroup = strings.TrimSpace(compatibilityGroup)
	if m == nil || compatibilityGroup == "" || !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return false
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range normalizeProviderKeys(providers) {
		providerSet[provider] = struct{}{}
	}
	type fallbackCandidate struct {
		auth       *Auth
		checkModel string
	}
	candidates := make([]fallbackCandidate, 0)
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled || candidate.Status == StatusDisabled {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if executorKeyForProviderSet(candidate, providerSet, m.executors) == "" {
			continue
		}
		if strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		capability := ResolveResponsesCompactionCapability(candidate)
		if !remoteCompactionCandidateAllowed(candidate, intent) || strings.TrimSpace(capability.CompatibilityGroup) != compatibilityGroup {
			continue
		}
		candidates = append(candidates, fallbackCandidate{
			auth:       candidate.Clone(),
			checkModel: m.selectionModelForAuth(candidate, model),
		})
	}
	m.mu.RUnlock()

	for _, candidate := range candidates {
		if blocked, _ := m.remoteCompactionBreakerBlockState(candidate.auth, model, intent, now); blocked {
			continue
		}
		includeHealth := !isGPTRetryRoute([]string{candidate.auth.Provider}, candidate.checkModel)
		blocked, _, _ := isAuthBlockedForModelRoute(candidate.auth, candidate.checkModel, now, includeHealth)
		if !blocked {
			return true
		}
	}
	return false
}

func (m *Manager) logRemoteCompactionFallback(ctx context.Context, model string, guard *remoteCompactionFallbackGuard, err error) {
	if guard == nil || !guard.enabled {
		return
	}
	fields := log.Fields{
		"event":                          "remote_compaction_failover",
		"model":                          canonicalModelKey(model),
		"compaction_compatibility_group": guard.compatibilityGroup,
		"failed_attempt":                 guard.attempts,
	}
	if code := strings.TrimSpace(errorCodeFromError(err)); code != "" {
		fields["error_code"] = code
	}
	if status := statusCodeFromError(err); status > 0 {
		fields["status"] = status
		fields["status_code"] = status
	}
	logEntryWithRequestID(ctx).WithFields(fields).Warn("remote_compaction_failover")
}

const (
	ResponsesCompactionLegacyNative        = "native"
	ResponsesCompactionUnsupported         = "unsupported"
	ResponsesCompactionTriggerNativeStream = "native-stream"
	ResponsesCompactionTriggerBridgeLegacy = "bridge-legacy"

	attributeResponsesCompactionLegacy            = "responses_compaction_legacy"
	attributeResponsesCompactionTrigger           = "responses_compaction_trigger"
	attributeResponsesCompactionContextManagement = "responses_compaction_context_management"
	attributeResponsesCompactionGroup             = "responses_compaction_group"
)

// ResponsesCompactionCapability is the normalized per-auth remote-compaction contract.
type ResponsesCompactionCapability struct {
	LegacyEndpoint     string
	TriggerMode        string
	ContextManagement  bool
	CompatibilityGroup string
}

// ResolveResponsesCompactionCapability returns explicit attributes or conservative route defaults.
func ResolveResponsesCompactionCapability(auth *Auth) ResponsesCompactionCapability {
	capability := ResponsesCompactionCapability{
		// Unknown routes must opt in. A normal Responses or Chat endpoint is
		// not proof that the route implements the compaction protocol.
		LegacyEndpoint: ResponsesCompactionUnsupported,
		TriggerMode:    ResponsesCompactionUnsupported,
	}
	if auth == nil {
		return capability
	}

	if isOfficialCodexOAuth(auth) {
		capability.LegacyEndpoint = ResponsesCompactionLegacyNative
		capability.TriggerMode = ResponsesCompactionTriggerNativeStream
		capability.CompatibilityGroup = "codex-official"
	} else if isOfficialOpenAIEndpoint(auth) {
		capability.LegacyEndpoint = ResponsesCompactionLegacyNative
		capability.TriggerMode = ResponsesCompactionTriggerNativeStream
		capability.ContextManagement = true
		capability.CompatibilityGroup = "openai-official"
	} else if strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") {
		capability.LegacyEndpoint = ResponsesCompactionLegacyNative
		capability.TriggerMode = ResponsesCompactionTriggerBridgeLegacy
		capability.CompatibilityGroup = "xai-official"
	}

	if auth.Attributes == nil {
		return capability
	}
	if value := normalizeLegacyCompactionMode(auth.Attributes[attributeResponsesCompactionLegacy]); value != "" {
		capability.LegacyEndpoint = value
	}
	if value := normalizeTriggerCompactionMode(auth.Attributes[attributeResponsesCompactionTrigger]); value != "" {
		capability.TriggerMode = value
	}
	if value := strings.TrimSpace(auth.Attributes[attributeResponsesCompactionContextManagement]); value != "" {
		capability.ContextManagement = strings.EqualFold(value, "true")
	}
	if value := strings.TrimSpace(auth.Attributes[attributeResponsesCompactionGroup]); value != "" {
		capability.CompatibilityGroup = value
	}
	return capability
}

func normalizeLegacyCompactionMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ResponsesCompactionLegacyNative:
		return ResponsesCompactionLegacyNative
	case ResponsesCompactionUnsupported:
		return ResponsesCompactionUnsupported
	default:
		return ""
	}
}

func normalizeTriggerCompactionMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ResponsesCompactionTriggerNativeStream:
		return ResponsesCompactionTriggerNativeStream
	case ResponsesCompactionTriggerBridgeLegacy:
		return ResponsesCompactionTriggerBridgeLegacy
	case ResponsesCompactionUnsupported:
		return ResponsesCompactionUnsupported
	default:
		return ""
	}
}

func isOfficialCodexOAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && auth.AuthKind() == AuthKindOAuth
}

func isOfficialOpenAIEndpoint(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(auth.Attributes["base_url"]))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "api.openai.com"
}

func compactionIntentFromRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) cliproxyexecutor.CompactionIntent {
	return cliproxyexecutor.CompactionIntentFromOptions(req, opts)
}

func remoteCompactionCandidateAllowed(auth *Auth, intent cliproxyexecutor.CompactionIntent) bool {
	capability := ResolveResponsesCompactionCapability(auth)
	switch intent {
	case cliproxyexecutor.CompactionIntentLegacyEndpoint:
		return capability.LegacyEndpoint == ResponsesCompactionLegacyNative
	case cliproxyexecutor.CompactionIntentV2Trigger:
		return capability.TriggerMode == ResponsesCompactionTriggerNativeStream || capability.TriggerMode == ResponsesCompactionTriggerBridgeLegacy
	case cliproxyexecutor.CompactionIntentContextManagement:
		return capability.ContextManagement
	default:
		return true
	}
}

func (m *Manager) remoteCompactionCandidateAllowed(auth *Auth, model string, intent cliproxyexecutor.CompactionIntent) bool {
	if !remoteCompactionCandidateAllowed(auth, intent) {
		return false
	}
	if m == nil || auth == nil || !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return true
	}
	blocked, _ := m.remoteCompactionBreakerBlockState(auth, model, intent, time.Now())
	return !blocked
}

func (m *Manager) remoteCompactionBreakerBlockState(auth *Auth, model string, intent cliproxyexecutor.CompactionIntent, now time.Time) (bool, time.Time) {
	if m == nil || auth == nil || !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return false, time.Time{}
	}
	key := compactionBreakerKey(auth, model, intent)
	if key == "" {
		return false, time.Time{}
	}
	m.compactionBreakerMu.Lock()
	defer m.compactionBreakerMu.Unlock()
	state := m.compactionBreakers[key]
	if state == nil || state.OpenUntil.IsZero() {
		return false, time.Time{}
	}
	if !now.Before(state.OpenUntil) {
		delete(m.compactionBreakers, key)
		return false, time.Time{}
	}
	return true, state.OpenUntil
}

func (m *Manager) remoteCompactionRouteAvailabilitySnapshot(providers []string, model string, intent cliproxyexecutor.CompactionIntent, now time.Time) gptRouteAvailabilitySnapshot {
	snapshot := gptRouteAvailabilitySnapshot{
		candidateRoutes: make(map[string]struct{}),
		eligibleRoutes:  make(map[string]struct{}),
		blockedRoutes:   make(map[string]string),
		breakerRoutes:   make(map[string]int),
	}
	if m == nil || !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return snapshot
	}

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range normalizeProviderKeys(providers) {
		providerSet[provider] = struct{}{}
	}
	registryRef := registry.GetGlobalRegistry()
	type availabilityCandidate struct {
		auth       *Auth
		checkModel string
	}
	candidates := make([]availabilityCandidate, 0)
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || executorKeyForProviderSet(auth, providerSet, m.executors) == "" {
			continue
		}
		if strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		if !remoteCompactionCandidateAllowed(auth, intent) {
			continue
		}
		candidates = append(candidates, availabilityCandidate{
			auth:       auth.Clone(),
			checkModel: m.selectionModelForAuth(auth, model),
		})
	}
	m.mu.RUnlock()

	for _, candidate := range candidates {
		auth := candidate.auth
		routeKey := routingChannelBaseKey(auth)
		if routeKey == "" {
			continue
		}
		snapshot.candidateRoutes[routeKey] = struct{}{}
		if blocked, next := m.remoteCompactionBreakerBlockState(auth, model, intent, now); blocked {
			snapshot.blockedRoutes[routeKey] = "compaction_breaker"
			snapshot.breakerRoutes[routeKey] = 0
			if next.After(now) && (snapshot.earliestRecovery.IsZero() || next.Before(snapshot.earliestRecovery)) {
				snapshot.earliestRecovery = next
			}
			continue
		}

		checkModel := candidate.checkModel
		includeHealth := !isGPTRetryRoute([]string{auth.Provider}, checkModel)
		blocked, reason, next := isAuthBlockedForModelRoute(auth, checkModel, now, includeHealth)
		if !blocked && includeHealth {
			var healthNext time.Time
			blocked, healthNext = m.healthSelectionBlocked(auth, checkModel, now)
			if blocked {
				reason = blockReasonOther
				next = healthNext
			}
		}
		if !blocked {
			snapshot.eligibleRoutes[routeKey] = struct{}{}
			continue
		}

		if _, exists := snapshot.blockedRoutes[routeKey]; !exists {
			snapshot.blockedRoutes[routeKey] = blockReasonLabel(reason)
		}
		if next.After(now) && (snapshot.earliestRecovery.IsZero() || next.Before(snapshot.earliestRecovery)) {
			snapshot.earliestRecovery = next
		}
		health := resolveHealthState(auth, checkModel)
		if health.BreakerState == HealthBreakerOpen && health.OpenUntil.After(now) {
			snapshot.breakerRoutes[routeKey] = health.LastStatusCode
			if snapshot.earliestRecovery.IsZero() || health.OpenUntil.Before(snapshot.earliestRecovery) {
				snapshot.earliestRecovery = health.OpenUntil
			}
		}
	}

	for routeKey := range snapshot.eligibleRoutes {
		delete(snapshot.blockedRoutes, routeKey)
		delete(snapshot.breakerRoutes, routeKey)
	}
	return snapshot
}

func (m *Manager) remoteCompactionAvailabilityError(providers []string, model string, intent cliproxyexecutor.CompactionIntent, cause error) error {
	if cause == nil || !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return cause
	}
	now := time.Now()
	snapshot := m.remoteCompactionRouteAvailabilitySnapshot(providers, model, intent, now)
	if len(snapshot.candidateRoutes) == 0 || len(snapshot.eligibleRoutes) > 0 {
		return cause
	}

	retryAfter := compactionBreakerTransientOpen
	if hinted := retryAfterFromError(cause); hinted != nil && *hinted > 0 {
		retryAfter = *hinted
	} else if snapshot.earliestRecovery.After(now) {
		retryAfter = snapshot.earliestRecovery.Sub(now)
	}
	return newRemoteCompactionRouteUnavailableError(retryAfter, cause)
}

func compactionBreakerKey(auth *Auth, model string, intent cliproxyexecutor.CompactionIntent) string {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return ""
	}
	capability := ResolveResponsesCompactionCapability(auth)
	mode := ""
	switch intent {
	case cliproxyexecutor.CompactionIntentLegacyEndpoint:
		mode = capability.LegacyEndpoint
	case cliproxyexecutor.CompactionIntentV2Trigger:
		mode = capability.TriggerMode
	case cliproxyexecutor.CompactionIntentContextManagement:
		mode = "context-management"
	}
	return strings.Join([]string{
		strings.TrimSpace(auth.ID),
		canonicalModelKey(model),
		string(intent),
		mode,
		strings.TrimSpace(capability.CompatibilityGroup),
	}, "|")
}

func (m *Manager) markRemoteCompactionResult(ctx context.Context, auth *Auth, model string, intent cliproxyexecutor.CompactionIntent, success bool, err error) {
	if m == nil || auth == nil || !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return
	}
	key := compactionBreakerKey(auth, model, intent)
	if key == "" {
		return
	}
	m.compactionBreakerMu.Lock()
	if success {
		delete(m.compactionBreakers, key)
		m.compactionBreakerMu.Unlock()
		return
	}

	failure, _ := failurecontract.As(err)
	if failure != nil && (failure.Scope == failurecontract.ScopeRequest || failure.Scope == failurecontract.ScopeCredential) {
		m.compactionBreakerMu.Unlock()
		return
	}
	status := statusCodeFromError(err)
	code := strings.TrimSpace(errorCodeFromError(err))
	protocolFailure := code == "invalid_compaction_response" || code == "invalid_compaction_stream" || status == http.StatusNotFound || status == http.StatusNotImplemented
	if !protocolFailure && status > 0 && status < http.StatusInternalServerError {
		m.compactionBreakerMu.Unlock()
		return
	}
	state := m.compactionBreakers[key]
	if state == nil {
		state = &compactionBreakerState{}
		m.compactionBreakers[key] = state
	}
	state.ConsecutiveFailures++
	state.LastErrorCode = code
	if protocolFailure {
		state.OpenUntil = time.Now().Add(compactionBreakerProtocolOpen)
	} else if state.ConsecutiveFailures >= compactionBreakerFailureThreshold {
		state.OpenUntil = time.Now().Add(compactionBreakerTransientOpen)
	}
	openUntil := state.OpenUntil
	failures := state.ConsecutiveFailures
	m.compactionBreakerMu.Unlock()

	fields := log.Fields{
		"event":                "compaction_breaker_update",
		"auth_index":           authMetricIndex(auth),
		"model":                canonicalModelKey(model),
		"compaction_intent":    string(intent),
		"consecutive_failures": failures,
		"error_code":           code,
	}
	if !openUntil.IsZero() {
		fields["open_until"] = openUntil.UTC().Format(time.RFC3339)
	}
	logEntryWithRequestID(ctx).WithFields(fields).Warn("compaction_breaker_update")
}

func (m *Manager) markExecutionResult(ctx context.Context, auth *Auth, selectorModel string, result Result, intent cliproxyexecutor.CompactionIntent) {
	if cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		m.markRemoteCompactionResult(ctx, auth, selectorModel, intent, result.Success, result.Cause)
		m.markSelectorLoadDone(ctx, result.AuthID, selectorModel)
		return
	}
	m.MarkResult(ctx, result)
}

func remoteCompactionSelectionError(intent cliproxyexecutor.CompactionIntent) error {
	code := "remote_compaction_unsupported"
	message := "no available route supports remote compaction"
	scope := failurecontract.ScopeModel
	switch intent {
	case cliproxyexecutor.CompactionIntentV2Trigger:
		code = "remote_compaction_trigger_unsupported"
		message = "no available route supports remote compaction v2"
	case cliproxyexecutor.CompactionIntentContextManagement:
		code = "remote_compaction_context_management_unsupported"
		message = "no available route supports context_management compaction"
		scope = failurecontract.ScopeRequest
	}
	return &failurecontract.Failure{
		Kind:          failurecontract.UnsupportedFeature,
		Scope:         scope,
		HTTPStatus:    http.StatusBadRequest,
		OuterStatus:   http.StatusBadRequest,
		ProviderCode:  code,
		SemanticCode:  code,
		SemanticType:  "invalid_request_error",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		Retryable:     false,
		PublicMessage: code + ": " + message,
	}
}

func remoteCompactionRouteUnavailableError() error {
	return newRemoteCompactionRouteUnavailableError(compactionBreakerTransientOpen, nil)
}

func newRemoteCompactionRouteUnavailableError(retryAfter time.Duration, cause error) error {
	if retryAfter <= 0 {
		retryAfter = compactionBreakerTransientOpen
	}
	return &failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeModel,
		HTTPStatus:    http.StatusServiceUnavailable,
		OuterStatus:   http.StatusServiceUnavailable,
		ProviderCode:  "compaction_route_unavailable",
		SemanticCode:  "compaction_route_unavailable",
		SemanticType:  "server_error",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		RetryAfter:    &retryAfter,
		Retryable:     true,
		Cause:         cause,
		PublicMessage: "compaction_route_unavailable: all compatible remote-compaction routes are temporarily unavailable",
	}
}

func withSelectedCompactionCapability(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, auth *Auth) cliproxyexecutor.Options {
	intent := cliproxyexecutor.CompactionIntentFromOptions(req, opts)
	if !cliproxyexecutor.IsRemoteCompactionIntent(intent) {
		return opts
	}
	metadata := make(map[string]any, len(opts.Metadata)+3)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	capability := ResolveResponsesCompactionCapability(auth)
	metadata[cliproxyexecutor.CompactionIntentMetadataKey] = string(intent)
	metadata[cliproxyexecutor.CompactionCompatibilityGroupMetadataKey] = capability.CompatibilityGroup
	metadata[cliproxyexecutor.CompactionTriggerModeMetadataKey] = capability.TriggerMode
	opts.Metadata = metadata
	return opts
}
