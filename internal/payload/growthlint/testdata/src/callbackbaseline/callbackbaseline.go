package callbackbaseline

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func appendTwice(items gjson.Result, body []byte) []byte {
	items.ForEach(func(_, item gjson.Result) bool {
		body, _ = sjson.SetRawBytes(body, "output.-1", []byte(item.Raw))
		body, _ = sjson.SetRawBytes(body, "output.-1", []byte(item.Raw)) // want "PG001 payload growth risk"
		return true
	})
	return body
}
