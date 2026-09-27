package contentaudit

import (
	"strings"
	"unicode/utf8"
)

// evidencePrefix keeps at most limit runes with work bounded by that window.
// An untruncated value is unchanged, including any invalid UTF-8 bytes.
func evidencePrefix(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	// A bounded precheck avoids slower per-rune decoding for short Unicode text.
	if len(text)/utf8.UTFMax <= limit && utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	end, invalid := 0, 0
	for count := 0; count < limit && end < len(text); count++ {
		width := 1
		if text[end] >= utf8.RuneSelf {
			_, width = utf8.DecodeRuneInString(text[end:])
			if width == 1 {
				invalid++
			}
		}
		end += width
	}
	if end == len(text) {
		return text, false
	}
	return copyEvidenceWindow(text[:end], invalid), true
}

// evidenceSuffix keeps at most limit runes with work bounded by that window.
func evidenceSuffix(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	if len(text)/utf8.UTFMax <= limit && utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	start, invalid := len(text), 0
	for count := 0; count < limit && start > 0; count++ {
		width := 1
		if text[start-1] >= utf8.RuneSelf {
			_, width = utf8.DecodeLastRuneInString(text[:start])
			if width == 1 {
				invalid++
			}
		}
		start -= width
	}
	if start == 0 {
		return text, false
	}
	return copyEvidenceWindow(text[start:], invalid), true
}

func copyEvidenceWindow(text string, invalid int) string {
	if invalid == 0 {
		// Do not retain a large request through a small evidence substring.
		return strings.Clone(text)
	}
	// Match string([]rune(text)): replace each invalid byte separately. ToValidUTF8
	// would instead collapse adjacent invalid bytes into one replacement.
	var out strings.Builder
	out.Grow(len(text) + invalid*(len(string(utf8.RuneError))-1))
	for _, r := range text {
		out.WriteRune(r)
	}
	return out.String()
}
