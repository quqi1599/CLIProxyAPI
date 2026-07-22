package util

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeToolResultContent(t *testing.T) {
	tests := []struct {
		name       string
		wrapper    string
		wantResult string
		wantRaw    bool
		wantImages int
	}{
		{
			name:       "StringContent",
			wrapper:    `{"content":"alpha"}`,
			wantResult: "alpha",
			wantRaw:    false,
			wantImages: 0,
		},
		{
			name:       "SingleTextBlock",
			wrapper:    `{"content":[{"type":"text","text":"alpha"}]}`,
			wantResult: `{"type":"text","text":"alpha"}`,
			wantRaw:    true,
			wantImages: 0,
		},
		{
			name:       "MultipleTextBlocks",
			wrapper:    `{"content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}`,
			wantResult: `[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]`,
			wantRaw:    true,
			wantImages: 0,
		},
		{
			name:       "TextAndImage",
			wrapper:    `{"content":[{"type":"text","text":"alpha"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`,
			wantResult: `{"type":"text","text":"alpha"}`,
			wantRaw:    true,
			wantImages: 1,
		},
		{
			name:       "ImageOnly",
			wrapper:    `{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`,
			wantResult: "",
			wantRaw:    false,
			wantImages: 1,
		},
		{
			name:       "ImageWithoutDataDropped",
			wrapper:    `{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]}`,
			wantResult: "",
			wantRaw:    false,
			wantImages: 0,
		},
		{
			name:       "ObjectContent",
			wrapper:    `{"content":{"foo":"bar"}}`,
			wantResult: `{"foo":"bar"}`,
			wantRaw:    true,
			wantImages: 0,
		},
		{
			name:       "ObjectImage",
			wrapper:    `{"content":{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}}`,
			wantResult: "",
			wantRaw:    false,
			wantImages: 1,
		},
		{
			name:       "AbsentContent",
			wrapper:    `{}`,
			wantResult: "",
			wantRaw:    false,
			wantImages: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertClaudeToolResultContent(gjson.Get(tt.wrapper, "content"))
			if got.Result != tt.wantResult {
				t.Errorf("Result = %q, want %q", got.Result, tt.wantResult)
			}
			if got.ResultIsRaw != tt.wantRaw {
				t.Errorf("ResultIsRaw = %v, want %v", got.ResultIsRaw, tt.wantRaw)
			}
			if len(got.Images) != tt.wantImages {
				t.Errorf("len(Images) = %d, want %d", len(got.Images), tt.wantImages)
			}
		})
	}
}

func TestConvertClaudeToolResultContentPayloadGrowth(t *testing.T) {
	for _, size := range []int{16, 64, 256, 1024} {
		t.Run(fmt.Sprintf("items_%d", size), func(t *testing.T) {
			contentJSON := buildClaudeToolResultContent(size)
			content := gjson.Parse(contentJSON)
			original := content.Raw

			got := ConvertClaudeToolResultContent(content)
			if content.Raw != original {
				t.Fatal("input content was mutated")
			}
			if !got.ResultIsRaw || !gjson.Valid(got.Result) {
				t.Fatalf("result is not valid raw JSON")
			}
			items := gjson.Parse(got.Result).Array()
			if len(items) != size {
				t.Fatalf("result item count = %d, want %d", len(items), size)
			}
			for i, item := range items {
				if gotIndex := item.Get("index").Int(); gotIndex != int64(i) {
					t.Fatalf("item %d index = %d", i, gotIndex)
				}
			}
		})
	}
}

func BenchmarkPayloadGrowthConvertClaudeToolResultContent(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		content := gjson.Parse(buildClaudeToolResultContent(size))
		b.Run(fmt.Sprintf("items_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				result := ConvertClaudeToolResultContent(content)
				if result.Result == "" {
					b.Fatal("empty result")
				}
			}
		})
	}
}

func buildClaudeToolResultContent(size int) string {
	var out strings.Builder
	out.Grow(size * 48)
	out.WriteByte('[')
	for i := 0; i < size; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, `{"type":"text","index":%d,"text":"value"}`, i)
	}
	out.WriteByte(']')
	return out.String()
}

func TestConvertClaudeToolResultContent_ImageFields(t *testing.T) {
	content := gjson.Get(`{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`, "content")
	got := ConvertClaudeToolResultContent(content)
	if len(got.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(got.Images))
	}
	if got.Images[0].MimeType != "image/png" {
		t.Errorf("MimeType = %q, want image/png", got.Images[0].MimeType)
	}
	if got.Images[0].Data != "aGVsbG8=" {
		t.Errorf("Data = %q, want aGVsbG8=", got.Images[0].Data)
	}
}
