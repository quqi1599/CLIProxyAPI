package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// ResponsesCompactAlt identifies the legacy Responses compaction endpoint.
const ResponsesCompactAlt = "responses/compact"

// CompactionIntentMetadataKey stores the normalized remote-compaction request intent.
const CompactionIntentMetadataKey = "compaction_intent"

// CompactionCompatibilityGroupMetadataKey stores the selected opaque-state compatibility group.
const CompactionCompatibilityGroupMetadataKey = "compaction_compatibility_group"

// CompactionTriggerModeMetadataKey stores the selected v2 trigger transport mode.
const CompactionTriggerModeMetadataKey = "compaction_trigger_mode"

// CompactionIntent identifies one Responses compaction protocol family.
type CompactionIntent string

const (
	CompactionIntentNone              CompactionIntent = "none"
	CompactionIntentLegacyEndpoint    CompactionIntent = "legacy_endpoint"
	CompactionIntentV2Trigger         CompactionIntent = "v2_trigger"
	CompactionIntentContextManagement CompactionIntent = "context_management"
	CompactionIntentReplay            CompactionIntent = "replay"
)

// DetectCompactionIntent classifies a Responses request before provider translation.
// A v2 trigger must occur exactly once and be the final input item.
func DetectCompactionIntent(payload []byte, alt string) (CompactionIntent, error) {
	if strings.EqualFold(strings.TrimSpace(alt), ResponsesCompactAlt) {
		return CompactionIntentLegacyEndpoint, nil
	}

	input := gjson.GetBytes(payload, "input")
	triggerCount := 0
	triggerIndex := -1
	replay := false
	if input.IsArray() {
		items := input.Array()
		for i := range items {
			switch strings.TrimSpace(items[i].Get("type").String()) {
			case "compaction_trigger":
				triggerCount++
				triggerIndex = i
			case "compaction", "compaction_summary":
				replay = true
			}
		}
		if triggerCount > 0 {
			if triggerCount != 1 || triggerIndex != len(items)-1 {
				return CompactionIntentNone, fmt.Errorf("compaction_trigger must be the final input item and appear exactly once")
			}
		}
	}

	contextCompaction := false
	contextManagement := gjson.GetBytes(payload, "context_management")
	if contextManagement.IsArray() {
		for _, item := range contextManagement.Array() {
			if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "compaction") {
				contextCompaction = true
				break
			}
		}
	}
	if triggerCount > 0 && contextCompaction {
		return CompactionIntentNone, fmt.Errorf("compaction_trigger and context_management compaction cannot be combined")
	}
	if triggerCount > 0 {
		return CompactionIntentV2Trigger, nil
	}
	if contextCompaction {
		return CompactionIntentContextManagement, nil
	}
	if replay {
		return CompactionIntentReplay, nil
	}
	return CompactionIntentNone, nil
}

// SetCompactionIntentMetadata classifies payload and records the result when metadata is available.
func SetCompactionIntentMetadata(metadata map[string]any, payload []byte, alt string) (CompactionIntent, error) {
	intent, err := DetectCompactionIntent(payload, alt)
	if err != nil {
		return CompactionIntentNone, err
	}
	if metadata != nil {
		metadata[CompactionIntentMetadataKey] = string(intent)
	}
	return intent, nil
}

// CompactionIntentFromOptions returns the preclassified intent, falling back to request inspection.
func CompactionIntentFromOptions(req Request, opts Options) CompactionIntent {
	if opts.Metadata != nil {
		if value, ok := opts.Metadata[CompactionIntentMetadataKey].(string); ok {
			switch intent := CompactionIntent(strings.TrimSpace(value)); intent {
			case CompactionIntentLegacyEndpoint, CompactionIntentV2Trigger, CompactionIntentContextManagement, CompactionIntentReplay:
				return intent
			}
		}
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = opts.OriginalRequest
	}
	intent, err := DetectCompactionIntent(payload, opts.Alt)
	if err != nil {
		return CompactionIntentNone
	}
	return intent
}

// IsRemoteCompactionIntent reports whether intent requires a compaction-capable route.
func IsRemoteCompactionIntent(intent CompactionIntent) bool {
	switch intent {
	case CompactionIntentLegacyEndpoint, CompactionIntentV2Trigger, CompactionIntentContextManagement:
		return true
	default:
		return false
	}
}

// CompactionTriggerModeFromOptions returns the transport mode selected for a
// v2 compaction trigger.
func CompactionTriggerModeFromOptions(opts Options) string {
	if opts.Metadata == nil {
		return ""
	}
	value, _ := opts.Metadata[CompactionTriggerModeMetadataKey].(string)
	return strings.TrimSpace(value)
}

// RequestedModelMetadataKey stores the client-requested model name in Options.Metadata.
const RequestedModelMetadataKey = "requested_model"

// RequestPathMetadataKey stores the inbound HTTP request path (e.g. "/v1/images/generations") in Options.Metadata.
// It is optional and may be absent for non-HTTP executions.
const RequestPathMetadataKey = "request_path"

// DisallowFreeAuthMetadataKey instructs auth selection to skip known free-tier credentials.
const DisallowFreeAuthMetadataKey = "disallow_free_auth"

// ReasoningEffortMetadataKey stores the client-requested reasoning effort for usage logs.
const ReasoningEffortMetadataKey = "reasoning_effort"

// MessageCountMetadataKey stores the inbound request message/input item count for safe failure logs.
const MessageCountMetadataKey = "message_count"

// RequestBodyBytesMetadataKey stores the inbound JSON request body size.
const RequestBodyBytesMetadataKey = "request_body_bytes"

// RequestWireBytesMetadataKey stores the inbound body size before content decoding.
const RequestWireBytesMetadataKey = "request_wire_bytes"

// ContentPartCountMetadataKey stores the inbound content part count.
const ContentPartCountMetadataKey = "content_part_count"

// ToolCallCountMetadataKey stores the inbound tool call count, excluding tool results.
const ToolCallCountMetadataKey = "tool_call_count"

// ToolOutputBytesMetadataKey stores the decoded tool result payload bytes.
const ToolOutputBytesMetadataKey = "tool_output_bytes"

// InlineImageBytesMetadataKey stores the estimated decoded bytes of inline images.
const InlineImageBytesMetadataKey = "inline_image_bytes"

// ReasoningBytesMetadataKey stores inbound reasoning and thinking content bytes.
const ReasoningBytesMetadataKey = "reasoning_bytes"

// RequestSourceFormatMetadataKey stores a bounded inbound protocol family.
const RequestSourceFormatMetadataKey = "request_source_format"

// RequestEndpointMetadataKey stores a bounded endpoint class.
const RequestEndpointMetadataKey = "request_endpoint"

// RequestStreamMetadataKey reports whether the downstream execution is streaming.
const RequestStreamMetadataKey = "request_stream"

// ToolCountMetadataKey stores the inbound request tool/tool-call item count for safe failure logs.
const ToolCountMetadataKey = "tool_count"

// DeclaredToolCountMetadataKey stores the inbound declared tool count.
const DeclaredToolCountMetadataKey = "declared_tool_count"

// ToolInteractionCountMetadataKey stores inbound tool call/result item count.
const ToolInteractionCountMetadataKey = "tool_interaction_count"

// MCPToolCountMetadataKey stores inbound MCP-shaped tool item count.
const MCPToolCountMetadataKey = "mcp_tool_count"

// BuiltinToolCountMetadataKey stores inbound built-in tool item count.
const BuiltinToolCountMetadataKey = "builtin_tool_count"

// ToolShapeTypesMetadataKey stores a bounded comma-separated set of sanitized tool types.
const ToolShapeTypesMetadataKey = "tool_types"

// ToolNameHashesMetadataKey stores a bounded comma-separated set of tool name hashes.
const ToolNameHashesMetadataKey = "tool_name_hashes"

// ClientProfileMetadataKey stores a lightweight inferred client profile such as claude_code.
const ClientProfileMetadataKey = "client_profile"

// ReasoningEffortSourceMetadataKey stores which request field carried the original effort.
const ReasoningEffortSourceMetadataKey = "reasoning_effort_source"

// ReasoningEffortOriginalMetadataKey stores the original effort value before any provider normalization.
const ReasoningEffortOriginalMetadataKey = "reasoning_effort_original"

// ModelContextHintMetadataKey stores a lightweight public model hint such as [1m].
const ModelContextHintMetadataKey = "model_context_hint"

// ServiceTierMetadataKey stores the client-requested service tier for usage logs.
const ServiceTierMetadataKey = "service_tier"

const (
	// PinnedAuthMetadataKey locks execution to a specific auth ID.
	PinnedAuthMetadataKey = "pinned_auth_id"
	// SelectedAuthMetadataKey stores the auth ID selected by the scheduler.
	SelectedAuthMetadataKey = "selected_auth_id"
	// SelectedAuthCallbackMetadataKey carries an optional callback invoked with the selected auth ID.
	SelectedAuthCallbackMetadataKey = "selected_auth_callback"
	// ExecutionSessionMetadataKey identifies a long-lived downstream execution session.
	ExecutionSessionMetadataKey = "execution_session_id"
)

// Request encapsulates the translated payload that will be sent to a provider executor.
type Request struct {
	// Model is the upstream model identifier after translation.
	Model string
	// Payload is the provider specific JSON payload.
	Payload []byte
	// Format represents the provider payload schema.
	Format sdktranslator.Format
	// Metadata carries optional provider specific execution hints.
	Metadata map[string]any
}

// RequestAfterAuthInterceptor rewrites a request after credential selection and before executor translation.
type RequestAfterAuthInterceptor func(context.Context, RequestAfterAuthInterceptRequest) RequestAfterAuthInterceptResponse

// RequestAfterAuthInterceptRequest describes a selected-auth request before executor translation.
type RequestAfterAuthInterceptRequest struct {
	// SourceFormat is the original client protocol format.
	SourceFormat sdktranslator.Format
	// ToFormat is the selected upstream protocol format.
	ToFormat sdktranslator.Format
	// Model is the selected upstream model for this attempt.
	Model string
	// RequestedModel is the client-requested model before alias/model-pool rewriting.
	RequestedModel string
	// Stream reports whether the request expects streaming output.
	Stream bool
	// Headers contains the current upstream request headers.
	Headers http.Header
	// Body contains the current request payload.
	Body []byte
	// Metadata is a best-effort cloned context snapshot. Treat it as read-only and JSON-like.
	Metadata map[string]any
}

// RequestAfterAuthInterceptResponse returns selected-auth request modifications.
type RequestAfterAuthInterceptResponse struct {
	// Headers replaces matching current request headers and preserves headers not mentioned here.
	Headers http.Header
	// Body replaces the current request body only when non-empty.
	Body []byte
	// ClearHeaders explicitly removes current request headers before Headers is applied.
	ClearHeaders []string
}

// Options controls execution behavior for both streaming and non-streaming calls.
type Options struct {
	// Stream toggles streaming mode.
	Stream bool
	// Alt carries optional alternate format hint (e.g. SSE JSON key).
	Alt string
	// Headers are forwarded to the provider request builder.
	Headers http.Header
	// Query contains optional query string parameters.
	Query url.Values
	// OriginalRequest preserves the inbound request bytes prior to translation.
	OriginalRequest []byte
	// SourceFormat identifies the inbound schema.
	SourceFormat sdktranslator.Format
	// ResponseFormat identifies the downstream response schema.
	// Empty means responses should use SourceFormat for backward compatibility.
	ResponseFormat sdktranslator.Format
	// Metadata carries extra execution hints shared across selection and executors.
	Metadata map[string]any
	// RequestAfterAuthInterceptor runs after credential selection and before executor translation.
	RequestAfterAuthInterceptor RequestAfterAuthInterceptor
	// InternalAuthSelectionCapability carries an opaque in-process auth-selection capability.
	// HTTP request metadata must never be copied into this field. Auth managers only honor
	// private capability values that cannot be synthesized from request data.
	InternalAuthSelectionCapability any
}

// ResponseFormatOrSource returns the response target format for an execution.
func ResponseFormatOrSource(opts Options) sdktranslator.Format {
	if opts.ResponseFormat != "" {
		return opts.ResponseFormat
	}
	return opts.SourceFormat
}

// Response wraps either a full provider response or metadata for streaming flows.
type Response struct {
	// Payload is the provider response in the executor format.
	Payload []byte
	// Metadata exposes optional structured data for translators.
	Metadata map[string]any
	// Headers carries upstream HTTP response headers for passthrough to clients.
	Headers http.Header
}

// CodexAlphaSearchEndpoint identifies Codex's standalone search exchange.
const CodexAlphaSearchEndpoint = "codex.alpha_search"

// RawEndpointRequest carries an untranslated provider endpoint request.
type RawEndpointRequest struct {
	Endpoint string
	Body     []byte
	Headers  http.Header
}

// RawEndpointResponse carries a bounded untranslated provider response.
type RawEndpointResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// StreamChunk represents a single streaming payload unit emitted by provider executors.
type StreamChunk struct {
	// Payload is the raw provider chunk payload.
	// Executors transfer read-only ownership on send and must not mutate it afterward.
	Payload []byte
	// Err reports any terminal error encountered while producing chunks.
	Err error
}

// StreamResult wraps the streaming response, providing both the chunk channel
// and the upstream HTTP response headers captured before streaming begins.
type StreamResult struct {
	// Headers carries upstream HTTP response headers from the initial connection.
	Headers http.Header
	// Chunks is the channel of streaming payload units.
	Chunks <-chan StreamChunk
	// Cancel aborts the upstream stream when downstream no longer needs it.
	// It must be safe to call multiple times.
	Cancel func()
}

// Close aborts the upstream stream if a cancellation hook is available.
func (r *StreamResult) Close() {
	if r == nil || r.Cancel == nil {
		return
	}
	r.Cancel()
}

// StatusError represents an error that carries an HTTP-like status code.
// Provider executors should implement this when possible to enable
// better auth state updates on failures (e.g., 401/402/429).
type StatusError interface {
	error
	StatusCode() int
}

// RequestScopedError identifies a failure tied to the current request rather
// than the selected credential.
type RequestScopedError interface {
	error
	IsRequestScoped() bool
}
