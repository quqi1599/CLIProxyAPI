package signature

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func newSignatureDebugHook(t *testing.T) *test.Hook {
	t.Helper()

	previousLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previousLevel)
	})
	return hook
}

func assertSignatureDebugDoesNotLeak(t *testing.T, hook *test.Hook, forbidden string) {
	t.Helper()

	if forbidden == "" {
		return
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, forbidden) {
			t.Fatalf("debug log leaked signature in message: %q", entry.Message)
		}
		for key, value := range entry.Data {
			if strings.Contains(fmt.Sprint(value), forbidden) {
				t.Fatalf("debug log leaked signature in field %q: %v", key, value)
			}
		}
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesPreservesGeminiSignature(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want %q. Output: %s", got, sig, string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesReplacesBase64UUIDFunctionCall(t *testing.T) {
	sig := testGeminiThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"thoughtSignature":"` + sig + `"}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "contents.0.parts.0.functionCall.thoughtSignature").Exists() {
		t.Fatalf("nested functionCall thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLogsBypassReplacement(t *testing.T) {
	hook := newSignatureDebugHook(t)
	sig := testGeminiThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"thoughtSignature":"` + sig + `"}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")
	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, string(out))
	}

	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.DebugLevel {
			continue
		}
		if entry.Data["component"] != "signature_sanitizer" ||
			entry.Data["target_provider"] != string(SignatureProviderGemini) ||
			entry.Data["action"] != "replace_with_gemini_bypass" {
			continue
		}
		if entry.Data["block_kind"] != string(SignatureBlockKindGeminiFunctionCall) {
			t.Fatalf("block_kind = %v, want %s", entry.Data["block_kind"], SignatureBlockKindGeminiFunctionCall)
		}
		found = true
	}
	if !found {
		t.Fatal("expected debug log for Gemini thoughtSignature bypass replacement")
	}
	assertSignatureDebugDoesNotLeak(t, hook, sig)
}

func TestSanitizeGeminiRequestThoughtSignaturesReplacesField2WrappedUUIDFunctionCall(t *testing.T) {
	sig := testGemini3ThoughtSignature([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + sig + `"}]}]}}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "request.contents")

	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesFunctionResponseSignature(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"},"thoughtSignature":"bad"},"thoughtSignature":"bad"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse top-level thoughtSignature should be removed. Output: %s", string(out))
	}
	if gjson.GetBytes(out, "contents.0.parts.0.functionResponse.thoughtSignature").Exists() {
		t.Fatalf("functionResponse nested thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLargeHistoryIsLinearAndImmutable(t *testing.T) {
	input := largeGeminiSignatureHistory(1024)
	original := bytes.Clone(input)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if !bytes.Equal(input, original) {
		t.Fatal("sanitizer mutated caller input")
	}
	parts := gjson.GetBytes(out, "contents.0.parts").Array()
	if len(parts) != 1024 {
		t.Fatalf("parts = %d, want 1024", len(parts))
	}
	for _, index := range []int{0, len(parts) - 1} {
		part := parts[index]
		if got := part.Get("thoughtSignature").String(); got != GeminiSkipThoughtSignatureValidator {
			t.Fatalf("parts.%d thoughtSignature = %q", index, got)
		}
		if part.Get("functionCall.thought_signature").Exists() {
			t.Fatalf("parts.%d retained nested thought signature", index)
		}
		if strings.Index(part.Raw, `"before"`) > strings.Index(part.Raw, `"functionCall"`) || strings.Index(part.Raw, `"functionCall"`) > strings.Index(part.Raw, `"after"`) {
			t.Fatalf("parts.%d field order changed: %s", index, part.Raw)
		}
	}
	if got := gjson.GetBytes(out, "tail.keep").Bool(); !got {
		t.Fatal("unknown top-level field was not preserved")
	}
}

func BenchmarkSanitizeGeminiRequestThoughtSignaturesLargeHistory(b *testing.B) {
	input := largeGeminiSignatureHistory(1024)
	b.ReportAllocs()
	for b.Loop() {
		if out := SanitizeGeminiRequestThoughtSignatures(input, "contents"); len(out) == 0 {
			b.Fatal("empty output")
		}
	}
}

func largeGeminiSignatureHistory(parts int) []byte {
	var builder strings.Builder
	builder.Grow(parts * 128)
	builder.WriteString(`{"contents":[{"role":"model","parts":[`)
	for index := 0; index < parts; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"before":1,"functionCall":{"name":"f","args":{},"thought_signature":"bad"},"after":2}`)
	}
	builder.WriteString(`]}],"tail":{"keep":true}}`)
	return []byte(builder.String())
}
