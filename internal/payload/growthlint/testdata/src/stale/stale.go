package stale // want "PG001 payload growth baseline is stale"

import "github.com/tidwall/sjson"

func appendOnce(body []byte, values [][]byte) []byte {
	for _, value := range values {
		body, _ = sjson.SetRawBytes(body, "output.-1", value)
	}
	return body
}
