package helps

import "github.com/tidwall/gjson"

// CodexResponseCompleted recognizes an unambiguous successful terminal envelope.
// Callers must still classify provider errors before accepting the completion.
func CodexResponseCompleted(payload []byte) bool {
	if _, ok := indexJSONViewFields(payload); !ok {
		return false
	}
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return false
	}
	response := gjson.GetBytes(payload, "response")
	if !response.IsObject() || response.Index < 0 || response.Index+len(response.Raw) > len(payload) {
		return false
	}
	if _, ok := indexJSONViewFields(payload[response.Index : response.Index+len(response.Raw)]); !ok {
		return false
	}
	if response.Get("status").String() != "completed" || response.Get("id").Type != gjson.String || response.Get("id").String() == "" {
		return false
	}
	for _, err := range []gjson.Result{gjson.GetBytes(payload, "error"), response.Get("error")} {
		if err.Exists() && err.Type != gjson.Null {
			return false
		}
	}
	return true
}
