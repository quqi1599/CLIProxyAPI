package helps

import (
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// StripVertexOpenAIResponsesToolCallIDs removes OpenAI Responses call IDs that
// Vertex rejects in Gemini functionCall/functionResponse payloads.
func StripVertexOpenAIResponsesToolCallIDs(payload []byte, sourceFormat string) []byte {
	if !strings.EqualFold(strings.TrimSpace(sourceFormat), "openai-response") {
		return payload
	}

	contents := gjson.GetBytes(payload, "contents")
	if !contents.IsArray() {
		return payload
	}

	contentResults := contents.Array()
	outContents := make([][]byte, 0, len(contentResults))
	changed := false
	for _, content := range contentResults {
		parts := content.Get("parts")
		if !parts.IsArray() {
			outContents = append(outContents, []byte(content.Raw))
			continue
		}
		partResults := parts.Array()
		outParts := make([][]byte, 0, len(partResults))
		contentChanged := false
		for _, part := range partResults {
			partRaw := []byte(part.Raw)
			if part.Get("functionCall.id").Exists() {
				if updated, errDelete := sjson.DeleteBytes(partRaw, "functionCall.id"); errDelete == nil {
					partRaw = updated
					contentChanged = true
				}
			}
			if part.Get("functionResponse.id").Exists() {
				if updated, errDelete := sjson.DeleteBytes(partRaw, "functionResponse.id"); errDelete == nil {
					partRaw = updated
					contentChanged = true
				}
			}
			outParts = append(outParts, partRaw)
		}
		if !contentChanged {
			outContents = append(outContents, []byte(content.Raw))
			continue
		}
		updatedContent, errSet := sjson.SetRawBytes([]byte(content.Raw), "parts", internalpayload.BuildRaw(outParts))
		if errSet != nil {
			outContents = append(outContents, []byte(content.Raw))
			continue
		}
		outContents = append(outContents, updatedContent)
		changed = true
	}
	if !changed {
		return payload
	}
	out, errSet := sjson.SetRawBytes(payload, "contents", internalpayload.BuildRaw(outContents))
	if errSet != nil {
		return payload
	}
	return out
}
