package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxBufferedCompactionStreamBytes = 64 << 20

// BuildLegacyResponsesCompactionRequest converts a v2 trigger request into a
// self-contained legacy /responses/compact request. A prior transcript is
// required because a previous_response_id may refer to opaque state owned by a
// different compatibility group.
func BuildLegacyResponsesCompactionRequest(body []byte) ([]byte, error) {
	if len(body) == 0 || !json.Valid(body) {
		return nil, newCompactionRequestFailure("compaction request is not valid JSON")
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil, newCompactionRequestFailure("compaction_trigger requires an input transcript")
	}
	items := input.Array()
	if len(items) < 2 || strings.TrimSpace(items[len(items)-1].Get("type").String()) != "compaction_trigger" {
		return nil, newCompactionRequestFailure("compaction_trigger requires a self-contained input transcript")
	}
	kept := make([]string, 0, len(items)-1)
	for i := 0; i < len(items)-1; i++ {
		if strings.TrimSpace(items[i].Get("type").String()) == "compaction_trigger" {
			return nil, newCompactionRequestFailure("compaction_trigger must appear exactly once at the end of input")
		}
		kept = append(kept, items[i].Raw)
	}
	out, err := sjson.SetRawBytes(payload.CloneBytes(body), "input", payload.BuildRaw(kept))
	if err != nil {
		return nil, newCompactionRequestFailure("compaction request could not be converted")
	}
	out, _ = sjson.DeleteBytes(out, "stream")
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	out, _ = sjson.DeleteBytes(out, "context_management")
	return out, nil
}

// ValidateLegacyResponsesCompaction validates the compact endpoint response.
// The response may contain user messages before exactly one terminal
// compaction item, matching the legacy Responses compaction contract.
func ValidateLegacyResponsesCompaction(data []byte) error {
	if len(data) == 0 || !json.Valid(data) {
		return newCompactionProtocolFailure("invalid_compaction_response", "upstream compaction response is not valid JSON", nil)
	}
	output := gjson.GetBytes(data, "output")
	if !output.IsArray() {
		return newCompactionProtocolFailure("invalid_compaction_response", "upstream compaction response has no output array", nil)
	}
	compactions := 0
	seenCompaction := false
	for _, item := range output.Array() {
		itemType := strings.TrimSpace(item.Get("type").String())
		switch itemType {
		case "compaction":
			compactions++
			seenCompaction = true
			if !hasNonEmptyCompactionEncryptedContent(item) {
				return newCompactionProtocolFailure("invalid_compaction_response", "upstream compaction output has no encrypted_content", nil)
			}
		case "message":
			if seenCompaction || !strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
				return newCompactionProtocolFailure("invalid_compaction_response", "upstream compact output contains an unsupported item", nil)
			}
		default:
			return newCompactionProtocolFailure("invalid_compaction_response", "upstream compact output contains an unsupported item", nil)
		}
	}
	if compactions != 1 {
		return newCompactionProtocolFailure("invalid_compaction_response", "upstream compact output must contain exactly one compaction item", nil)
	}
	return nil
}

// ValidateContextManagementCompactionResponse validates a non-streaming
// server-side compaction response without applying the legacy endpoint's
// user-message-only allow-list.
func ValidateContextManagementCompactionResponse(data []byte) error {
	if len(data) == 0 || !json.Valid(data) {
		return newCompactionProtocolFailure("invalid_compaction_response", "upstream context-management compaction response is not valid JSON", nil)
	}
	output := gjson.GetBytes(data, "output")
	if !output.IsArray() {
		return newCompactionProtocolFailure("invalid_compaction_response", "upstream context-management compaction response has no output array", nil)
	}
	compactions := 0
	for _, item := range output.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "compaction" {
			continue
		}
		compactions++
		if !hasNonEmptyCompactionEncryptedContent(item) {
			return newCompactionProtocolFailure("invalid_compaction_response", "context-management compaction output has no encrypted_content", nil)
		}
	}
	if compactions != 1 {
		return newCompactionProtocolFailure("invalid_compaction_response", "context-management response must contain exactly one compaction item", nil)
	}
	return nil
}

// WrapResponsesCompactionStream buffers a v2 stream until response.completed
// has been validated, so ordinary model output can never be committed as a
// successful compaction response.
func WrapResponsesCompactionStream(ctx context.Context, result *cliproxyexecutor.StreamResult) *cliproxyexecutor.StreamResult {
	if result == nil || result.Chunks == nil {
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		chunks := make([]cliproxyexecutor.StreamChunk, 0, 16)
		var combined bytes.Buffer
		for {
			var (
				chunk cliproxyexecutor.StreamChunk
				ok    bool
			)
			if ctx == nil {
				chunk, ok = <-result.Chunks
			} else {
				select {
				case <-ctx.Done():
					if result.Cancel != nil {
						result.Cancel()
					}
					return
				case chunk, ok = <-result.Chunks:
				}
			}
			if !ok {
				break
			}
			if chunk.Err != nil {
				chunk.Err = normalizeBufferedCompactionError(chunk.Err)
				emitCompactionChunk(ctx, out, chunk)
				return
			}
			if combined.Len()+len(chunk.Payload) > maxBufferedCompactionStreamBytes {
				if result.Cancel != nil {
					result.Cancel()
				}
				emitCompactionChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream exceeded the validation limit", nil)})
				return
			}
			chunks = append(chunks, chunk)
			if combined.Len() > 0 && len(chunk.Payload) > 0 {
				previous := combined.Bytes()[combined.Len()-1]
				if previous != '\n' && chunk.Payload[0] != '\n' && chunk.Payload[0] != '\r' {
					_ = combined.WriteByte('\n')
				}
			}
			_, _ = combined.Write(chunk.Payload)
		}
		validationErr := ValidateResponsesCompactionStream(combined.Bytes())
		RecordResponsesCompactionValidation(ctx, "v2_stream", combined.Bytes(), validationErr)
		if validationErr != nil {
			emitCompactionChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: validationErr})
			return
		}
		for _, chunk := range chunks {
			if !emitCompactionChunk(ctx, out, chunk) {
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out, Cancel: result.Cancel}
}

func normalizeBufferedCompactionError(err error) error {
	failure, ok := failurecontract.As(err)
	if !ok || failure == nil {
		return err
	}
	normalized := *failure
	normalized.StreamPhase = failurecontract.StreamPhaseBeforeOutput
	normalized.OutputCommitted = false
	return &normalized
}

// RecordResponsesCompactionValidation logs only bounded structural metadata.
// It never logs input, output content, or encrypted_content values.
func RecordResponsesCompactionValidation(ctx context.Context, protocol string, data []byte, validationErr error) {
	fields := responsesCompactionValidationFields(protocol, data, validationErr)
	entry := LogWithRequestID(ctx).WithFields(fields)
	if validationErr != nil {
		entry.WithField("error_code", compactionValidationErrorCode(validationErr)).Warn("compaction_validation")
		return
	}
	entry.Info("compaction_validation")
}

func responsesCompactionValidationFields(protocol string, data []byte, validationErr error) map[string]any {
	output, terminalEvent := compactionValidationOutput(protocol, data)
	outputCount := 0
	compactionCount := 0
	encryptedContentBytes := 0
	itemTypes := make([]string, 0, 4)
	seenTypes := make(map[string]struct{})
	if output.IsArray() {
		items := output.Array()
		outputCount = len(items)
		for _, item := range items {
			itemType := sanitizeCompactionLogType(item.Get("type").String())
			if itemType != "" {
				if _, seen := seenTypes[itemType]; !seen && len(itemTypes) < 16 {
					seenTypes[itemType] = struct{}{}
					itemTypes = append(itemTypes, itemType)
				}
			}
			if itemType != "compaction" {
				continue
			}
			compactionCount++
			encryptedContent := item.Get("encrypted_content")
			if encryptedContent.Type == gjson.String {
				encryptedContentBytes += len(encryptedContent.String())
			}
		}
	}
	return map[string]any{
		"event":                   "compaction_validation",
		"compaction_protocol":     strings.TrimSpace(protocol),
		"validation_success":      validationErr == nil,
		"response_bytes":          len(data),
		"response_output_count":   outputCount,
		"compaction_item_count":   compactionCount,
		"output_item_types":       strings.Join(itemTypes, ","),
		"encrypted_content_bytes": encryptedContentBytes,
		"terminal_event":          terminalEvent,
	}
}

func compactionValidationOutput(protocol string, data []byte) (gjson.Result, string) {
	if strings.TrimSpace(protocol) != "v2_stream" {
		return gjson.GetBytes(data, "output"), ""
	}
	var completed gjson.Result
	terminalEvent := ""
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	for _, line := range bytes.Split(normalized, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if !json.Valid(line) {
			continue
		}
		eventType := strings.TrimSpace(gjson.GetBytes(line, "type").String())
		if eventType != "response.completed" && eventType != "response.done" {
			continue
		}
		terminalEvent = eventType
		completed = gjson.GetBytes(line, "response.output")
	}
	return completed, terminalEvent
}

func sanitizeCompactionLogType(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		value = value[:64]
	}
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		char := value[i]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			out.WriteByte(char)
			continue
		}
		out.WriteByte('_')
	}
	return out.String()
}

func compactionValidationErrorCode(err error) string {
	if failure, ok := failurecontract.As(err); ok {
		return failure.ErrorCode()
	}
	return ""
}

// ValidateResponsesCompactionStream validates the authoritative completed
// response in a Responses SSE stream.
func ValidateResponsesCompactionStream(data []byte) error {
	completed, err := validateResponsesCompactionEventSequence(data)
	if err != nil {
		return err
	}
	if len(completed) == 0 {
		return newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream has no response.completed event", nil)
	}
	output := gjson.GetBytes(completed, "response.output")
	if !output.IsArray() {
		return newCompactionProtocolFailure("invalid_compaction_response", "completed compaction response has no output array", nil)
	}
	if status := strings.TrimSpace(gjson.GetBytes(completed, "response.status").String()); status != "" && status != "completed" {
		return newCompactionProtocolFailure("invalid_compaction_response", "compaction response did not complete successfully", nil)
	}
	compactions := 0
	for _, item := range output.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "compaction" {
			continue
		}
		compactions++
		if !hasNonEmptyCompactionEncryptedContent(item) {
			return newCompactionProtocolFailure("invalid_compaction_response", "completed compaction output has no encrypted_content", nil)
		}
	}
	if compactions != 1 {
		return newCompactionProtocolFailure("invalid_compaction_response", "completed response must contain exactly one compaction item", nil)
	}
	return nil
}

func hasNonEmptyCompactionEncryptedContent(item gjson.Result) bool {
	encryptedContent := item.Get("encrypted_content")
	return encryptedContent.Type == gjson.String && strings.TrimSpace(encryptedContent.String()) != ""
}

func validateResponsesCompactionEventSequence(data []byte) ([]byte, error) {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	completedCount := 0
	completedIndex := -1
	eventIndex := 0
	lastSequence := int64(-1)
	sequenceSeen := false
	var completed []byte
	pendingEventName := ""
	outputItemsByIndex := make(map[int64]json.RawMessage)
	outputItemsFallback := make([]json.RawMessage, 0, 1)
	for _, line := range bytes.Split(normalized, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			pendingEventName = ""
			continue
		}
		if bytes.HasPrefix(line, []byte(":")) || bytes.HasPrefix(line, []byte("id:")) || bytes.HasPrefix(line, []byte("retry:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			pendingEventName = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
			continue
		}

		payloadData := line
		if bytes.HasPrefix(line, []byte("data:")) {
			payloadData = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(payloadData, []byte("[DONE]")) {
				pendingEventName = ""
				continue
			}
		}
		if !json.Valid(payloadData) {
			return nil, newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream contains an invalid event payload", nil)
		}
		eventType := strings.TrimSpace(gjson.GetBytes(payloadData, "type").String())
		if eventType == "" {
			return nil, newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream contains an event without a type", nil)
		}
		if pendingEventName != "" && pendingEventName != eventType {
			return nil, newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream event name does not match its payload type", nil)
		}
		pendingEventName = ""
		normalizedEventType := eventType
		if normalizedEventType == "response.done" {
			normalizedEventType = "response.completed"
		}
		switch normalizedEventType {
		case "response.completed":
			completedCount++
			completedIndex = eventIndex
			completed = payload.CloneBytes(payloadData)
			if eventType == "response.done" {
				completed, _ = sjson.SetBytes(completed, "type", "response.completed")
			}
			completed = restoreCompactionCompletedOutput(completed, outputItemsByIndex, outputItemsFallback)
		case "response.failed", "response.incomplete", "error":
			return nil, newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream terminated with an error", nil)
		case "response.output_item.done":
			item := gjson.GetBytes(payloadData, "item")
			if item.Exists() && item.IsObject() {
				itemCopy := json.RawMessage(payload.CloneBytes([]byte(item.Raw)))
				if outputIndex := gjson.GetBytes(payloadData, "output_index"); outputIndex.Exists() {
					outputItemsByIndex[outputIndex.Int()] = itemCopy
				} else {
					outputItemsFallback = append(outputItemsFallback, itemCopy)
				}
			}
		}
		if sequence := gjson.GetBytes(payloadData, "sequence_number"); sequence.Exists() {
			value := sequence.Int()
			if sequenceSeen && value <= lastSequence {
				return nil, newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream sequence numbers are not increasing", nil)
			}
			lastSequence = value
			sequenceSeen = true
		}
		eventIndex++
	}
	if completedCount != 1 {
		return nil, newCompactionProtocolFailure("invalid_compaction_stream", "upstream compaction stream must contain exactly one response.completed event", nil)
	}
	if completedIndex != eventIndex-1 {
		return nil, newCompactionProtocolFailure("invalid_compaction_stream", "response.completed must be the final semantic event", nil)
	}
	return completed, nil
}

func restoreCompactionCompletedOutput(completed []byte, outputItemsByIndex map[int64]json.RawMessage, outputItemsFallback []json.RawMessage) []byte {
	output := gjson.GetBytes(completed, "response.output")
	if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		return completed
	}
	if len(outputItemsByIndex) == 0 && len(outputItemsFallback) == 0 {
		return completed
	}
	indexes := make([]int64, 0, len(outputItemsByIndex))
	for index := range outputItemsByIndex {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	items := make([]json.RawMessage, 0, len(outputItemsByIndex)+len(outputItemsFallback))
	for _, index := range indexes {
		items = append(items, outputItemsByIndex[index])
	}
	items = append(items, outputItemsFallback...)
	restoredOutput, err := json.Marshal(items)
	if err != nil {
		return completed
	}
	restored, err := sjson.SetRawBytes(completed, "response.output", restoredOutput)
	if err != nil {
		return completed
	}
	return restored
}

// BuildResponsesCompactionTriggerStream converts a validated legacy compact
// response into a semantic v2 Responses SSE sequence.
func BuildResponsesCompactionTriggerStream(compactData []byte, model string) ([][]byte, error) {
	if err := ValidateLegacyResponsesCompaction(compactData); err != nil {
		return nil, err
	}
	response := payload.CloneBytes(compactData)
	responseID := strings.TrimSpace(gjson.GetBytes(response, "id").String())
	if responseID == "" {
		responseID = fmt.Sprintf("resp_compaction_%d", time.Now().UnixNano())
		response, _ = sjson.SetBytes(response, "id", responseID)
	}
	if !gjson.GetBytes(response, "object").Exists() {
		response, _ = sjson.SetBytes(response, "object", "response")
	}
	if strings.TrimSpace(gjson.GetBytes(response, "model").String()) == "" && strings.TrimSpace(model) != "" {
		response, _ = sjson.SetBytes(response, "model", model)
	}
	response, _ = sjson.SetBytes(response, "status", "completed")
	output := gjson.GetBytes(response, "output").Array()
	sequence := int64(0)
	createdResponse, _ := sjson.SetBytes(payload.CloneBytes(response), "status", "in_progress")
	createdResponse, _ = sjson.SetRawBytes(createdResponse, "output", []byte("[]"))
	frames := [][]byte{
		buildResponsesSSEFrame("response.created", map[string]any{"type": "response.created", "sequence_number": sequence, "response": json.RawMessage(createdResponse)}),
	}
	sequence++
	frames = append(frames, buildResponsesSSEFrame("response.in_progress", map[string]any{"type": "response.in_progress", "sequence_number": sequence, "response": json.RawMessage(createdResponse)}))
	sequence++
	for index, item := range output {
		frames = append(frames, buildResponsesSSEFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": sequence, "output_index": index, "item": json.RawMessage(item.Raw),
		}))
		sequence++
		frames = append(frames, buildResponsesSSEFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": sequence, "output_index": index, "item": json.RawMessage(item.Raw),
		}))
		sequence++
	}
	frames = append(frames, buildResponsesSSEFrame("response.completed", map[string]any{
		"type": "response.completed", "sequence_number": sequence, "response": json.RawMessage(response),
	}))
	return frames, nil
}

func buildResponsesSSEFrame(event string, value any) []byte {
	data, _ := json.Marshal(value)
	frame := make([]byte, 0, len(event)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, '\n')
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return frame
}

func emitCompactionChunk(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, chunk cliproxyexecutor.StreamChunk) bool {
	if ctx == nil {
		out <- chunk
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case out <- chunk:
		return true
	}
}

func newCompactionRequestFailure(message string) *failurecontract.Failure {
	return &failurecontract.Failure{
		Kind:          failurecontract.InvalidRequest,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusBadRequest,
		OuterStatus:   http.StatusBadRequest,
		ProviderCode:  "compaction_context_unavailable",
		SemanticCode:  "compaction_context_unavailable",
		SemanticType:  "invalid_request_error",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		Retryable:     false,
		PublicMessage: "compaction_context_unavailable: " + message,
	}
}

func newCompactionProtocolFailure(code, message string, cause error) *failurecontract.Failure {
	return &failurecontract.Failure{
		Kind:          failurecontract.UpstreamProtocolError,
		Scope:         failurecontract.ScopeProvider,
		HTTPStatus:    http.StatusBadGateway,
		OuterStatus:   http.StatusBadGateway,
		ProviderCode:  code,
		SemanticCode:  code,
		SemanticType:  "server_error",
		StreamPhase:   failurecontract.StreamPhaseBeforeOutput,
		Retryable:     false,
		Cause:         cause,
		PublicMessage: code + ": " + message,
	}
}
