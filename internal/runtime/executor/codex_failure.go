package executor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexSemanticIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,127}$`)

type codexFailureInput struct {
	outerStatus     int
	headers         http.Header
	body            []byte
	streamPhase     failurecontract.StreamPhase
	outputCommitted bool
	now             time.Time
}

type codexFailureSemantics struct {
	code    string
	typeID  string
	message string
}

// canonicalCodexFailure is the single Codex failure classification entrypoint.
// It keeps transport status separate from semantic failure data so an upstream
// HTTP 400 carrying an explicit server_error can be retried without broadening
// retry behavior for ordinary request errors.
func canonicalCodexFailure(input codexFailureInput) (*failurecontract.Failure, []byte) {
	originalBody := input.body
	classifiedBody := sanitizeCodexStatusErrorBody(originalBody)
	semantics := codexFailureSemanticFields(classifiedBody)
	phase := input.streamPhase
	if input.outputCommitted {
		phase = failurecontract.StreamPhaseAfterOutput
	} else if phase == "" || phase == failurecontract.StreamPhaseUnknown {
		phase = failurecontract.StreamPhaseBeforeOutput
	}
	if input.now.IsZero() {
		input.now = time.Now()
	}

	failure := &failurecontract.Failure{
		OuterStatus:       input.outerStatus,
		SemanticCode:      semantics.code,
		SemanticType:      semantics.typeID,
		ProviderCode:      semantics.code,
		StreamPhase:       phase,
		OutputCommitted:   input.outputCommitted,
		UpstreamRequestID: codexUpstreamRequestID(input.headers, classifiedBody),
	}
	failure.RetryAfter = codexCanonicalRetryAfter(input.headers, classifiedBody, input.now)
	classifyCodexFailureSemantics(failure, semantics.message, classifiedBody)
	if failure.OutputCommitted {
		failure.Retryable = false
	}
	return failure, classifiedBody
}

func newCodexTransportStatusErr(outerStatus int, headers http.Header, cause error, outputCommitted bool) error {
	return newCodexTransportStatusErrWithCancellation(outerStatus, headers, cause, outputCommitted, true)
}

func newCodexTransportStatusErrWithCancellation(outerStatus int, headers http.Header, cause error, outputCommitted, callerCancellation bool) error {
	if outerStatus <= 0 {
		outerStatus = http.StatusOK
	}
	phase := failurecontract.StreamPhaseBeforeOutput
	if outputCommitted {
		phase = failurecontract.StreamPhaseAfterOutput
	}
	normalizedStatus := http.StatusBadGateway
	semanticCode := "upstream_transport_error"
	semanticType := "server_error"
	retryAfter := codexCanonicalRetryAfter(headers, nil, time.Now())
	requestID := codexUpstreamRequestID(headers, nil)
	existing, hasExisting := failurecontract.As(cause)
	if hasExisting {
		if code := strings.TrimSpace(existing.SemanticCode); code != "" {
			semanticCode = code
		} else if code := strings.TrimSpace(existing.ProviderCode); code != "" {
			semanticCode = code
		}
		if typeID := strings.TrimSpace(existing.SemanticType); typeID != "" {
			semanticType = typeID
		}
		if existing.RetryAfter != nil {
			retryAfter = existing.RetryAfter
		}
		if requestID == "" {
			requestID = strings.TrimSpace(existing.UpstreamRequestID)
		}
	}
	if hasExisting && (existing.Scope == failurecontract.ScopeRequest || existing.Kind == failurecontract.Cancelled) {
		return enrichCodexEstablishedFailure(outerStatus, headers, cause, existing, phase, outputCommitted, requestID, retryAfter)
	}
	var requestScoped interface{ IsRequestScoped() bool }
	if errors.As(cause, &requestScoped) && requestScoped.IsRequestScoped() {
		classified := failurecontract.Classify(cause)
		return enrichCodexEstablishedFailure(outerStatus, headers, cause, classified, phase, outputCommitted, requestID, retryAfter)
	}
	if codexTransportFailureIsTimeout(cause, semanticCode) {
		normalizedStatus = http.StatusGatewayTimeout
		semanticCode = "upstream_timeout"
	}
	kind := failurecontract.TransportError
	scope := failurecontract.ScopeProvider
	retryable := !outputCommitted
	if callerCancellation && errors.Is(cause, context.Canceled) {
		normalizedStatus = 499
		semanticCode = "request_cancelled"
		semanticType = "cancelled"
		kind = failurecontract.Cancelled
		scope = failurecontract.ScopeRequest
		retryable = false
	}
	message := []byte(`{"error":{"message":"upstream transport failed","type":"server_error","code":"upstream_transport_error"}}`)
	message, _ = sjson.SetBytes(message, "error.type", semanticType)
	message, _ = sjson.SetBytes(message, "error.code", semanticCode)
	failure := &failurecontract.Failure{
		Kind:              kind,
		Scope:             scope,
		HTTPStatus:        normalizedStatus,
		OuterStatus:       outerStatus,
		ProviderCode:      semanticCode,
		SemanticCode:      semanticCode,
		SemanticType:      semanticType,
		StreamPhase:       phase,
		OutputCommitted:   outputCommitted,
		UpstreamRequestID: requestID,
		RetryAfter:        retryAfter,
		Retryable:         retryable,
		Cause:             cause,
		PublicMessage:     string(message),
	}
	var clonedHeaders http.Header
	if headers != nil {
		clonedHeaders = headers.Clone()
	}
	return statusErr{
		code:               normalizedStatus,
		providerStatusCode: outerStatus,
		msg:                failure.PublicMessage,
		errorCode:          semanticCode,
		retryAfter:         retryAfter,
		headers:            clonedHeaders,
		failure:            failure,
	}
}

// newCodexHTTPDoStatusErr canonicalizes only errors returned by http.Client.Do.
// Keeping this conversion at the transport boundary prevents unrelated plain
// executor errors from becoming retryable provider failures.
func newCodexHTTPDoStatusErr(ctx context.Context, cause error, outputCommitted bool) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return newCodexCallerCancellationStatusErr(499, nil, ctx.Err(), outputCommitted)
	}
	if _, ok := failurecontract.As(cause); ok {
		return cause
	}
	status := http.StatusBadGateway
	if codexTransportFailureIsTimeout(cause, "") {
		status = http.StatusGatewayTimeout
	}
	return newCodexTransportStatusErrWithCancellation(status, nil, cause, outputCommitted, false)
}

func enrichCodexEstablishedFailure(outerStatus int, headers http.Header, cause error, existing *failurecontract.Failure, phase failurecontract.StreamPhase, outputCommitted bool, requestID string, retryAfter *time.Duration) error {
	classified := failurecontract.Classify(cause)
	if existing != nil {
		clone := *existing
		classified = &clone
	}
	if classified == nil {
		classified = &failurecontract.Failure{}
	}
	if classified.HTTPStatus <= 0 {
		classified.HTTPStatus = http.StatusBadRequest
	}
	if classified.Kind == "" {
		if classified.HTTPStatus == http.StatusRequestEntityTooLarge {
			classified.Kind = failurecontract.RequestTooLarge
		} else {
			classified.Kind = failurecontract.InvalidRequest
		}
	}
	classified.Scope = failurecontract.ScopeRequest
	classified.OuterStatus = outerStatus
	classified.StreamPhase = phase
	classified.OutputCommitted = outputCommitted
	classified.Retryable = false
	classified.Cause = cause
	if requestID != "" {
		classified.UpstreamRequestID = requestID
	}
	if classified.RetryAfter == nil {
		classified.RetryAfter = retryAfter
	}
	if strings.TrimSpace(classified.PublicMessage) == "" {
		classified.PublicMessage = cause.Error()
	}
	semanticCode := strings.TrimSpace(classified.SemanticCode)
	if semanticCode == "" {
		semanticCode = strings.TrimSpace(classified.ProviderCode)
	}
	var clonedHeaders http.Header
	if headers != nil {
		clonedHeaders = headers.Clone()
	}
	statusFailure := statusErr{
		code:               classified.HTTPStatus,
		providerStatusCode: outerStatus,
		msg:                classified.PublicMessage,
		errorCode:          semanticCode,
		retryAfter:         classified.RetryAfter,
		headers:            clonedHeaders,
		failure:            classified,
	}
	if classified.Scope == failurecontract.ScopeRequest {
		return codexCanonicalRequestStatusErr{statusErr: statusFailure}
	}
	return statusFailure
}

type codexCanonicalRequestStatusErr struct {
	statusErr
}

func (codexCanonicalRequestStatusErr) IsRequestScoped() bool { return true }

func codexTransportFailureIsTimeout(cause error, semanticCode string) bool {
	if errors.Is(cause, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(cause, &netErr) && netErr.Timeout() {
		return true
	}
	var statusProvider interface{ StatusCode() int }
	if errors.As(cause, &statusProvider) && statusProvider.StatusCode() == http.StatusGatewayTimeout {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(semanticCode)) {
	case "timeout", "upstream_timeout", "deadline_exceeded":
		return true
	default:
		return false
	}
}

func newCodexCallerCancellationStatusErr(outerStatus int, headers http.Header, cause error, outputCommitted bool) error {
	if cause == nil {
		cause = context.Canceled
	}
	return newCodexTransportStatusErr(outerStatus, headers, codexCallerCancellationCause{cause: cause}, outputCommitted)
}

type codexCallerCancellationCause struct {
	cause error
}

func (e codexCallerCancellationCause) Error() string { return e.cause.Error() }
func (e codexCallerCancellationCause) Unwrap() error { return e.cause }
func (e codexCallerCancellationCause) Is(target error) bool {
	return target == context.Canceled
}

func codexFailureSemanticFields(body []byte) codexFailureSemantics {
	envelopes := []string{"error", "body.error", "response.error", "data.error", "detail.error", ""}
	parsed := make([]codexFailureSemantics, 0, len(envelopes))
	var fallback codexFailureSemantics
	for _, prefix := range envelopes {
		semantics := codexFailureSemanticsAt(body, prefix)
		if semantics == (codexFailureSemantics{}) {
			continue
		}
		if fallback == (codexFailureSemantics{}) {
			fallback = semantics
		}
		parsed = append(parsed, semantics)
	}
	for _, semantics := range parsed {
		if semantics.code != "" && semantics.code != "invalid_request" && semantics.code != "invalid_request_error" {
			return mergeCodexFailureSemanticFallback(semantics, fallback)
		}
	}
	for _, semantics := range parsed {
		if semantics.typeID != "" && semantics.typeID != "invalid_request_error" {
			return mergeCodexFailureSemanticFallback(semantics, fallback)
		}
	}
	return fallback
}

func mergeCodexFailureSemanticFallback(semantics, fallback codexFailureSemantics) codexFailureSemantics {
	if semantics.typeID == "" {
		semantics.typeID = fallback.typeID
	}
	if semantics.message == "" {
		semantics.message = fallback.message
	}
	return semantics
}

func codexFailureSemanticsAt(body []byte, prefix string) codexFailureSemantics {
	path := func(field string) string {
		if prefix == "" {
			return field
		}
		return prefix + "." + field
	}
	codePaths := []string{path("code"), path("err_code")}
	if prefix == "" {
		codePaths = append([]string{"error_code", "err_code"}, codePaths...)
	}
	code := firstCodexSemanticIdentifier(body, codePaths...)
	typePaths := []string{path("type")}
	if prefix == "" {
		typePaths = append([]string{"error_type"}, typePaths...)
	}
	typeID := firstCodexSemanticIdentifier(body, typePaths...)
	if isCodexEventEnvelopeType(typeID) {
		typeID = ""
	}
	message := firstCodexSemanticMessage(body, path("message"))
	if prefix == "" && message == "" {
		message = firstCodexSemanticMessage(body, "detail")
	}
	return codexFailureSemantics{code: code, typeID: typeID, message: message}
}

func firstCodexSemanticIdentifier(body []byte, paths ...string) string {
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		candidate := strings.ToLower(strings.TrimSpace(value.String()))
		if candidate == "" {
			candidate = strings.ToLower(strings.Trim(strings.TrimSpace(value.Raw), `"`))
		}
		if isCodexPlaceholderCode(candidate) {
			continue
		}
		if codexSemanticIdentifierPattern.MatchString(candidate) {
			return candidate
		}
	}
	return ""
}

func firstCodexSemanticMessage(body []byte, paths ...string) string {
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() || value.Type == gjson.Null || value.Type == gjson.JSON {
			continue
		}
		if candidate := strings.TrimSpace(value.String()); candidate != "" {
			return candidate
		}
	}
	return ""
}

func isCodexPlaceholderCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "", "0", "null", "none", "nil":
		return true
	default:
		return false
	}
}

func isCodexEventEnvelopeType(typeID string) bool {
	switch strings.ToLower(strings.TrimSpace(typeID)) {
	case "error", "response.failed", "response.completed", "response.done":
		return true
	default:
		return false
	}
}

func classifyCodexFailureSemantics(failure *failurecontract.Failure, message string, body []byte) {
	if failure == nil {
		return
	}
	outerStatus := failure.OuterStatus
	code := strings.ToLower(strings.TrimSpace(failure.SemanticCode))
	typeID := strings.ToLower(strings.TrimSpace(failure.SemanticType))
	lowerMessage := strings.ToLower(strings.TrimSpace(message))
	lowerBody := strings.ToLower(strings.TrimSpace(string(body)))
	contains := func(values ...string) bool {
		for _, value := range values {
			if value != "" && (strings.Contains(lowerMessage, value) || strings.Contains(lowerBody, value)) {
				return true
			}
		}
		return false
	}
	identifierIs := func(values ...string) bool {
		for _, value := range values {
			if code == value || typeID == value {
				return true
			}
		}
		return false
	}
	requestSemantic := (typeID == "" || typeID == "invalid_request_error") &&
		(code == "" || code == "invalid_request" || code == "invalid_request_error")
	setSemantic := func(semanticCode, semanticType string) {
		failure.SemanticCode = semanticCode
		failure.ProviderCode = semanticCode
		failure.SemanticType = semanticType
	}

	switch {
	case outerStatus == http.StatusRequestEntityTooLarge || identifierIs("context_length_exceeded", "context_too_large") || requestSemantic && contains("context window", "context length", "context_length", "maximum context", "too many tokens"):
		setSemantic("context_too_large", "invalid_request_error")
		failure.HTTPStatus = outerStatus
		if failure.HTTPStatus != http.StatusRequestEntityTooLarge {
			failure.HTTPStatus = http.StatusBadRequest
		}
		failure.Kind = failurecontract.ContextLengthExceeded
		failure.Scope = failurecontract.ScopeRequest
	case identifierIs("thinking_signature_invalid") || contains("invalid signature in thinking block") || isCodexInvalidEncryptedContentError(outerStatus, body):
		setSemantic("thinking_signature_invalid", "invalid_request_error")
		failure.HTTPStatus = http.StatusBadRequest
		failure.Kind = failurecontract.InvalidSignature
		failure.Scope = failurecontract.ScopeRequest
	case identifierIs("previous_response_not_found") || requestSemantic && (contains("previous_response_not_found", "items are not persisted") || contains("previous_response_id") && contains("not found")):
		setSemantic("previous_response_not_found", "invalid_request_error")
		failure.HTTPStatus = outerStatus
		if failure.HTTPStatus != http.StatusNotFound {
			failure.HTTPStatus = http.StatusBadRequest
		}
		failure.Kind = failurecontract.InvalidRequest
		failure.Scope = failurecontract.ScopeRequest
	case identifierIs("request_feature_unsupported", "unsupported_builtin_tool", "unsupported_feature"):
		if code == "" {
			code = "request_feature_unsupported"
		}
		setSemantic(code, "invalid_request_error")
		failure.HTTPStatus = http.StatusBadRequest
		failure.Kind = failurecontract.UnsupportedFeature
		failure.Scope = failurecontract.ScopeRequest
	case identifierIs("invalid_thinking_history") || contains("reasoning_content") && contains("missing", "incomplete", "must be fully passed back", "must be included", "缺少", "不完整"):
		setSemantic("invalid_thinking_history", "invalid_request_error")
		failure.HTTPStatus = http.StatusBadRequest
		failure.Kind = failurecontract.InvalidThinkingHistory
		failure.Scope = failurecontract.ScopeRequest
	case identifierIs("content_policy_violation", "data_inspection_failed", "datainspectionfailed", "policy_denied"):
		if code == "" {
			code = "content_policy_violation"
		}
		setSemantic(code, "invalid_request_error")
		failure.HTTPStatus = http.StatusBadRequest
		failure.Kind = failurecontract.ContentSafetyBlocked
		failure.Scope = failurecontract.ScopeRequest
	case codexDeterministicRequestCode(code) || code == "" && codexDeterministicRequestType(typeID) || requestSemantic && contains("invalid parameter", "invalid function arguments", "invalid tool", "tool schema", "schema validation"):
		if code == "" {
			code = "invalid_request_error"
		}
		if typeID == "" || typeID == "server_error" {
			typeID = "invalid_request_error"
		}
		setSemantic(code, typeID)
		failure.HTTPStatus = http.StatusBadRequest
		failure.Kind = failurecontract.InvalidRequest
		failure.Scope = failurecontract.ScopeRequest
	case codexAuthenticationIdentifier(code, typeID):
		setSemantic("auth_unavailable", "authentication_error")
		failure.HTTPStatus = outerStatus
		if failure.HTTPStatus <= 0 || failure.HTTPStatus == http.StatusOK {
			failure.HTTPStatus = http.StatusUnauthorized
		}
		failure.Kind = failurecontract.AuthenticationFailed
		failure.Scope = failurecontract.ScopeCredential
	case codexModelAccessIdentifier(code, typeID):
		if code == "" {
			code = typeID
		}
		setSemantic(code, typeID)
		failure.HTTPStatus = outerStatus
		failure.Kind = failurecontract.ModelUnavailable
		failure.Scope = failurecontract.ScopeModel
		failure.Retryable = true
	case codexQuotaIdentifier(code, typeID):
		if code == "" {
			code = typeID
		}
		setSemantic(code, typeID)
		failure.HTTPStatus = http.StatusTooManyRequests
		failure.Kind = failurecontract.QuotaExceeded
		failure.Scope = failurecontract.ScopeCredential
		failure.Retryable = outerStatus != http.StatusBadRequest
	case outerStatus == http.StatusUnauthorized:
		setSemantic("auth_unavailable", "authentication_error")
		failure.HTTPStatus = outerStatus
		failure.Kind = failurecontract.AuthenticationFailed
		failure.Scope = failurecontract.ScopeCredential
	case outerStatus == http.StatusForbidden:
		failure.HTTPStatus = outerStatus
	case identifierIs("model_at_capacity") || isCodexModelCapacityError(body):
		setSemantic("model_at_capacity", "server_error")
		failure.HTTPStatus = http.StatusTooManyRequests
		failure.Kind = failurecontract.RateLimited
		failure.Scope = failurecontract.ScopeModel
		failure.Retryable = true
	case codexRateLimitIdentifier(code, typeID) || outerStatus == http.StatusTooManyRequests:
		if code == "" {
			code = "rate_limit_exceeded"
		}
		if typeID == "" {
			typeID = "rate_limit_error"
		}
		setSemantic(code, typeID)
		failure.HTTPStatus = http.StatusTooManyRequests
		failure.Kind = failurecontract.RateLimited
		failure.Scope = failurecontract.ScopeModel
		failure.Retryable = true
	case codexProviderFailureIdentifier(code, typeID):
		if code == "" || isCodexPlaceholderCode(code) {
			code = typeID
		}
		if code == "" {
			code = "server_error"
		}
		if typeID == "" {
			typeID = "server_error"
		}
		setSemantic(code, typeID)
		failure.HTTPStatus = outerStatus
		if failure.HTTPStatus < http.StatusInternalServerError {
			failure.HTTPStatus = http.StatusBadGateway
		}
		failure.Kind = failurecontract.ProviderUnavailable
		failure.Scope = failurecontract.ScopeProvider
		failure.Retryable = true
	case outerStatus == http.StatusRequestTimeout || outerStatus == 524:
		if code == "" {
			code = "upstream_timeout"
		}
		setSemantic(code, typeID)
		failure.HTTPStatus = http.StatusGatewayTimeout
		failure.Kind = failurecontract.TransportError
		failure.Scope = failurecontract.ScopeProvider
		failure.Retryable = true
	case outerStatus >= http.StatusInternalServerError:
		failure.HTTPStatus = outerStatus
		failure.Kind = failurecontract.ProviderUnavailable
		failure.Scope = failurecontract.ScopeProvider
		failure.Retryable = true
	case outerStatus == http.StatusBadRequest || outerStatus == http.StatusUnprocessableEntity:
		failure.HTTPStatus = outerStatus
		failure.Kind = failurecontract.InvalidRequest
		failure.Scope = failurecontract.ScopeRequest
	case outerStatus == http.StatusNotFound:
		failure.HTTPStatus = outerStatus
		failure.Kind = failurecontract.InvalidRequest
		failure.Scope = failurecontract.ScopeRequest
	case outerStatus == http.StatusOK:
		failure.HTTPStatus = http.StatusBadGateway
		failure.Kind = failurecontract.UpstreamProtocolError
		failure.Scope = failurecontract.ScopeProvider
	default:
		failure.HTTPStatus = outerStatus
	}
}

func codexDeterministicRequestCode(code string) bool {
	switch code {
	case "invalid_request", "invalid_request_error", "invalid_argument", "invalidargument", "invalid_parameter",
		"invalid_function_arguments", "invalid_tool_arguments", "tool_schema_error", "schema_validation_error",
		"billing_config_error", "request_too_large", "unprocessable_entity":
		return true
	}
	return false
}

func codexDeterministicRequestType(typeID string) bool {
	return codexDeterministicRequestCode(typeID)
}

func codexAuthenticationIdentifier(code, typeID string) bool {
	for _, candidate := range []string{code, typeID} {
		switch candidate {
		case "authentication_error", "invalid_api_key", "invalid_grant", "unauthenticated", "unauthorized":
			return true
		}
	}
	return false
}

func codexModelAccessIdentifier(code, typeID string) bool {
	for _, candidate := range []string{code, typeID} {
		switch candidate {
		case "auth_unavailable", "permission_denied", "permission_error":
			return true
		}
	}
	return false
}

func codexQuotaIdentifier(code, typeID string) bool {
	for _, candidate := range []string{code, typeID} {
		switch candidate {
		case "usage_limit_reached", "insufficient_balance", "insufficient_quota", "billing_cycle_quota", "quota_exhausted", "quota_exceeded":
			return true
		}
	}
	return false
}

func codexRateLimitIdentifier(code, typeID string) bool {
	for _, candidate := range []string{code, typeID} {
		switch candidate {
		case "rate_limit", "rate_limit_error", "rate_limit_exceeded", "resource_exhausted":
			return true
		}
	}
	return false
}

func codexProviderFailureIdentifier(code, typeID string) bool {
	for _, candidate := range []string{code, typeID} {
		switch candidate {
		case "server_error", "internal_server_error", "upstream_error", "provider_error", "service_unavailable",
			"unavailable", "overloaded", "overloaded_error", "deadline_exceeded", "timeout", "upstream_timeout":
			return true
		}
	}
	return false
}

func codexCanonicalRetryAfter(headers http.Header, body []byte, now time.Time) *time.Duration {
	if retryAfter := codexRetryAfterFromBody(body, now); retryAfter != nil {
		return retryAfter
	}
	if headers != nil {
		if retryAfter := parseOpenAICompatRetryAfterString(headers.Get("Retry-After"), false, now); retryAfter != nil {
			return retryAfter
		}
	}
	return nil
}

func codexRetryAfterFromBody(body []byte, now time.Time) *time.Duration {
	for _, prefix := range []string{"error", "body.error", "response.error", "data.error", "detail.error"} {
		typeID := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, prefix+".type").String()))
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, prefix+".code").String()))
		if !codexQuotaIdentifier(code, typeID) {
			continue
		}
		if resetsAt := gjson.GetBytes(body, prefix+".resets_at").Int(); resetsAt > 0 {
			resetAtTime := time.Unix(resetsAt, 0)
			if resetAtTime.After(now) {
				retryAfter := resetAtTime.Sub(now)
				return &retryAfter
			}
		}
		if resetsInSeconds := gjson.GetBytes(body, prefix+".resets_in_seconds").Int(); resetsInSeconds > 0 {
			retryAfter := time.Duration(resetsInSeconds) * time.Second
			return &retryAfter
		}
		if retryAfterSeconds := gjson.GetBytes(body, prefix+".retry_after").Float(); retryAfterSeconds > 0 {
			retryAfter := time.Duration(retryAfterSeconds * float64(time.Second))
			return &retryAfter
		}
	}
	return nil
}

func codexUpstreamRequestID(headers http.Header, body []byte) string {
	for _, name := range []string{"X-Request-Id", "Request-Id", "X-Tt-Logid", "X-Volc-Request-Id"} {
		if requestID := strings.TrimSpace(headers.Get(name)); requestID != "" && len(requestID) <= 256 {
			return requestID
		}
	}
	for _, path := range []string{
		"request_id", "requestId", "error.request_id", "body.request_id", "body.error.request_id",
		"response.request_id", "response.error.request_id", "data.request_id", "data.error.request_id",
	} {
		if requestID := strings.TrimSpace(gjson.GetBytes(body, path).String()); requestID != "" && len(requestID) <= 256 {
			return requestID
		}
	}
	return ""
}
