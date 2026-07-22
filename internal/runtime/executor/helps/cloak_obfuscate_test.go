package helps

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestBuildSensitiveWordMatcherMemoized verifies that identical word lists
// return the same compiled matcher pointer, confirming memoization replaces
// per-request regexp.Compile.
func TestBuildSensitiveWordMatcherMemoized(t *testing.T) {
	words := []string{"secret", "password", "token"}

	first := BuildSensitiveWordMatcher(words)
	second := BuildSensitiveWordMatcher([]string{"secret", "password", "token"})

	if first == nil || second == nil {
		t.Fatalf("expected non-nil matchers, got first=%v second=%v", first, second)
	}
	if first != second {
		t.Fatalf("expected same matcher pointer for identical word list, got %p and %p", first, second)
	}
}

func TestObfuscateSensitiveWordsPayloadGrowth(t *testing.T) {
	matcher := BuildSensitiveWordMatcher([]string{"secret"})
	for _, size := range []int{16, 64, 256, 1024} {
		t.Run(fmt.Sprintf("items_%d", size), func(t *testing.T) {
			input := buildCloakPayload(size)
			original := bytes.Clone(input)
			out := ObfuscateSensitiveWords(input, matcher)

			if !bytes.Equal(input, original) {
				t.Fatal("input payload was mutated")
			}
			if !gjson.ValidBytes(out) {
				t.Fatal("output is not valid JSON")
			}
			if got := len(gjson.GetBytes(out, "system").Array()); got != size {
				t.Fatalf("system block count = %d, want %d", got, size)
			}
			if got := len(gjson.GetBytes(out, "messages").Array()); got != size {
				t.Fatalf("message count = %d, want %d", got, size)
			}
			if gjson.GetBytes(out, "untouched.keep").String() != "yes" {
				t.Fatal("unknown top-level fields were not preserved")
			}
			if bytes.Contains(out, []byte("secret")) {
				t.Fatal("sensitive word remained unobfuscated")
			}
		})
	}
}

func BenchmarkPayloadGrowthObfuscateSensitiveWords(b *testing.B) {
	matcher := BuildSensitiveWordMatcher([]string{"secret"})
	for _, size := range []int{16, 64, 256, 1024} {
		input := buildCloakPayload(size)
		b.Run(fmt.Sprintf("items_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				out := ObfuscateSensitiveWords(input, matcher)
				if len(out) == 0 {
					b.Fatal("empty output")
				}
			}
		})
	}
}

func buildCloakPayload(size int) []byte {
	var out strings.Builder
	out.Grow(size * 180)
	out.WriteString(`{"untouched":{"keep":"yes"},"system":[`)
	for i := 0; i < size; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, `{"type":"text","text":"system secret %d","unknown":%d}`, i, i)
	}
	out.WriteString(`],"messages":[`)
	for i := 0; i < size; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, `{"role":"user","content":[{"type":"text","text":"message secret %d","unknown":true},{"type":"image","source":"keep"}],"id":%d}`, i, i)
	}
	out.WriteString(`]}`)
	return []byte(out.String())
}

// TestBuildSensitiveWordMatcherDistinctWords verifies distinct word lists yield
// distinct matchers.
func TestBuildSensitiveWordMatcherDistinctWords(t *testing.T) {
	a := BuildSensitiveWordMatcher([]string{"alpha", "beta"})
	b := BuildSensitiveWordMatcher([]string{"gamma", "delta"})

	if a == b {
		t.Fatalf("expected distinct matchers for distinct word lists")
	}
}

// TestBuildSensitiveWordMatcherEmpty verifies the no-word and no-valid-word
// cases return nil (and remain consistent across calls).
func TestBuildSensitiveWordMatcherEmpty(t *testing.T) {
	if m := BuildSensitiveWordMatcher(nil); m != nil {
		t.Fatalf("expected nil matcher for empty word list, got %v", m)
	}
	// All words too short (<2 runes) → no valid words → nil.
	if m := BuildSensitiveWordMatcher([]string{"a", "b"}); m != nil {
		t.Fatalf("expected nil matcher when no valid words, got %v", m)
	}
}

// TestObfuscateSensitiveWordsStillWorks confirms the memoized matcher still
// obfuscates system and message content as before.
func TestObfuscateSensitiveWordsStillWorks(t *testing.T) {
	matcher := BuildSensitiveWordMatcher([]string{"secret"})
	if matcher == nil {
		t.Fatalf("expected matcher")
	}

	payload := []byte(`{"system":"a secret value","messages":[{"role":"user","content":"another secret"}]}`)
	out := ObfuscateSensitiveWords(payload, matcher)

	if !strings.Contains(string(out), zeroWidthSpace) {
		t.Fatalf("expected obfuscated output to contain zero-width space, got %s", out)
	}
	if strings.Count(string(out), zeroWidthSpace) != 2 {
		t.Fatalf("expected two obfuscations (system + message), got %d in %s", strings.Count(string(out), zeroWidthSpace), out)
	}
}
