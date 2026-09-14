package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type openAIResponsesStreamErrorChunk struct {
	Type           string         `json:"type"`
	Error          map[string]any `json:"error"`
	SequenceNumber int            `json:"sequence_number"`
}

func unmarshalJSONWithNumber(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

func openAIResponsesStreamErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusForbidden:
		return "insufficient_quota"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusRequestTimeout:
		return "request_timeout"
	default:
		if status >= http.StatusInternalServerError {
			return "internal_server_error"
		}
		if status >= http.StatusBadRequest {
			return "invalid_request_error"
		}
		return "unknown_error"
	}
}

// BuildOpenAIResponsesStreamErrorChunk builds an OpenAI Responses streaming error chunk.
//
// Important: OpenAI's HTTP error bodies are shaped like {"error":{...}}; those are valid for
// non-streaming responses, but streaming clients validate SSE `data:` payloads against a union
// of chunks that requires a top-level `type` field.
func BuildOpenAIResponsesStreamErrorChunk(status int, errText string, sequenceNumber int) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	status = NormalizeKnownUserErrorStatus(status, errText)
	if sequenceNumber < 0 {
		sequenceNumber = 0
	}

	message := strings.TrimSpace(errText)
	if message == "" {
		message = http.StatusText(status)
	}

	code := openAIResponsesStreamErrorCode(status)
	direction, isContentSafety := contentSafetyDirection(status, errText)
	if isContentSafety {
		message = UserFacingContentSafetyMessage(direction)
		code = contentPolicyViolationErrorCode
	}
	_, isContextWindowExceeded := contextWindowExceededErrorDetail(status, errText)
	if isContextWindowExceeded {
		message = UserFacingContextWindowMessage()
		code = contextWindowExceededErrorCode
	}
	requestFeatureDetail, isRequestFeatureUnsupported := requestFeatureUnsupportedErrorDetail(status, errText)
	if isRequestFeatureUnsupported {
		message = requestFeatureDetail.Message
		code = requestFeatureDetail.Code
	}
	remoteCompactionDetail, _, isRemoteCompactionError := remoteCompactionErrorDetail(status, errText)
	if isRemoteCompactionError {
		message = remoteCompactionDetail.Message
		code = remoteCompactionDetail.Code
	}
	isNormalizedError := isContentSafety || isContextWindowExceeded || isRequestFeatureUnsupported || isRemoteCompactionError
	if !isNormalizedError {
		detail, isClientHint := clientHintErrorDetail(status, errText)
		if isClientHint {
			message = detail.Message
			code = detail.Code
			isNormalizedError = true
		}
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		var payload map[string]any
		if err := unmarshalJSONWithNumber([]byte(trimmed), &payload); err == nil {
			if v, ok := payload["sequence_number"]; ok && sequenceNumber == 0 {
				switch n := v.(type) {
				case json.Number:
					if seq, err := n.Int64(); err == nil {
						sequenceNumber = int(seq)
					}
				case float64:
					sequenceNumber = int(n)
				}
			}
			if t, ok := payload["type"].(string); ok && strings.TrimSpace(t) == "error" {
				if !isNormalizedError {
					if m, ok := payload["message"].(string); ok && strings.TrimSpace(m) != "" {
						message = strings.TrimSpace(m)
					}
				}
				if !isNormalizedError {
					if v, ok := payload["code"]; ok && v != nil {
						if c, ok := v.(string); ok && strings.TrimSpace(c) != "" {
							code = strings.TrimSpace(c)
						} else {
							code = strings.TrimSpace(fmt.Sprint(v))
						}
					}
				}
			}
			if e, ok := payload["error"].(map[string]any); ok {
				if !isNormalizedError {
					if m, ok := e["message"].(string); ok && strings.TrimSpace(m) != "" {
						message = strings.TrimSpace(m)
					}
				}
				if !isNormalizedError {
					if v, ok := e["code"]; ok && v != nil {
						if c, ok := v.(string); ok && strings.TrimSpace(c) != "" {
							code = strings.TrimSpace(c)
						} else {
							code = strings.TrimSpace(fmt.Sprint(v))
						}
					}
				}
			}
		}
	}

	if strings.TrimSpace(code) == "" {
		code = "unknown_error"
	}
	detail := map[string]any{"type": "server_error", "code": code, "message": message, "param": nil}
	if status < http.StatusInternalServerError {
		detail["type"] = "invalid_request_error"
	}
	if !isNormalizedError && trimmed != "" && json.Valid([]byte(trimmed)) {
		var payload map[string]any
		if err := unmarshalJSONWithNumber([]byte(trimmed), &payload); err == nil {
			if nested, ok := payload["error"].(map[string]any); ok {
				detail = nested
			}
		}
	}

	data, err := json.Marshal(openAIResponsesStreamErrorChunk{
		Type:           "error",
		Error:          detail,
		SequenceNumber: sequenceNumber,
	})
	if err == nil {
		return data
	}

	// Extremely defensive fallback.
	data, _ = json.Marshal(openAIResponsesStreamErrorChunk{
		Type:           "error",
		Error:          map[string]any{"type": "server_error", "code": "internal_server_error", "message": message, "param": nil},
		SequenceNumber: sequenceNumber,
	})
	if len(data) > 0 {
		return data
	}
	return []byte(`{"type":"error","error":{"type":"server_error","code":"internal_server_error","message":"internal error","param":null},"sequence_number":0}`)
}
