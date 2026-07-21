package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
)

type fakeCodexReasoningReplayKVClient struct {
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

func newFakeCodexReasoningReplayKVClient() *fakeCodexReasoningReplayKVClient {
	return &fakeCodexReasoningReplayKVClient{values: make(map[string][]byte)}
}

func (c *fakeCodexReasoningReplayKVClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
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

func (c *fakeCodexReasoningReplayKVClient) KVSet(_ context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error) {
	c.setCount++
	c.lastSetTTL = opts.EX
	if c.setErr != nil {
		return false, c.setErr
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeCodexReasoningReplayKVClient) KVDel(_ context.Context, keys ...string) (int64, error) {
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

func (c *fakeCodexReasoningReplayKVClient) KVExpire(_ context.Context, _ string, ttl time.Duration) (bool, error) {
	c.expireCount++
	c.lastExpireTTL = ttl
	if c.expireErr != nil {
		return false, c.expireErr
	}
	return true, nil
}

func useFakeCodexReasoningReplayKVClient(t *testing.T, client *fakeCodexReasoningReplayKVClient, homeMode bool, errClient error) {
	t.Helper()
	previous := currentCodexReasoningReplayKVClient
	currentCodexReasoningReplayKVClient = func() (codexReasoningReplayKVClient, bool, error) {
		return client, homeMode, errClient
	}
	t.Cleanup(func() {
		currentCodexReasoningReplayKVClient = previous
	})
}

func validCodexReasoningReplayEncryptedContentForTest(seed byte) string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func validCodexReasoningReplayItemForTest(seed byte) []byte {
	return []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + validCodexReasoningReplayEncryptedContentForTest(seed) + `"}`)
}

func mustCodexReasoningReplayJSON(t *testing.T, items [][]byte) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(items)
	if errMarshal != nil {
		t.Fatalf("marshal replay items: %v", errMarshal)
	}
	return raw
}

func TestCodexReasoningReplayCacheRejectsInvalidItems(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)

	if CacheCodexReasoningReplayItem("gpt-5.4", "session", []byte(`{"type":"reasoning","encrypted_content":"bad","summary":[]}`)) {
		t.Fatal("invalid encrypted_content should not be cached")
	}
	if _, ok := GetCodexReasoningReplayItem("gpt-5.4", "session"); ok {
		t.Fatal("invalid item was cached")
	}
}

func TestCodexReasoningReplayRequiredHomeReadAndSlidingExpire(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	client := newFakeCodexReasoningReplayKVClient()
	key := codexReasoningReplayKVKey("gpt-5.4", "session-home")
	item := validCodexReasoningReplayItemForTest(3)
	client.values[key] = mustCodexReasoningReplayJSON(t, [][]byte{item})
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	items, found, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-home")
	if errGet != nil {
		t.Fatalf("GetCodexReasoningReplayItemsRequired() error = %v", errGet)
	}
	if !found || len(items) != 1 || string(items[0]) != string(item) {
		t.Fatalf("GetCodexReasoningReplayItemsRequired() = %q, %v, want item, true", items, found)
	}
	if client.expireCount != 1 || client.lastExpireTTL != CodexReasoningReplayCacheTTL {
		t.Fatalf("KVExpire count/ttl = %d/%v, want 1/%v", client.expireCount, client.lastExpireTTL, CodexReasoningReplayCacheTTL)
	}
}

func TestCodexReasoningReplayHomeNormalizesLegacyPayload(t *testing.T) {
	client := newFakeCodexReasoningReplayKVClient()
	key := codexReasoningReplayKVKey("gpt-5.4", "session-legacy")
	encryptedContent := validCodexReasoningReplayEncryptedContentForTest(4)
	legacyItem := []byte(`{"type":"reasoning","summary":[{"type":"summary_text","text":"old"}],"content":"old","encrypted_content":"` + encryptedContent + `","extra":true}`)
	client.values[key] = mustCodexReasoningReplayJSON(t, [][]byte{legacyItem})
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	items, found, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-legacy")
	if errGet != nil || !found || len(items) != 1 {
		t.Fatalf("legacy home replay = %q, %v, %v; want one normalized item", items, found, errGet)
	}
	want := `{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"}`
	if got := string(items[0]); got != want {
		t.Fatalf("normalized legacy item = %s, want %s", got, want)
	}
	if client.setCount != 1 || client.expireCount != 0 || client.lastSetTTL != CodexReasoningReplayCacheTTL {
		t.Fatalf("legacy replay set/expire/ttl = %d/%d/%v, want 1/0/%v", client.setCount, client.expireCount, client.lastSetTTL, CodexReasoningReplayCacheTTL)
	}
	if got := client.values[key]; !bytes.Equal(got, mustCodexReasoningReplayJSON(t, [][]byte{[]byte(want)})) {
		t.Fatalf("legacy replay Home value = %s, want canonical collection", got)
	}
}

func TestCodexReasoningReplayHomeDeletesInvalidCollections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items [][]byte
	}{
		{name: "empty", items: [][]byte{}},
		{name: "invalid", items: [][]byte{[]byte(`{"type":"reasoning","encrypted_content":"bad"}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeCodexReasoningReplayKVClient()
			key := codexReasoningReplayKVKey("gpt-5.4", "session-"+tc.name)
			client.values[key] = mustCodexReasoningReplayJSON(t, tc.items)
			useFakeCodexReasoningReplayKVClient(t, client, true, nil)

			items, found, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-"+tc.name)
			if errGet != nil || found || items != nil {
				t.Fatalf("invalid home replay = %q, %v, %v; want nil, false, nil", items, found, errGet)
			}
			if client.delCount != 1 || client.expireCount != 0 {
				t.Fatalf("invalid replay del/expire = %d/%d, want 1/0", client.delCount, client.expireCount)
			}
		})
	}
}

func TestCodexReasoningReplayHomeFiltersMixedCollection(t *testing.T) {
	client := newFakeCodexReasoningReplayKVClient()
	key := codexReasoningReplayKVKey("gpt-5.4", "session-mixed")
	validItem := validCodexReasoningReplayItemForTest(5)
	client.values[key] = mustCodexReasoningReplayJSON(t, [][]byte{
		validItem,
		[]byte(`{"type":"message","content":"ignored"}`),
	})
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	items, found, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-mixed")
	if errGet != nil || !found || len(items) != 1 || string(items[0]) != string(validItem) {
		t.Fatalf("mixed home replay = %q, %v, %v; want valid subset", items, found, errGet)
	}
	if client.delCount != 0 || client.setCount != 1 || client.expireCount != 0 || client.lastSetTTL != CodexReasoningReplayCacheTTL {
		t.Fatalf("mixed replay del/set/expire/ttl = %d/%d/%d/%v, want 0/1/0/%v", client.delCount, client.setCount, client.expireCount, client.lastSetTTL, CodexReasoningReplayCacheTTL)
	}
	if got := client.values[key]; !bytes.Equal(got, mustCodexReasoningReplayJSON(t, [][]byte{validItem})) {
		t.Fatalf("mixed replay Home value = %s, want canonical valid subset", got)
	}
}

func TestCodexReasoningReplayHomeDeletesMalformedPayload(t *testing.T) {
	client := newFakeCodexReasoningReplayKVClient()
	key := codexReasoningReplayKVKey("gpt-5.4", "session-malformed")
	client.values[key] = []byte(`{"broken"`)
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	if _, found, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-malformed"); errGet == nil || found {
		t.Fatalf("malformed home replay = found %v err %v, want error and miss", found, errGet)
	}
	if client.delCount != 1 || client.expireCount != 0 {
		t.Fatalf("malformed replay del/expire = %d/%d, want 1/0", client.delCount, client.expireCount)
	}
}

func TestCodexReasoningReplayHomeRejectsOversizedEncodedPayloadBeforeDecode(t *testing.T) {
	client := newFakeCodexReasoningReplayKVClient()
	key := codexReasoningReplayKVKey("gpt-5.4", "session-oversized-encoded")
	client.values[key] = bytes.Repeat([]byte{'x'}, int(reasoningReplayCacheMaxEncodedBytes)+1)
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	items, found, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-oversized-encoded")
	if errGet != nil || found || items != nil {
		t.Fatalf("oversized encoded replay = %q, %v, %v; want nil, false, nil", items, found, errGet)
	}
	if client.delCount != 1 || client.expireCount != 0 {
		t.Fatalf("oversized replay del/expire = %d/%d, want 1/0", client.delCount, client.expireCount)
	}
}

func TestCodexReasoningReplayHomeNilClientDoesNotPanic(t *testing.T) {
	useFakeCodexReasoningReplayKVClient(t, nil, true, nil)
	item := validCodexReasoningReplayItemForTest(6)
	if CacheCodexReasoningReplayItemsBestEffort(context.Background(), "gpt-5.4", "session-nil", [][]byte{item}) {
		t.Fatal("save with nil home client unexpectedly succeeded")
	}
	if _, _, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-nil"); !errors.Is(errGet, errReasoningReplayKVUnavailable) {
		t.Fatalf("load with nil home client error = %v", errGet)
	}
	if errDelete := DeleteCodexReasoningReplayItemRequired(context.Background(), "gpt-5.4", "session-nil"); !errors.Is(errDelete, errReasoningReplayKVUnavailable) {
		t.Fatalf("delete with nil home client error = %v", errDelete)
	}
}

func TestCodexReasoningReplayRequiredHomeFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *fakeCodexReasoningReplayKVClient
	}{
		{name: "get", client: &fakeCodexReasoningReplayKVClient{values: make(map[string][]byte), getErr: errors.New("get failed")}},
		{name: "expire", client: &fakeCodexReasoningReplayKVClient{values: map[string][]byte{
			codexReasoningReplayKVKey("gpt-5.4", "session-home"): mustCodexReasoningReplayJSON(t, [][]byte{validCodexReasoningReplayItemForTest(4)}),
		}, expireErr: errors.New("expire failed")}},
		{name: "delete", client: &fakeCodexReasoningReplayKVClient{values: make(map[string][]byte), delErr: errors.New("delete failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakeCodexReasoningReplayKVClient(t, tc.client, true, nil)
			switch tc.name {
			case "delete":
				if errDel := DeleteCodexReasoningReplayItemRequired(context.Background(), "gpt-5.4", "session-home"); errDel == nil {
					t.Fatalf("DeleteCodexReasoningReplayItemRequired() error = nil, want error")
				}
			default:
				if _, _, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "gpt-5.4", "session-home"); errGet == nil {
					t.Fatalf("GetCodexReasoningReplayItemsRequired() error = nil, want error")
				}
			}
		})
	}
}

func TestCodexReasoningReplayBestEffortHomeWriteFailureDoesNotUseLocalCache(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)
	client := newFakeCodexReasoningReplayKVClient()
	client.setErr = errors.New("set failed")
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	if CacheCodexReasoningReplayItemsBestEffort(context.Background(), "gpt-5.4", "session-home", [][]byte{validCodexReasoningReplayItemForTest(5)}) {
		t.Fatalf("CacheCodexReasoningReplayItemsBestEffort() = true, want false")
	}
	useFakeCodexReasoningReplayKVClient(t, newFakeCodexReasoningReplayKVClient(), false, nil)
	if _, found := GetCodexReasoningReplayItems("gpt-5.4", "session-home"); found {
		t.Fatalf("local replay cache was populated after Home best-effort write failure")
	}
}

func TestCodexReasoningReplayHomeRejectsEmptyScopeWithoutKV(t *testing.T) {
	client := newFakeCodexReasoningReplayKVClient()
	useFakeCodexReasoningReplayKVClient(t, client, true, nil)

	if _, found, errGet := GetCodexReasoningReplayItemsRequired(context.Background(), "", "session-home"); errGet != nil || found {
		t.Fatalf("GetCodexReasoningReplayItemsRequired(empty model) = found %v err %v, want false nil", found, errGet)
	}
	if CacheCodexReasoningReplayItemsBestEffort(context.Background(), "gpt-5.4", "", [][]byte{validCodexReasoningReplayItemForTest(6)}) {
		t.Fatalf("CacheCodexReasoningReplayItemsBestEffort(empty session) = true, want false")
	}
	if errDel := DeleteCodexReasoningReplayItemRequired(context.Background(), "gpt-5.4", ""); errDel != nil {
		t.Fatalf("DeleteCodexReasoningReplayItemRequired(empty session) error = %v", errDel)
	}
	if client.getCount != 0 || client.setCount != 0 || client.delCount != 0 || client.expireCount != 0 {
		t.Fatalf("KV calls = get %d set %d del %d expire %d, want all zero", client.getCount, client.setCount, client.delCount, client.expireCount)
	}
}

func TestCodexReasoningReplayCacheScopesByModelAndSession(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReasoningReplayEncryptedContentForTest(7)
	if !CacheCodexReasoningReplayItem("gpt-5.4", "session-a", []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+encryptedContent+`"}`)) {
		t.Fatal("valid item was not cached")
	}

	if _, ok := GetCodexReasoningReplayItem("gpt-5.5", "session-a"); ok {
		t.Fatal("cache should not hit across models")
	}
	if _, ok := GetCodexReasoningReplayItem("gpt-5.4", "session-b"); ok {
		t.Fatal("cache should not hit across sessions")
	}

	item, ok := GetCodexReasoningReplayItem("gpt-5.4", "session-a")
	if !ok {
		t.Fatal("cache miss for original model and session")
	}
	if string(item) != `{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+encryptedContent+`"}` {
		t.Fatalf("normalized item = %s", string(item))
	}
}

func TestCodexReasoningReplayCacheBatchEvictsWhenFull(t *testing.T) {
	ClearCodexReasoningReplayCache()
	t.Cleanup(ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReasoningReplayEncryptedContentForTest(9)
	item := []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"}`)
	for i := 0; i <= CodexReasoningReplayCacheMaxEntries; i++ {
		if !CacheCodexReasoningReplayItem("gpt-5.4", fmt.Sprintf("session-%d", i), item) {
			t.Fatalf("cache insert %d failed", i)
		}
	}

	gotLen, _ := codexReasoningReplayStore.stats()
	if gotLen >= CodexReasoningReplayCacheMaxEntries {
		t.Fatalf("cache entries = %d, want batch eviction below max %d", gotLen, CodexReasoningReplayCacheMaxEntries)
	}
}
