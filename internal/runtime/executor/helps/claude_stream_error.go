package helps

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// ClaudeSSEErrorPayload recognizes protocol errors before a translator or native
// passthrough can mistake a clean transport EOF for successful generation.
// Only envelope fields are inspected; generated text is never classified.
func ClaudeSSEErrorPayload(event []byte) ([]byte, bool) {
	payload, errorEvent := claudeSSEPayload(event)
	if errorEvent {
		return payload, true
	}
	if !gjson.ValidBytes(payload) {
		return nil, false
	}
	root := gjson.ParseBytes(payload)
	upstreamError := root.Get("error")
	if root.Get("type").String() == "error" || (upstreamError.Exists() && upstreamError.Type != gjson.Null) ||
		(root.Get("type").String() == "" && root.Get("message").Type == gjson.String) {
		return payload, true
	}
	return nil, false
}

// ClaudeSSEMessageComplete recognizes a real terminal envelope, never text
// emitted inside a content block or an event name without valid JSON data.
func ClaudeSSEMessageComplete(event []byte) bool {
	payload, isError := claudeSSEPayload(event)
	return !isError && gjson.ValidBytes(payload) && gjson.GetBytes(payload, "type").String() == "message_stop"
}

func claudeSSEPayload(event []byte) ([]byte, bool) {
	var data [][]byte
	errorEvent := false
	for _, raw := range bytes.Split(event, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if bytes.HasPrefix(line, []byte("event:")) {
			errorEvent = bytes.Equal(bytes.TrimSpace(line[len("event:"):]), []byte("error"))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(line[len("data:"):]))
		}
	}
	return bytes.Join(data, []byte("\n")), errorEvent
}
