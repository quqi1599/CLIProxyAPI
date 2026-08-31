package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	log "github.com/sirupsen/logrus"
)

const (
	preRouteFailureEvent = "pre_route_failure"

	preRouteFailureKindProviderResolution = "provider_resolution"
	preRouteFailureKindPluginExecutor     = "plugin_executor"

	preRouteErrorProviderNotResolved         = "provider_not_resolved"
	preRouteErrorPluginHostUnavailable       = "plugin_executor_host_unavailable"
	preRouteErrorPluginExecutionFailed       = "plugin_executor_failed"
	preRouteErrorPluginStreamUnavailable     = "plugin_executor_stream_unavailable"
	preRouteFallbackBadGatewayClassification = "pre_route_bad_gateway"
)

type preRouteFailureLog struct {
	StatusCode     int
	FailureKind    string
	ErrorCode      string
	EntryProtocol  string
	RequestedModel string
	PluginID       string
	Stream         bool
	RawJSON        []byte
	Options        modelExecutionOptions
}

func logProviderResolutionFailure(ctx context.Context, statusCode int, entryProtocol, requestedModel string, stream bool, rawJSON []byte, options modelExecutionOptions) {
	logPreRouteFailure(ctx, preRouteFailureLog{
		StatusCode:     statusCode,
		FailureKind:    preRouteFailureKindProviderResolution,
		ErrorCode:      preRouteErrorProviderNotResolved,
		EntryProtocol:  entryProtocol,
		RequestedModel: requestedModel,
		Stream:         stream,
		RawJSON:        rawJSON,
		Options:        options,
	})
}

func logPluginExecutorFailure(ctx context.Context, statusCode int, errorCode, entryProtocol, requestedModel, pluginID string, stream bool, rawJSON []byte, options modelExecutionOptions) {
	logPreRouteFailure(ctx, preRouteFailureLog{
		StatusCode:     statusCode,
		FailureKind:    preRouteFailureKindPluginExecutor,
		ErrorCode:      errorCode,
		EntryProtocol:  entryProtocol,
		RequestedModel: requestedModel,
		PluginID:       pluginID,
		Stream:         stream,
		RawJSON:        rawJSON,
		Options:        options,
	})
}

// logPreRouteFailure records request-safe correlation fields for gateway errors
// returned before the core auth manager emits route selection telemetry.
func logPreRouteFailure(ctx context.Context, failure preRouteFailureLog) {
	if failure.StatusCode != http.StatusBadGateway {
		return
	}

	failureKind := strings.TrimSpace(failure.FailureKind)
	if failureKind == "" {
		failureKind = preRouteFallbackBadGatewayClassification
	}
	errorCode := strings.TrimSpace(failure.ErrorCode)
	if errorCode == "" {
		errorCode = preRouteFallbackBadGatewayClassification
	}

	fields := log.Fields{
		"event":          preRouteFailureEvent,
		"routing_phase":  "pre_route",
		"failure_class":  "pre_route_bad_gateway",
		"failure_kind":   failureKind,
		"failure_scope":  "request",
		"status_code":    http.StatusBadGateway,
		"error_code":     errorCode,
		"attempt_count":  0,
		"request_stream": failure.Stream,
	}

	if requestID := logging.NormalizeClientRequestID(logging.GetRequestID(ctx)); requestID != "" {
		fields["request_id"] = requestID
	}
	if clientRequestID := logging.NormalizeClientRequestID(logging.GetClientRequestID(ctx)); clientRequestID != "" {
		fields["client_request_id"] = clientRequestID
	}
	if method := safePreRouteLogLabel(logging.GetEndpointMethod(ctx), 16); method != "" {
		fields["endpoint_method"] = method
	}
	if path := safePreRouteLogLabel(logging.GetEndpointPath(ctx), 160); path != "" {
		fields["endpoint_path"] = path
		fields["request_path"] = path
	}
	if sourceFormat := normalizeComplexitySourceFormat(failure.EntryProtocol); sourceFormat != "" {
		fields["source_format"] = sourceFormat
	}
	addSafePreRouteIdentifier(fields, "requested_model", failure.RequestedModel)
	addSafePreRouteIdentifier(fields, "plugin_id", failure.PluginID)
	addPreRouteRequestShape(fields, failure)
	if profile := preRouteClientProfile(ctx); profile != "" {
		fields["client_profile"] = profile
	}

	log.WithFields(fields).Warn("pre-route failure")
}

func addPreRouteRequestShape(fields log.Fields, failure preRouteFailureLog) {
	if len(fields) == 0 {
		return
	}
	payloadDigest := sha256.Sum256(failure.RawJSON)
	fields["payload_bytes"] = len(failure.RawJSON)
	fields["payload_sha256"] = hex.EncodeToString(payloadDigest[:])

	var vector complexityVector
	valid := false
	if failure.Options.complexity != nil && failure.Options.complexityValid {
		vector = *failure.Options.complexity
		vector.applyDimensions(failure.Options.complexityDimensions)
		valid = true
	} else {
		vector, valid = inspectRequestComplexityWithDimensions(failure.RawJSON, failure.Options.complexityDimensions)
	}
	if valid {
		if vector.MessageCount > 0 {
			fields["message_count"] = vector.MessageCount
		}
		toolCount := vector.DeclaredToolCount
		if vector.InteractionCount > toolCount {
			toolCount = vector.InteractionCount
		}
		if toolCount > 0 {
			fields["tool_count"] = toolCount
		}
		if vector.DeclaredToolCount > 0 {
			fields["declared_tool_count"] = vector.DeclaredToolCount
		}
		if vector.InteractionCount > 0 {
			fields["tool_interaction_count"] = vector.InteractionCount
		}
		if vector.MCPToolCount > 0 {
			fields["mcp_tool_count"] = vector.MCPToolCount
		}
		if vector.BuiltinToolCount > 0 {
			fields["builtin_tool_count"] = vector.BuiltinToolCount
		}
		if len(vector.ToolTypes) > 0 {
			fields["tool_types"] = strings.Join(vector.ToolTypes, ",")
		}
		if len(vector.ToolNameHashes) > 0 {
			fields["tool_name_hashes"] = strings.Join(vector.ToolNameHashes, ",")
		}
	}

	if effort := thinking.ExtractReasoningEffort(failure.RawJSON, failure.EntryProtocol, failure.RequestedModel); effort != "" {
		fields["reasoning_effort"] = effort
	}
}

func preRouteClientProfile(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return ""
	}
	return inferClientProfileFromHeaders(ginCtx.Request.Header)
}

func addSafePreRouteIdentifier(fields log.Fields, fieldName, value string) {
	if len(fields) == 0 || strings.TrimSpace(value) == "" {
		return
	}
	if safe, ok := safePreRouteIdentifier(value); ok {
		fields[fieldName] = safe
		return
	}
	digest := sha256.Sum256([]byte(value))
	fields[fieldName+"_hash"] = hex.EncodeToString(digest[:8])
	fields[fieldName+"_bytes"] = len(value)
}

func safePreRouteIdentifier(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if value == "" || len(value) > 160 {
		return "", false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '-', '_', '.', ':', '/', '(', ')', '+':
			continue
		default:
			return "", false
		}
	}
	return value, true
}

func safePreRouteLogLabel(value string, maxBytes int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if value == "" || maxBytes <= 0 {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	for len(value) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		if size <= 0 {
			return ""
		}
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}
