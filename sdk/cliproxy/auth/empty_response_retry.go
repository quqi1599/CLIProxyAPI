package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	defaultEmptyResponseBufferBytes  = 8 * 1024 * 1024
	defaultEmptyResponseBufferEvents = 2000
	maxEmptyResponseBufferBytes      = 8 * 1024 * 1024
	maxEmptyResponseBufferEvents     = 2000
	maxEmptyResponseParserPending    = 256 * 1024
)

var (
	defaultEmptyResponseModels         = []string{"gpt-5.6-sol"}
	defaultEmptyResponseClientProfiles = []string{"workbuddy"}
	defaultEmptyResponseSourceFormats  = []string{
		string(sdktranslator.FormatOpenAI),
		string(sdktranslator.FormatOpenAIResponse),
	}
)

type emptyResponsePolicy struct {
	enabled       bool
	auditOnly     bool
	format        sdktranslator.Format
	clientProfile string
	maxBytes      int
	maxEvents     int
}

func (m *Manager) emptyResponsePolicy(model string, opts cliproxyexecutor.Options) emptyResponsePolicy {
	if m == nil {
		return emptyResponsePolicy{}
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.EmptyResponseRetry.Enabled {
		return emptyResponsePolicy{}
	}

	target := cfg.EmptyResponseRetry
	model = canonicalModelKey(model)
	if !emptyResponseTargetMatches(target.Models, defaultEmptyResponseModels, model) {
		return emptyResponsePolicy{}
	}
	clientProfile := strings.ToLower(strings.TrimSpace(metadataString(opts.Metadata, cliproxyexecutor.ClientProfileMetadataKey)))
	if !emptyResponseTargetMatches(target.ClientProfiles, defaultEmptyResponseClientProfiles, clientProfile) {
		return emptyResponsePolicy{}
	}
	format := cliproxyexecutor.ResponseFormatOrSource(opts)
	if !emptyResponseFormatMatches(target.SourceFormats, defaultEmptyResponseSourceFormats, format.String()) {
		return emptyResponsePolicy{}
	}

	return emptyResponsePolicy{
		enabled:       true,
		auditOnly:     target.AuditOnly,
		format:        format,
		clientProfile: clientProfile,
		maxBytes:      boundedEmptyResponseLimit(target.MaxBufferBytes, defaultEmptyResponseBufferBytes, maxEmptyResponseBufferBytes),
		maxEvents:     boundedEmptyResponseLimit(target.MaxBufferEvents, defaultEmptyResponseBufferEvents, maxEmptyResponseBufferEvents),
	}
}

func emptyResponseTargetMatches(configured, defaults []string, actual string) bool {
	if len(configured) == 0 {
		configured = defaults
	}
	actual = strings.ToLower(strings.TrimSpace(actual))
	if actual == "" {
		return false
	}
	for _, candidate := range configured {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "*" || candidate == actual {
			return true
		}
	}
	return false
}

func emptyResponseFormatMatches(configured, defaults []string, actual string) bool {
	if len(configured) == 0 {
		configured = defaults
	}
	actual = normalizedEmptyResponseFormat(actual)
	if actual == "" {
		return false
	}
	for _, candidate := range configured {
		candidate = normalizedEmptyResponseFormat(candidate)
		if candidate == "*" || candidate == actual {
			return true
		}
	}
	return false
}

func normalizedEmptyResponseFormat(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "openai_chat", "openai-chat":
		return string(sdktranslator.FormatOpenAI)
	case "openai_responses", "openai-responses":
		return string(sdktranslator.FormatOpenAIResponse)
	case "codex_responses", "codex-responses":
		return string(sdktranslator.FormatCodex)
	default:
		return value
	}
}

func boundedEmptyResponseLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

type deliverableToolCallState struct {
	id   bool
	name bool
}

type deliverableOutputTracker struct {
	format sdktranslator.Format

	pending       []byte
	eventType     string
	toolCalls     map[string]deliverableToolCallState
	chunksCount   int
	bytesReceived int

	deliverable  bool
	sawText      bool
	sawToolCall  bool
	sawMedia     bool
	sawRefusal   bool
	sawReasoning bool
	sawTerminal  bool
	sawError     bool
	finishReason string
}

func newDeliverableOutputTracker(format sdktranslator.Format) *deliverableOutputTracker {
	return &deliverableOutputTracker{
		format:    format,
		toolCalls: make(map[string]deliverableToolCallState),
	}
}

func (t *deliverableOutputTracker) Observe(payload []byte) {
	if t == nil || len(payload) == 0 {
		return
	}
	t.chunksCount++
	t.bytesReceived += len(payload)

	if len(t.pending) > 0 && startsSSEField(payload) && pendingSSEFieldComplete(t.pending) {
		t.observeLine(t.pending)
		t.pending = t.pending[:0]
	}

	remaining := payload
	for len(remaining) > 0 {
		newline := bytes.IndexByte(remaining, '\n')
		if newline < 0 {
			t.pending = append(t.pending, remaining...)
			t.consumeCompletePending()
			return
		}
		t.pending = append(t.pending, remaining[:newline]...)
		t.observeLine(t.pending)
		t.pending = t.pending[:0]
		remaining = remaining[newline+1:]
	}
}

func (t *deliverableOutputTracker) Finish() {
	if t == nil || len(t.pending) == 0 {
		return
	}
	t.consumeCompletePending()
	if len(t.pending) == 0 {
		return
	}
	trimmed := bytes.TrimSpace(t.pending)
	if len(trimmed) > 0 {
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			t.eventType = strings.TrimSpace(string(trimmed[len("event:"):]))
		} else {
			t.markUnknownDeliverable()
		}
	}
	t.pending = t.pending[:0]
}

func (t *deliverableOutputTracker) consumeCompletePending() {
	if t == nil || len(t.pending) == 0 {
		return
	}
	trimmed := bytes.TrimSpace(t.pending)
	if len(trimmed) == 0 {
		t.pending = t.pending[:0]
		return
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		t.observeLine(trimmed)
		t.pending = t.pending[:0]
		return
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) || gjson.ValidBytes(data) {
			t.observeLine(trimmed)
			t.pending = t.pending[:0]
		}
		return
	}
	if gjson.ValidBytes(trimmed) {
		t.observeLine(trimmed)
		t.pending = t.pending[:0]
		return
	}
	if len(trimmed) > maxEmptyResponseParserPending {
		t.markUnknownDeliverable()
		t.pending = t.pending[:0]
		return
	}
	if !looksLikeStructuredOutput(trimmed) {
		t.markUnknownDeliverable()
		t.pending = t.pending[:0]
	}
}

func startsSSEField(payload []byte) bool {
	trimmed := bytes.TrimLeft(payload, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte("id:")) ||
		bytes.HasPrefix(trimmed, []byte("retry:")) ||
		bytes.HasPrefix(trimmed, []byte(":"))
}

func pendingSSEFieldComplete(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	return bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte("id:")) ||
		bytes.HasPrefix(trimmed, []byte("retry:")) ||
		bytes.HasPrefix(trimmed, []byte(":"))
}

func looksLikeStructuredOutput(payload []byte) bool {
	return bytes.HasPrefix(payload, []byte("{")) ||
		bytes.HasPrefix(payload, []byte("[")) ||
		bytes.HasPrefix(payload, []byte("data:")) ||
		bytes.HasPrefix(payload, []byte("event:")) ||
		bytes.HasPrefix(payload, []byte("id:")) ||
		bytes.HasPrefix(payload, []byte("retry:")) ||
		bytes.HasPrefix(payload, []byte(":"))
}

func (t *deliverableOutputTracker) observeLine(line []byte) {
	if t == nil {
		return
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	switch {
	case bytes.HasPrefix(trimmed, []byte("event:")):
		t.eventType = strings.TrimSpace(string(trimmed[len("event:"):]))
		return
	case bytes.HasPrefix(trimmed, []byte("id:")),
		bytes.HasPrefix(trimmed, []byte("retry:")),
		bytes.HasPrefix(trimmed, []byte(":")):
		return
	case bytes.HasPrefix(trimmed, []byte("data:")):
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		t.sawTerminal = true
		if t.finishReason == "" {
			t.finishReason = "done"
		}
		t.eventType = ""
		return
	}
	if !gjson.ValidBytes(trimmed) {
		t.markUnknownDeliverable()
		t.eventType = ""
		return
	}
	t.observeJSON(gjson.ParseBytes(trimmed), t.eventType)
	t.eventType = ""
}

func (t *deliverableOutputTracker) observeJSON(root gjson.Result, eventType string) {
	if t == nil {
		return
	}
	if eventType == "" {
		eventType = root.Get("type").String()
	}
	if resultHasExplicitError(root.Get("error")) ||
		resultHasExplicitError(root.Get("response.error")) ||
		strings.EqualFold(strings.TrimSpace(eventType), "error") {
		t.sawError = true
		t.sawTerminal = true
		t.deliverable = true
		if t.finishReason == "" {
			t.finishReason = "error"
		}
		return
	}

	switch t.format {
	case sdktranslator.FormatOpenAI:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(eventType)), "response.") ||
			root.Get("response").Exists() || root.Get("output").Exists() {
			t.observeResponsesJSON(root, eventType)
			return
		}
		t.observeOpenAIJSON(root)
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
		t.observeResponsesJSON(root, eventType)
	default:
		t.markUnknownDeliverable()
	}
}

func (t *deliverableOutputTracker) observeOpenAIJSON(root gjson.Result) {
	choices := root.Get("choices")
	if !choices.Exists() || !choices.IsArray() {
		if root.Get("usage").Exists() {
			return
		}
		t.markUnknownDeliverable()
		return
	}

	for _, choice := range choices.Array() {
		if reason := cleanSummaryString(choice.Get("finish_reason").String()); reason != "" {
			t.sawTerminal = true
			t.finishReason = reason
			if !emptyResponseTerminalReason(reason) {
				t.deliverable = true
			}
		}
		if reason := cleanSummaryString(choice.Get("native_finish_reason").String()); reason != "" && t.finishReason == "" {
			t.sawTerminal = true
			t.finishReason = reason
			if !emptyResponseTerminalReason(reason) {
				t.deliverable = true
			}
		}
		if delta := choice.Get("delta"); delta.Exists() {
			t.observeOpenAIMessage(delta)
		}
		if message := choice.Get("message"); message.Exists() {
			t.observeOpenAIMessage(message)
		}
		for key := range choice.Map() {
			switch key {
			case "index", "delta", "message", "finish_reason", "native_finish_reason", "logprobs":
			default:
				t.markUnknownDeliverable()
			}
		}
	}
}

func (t *deliverableOutputTracker) observeOpenAIMessage(message gjson.Result) {
	if resultHasVisibleText(message.Get("content")) {
		t.markText()
	}
	if resultHasVisibleText(message.Get("refusal")) {
		t.sawRefusal = true
		t.deliverable = true
	}
	if resultHasMeaningfulValue(message.Get("audio")) ||
		resultHasMeaningfulValue(message.Get("image")) ||
		resultHasMeaningfulValue(message.Get("images")) ||
		resultHasMeaningfulValue(message.Get("file")) ||
		resultHasMeaningfulValue(message.Get("files")) {
		t.sawMedia = true
		t.deliverable = true
	}
	if reasoning := message.Get("reasoning_content"); resultHasVisibleText(reasoning) {
		t.sawReasoning = true
	}
	if reasoning := message.Get("reasoning"); resultHasMeaningfulValue(reasoning) {
		t.sawReasoning = true
	}
	if reasoning := message.Get("reasoning_details"); resultHasMeaningfulValue(reasoning) {
		t.sawReasoning = true
	}
	if reasoning := message.Get("thinking"); resultHasMeaningfulValue(reasoning) {
		t.sawReasoning = true
	}
	if calls := message.Get("tool_calls"); calls.Exists() && calls.IsArray() {
		for _, call := range calls.Array() {
			t.observeToolCall(call, false)
		}
	}
	if call := message.Get("function_call"); call.Exists() {
		t.observeToolCall(call, true)
	}
	if call := message.Get("custom_tool_call"); call.Exists() {
		t.observeToolCall(call, false)
	}

	for key := range message.Map() {
		switch key {
		case "role", "content", "refusal", "audio", "image", "images", "file", "files",
			"reasoning_content", "reasoning", "reasoning_details", "thinking",
			"tool_calls", "function_call", "custom_tool_call",
			"annotations":
		default:
			t.markUnknownDeliverable()
		}
	}
}

func (t *deliverableOutputTracker) observeResponsesJSON(root gjson.Result, eventType string) {
	eventType = strings.ToLower(strings.TrimSpace(eventType))
	switch eventType {
	case "response.created", "response.in_progress", "response.queued":
		return
	case "response.completed", "response.done":
		t.sawTerminal = true
		if t.finishReason == "" {
			t.finishReason = "done"
		}
		t.observeResponsesOutput(root.Get("response.output"))
		return
	case "response.failed", "response.incomplete", "error":
		t.sawError = true
		t.sawTerminal = true
		t.deliverable = true
		t.finishReason = strings.TrimPrefix(eventType, "response.")
		return
	case "response.output_text.delta", "response.output_text.done":
		if resultHasVisibleText(root.Get("delta")) || resultHasVisibleText(root.Get("text")) {
			t.markText()
		}
		return
	case "response.refusal.delta", "response.refusal.done":
		if resultHasVisibleText(root.Get("delta")) || resultHasVisibleText(root.Get("refusal")) {
			t.sawRefusal = true
			t.deliverable = true
		}
		return
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		t.sawReasoning = true
		return
	case "response.output_item.added", "response.output_item.done":
		t.observeResponsesOutputItem(root.Get("item"))
		return
	case "response.content_part.added", "response.content_part.done":
		t.observeResponsesContentPart(root.Get("part"))
		return
	case "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		t.observeToolCall(root, false)
		return
	}

	if eventType != "" {
		t.markUnknownDeliverable()
		return
	}
	if status := strings.ToLower(strings.TrimSpace(root.Get("status").String())); status != "" {
		t.observeResponsesStatus(status)
	}
	if response := root.Get("response"); response.Exists() {
		if status := strings.ToLower(strings.TrimSpace(response.Get("status").String())); status != "" {
			t.observeResponsesStatus(status)
		}
		t.observeResponsesOutput(response.Get("output"))
		return
	}
	if output := root.Get("output"); output.Exists() {
		t.observeResponsesOutput(output)
		return
	}
	t.markUnknownDeliverable()
}

func (t *deliverableOutputTracker) observeResponsesStatus(status string) {
	if t == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
		t.sawTerminal = true
		t.finishReason = "done"
	case "failed", "incomplete", "cancelled", "canceled":
		t.sawError = true
		t.sawTerminal = true
		t.deliverable = true
		t.finishReason = strings.ToLower(strings.TrimSpace(status))
	}
}

func emptyResponseTerminalReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "stop", "done", "completed", "complete", "eof":
		return true
	default:
		return false
	}
}

func (t *deliverableOutputTracker) observeResponsesOutput(output gjson.Result) {
	if !output.Exists() || !output.IsArray() {
		return
	}
	for _, item := range output.Array() {
		t.observeResponsesOutputItem(item)
	}
}

func (t *deliverableOutputTracker) observeResponsesOutputItem(item gjson.Result) {
	if !item.Exists() {
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	switch itemType {
	case "message":
		content := item.Get("content")
		if content.Type == gjson.String {
			if resultHasVisibleText(content) {
				t.markText()
			}
		} else if content.IsArray() {
			for _, part := range content.Array() {
				t.observeResponsesContentPart(part)
			}
		} else if content.IsObject() {
			t.observeResponsesContentPart(content)
		}
		if resultHasVisibleText(item.Get("refusal")) {
			t.sawRefusal = true
			t.deliverable = true
		}
	case "function_call", "custom_tool_call", "tool_call":
		t.observeToolCall(item, false)
	case "reasoning":
		t.sawReasoning = true
	case "refusal":
		if resultHasVisibleText(item.Get("refusal")) || resultHasVisibleText(item.Get("content")) {
			t.sawRefusal = true
			t.deliverable = true
		}
	case "image", "image_generation_call", "audio", "file":
		if resultHasMeaningfulValue(item) {
			t.sawMedia = true
			t.deliverable = true
		}
	default:
		if strings.Contains(itemType, "call") || itemType == "computer_use" {
			t.sawToolCall = true
			t.deliverable = true
			return
		}
		if itemType != "" {
			t.markUnknownDeliverable()
		}
	}
}

func (t *deliverableOutputTracker) observeResponsesContentPart(part gjson.Result) {
	if !part.Exists() {
		return
	}
	partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
	switch partType {
	case "output_text", "text":
		if resultHasVisibleText(part.Get("text")) {
			t.markText()
		}
	case "reasoning", "reasoning_text", "summary_text":
		t.sawReasoning = true
	case "refusal":
		if resultHasVisibleText(part.Get("refusal")) || resultHasVisibleText(part.Get("text")) {
			t.sawRefusal = true
			t.deliverable = true
		}
	case "input_image", "output_image", "image", "audio", "file":
		if resultHasMeaningfulValue(part) {
			t.sawMedia = true
			t.deliverable = true
		}
	default:
		if partType != "" {
			t.markUnknownDeliverable()
		}
	}
}

func (t *deliverableOutputTracker) observeToolCall(call gjson.Result, legacy bool) {
	if t == nil || !call.Exists() {
		return
	}
	name := strings.TrimSpace(call.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(call.Get("function.name").String())
	}
	if legacy {
		if name != "" {
			t.sawToolCall = true
			t.deliverable = true
		}
		return
	}

	id := strings.TrimSpace(call.Get("call_id").String())
	if id == "" {
		id = strings.TrimSpace(call.Get("id").String())
	}
	key := strings.TrimSpace(call.Get("index").String())
	if key == "" {
		key = strings.TrimSpace(call.Get("item_id").String())
	}
	if key == "" {
		key = id
	}
	if key == "" {
		key = "default"
	}
	state := t.toolCalls[key]
	state.id = state.id || id != ""
	state.name = state.name || name != ""
	t.toolCalls[key] = state
	if state.id && state.name {
		t.sawToolCall = true
		t.deliverable = true
	}
}

func resultHasExplicitError(value gjson.Result) bool {
	if !value.Exists() || value.Type == gjson.Null {
		return false
	}
	if value.Type == gjson.String {
		return strings.TrimSpace(value.String()) != ""
	}
	if value.IsObject() || value.IsArray() {
		return len(value.Raw) > 2
	}
	return true
}

func (t *deliverableOutputTracker) markText() {
	t.sawText = true
	t.deliverable = true
}

func (t *deliverableOutputTracker) markUnknownDeliverable() {
	t.deliverable = true
}

func resultHasVisibleText(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.Type == gjson.String {
		return strings.TrimSpace(value.String()) != ""
	}
	if value.IsArray() {
		for _, item := range value.Array() {
			if resultHasVisibleText(item) {
				return true
			}
		}
		return false
	}
	if value.IsObject() {
		for _, path := range []string{"text", "output_text", "content", "refusal", "value"} {
			if resultHasVisibleText(value.Get(path)) {
				return true
			}
		}
	}
	return false
}

func resultHasMeaningfulValue(value gjson.Result) bool {
	if !value.Exists() || value.Type == gjson.Null {
		return false
	}
	if value.Type == gjson.String {
		return strings.TrimSpace(value.String()) != ""
	}
	if value.IsArray() {
		for _, item := range value.Array() {
			if resultHasMeaningfulValue(item) {
				return true
			}
		}
		return false
	}
	if value.IsObject() {
		for key, item := range value.Map() {
			switch key {
			case "type", "status", "role", "index":
				continue
			}
			if resultHasMeaningfulValue(item) {
				return true
			}
		}
		return false
	}
	return value.Bool() || value.Int() != 0 || value.Float() != 0
}

type emptyUpstreamResponseFailure struct {
	cause *failurecontract.Failure
}

func (e *emptyUpstreamResponseFailure) Error() string {
	return `{"error":{"message":"All available upstream channels completed without deliverable output","type":"upstream_error","code":"` +
		emptyUpstreamResponseErrorCode + `"}}`
}

func (e *emptyUpstreamResponseFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newEmptyUpstreamResponseFailure() error {
	cause := &failurecontract.Failure{
		Kind:          failurecontract.UpstreamProtocolError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		ProviderCode:  emptyUpstreamResponseErrorCode,
		Retryable:     true,
		PublicMessage: "Upstream completed without deliverable output",
	}
	return &emptyUpstreamResponseFailure{cause: cause}
}

type emptyResponseAudit struct {
	policy     emptyResponsePolicy
	tracker    *deliverableOutputTracker
	routeModel string
	opts       cliproxyexecutor.Options
}

func newEmptyResponseAudit(policy emptyResponsePolicy, routeModel string, opts cliproxyexecutor.Options) *emptyResponseAudit {
	if !policy.enabled || !policy.auditOnly {
		return nil
	}
	return &emptyResponseAudit{
		policy:     policy,
		tracker:    newDeliverableOutputTracker(policy.format),
		routeModel: routeModel,
		opts:       opts,
	}
}

func (m *Manager) logEmptyResponseDetected(ctx context.Context, auth *Auth, provider, routeModel string, opts cliproxyexecutor.Options, tracker *deliverableOutputTracker, auditOnly, clientGone bool, downstreamBytes int64) {
	if tracker == nil {
		return
	}
	attempt := coreusage.RequestAttemptFromContext(ctx)
	fields := log.Fields{
		"event":                      "empty_response_detected",
		"model":                      canonicalModelKey(routeModel),
		"provider":                   strings.TrimSpace(provider),
		"request_path":               metadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey),
		"source_format":              cliproxyexecutor.ResponseFormatOrSource(opts).String(),
		"client_profile":             metadataString(opts.Metadata, cliproxyexecutor.ClientProfileMetadataKey),
		"attempt_no":                 attempt.AttemptNo,
		"upstream_status":            http.StatusOK,
		"upstream_chunks_received":   tracker.chunksCount,
		"upstream_bytes_received":    tracker.bytesReceived,
		"downstream_bytes_out":       downstreamBytes,
		"deliverable_text":           tracker.sawText,
		"deliverable_tool_call":      tracker.sawToolCall,
		"deliverable_media":          tracker.sawMedia,
		"deliverable_refusal":        tracker.sawRefusal,
		"reasoning_only":             tracker.sawReasoning,
		"client_gone":                clientGone,
		"finish_reason":              tracker.finishReason,
		"retryable":                  !auditOnly && !clientGone,
		"retry_reason":               emptyUpstreamResponseErrorCode,
		"audit_only":                 auditOnly,
		"would_retry_empty_response": auditOnly && !clientGone,
	}
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		fields["round_no"] = trace.gptRoundValue()
		trace.recordEmptyResponse(routingChannelBaseKey(auth))
	}
	if auth != nil {
		if group := authRoutingGroup(auth); group != "" {
			fields["routing_group"] = group
		}
		if host := emptyResponseBaseURLHost(auth); host != "" {
			fields["base_url_host"] = host
		}
	}
	logEntryWithRequestID(ctx).WithFields(fields).Warn("upstream completed without downstream-deliverable output")
}

func emptyResponseBaseURLHost(auth *Auth) string {
	raw := authMetricBaseURL(auth)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname()))
}

func (m *Manager) logEmptyResponseBufferLimit(ctx context.Context, auth *Auth, provider, routeModel string, opts cliproxyexecutor.Options, tracker *deliverableOutputTracker, policy emptyResponsePolicy) {
	if tracker == nil {
		return
	}
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		trace.recordEmptyResponse(routingChannelBaseKey(auth))
	}
	fields := log.Fields{
		"event":                    "empty_response_buffer_limit",
		"model":                    canonicalModelKey(routeModel),
		"provider":                 strings.TrimSpace(provider),
		"request_path":             metadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey),
		"source_format":            policy.format.String(),
		"client_profile":           policy.clientProfile,
		"upstream_chunks_received": tracker.chunksCount,
		"upstream_bytes_received":  tracker.bytesReceived,
		"max_buffer_bytes":         policy.maxBytes,
		"max_buffer_events":        policy.maxEvents,
		"action":                   "retry_bootstrap",
	}
	if auth != nil {
		if group := authRoutingGroup(auth); group != "" {
			fields["routing_group"] = group
		}
		if host := emptyResponseBaseURLHost(auth); host != "" {
			fields["base_url_host"] = host
		}
	}
	logEntryWithRequestID(ctx).WithFields(fields).Warn("empty-response bootstrap buffer limit reached; preserving the current stream")
}
