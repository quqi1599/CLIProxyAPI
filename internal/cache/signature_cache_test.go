package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	log "github.com/sirupsen/logrus"
)

const testModelName = "claude-sonnet-4-5"

type fakeSignatureKVClient struct {
	values        map[string][]byte
	getErr        error
	setErr        error
	delErr        error
	expireErr     error
	getCount      int
	setCount      int
	delCount      int
	expireCount   int
	lastSetTTL    time.Duration
	lastExpireTTL time.Duration
}

func newFakeSignatureKVClient() *fakeSignatureKVClient {
	return &fakeSignatureKVClient{values: make(map[string][]byte)}
}

func (c *fakeSignatureKVClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.getCount++
	if c.getErr != nil {
		return nil, false, c.getErr
	}
	value, ok := c.values[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), value...), true, nil
}

func (c *fakeSignatureKVClient) KVSet(_ context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error) {
	c.setCount++
	c.lastSetTTL = opts.EX
	if c.setErr != nil {
		return false, c.setErr
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeSignatureKVClient) KVDel(_ context.Context, keys ...string) (int64, error) {
	c.delCount++
	if c.delErr != nil {
		return 0, c.delErr
	}
	var deleted int64
	for _, key := range keys {
		if _, ok := c.values[key]; ok {
			delete(c.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *fakeSignatureKVClient) KVExpire(_ context.Context, _ string, ttl time.Duration) (bool, error) {
	c.expireCount++
	c.lastExpireTTL = ttl
	if c.expireErr != nil {
		return false, c.expireErr
	}
	return true, nil
}

func useFakeSignatureKVClient(t *testing.T, client *fakeSignatureKVClient, homeMode bool, errClient error) {
	t.Helper()
	previous := currentSignatureKVClient
	currentSignatureKVClient = func() (signatureKVClient, bool, error) {
		return client, homeMode, errClient
	}
	t.Cleanup(func() {
		currentSignatureKVClient = previous
	})
}

func useTestSignatureLocalCache(t *testing.T, maxEntries int, maxBytes int64, now func() time.Time) {
	t.Helper()
	previousCache := signatureLocalCache
	previousNow := signatureCacheNow
	previousClient := currentSignatureKVClient
	signatureLocalCache = newBoundedLRU[signatureLocalKey, string](SignatureCacheTTL, maxEntries, 1, maxBytes)
	signatureCacheNow = now
	currentSignatureKVClient = func() (signatureKVClient, bool, error) {
		return nil, false, nil
	}
	t.Cleanup(func() {
		signatureLocalCache = previousCache
		signatureCacheNow = previousNow
		currentSignatureKVClient = previousClient
	})
}

func TestCacheSignature_BasicStorageAndRetrieval(t *testing.T) {
	ClearSignatureCache("")

	text := "This is some thinking text content"
	signature := "abc123validSignature1234567890123456789012345678901234567890"

	// Store signature
	CacheSignature(testModelName, text, signature)

	// Retrieve signature
	retrieved := GetCachedSignature(testModelName, text)
	if retrieved != signature {
		t.Errorf("Expected signature '%s', got '%s'", signature, retrieved)
	}
}

func TestGetCachedSignatureRequiredHomeReadAndSlidingExpire(t *testing.T) {
	ClearSignatureCache("")
	text := "thinking text"
	signature := "abc123validSignature1234567890123456789012345678901234567890"
	client := newFakeSignatureKVClient()
	client.values[signatureKVKey(testModelName, text)] = []byte(signature)
	useFakeSignatureKVClient(t, client, true, nil)

	got, errGet := GetCachedSignatureRequired(context.Background(), testModelName, text)
	if errGet != nil {
		t.Fatalf("GetCachedSignatureRequired() error = %v", errGet)
	}
	if got != signature {
		t.Fatalf("GetCachedSignatureRequired() = %q, want %q", got, signature)
	}
	if client.expireCount != 1 || client.lastExpireTTL != SignatureCacheTTL {
		t.Fatalf("KVExpire count/ttl = %d/%v, want 1/%v", client.expireCount, client.lastExpireTTL, SignatureCacheTTL)
	}
}

func TestCacheSignatureBestEffortHomeUsesRawValueAndTTL(t *testing.T) {
	text := "thinking text"
	signature := strings.Repeat("s", MinValidSignatureLen)
	client := newFakeSignatureKVClient()
	useFakeSignatureKVClient(t, client, true, nil)

	if !CacheSignatureBestEffort(context.Background(), testModelName, text, signature) {
		t.Fatal("CacheSignatureBestEffort() = false, want true")
	}
	if got := client.values[signatureKVKey(testModelName, text)]; !bytes.Equal(got, []byte(signature)) {
		t.Fatalf("Home value = %q, want raw signature %q", got, signature)
	}
	if client.setCount != 1 || client.lastSetTTL != SignatureCacheTTL {
		t.Fatalf("KVSet count/ttl = %d/%v, want 1/%v", client.setCount, client.lastSetTTL, SignatureCacheTTL)
	}
}

func TestCacheSignatureBestEffortRejectsOversizedValueBeforeHomeWrite(t *testing.T) {
	client := newFakeSignatureKVClient()
	useFakeSignatureKVClient(t, client, true, nil)

	if CacheSignatureBestEffort(context.Background(), testModelName, "thinking text", strings.Repeat("s", signatureCacheMaxValueBytes+1)) {
		t.Fatal("oversized signature write unexpectedly succeeded")
	}
	if client.setCount != 0 {
		t.Fatalf("oversized signature KVSet count = %d, want 0", client.setCount)
	}
}

func TestSignatureHomeAcceptsValueAtExistingValidationLimit(t *testing.T) {
	client := newFakeSignatureKVClient()
	useFakeSignatureKVClient(t, client, true, nil)
	value := strings.Repeat("s", signatureCacheMaxValueBytes)

	if !CacheSignatureBestEffort(context.Background(), testModelName, "thinking text", value) {
		t.Fatal("exact-limit signature write failed")
	}
	got, errGet := GetCachedSignatureRequired(context.Background(), testModelName, "thinking text")
	if errGet != nil || got != value {
		t.Fatalf("exact-limit Home signature length/error = %d/%v, want %d/nil", len(got), errGet, len(value))
	}
	if client.setCount != 1 || client.expireCount != 1 {
		t.Fatalf("exact-limit Home signature set/expire = %d/%d, want 1/1", client.setCount, client.expireCount)
	}
}

func TestGetCachedSignatureRequiredDeletesOutOfBoundsHomeValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		modelName string
		value     []byte
		want      string
	}{
		{name: "short claude", modelName: testModelName, value: []byte("short")},
		{name: "oversized claude", modelName: testModelName, value: bytes.Repeat([]byte{'s'}, signatureCacheMaxValueBytes+1)},
		{name: "invalid gemini", modelName: "gemini-3-pro-preview", value: []byte("short"), want: "skip_thought_signature_validator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeSignatureKVClient()
			key := signatureKVKey(tc.modelName, "thinking text")
			client.values[key] = tc.value
			useFakeSignatureKVClient(t, client, true, nil)

			got, errGet := GetCachedSignatureRequired(context.Background(), tc.modelName, "thinking text")
			if errGet != nil || got != tc.want {
				t.Fatalf("invalid Home signature = %q, %v; want %q, nil", got, errGet, tc.want)
			}
			if client.delCount != 1 || client.expireCount != 0 {
				t.Fatalf("invalid Home signature del/expire = %d/%d, want 1/0", client.delCount, client.expireCount)
			}
		})
	}
}

func TestGetCachedSignatureRequiredPreservesGeminiSentinelInHome(t *testing.T) {
	const modelName = "gemini-3-pro-preview"
	const sentinel = "skip_thought_signature_validator"
	client := newFakeSignatureKVClient()
	client.values[signatureKVKey(modelName, "thinking text")] = []byte(sentinel)
	useFakeSignatureKVClient(t, client, true, nil)

	got, errGet := GetCachedSignatureRequired(context.Background(), modelName, "thinking text")
	if errGet != nil || got != sentinel {
		t.Fatalf("Gemini Home sentinel = %q, %v; want %q, nil", got, errGet, sentinel)
	}
	if client.delCount != 0 || client.expireCount != 1 {
		t.Fatalf("Gemini Home sentinel del/expire = %d/%d, want 0/1", client.delCount, client.expireCount)
	}
}

func TestSignatureHomeTypedNilClientDoesNotPanic(t *testing.T) {
	useFakeSignatureKVClient(t, nil, true, nil)
	text := "thinking text"
	signature := strings.Repeat("s", MinValidSignatureLen)

	if CacheSignatureBestEffort(context.Background(), testModelName, text, signature) {
		t.Fatal("CacheSignatureBestEffort() = true with typed-nil Home client")
	}
	if _, errGet := GetCachedSignatureRequired(context.Background(), testModelName, text); !errors.Is(errGet, errSignatureKVUnavailable) {
		t.Fatalf("GetCachedSignatureRequired() error = %v, want %v", errGet, errSignatureKVUnavailable)
	}
	if errDelete := DeleteCachedSignatureRequired(context.Background(), testModelName, text); !errors.Is(errDelete, errSignatureKVUnavailable) {
		t.Fatalf("DeleteCachedSignatureRequired() error = %v, want %v", errDelete, errSignatureKVUnavailable)
	}
}

func TestGetCachedSignatureRequiredHomeFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *fakeSignatureKVClient
	}{
		{name: "get", client: &fakeSignatureKVClient{values: make(map[string][]byte), getErr: errors.New("get failed")}},
		{name: "expire", client: &fakeSignatureKVClient{values: map[string][]byte{
			signatureKVKey(testModelName, "thinking text"): []byte("abc123validSignature1234567890123456789012345678901234567890"),
		}, expireErr: errors.New("expire failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakeSignatureKVClient(t, tc.client, true, nil)
			if _, errGet := GetCachedSignatureRequired(context.Background(), testModelName, "thinking text"); errGet == nil {
				t.Fatalf("GetCachedSignatureRequired() error = nil, want error")
			}
		})
	}
}

func TestGetCachedSignatureRequiredHomeMissDoesNotFallbackToLocalCache(t *testing.T) {
	ClearSignatureCache("")
	text := "thinking text"
	signature := "abc123validSignature1234567890123456789012345678901234567890"
	CacheSignature(testModelName, text, signature)

	client := newFakeSignatureKVClient()
	useFakeSignatureKVClient(t, client, true, nil)

	got, errGet := GetCachedSignatureRequired(context.Background(), testModelName, text)
	if errGet != nil {
		t.Fatalf("GetCachedSignatureRequired() error = %v", errGet)
	}
	if got != "" {
		t.Fatalf("GetCachedSignatureRequired() = %q, want Home miss without local fallback", got)
	}
}

func TestCacheSignatureBestEffortHomeWriteFailureDoesNotUseLocalCache(t *testing.T) {
	ClearSignatureCache("")
	text := "thinking text"
	signature := "abc123validSignature1234567890123456789012345678901234567890"
	client := newFakeSignatureKVClient()
	client.setErr = errors.New("set failed")
	useFakeSignatureKVClient(t, client, true, nil)

	if CacheSignatureBestEffort(context.Background(), testModelName, text, signature) {
		t.Fatalf("CacheSignatureBestEffort() = true, want false")
	}
	useFakeSignatureKVClient(t, newFakeSignatureKVClient(), false, nil)
	if got := GetCachedSignature(testModelName, text); got != "" {
		t.Fatalf("local cache = %q, want empty after Home write failure", got)
	}
}

func TestDeleteCachedSignatureRequiredHomeExactKey(t *testing.T) {
	ClearSignatureCache("")
	text := "thinking text"
	signature := "abc123validSignature1234567890123456789012345678901234567890"
	client := newFakeSignatureKVClient()
	client.values[signatureKVKey(testModelName, text)] = []byte(signature)
	useFakeSignatureKVClient(t, client, true, nil)

	if errDel := DeleteCachedSignatureRequired(context.Background(), testModelName, text); errDel != nil {
		t.Fatalf("DeleteCachedSignatureRequired() error = %v", errDel)
	}
	if _, ok := client.values[signatureKVKey(testModelName, text)]; ok {
		t.Fatalf("signature key was not deleted")
	}
	if client.delCount != 1 {
		t.Fatalf("KVDel count = %d, want 1", client.delCount)
	}
}

func TestClearSignatureCacheHomeDoesNotPrefixDelete(t *testing.T) {
	client := newFakeSignatureKVClient()
	useFakeSignatureKVClient(t, client, true, nil)

	ClearSignatureCache("")
	ClearSignatureCache(testModelName)

	if client.delCount != 0 {
		t.Fatalf("ClearSignatureCache() KVDel count = %d, want 0", client.delCount)
	}
}

func TestGetCachedSignatureRequiredGeminiEmptyThinkingSentinel(t *testing.T) {
	client := newFakeSignatureKVClient()
	client.getErr = errors.New("get should not be called")
	useFakeSignatureKVClient(t, client, true, nil)

	got, errGet := GetCachedSignatureRequired(context.Background(), "gemini-3-pro-preview", "")
	if errGet != nil {
		t.Fatalf("GetCachedSignatureRequired() error = %v", errGet)
	}
	if got != "skip_thought_signature_validator" {
		t.Fatalf("GetCachedSignatureRequired() = %q, want Gemini sentinel", got)
	}
	if client.getCount != 0 {
		t.Fatalf("KVGet count = %d, want 0", client.getCount)
	}
}

func TestGetCachedSignatureRequiredGeminiNonEmptyMissAndExpirationSentinel(t *testing.T) {
	now := time.Unix(100, 0)
	useTestSignatureLocalCache(t, 10, 1<<20, func() time.Time { return now })
	modelName := "gemini-3-pro-preview"
	const sentinel = "skip_thought_signature_validator"

	if got := GetCachedSignature(modelName, "missing"); got != sentinel {
		t.Fatalf("non-empty Gemini miss = %q, want sentinel", got)
	}
	CacheSignature(modelName, "expired", strings.Repeat("s", MinValidSignatureLen))
	now = now.Add(SignatureCacheTTL + time.Nanosecond)
	if got := GetCachedSignature(modelName, "expired"); got != sentinel {
		t.Fatalf("expired Gemini signature = %q, want sentinel", got)
	}
}

func TestCacheSignature_DifferentModelGroups(t *testing.T) {
	ClearSignatureCache("")

	text := "Same text across models"
	sig1 := "signature1_1234567890123456789012345678901234567890123456"
	sig2 := "signature2_1234567890123456789012345678901234567890123456"

	geminiModel := "gemini-3-pro-preview"
	CacheSignature(testModelName, text, sig1)
	CacheSignature(geminiModel, text, sig2)

	if GetCachedSignature(testModelName, text) != sig1 {
		t.Error("Claude signature mismatch")
	}
	if GetCachedSignature(geminiModel, text) != sig2 {
		t.Error("Gemini signature mismatch")
	}
}

func TestCacheSignature_NotFound(t *testing.T) {
	ClearSignatureCache("")

	// Non-existent session
	if got := GetCachedSignature(testModelName, "some text"); got != "" {
		t.Errorf("Expected empty string for nonexistent session, got '%s'", got)
	}

	// Existing session but different text
	CacheSignature(testModelName, "text-a", "sigA12345678901234567890123456789012345678901234567890")
	if got := GetCachedSignature(testModelName, "text-b"); got != "" {
		t.Errorf("Expected empty string for different text, got '%s'", got)
	}
}

func TestCacheSignature_EmptyInputs(t *testing.T) {
	ClearSignatureCache("")

	// All empty/invalid inputs should be no-ops
	CacheSignature(testModelName, "", "sig12345678901234567890123456789012345678901234567890")
	CacheSignature(testModelName, "text", "")
	CacheSignature(testModelName, "text", "short") // Too short

	if got := GetCachedSignature(testModelName, "text"); got != "" {
		t.Errorf("Expected empty after invalid cache attempts, got '%s'", got)
	}
}

func TestCacheSignature_ShortSignatureRejected(t *testing.T) {
	ClearSignatureCache("")

	text := "Some text"
	shortSig := "abc123" // Less than 50 chars

	CacheSignature(testModelName, text, shortSig)

	if got := GetCachedSignature(testModelName, text); got != "" {
		t.Errorf("Short signature should be rejected, got '%s'", got)
	}
}

func TestClearSignatureCache_ModelGroup(t *testing.T) {
	ClearSignatureCache("")

	sig := "validSig1234567890123456789012345678901234567890123456"
	CacheSignature(testModelName, "text", sig)
	CacheSignature(testModelName, "text-2", sig)

	ClearSignatureCache("session-1")

	if got := GetCachedSignature(testModelName, "text"); got != sig {
		t.Error("signature should remain when clearing unknown session")
	}
}

func TestClearSignatureCache_AllSessions(t *testing.T) {
	ClearSignatureCache("")

	sig := "validSig1234567890123456789012345678901234567890123456"
	CacheSignature(testModelName, "text", sig)
	CacheSignature(testModelName, "text-2", sig)

	ClearSignatureCache("")

	if got := GetCachedSignature(testModelName, "text"); got != "" {
		t.Error("text should be cleared")
	}
	if got := GetCachedSignature(testModelName, "text-2"); got != "" {
		t.Error("text-2 should be cleared")
	}
}

func TestHasValidSignature(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		signature string
		expected  bool
	}{
		{"valid long signature", testModelName, "abc123validSignature1234567890123456789012345678901234567890", true},
		{"exactly 50 chars", testModelName, "12345678901234567890123456789012345678901234567890", true},
		{"49 chars - invalid", testModelName, "1234567890123456789012345678901234567890123456789", false},
		{"empty string", testModelName, "", false},
		{"short signature", testModelName, "abc", false},
		{"gemini sentinel", "gemini-3-pro-preview", "skip_thought_signature_validator", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HasValidSignature(tt.modelName, tt.signature)
			if result != tt.expected {
				t.Errorf("HasValidSignature(%q) = %v, expected %v", tt.signature, result, tt.expected)
			}
		})
	}
}

func TestCacheSignature_TextHashCollisionResistance(t *testing.T) {
	ClearSignatureCache("")

	// Different texts should produce different hashes
	text1 := "First thinking text"
	text2 := "Second thinking text"
	sig1 := "signature1_1234567890123456789012345678901234567890123456"
	sig2 := "signature2_1234567890123456789012345678901234567890123456"

	CacheSignature(testModelName, text1, sig1)
	CacheSignature(testModelName, text2, sig2)

	if GetCachedSignature(testModelName, text1) != sig1 {
		t.Error("text1 signature mismatch")
	}
	if GetCachedSignature(testModelName, text2) != sig2 {
		t.Error("text2 signature mismatch")
	}
}

func TestCacheSignature_UnicodeText(t *testing.T) {
	ClearSignatureCache("")

	text := "한글 텍스트와 이모지 🎉 그리고 特殊文字"
	sig := "unicodeSig123456789012345678901234567890123456789012345"

	CacheSignature(testModelName, text, sig)

	if got := GetCachedSignature(testModelName, text); got != sig {
		t.Errorf("Unicode text signature retrieval failed, got '%s'", got)
	}
}

func TestCacheSignature_Overwrite(t *testing.T) {
	ClearSignatureCache("")

	text := "Same text"
	sig1 := "firstSignature12345678901234567890123456789012345678901"
	sig2 := "secondSignature1234567890123456789012345678901234567890"

	CacheSignature(testModelName, text, sig1)
	CacheSignature(testModelName, text, sig2) // Overwrite

	if got := GetCachedSignature(testModelName, text); got != sig2 {
		t.Errorf("Expected overwritten signature '%s', got '%s'", sig2, got)
	}
}

func TestCacheSignature_ExpirationLogic(t *testing.T) {
	now := time.Unix(100, 0)
	useTestSignatureLocalCache(t, 10, 1<<20, func() time.Time { return now })
	text := "text"
	sig := strings.Repeat("s", MinValidSignatureLen)

	CacheSignature(testModelName, text, sig)
	now = now.Add(SignatureCacheTTL)
	if got := GetCachedSignature(testModelName, text); got != sig {
		t.Fatalf("signature at exact TTL = %q, want hit", got)
	}
	now = now.Add(SignatureCacheTTL)
	if got := GetCachedSignature(testModelName, text); got != sig {
		t.Fatalf("signature at exact sliding TTL = %q, want hit", got)
	}
	now = now.Add(SignatureCacheTTL + time.Nanosecond)
	if got := GetCachedSignature(testModelName, text); got != "" {
		t.Fatalf("expired signature = %q, want miss", got)
	}
	if entries, bytes := signatureLocalCache.Stats(); entries != 0 || bytes != 0 {
		t.Fatalf("stats after expiration = %d/%d, want 0/0", entries, bytes)
	}
}

func TestSignatureLocalCacheEvictsLeastRecentlyUsedByEntryLimit(t *testing.T) {
	now := time.Unix(100, 0)
	useTestSignatureLocalCache(t, 2, 1<<20, func() time.Time { return now })
	signature := strings.Repeat("s", MinValidSignatureLen)

	CacheSignature(testModelName, "a", signature)
	now = now.Add(time.Second)
	CacheSignature(testModelName, "b", signature)
	now = now.Add(time.Second)
	if got := GetCachedSignature(testModelName, "a"); got != signature {
		t.Fatalf("touch a = %q, want hit", got)
	}
	now = now.Add(time.Second)
	CacheSignature(testModelName, "c", signature)

	if got := GetCachedSignature(testModelName, "b"); got != "" {
		t.Fatalf("least recently used b = %q, want eviction", got)
	}
	for _, text := range []string{"a", "c"} {
		if got := GetCachedSignature(testModelName, text); got != signature {
			t.Fatalf("signature %s = %q, want hit", text, got)
		}
	}
	if entries, _ := signatureLocalCache.Stats(); entries != 2 {
		t.Fatalf("entries after eviction = %d, want 2", entries)
	}
}

func TestSignatureLocalCacheByteBudgetAndAccounting(t *testing.T) {
	now := time.Unix(100, 0)
	signature := strings.Repeat("s", MinValidSignatureLen)
	key := newSignatureLocalKey(testModelName, "a")
	entrySize := signatureLocalEntrySize(key, signature)
	useTestSignatureLocalCache(t, 10, entrySize*2, func() time.Time { return now })

	CacheSignature(testModelName, "a", signature)
	now = now.Add(time.Second)
	CacheSignature(testModelName, "b", signature)
	now = now.Add(time.Second)
	if got := GetCachedSignature(testModelName, "a"); got != signature {
		t.Fatalf("touch a = %q, want hit", got)
	}
	now = now.Add(time.Second)
	CacheSignature(testModelName, "c", signature)

	if got := GetCachedSignature(testModelName, "b"); got != "" {
		t.Fatalf("byte-budget LRU b = %q, want eviction", got)
	}
	if entries, bytes := signatureLocalCache.Stats(); entries != 2 || bytes != entrySize*2 {
		t.Fatalf("stats after byte eviction = %d/%d, want 2/%d", entries, bytes, entrySize*2)
	}
	if errDelete := DeleteCachedSignatureRequired(context.Background(), testModelName, "c"); errDelete != nil {
		t.Fatalf("delete c: %v", errDelete)
	}

	overwrite := strings.Repeat("x", MinValidSignatureLen+10)
	if !CacheSignatureBestEffort(context.Background(), testModelName, "a", overwrite) {
		t.Fatal("overwrite failed")
	}
	overwriteSize := signatureLocalEntrySize(key, overwrite)
	if entries, bytes := signatureLocalCache.Stats(); entries != 1 || bytes != overwriteSize {
		t.Fatalf("stats after overwrite = %d/%d, want 1/%d", entries, bytes, overwriteSize)
	}
	if errDelete := DeleteCachedSignatureRequired(context.Background(), testModelName, "a"); errDelete != nil {
		t.Fatalf("delete: %v", errDelete)
	}
	if entries, bytes := signatureLocalCache.Stats(); entries != 0 || bytes != 0 {
		t.Fatalf("stats after delete = %d/%d, want 0/0", entries, bytes)
	}

	CacheSignature(testModelName, "a", signature)
	now = now.Add(SignatureCacheTTL + time.Nanosecond)
	signatureLocalCache.PurgeExpired(now)
	if entries, bytes := signatureLocalCache.Stats(); entries != 0 || bytes != 0 {
		t.Fatalf("stats after purge = %d/%d, want 0/0", entries, bytes)
	}
}

func TestSignatureLocalCacheRejectsOversizedOverwriteWithoutDataLoss(t *testing.T) {
	now := time.Unix(100, 0)
	signature := strings.Repeat("s", MinValidSignatureLen)
	key := newSignatureLocalKey(testModelName, "a")
	useTestSignatureLocalCache(t, 10, signatureLocalEntrySize(key, signature), func() time.Time { return now })

	if !CacheSignatureBestEffort(context.Background(), testModelName, "a", signature) {
		t.Fatal("exact-budget save failed")
	}
	if CacheSignatureBestEffort(context.Background(), testModelName, "a", signature+"x") {
		t.Fatal("oversized overwrite unexpectedly succeeded")
	}
	if got := GetCachedSignature(testModelName, "a"); got != signature {
		t.Fatalf("signature after rejected overwrite = %q, want original", got)
	}
}

func TestSignatureLocalCacheClearGroupAndAllAccounting(t *testing.T) {
	now := time.Unix(100, 0)
	useTestSignatureLocalCache(t, 10, 1<<20, func() time.Time { return now })
	signature := strings.Repeat("s", MinValidSignatureLen)

	CacheSignature(testModelName, "a", signature)
	CacheSignature(testModelName, "b", signature)
	CacheSignature("gemini-3-pro-preview", "c", signature)
	ClearSignatureCache("claude-opus")

	if got := GetCachedSignature(testModelName, "a"); got != "" {
		t.Fatalf("cleared Claude signature = %q, want miss", got)
	}
	if got := GetCachedSignature("gemini-3-pro-preview", "c"); got != signature {
		t.Fatalf("Gemini signature after Claude clear = %q, want hit", got)
	}
	geminiKey := newSignatureLocalKey("gemini-3-pro-preview", "c")
	if entries, bytes := signatureLocalCache.Stats(); entries != 1 || bytes != signatureLocalEntrySize(geminiKey, signature) {
		t.Fatalf("stats after group clear = %d/%d, want 1/%d", entries, bytes, signatureLocalEntrySize(geminiKey, signature))
	}

	ClearSignatureCache("")
	if entries, bytes := signatureLocalCache.Stats(); entries != 0 || bytes != 0 {
		t.Fatalf("stats after clear all = %d/%d, want 0/0", entries, bytes)
	}
}

func TestSignatureLocalCacheOwnsStoredAndReturnedStrings(t *testing.T) {
	now := time.Unix(100, 0)
	useTestSignatureLocalCache(t, 10, 1<<20, func() time.Time { return now })
	inputBytes := []byte(strings.Repeat("s", MinValidSignatureLen))
	input := unsafe.String(unsafe.SliceData(inputBytes), len(inputBytes))

	CacheSignature(testModelName, "a", input)
	inputBytes[0] = 'x'
	first := GetCachedSignature(testModelName, "a")
	second := GetCachedSignature(testModelName, "a")
	if first != strings.Repeat("s", MinValidSignatureLen) || second != first {
		t.Fatalf("stored signature changed with caller backing storage: first=%q second=%q", first, second)
	}
	if unsafe.StringData(input) == unsafe.StringData(first) || unsafe.StringData(first) == unsafe.StringData(second) {
		t.Fatal("stored or returned signature reused caller-visible backing storage")
	}
}

func TestSignatureLocalCacheConcurrentAccess(t *testing.T) {
	useTestSignatureLocalCache(t, 64, 4096, time.Now)
	signature := strings.Repeat("s", MinValidSignatureLen)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				text := fmt.Sprintf("%d-%d", worker, iteration%16)
				CacheSignature(testModelName, text, signature)
				_ = GetCachedSignature(testModelName, text)
				if iteration%3 == 0 {
					_ = DeleteCachedSignatureRequired(context.Background(), testModelName, text)
				}
				if iteration%17 == 0 {
					ClearSignatureCache(testModelName)
				}
			}
		}(worker)
	}
	workers.Wait()
	if entries, bytes := signatureLocalCache.Stats(); entries > 64 || bytes > 4096 {
		t.Fatalf("stats exceed limits after concurrent access = %d/%d", entries, bytes)
	}
}

func TestSignatureModeSetters_LogAtInfoLevel(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput := logger.Out
	previousLevel := logger.Level
	previousCache := SignatureCacheEnabled()
	previousStrict := SignatureBypassStrictMode()
	SetSignatureCacheEnabled(true)
	SetSignatureBypassStrictMode(false)
	buffer := &bytes.Buffer{}
	log.SetOutput(buffer)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetLevel(previousLevel)
		SetSignatureCacheEnabled(previousCache)
		SetSignatureBypassStrictMode(previousStrict)
	})

	SetSignatureCacheEnabled(false)
	SetSignatureBypassStrictMode(true)
	SetSignatureBypassStrictMode(false)

	output := buffer.String()
	if !strings.Contains(output, "antigravity signature cache DISABLED") {
		t.Fatalf("expected info output for disabling signature cache, got: %q", output)
	}
	if strings.Contains(output, "strict mode (protobuf tree)") {
		t.Fatalf("expected strict bypass mode log to stay below info level, got: %q", output)
	}
	if strings.Contains(output, "basic mode (R/E + 0x12)") {
		t.Fatalf("expected basic bypass mode log to stay below info level, got: %q", output)
	}
}

func TestSignatureModeSetters_DoNotRepeatSameStateLogs(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput := logger.Out
	previousLevel := logger.Level
	previousCache := SignatureCacheEnabled()
	previousStrict := SignatureBypassStrictMode()
	SetSignatureCacheEnabled(false)
	SetSignatureBypassStrictMode(true)
	buffer := &bytes.Buffer{}
	log.SetOutput(buffer)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetLevel(previousLevel)
		SetSignatureCacheEnabled(previousCache)
		SetSignatureBypassStrictMode(previousStrict)
	})

	SetSignatureCacheEnabled(false)
	SetSignatureBypassStrictMode(true)

	if buffer.Len() != 0 {
		t.Fatalf("expected repeated setter calls with unchanged state to stay silent, got: %q", buffer.String())
	}
}

func TestSignatureBypassStrictMode_LogsAtDebugLevel(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput := logger.Out
	previousLevel := logger.Level
	previousStrict := SignatureBypassStrictMode()
	SetSignatureBypassStrictMode(false)
	buffer := &bytes.Buffer{}
	log.SetOutput(buffer)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetLevel(previousLevel)
		SetSignatureBypassStrictMode(previousStrict)
	})

	SetSignatureBypassStrictMode(true)
	SetSignatureBypassStrictMode(false)

	output := buffer.String()
	if !strings.Contains(output, "strict mode (protobuf tree)") {
		t.Fatalf("expected debug output for strict bypass mode, got: %q", output)
	}
	if !strings.Contains(output, "basic mode (R/E + 0x12)") {
		t.Fatalf("expected debug output for basic bypass mode, got: %q", output)
	}
}
