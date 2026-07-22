package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const bodyMetadataPrefix = "[BODY METADATA v1] "

type bodyMetadata struct {
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type,omitempty"`
	Truncated   bool   `json:"truncated"`
}

// SummarizeBodyMetadata returns deterministic metadata without retaining or
// exposing the body. Content type is bounded and stripped of line breaks.
func SummarizeBodyMetadata(body []byte, contentType string) string {
	digest := sha256.Sum256(body)
	contentType = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(contentType))
	if len(contentType) > 128 {
		contentType = contentType[:128]
	}
	encoded, _ := json.Marshal(bodyMetadata{
		Bytes:       int64(len(body)),
		SHA256:      hex.EncodeToString(digest[:]),
		ContentType: contentType,
	})
	return bodyMetadataPrefix + string(encoded)
}
