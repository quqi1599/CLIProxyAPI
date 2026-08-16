package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	compactionBreakerFailureThreshold = 3
	compactionBreakerTransientOpen    = time.Minute
	compactionBreakerProtocolOpen     = 10 * time.Minute
)

type compactionBreakerState struct {
	ConsecutiveFailures int
	OpenUntil           time.Time
	LastErrorCode       string
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
	key := compactionBreakerKey(auth, model, intent)
	if key == "" {
		return true
	}
	now := time.Now()
	m.compactionBreakerMu.Lock()
	defer m.compactionBreakerMu.Unlock()
	state := m.compactionBreakers[key]
	if state == nil || state.OpenUntil.IsZero() {
		return true
	}
	if !now.Before(state.OpenUntil) {
		delete(m.compactionBreakers, key)
		return true
	}
	return false
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
	retryAfter := compactionBreakerTransientOpen
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
