package executor

import (
	"context"
	"net/http"
	"net/url"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

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

// ContentPartCountMetadataKey stores the inbound content part count.
const ContentPartCountMetadataKey = "content_part_count"

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

// StreamChunk represents a single streaming payload unit emitted by provider executors.
type StreamChunk struct {
	// Payload is the raw provider chunk payload.
	// Executors must treat emitted payload bytes as immutable after send.
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
