package helps

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// QwenInlineBase64ImagePayloadLimitBytes is the request-size guard used before
// forwarding inline Base64 images to Qwen-compatible upstreams.
const QwenInlineBase64ImagePayloadLimitBytes = 10 << 20

var (
	inlineBase64Marker = []byte(`base64`)
	inlineDataMarker   = []byte(`data:image/`)
)

// LargeQwenInlineBase64ImageRequest reports whether a Qwen-compatible payload
// exceeds the safe request size while carrying at least one inline Base64 image.
func LargeQwenInlineBase64ImageRequest(compatKind string, body []byte) (imageParts int, reject bool) {
	if !strings.EqualFold(strings.TrimSpace(compatKind), "qwen") ||
		len(body) <= QwenInlineBase64ImagePayloadLimitBytes {
		return 0, false
	}
	imageParts = CountInlineBase64ImageParts(body)
	return imageParts, imageParts > 0
}

// CountInlineBase64ImageParts recognizes OpenAI Chat, Responses, and Claude
// inline-image spellings without decoding or logging image data.
func CountInlineBase64ImageParts(body []byte) int {
	if len(body) == 0 ||
		(!bytes.Contains(body, inlineBase64Marker) && !bytes.Contains(body, inlineDataMarker)) ||
		!gjson.ValidBytes(body) {
		return 0
	}

	count := 0
	var walk func(gjson.Result)
	walk = func(value gjson.Result) {
		if value.IsArray() {
			value.ForEach(func(_, child gjson.Result) bool {
				walk(child)
				return true
			})
			return
		}
		if !value.IsObject() {
			return
		}

		partType := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
		switch partType {
		case "image", "image_url", "input_image":
			if isInlineBase64ImagePart(value) {
				count++
				return
			}
		}

		value.ForEach(func(_, child gjson.Result) bool {
			walk(child)
			return true
		})
	}
	walk(gjson.ParseBytes(body))
	return count
}

func isInlineBase64ImagePart(part gjson.Result) bool {
	if strings.EqualFold(strings.TrimSpace(part.Get("source.type").String()), "base64") &&
		strings.TrimSpace(part.Get("source.data").String()) != "" {
		return true
	}
	for _, path := range []string{"image_url.url", "image_url", "url", "source.url"} {
		if isInlineBase64ImageURL(part.Get(path).String()) {
			return true
		}
	}
	return false
}

func isInlineBase64ImageURL(value string) bool {
	value = strings.TrimSpace(value)
	comma := strings.IndexByte(value, ',')
	if comma < 0 || comma > 256 {
		return false
	}
	header := strings.ToLower(value[:comma+1])
	return strings.HasPrefix(header, "data:image/") && strings.Contains(header, ";base64,")
}
