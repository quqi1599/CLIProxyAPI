package callbacknegative

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type localIterator struct{}

func (localIterator) ForEach(iterator func(int) bool) {}

func unrelatedForEach(iterator localIterator, body []byte) []byte {
	iterator.ForEach(func(int) bool {
		body, _ = sjson.SetRawBytes(body, "output.-1", nil)
		return true
	})
	return body
}

func localPayloadPerCallback(items gjson.Result) {
	items.ForEach(func(_, _ gjson.Result) bool {
		body := []byte(`{"output":[]}`)
		body, _ = sjson.SetRawBytes(body, "output.-1", nil)
		_ = body
		return true
	})
}

func localContainerPerCallback(items gjson.Result) {
	items.ForEach(func(_, item gjson.Result) bool {
		values := make([]string, 0, 1)
		values = append(values, item.Raw)
		_, _ = json.Marshal(values)
		return true
	})
}

func resetCapturedContainer(items gjson.Result) []byte {
	var values []string
	var output []byte
	items.ForEach(func(_, item gjson.Result) bool {
		values = values[:0]
		values = append(values, item.Raw)
		output, _ = json.Marshal(values)
		return true
	})
	return output
}

func discardedRewrite(items gjson.Result, body []byte) {
	items.ForEach(func(_, item gjson.Result) bool {
		updated, _ := sjson.SetBytes(body, "model", item.Raw)
		_ = updated
		return true
	})
}
