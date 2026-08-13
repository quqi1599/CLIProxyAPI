package helps

import (
	"strings"
	"testing"
)

func TestCountInlineBase64ImagePartsAcrossProtocols(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "openai chat data URL", body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, want: 1},
		{name: "responses input image", body: `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/jpeg;base64,BBBB"}]}]}`, want: 1},
		{name: "claude image source", body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"CCCC"}}]}]}`, want: 1},
		{name: "remote image URL", body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]}`, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CountInlineBase64ImageParts([]byte(test.body)); got != test.want {
				t.Fatalf("CountInlineBase64ImageParts() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestLargeQwenInlineBase64ImageRequest(t *testing.T) {
	largeBase64 := strings.Repeat("A", QwenInlineBase64ImagePayloadLimitBytes)
	largeInline := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + largeBase64 + `"}}]}]}`)

	if images, reject := LargeQwenInlineBase64ImageRequest("qwen", largeInline); !reject || images != 1 {
		t.Fatalf("LargeQwenInlineBase64ImageRequest() = (%d, %t), want (1, true)", images, reject)
	}
	if images, reject := LargeQwenInlineBase64ImageRequest("deepseek", largeInline); reject || images != 0 {
		t.Fatalf("non-Qwen request = (%d, %t), want (0, false)", images, reject)
	}
	remoteURL := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/` + largeBase64 + `.png"}}]}]}`)
	if images, reject := LargeQwenInlineBase64ImageRequest("qwen", remoteURL); reject || images != 0 {
		t.Fatalf("remote URL request = (%d, %t), want (0, false)", images, reject)
	}
}
