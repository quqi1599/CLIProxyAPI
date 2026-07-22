package baseline

import "github.com/tidwall/sjson"

func appendTwice(body []byte, values [][]byte) []byte {
	for _, value := range values {
		body, _ = sjson.SetRawBytes(body, "output.-1", value)
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}
