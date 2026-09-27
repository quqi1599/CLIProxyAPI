package contentaudit

import (
	"encoding/json"
	"math/rand"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtractLargeEvidenceBoundaries(t *testing.T) {
	prior := "reference-head" + strings.Repeat("🙂", 5000) + "reference-tail"
	current := "current-head" + strings.Repeat("中", maxEvidenceStringRunes) + "current-tail continue previous"
	wantCurrent, _ := legacyEvidenceWindow(current, maxEvidenceStringRunes, true)
	wantDisplay, _ := legacyEvidenceWindow(current, maxEvidenceStringRunes, false)
	wantReference, _ := legacyEvidenceWindow(prior, 4096, true)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			key := "messages"
			if path == "/v1/responses" {
				key = "input"
			}
			body, err := json.Marshal(map[string]any{"model": "fixture", "stream": true, key: []any{
				map[string]any{"role": "user", "content": prior},
				map[string]any{"role": "user", "content": current},
			}})
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractJSONRequestForPath(body, path)
			if got.CurrentUserText != wantCurrent || got.EnforcementText != wantCurrent || got.Text != prior+"\n"+wantDisplay || got.ReferenceText != "user:\n"+wantReference {
				t.Fatalf("extraction changed boundaries: current=%v enforcement=%v display=%v reference=%v; reference bytes=%d expected=%d", got.CurrentUserText == wantCurrent, got.EnforcementText == wantCurrent, got.Text == prior+"\n"+wantDisplay, got.ReferenceText == "user:\n"+wantReference, len(got.ReferenceText), len("user:\n"+wantReference))
			}
			if !got.CurrentTruncated || !got.ContextIncomplete || got.Model != "fixture" || !got.Stream {
				t.Fatal("extraction lost truncation or request metadata")
			}
			var evidence struct {
				Text      string `json:"extracted_text"`
				Truncated bool   `json:"current_truncated"`
			}
			if err := json.Unmarshal(got.Evidence, &evidence); err != nil || evidence.Text != got.Text || !evidence.Truncated {
				t.Fatalf("inconsistent stored evidence: %v", err)
			}
		})
	}
}

func legacyEvidenceWindow(text string, limit int, suffix bool) (string, bool) {
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	if suffix {
		return string(runes[len(runes)-limit:]), true
	}
	return string(runes[:limit]), true
}

func checkEvidenceWindows(t testing.TB, text string, limit int) {
	t.Helper()
	for _, suffix := range []bool{false, true} {
		want, wantTruncated := legacyEvidenceWindow(text, limit, suffix)
		got, truncated := evidencePrefix(text, limit)
		if suffix {
			got, truncated = evidenceSuffix(text, limit)
		}
		if got != want || truncated != wantTruncated {
			t.Fatalf("suffix=%v limit=%d input=%q: got %q/%v, want %q/%v", suffix, limit, text, got, truncated, want, wantTruncated)
		}
	}
}

func TestEvidenceWindowsMatchLegacy(t *testing.T) {
	for _, text := range []string{"", "ascii", "你好🙂e\u0301", "\xff\xfeA\xc0\xaf中\xe2\x82", "a�b", "\xc2\xc2\xa2"} {
		for limit := 0; limit <= len(text)+1; limit++ {
			checkEvidenceWindows(t, text, limit)
		}
	}
	rng := rand.New(rand.NewSource(73))
	for i := 0; i < 2000; i++ {
		data := make([]byte, rng.Intn(256))
		_, _ = rng.Read(data)
		checkEvidenceWindows(t, string(data), rng.Intn(260))
	}
	for _, text := range []string{strings.Repeat("x", maxEvidenceStringRunes+1), strings.Repeat("中🙂", maxEvidenceStringRunes)} {
		checkEvidenceWindows(t, text, maxEvidenceStringRunes)
	}
}

func FuzzEvidenceWindowsMatchLegacy(f *testing.F) {
	for _, text := range []string{"", "ascii", "你好🙂", "\xff\xfea\xc2\xc2\xa2"} {
		f.Add(text, uint16(3))
	}
	f.Fuzz(func(t *testing.T, text string, limit uint16) {
		checkEvidenceWindows(t, text, int(limit)%1024)
	})
}

func legacyIsURLOrData(value string) bool {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		return true
	}
	parsed, err := url.Parse(trimmed)
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func TestURLPrefilterMatchesLegacy(t *testing.T) {
	values := []string{
		"", "ordinary text", "text containing https://example.test", "https://example.test/a?x=y#z",
		"\u2003HtTpS://user:pass@example.test:443/a\u2003", "custom+1.2-3://host/path", "http://[::1]:80/",
		"http://[fe80::1%25en0]/", "http://例子.测试/a", "//example.test", "https:opaque", "mailto:a@b",
		"http:///path", "http:////path", "http://", "://host", "1http://host", "h_ttp://host", "h🚀://host",
		"https://host/bad%zz", "https://[::1", "http://host\n", "http://ho\x00st", "http://\xff",
		"DATA:anything", " data:\xff\x00 ", "data without colon", "http://user%40name@host", "file://host/path",
	}
	for _, value := range values {
		if got, want := isURLOrData(value), legacyIsURLOrData(value); got != want {
			t.Fatalf("isURLOrData(%q)=%v, want %v", value, got, want)
		}
	}
	rng := rand.New(rand.NewSource(91))
	for i := 0; i < 2000; i++ {
		data := make([]byte, rng.Intn(256))
		_, _ = rng.Read(data)
		for _, value := range []string{string(data), "https://" + string(data), string(data) + "://host", "dAtA:" + string(data)} {
			if got, want := isURLOrData(value), legacyIsURLOrData(value); got != want {
				t.Fatalf("isURLOrData(%q)=%v, want %v", value, got, want)
			}
		}
	}
}

func FuzzURLPrefilterMatchesLegacy(f *testing.F) {
	for _, text := range []string{"https://host/a", "DATA:text/plain,abc", "//host", "custom+v1://host", "ordinary text: next", "\xff://host"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if got, want := isURLOrData(text), legacyIsURLOrData(text); got != want {
			t.Fatalf("isURLOrData(%q)=%v, want %v", text, got, want)
		}
	})
}
