package callbackpositive

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func claudeToOpenAIOldShape(items gjson.Result, body []byte) []byte {
	items.ForEach(func(_, item gjson.Result) bool {
		body, _ = sjson.SetRawBytes(body, "choices.-1", []byte(item.Raw)) // want "PG001 payload growth risk"
		return true
	})
	return body
}

func openAIToGeminiOldShape(items gjson.Result, body []byte) []byte {
	alias := body
	items.ForEach(func(_, _ gjson.Result) bool {
		updated, _ := sjson.DeleteBytes(alias, "contents.0") // want "PG002 payload growth risk"
		alias = updated
		return true
	})
	return alias
}

func callbackAlias(items gjson.Result, body []byte) []byte {
	callback := func(_, item gjson.Result) bool {
		body, _ = sjson.SetBytes(body, "model", item.Raw) // want "PG002 payload growth risk"
		return true
	}
	items.ForEach(callback)
	return body
}

func marshalGrowingClosure(items gjson.Result) []byte {
	var values []string
	var output []byte
	items.ForEach(func(_, item gjson.Result) bool {
		values = append(values, item.Raw)
		output, _ = json.Marshal(values) // want "PG003 payload growth risk"
		return true
	})
	return output
}
