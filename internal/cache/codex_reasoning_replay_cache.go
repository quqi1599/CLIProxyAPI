package cache

import (
	"context"
	"strings"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// CodexReasoningReplayCacheTTL limits how long encrypted reasoning replay
	// items stay in process memory.
	CodexReasoningReplayCacheTTL = 1 * time.Hour

	// CodexReasoningReplayCacheMaxEntries bounds process memory for replay
	// continuity. Oldest entries are evicted first.
	CodexReasoningReplayCacheMaxEntries = 10240

	// CodexReasoningReplayCacheEvictBatchSize leaves headroom after the cache
	// reaches capacity so high write volume does not rescan the map every turn.
	CodexReasoningReplayCacheEvictBatchSize = 128
)

type codexReasoningReplayKVClient = reasoningReplayKVClient

var currentCodexReasoningReplayKVClient = func() (codexReasoningReplayKVClient, bool, error) {
	return homekv.CurrentKVClient()
}

var codexReasoningReplayStore = newReasoningReplayStore(reasoningReplayStoreConfig{
	ttl:             CodexReasoningReplayCacheTTL,
	maxEntries:      CodexReasoningReplayCacheMaxEntries,
	evictBatchSize:  CodexReasoningReplayCacheEvictBatchSize,
	maxBytes:        reasoningReplayCacheMaxBytes,
	maxSessionBytes: reasoningReplayCacheMaxSessionBytes,
	maxEncodedBytes: reasoningReplayCacheMaxEncodedBytes,
	logLabel:        "codex reasoning replay",
	currentKVClient: func() (reasoningReplayKVClient, bool, error) {
		return currentCodexReasoningReplayKVClient()
	},
	normalizeItems: normalizeCodexReasoningReplayItems,
})

// CacheCodexReasoningReplayItem stores a final GPT/Codex reasoning item for
// stateless replay. The stored item is normalized to the minimal shape accepted
// by Responses input replay.
func CacheCodexReasoningReplayItem(modelName, sessionKey string, item []byte) bool {
	return CacheCodexReasoningReplayItems(modelName, sessionKey, [][]byte{item})
}

// CacheCodexReasoningReplayItems stores the final GPT/Codex assistant output
// items needed to replay a stateless next turn.
func CacheCodexReasoningReplayItems(modelName, sessionKey string, items [][]byte) bool {
	return CacheCodexReasoningReplayItemsBestEffort(context.Background(), modelName, sessionKey, items)
}

// CacheCodexReasoningReplayItemsBestEffort stores replay items for completed response paths.
func CacheCodexReasoningReplayItemsBestEffort(ctx context.Context, modelName, sessionKey string, items [][]byte) bool {
	return codexReasoningReplayStore.SaveBestEffort(ctx, codexReasoningReplayStoreScope(modelName, sessionKey), items)
}

// GetCodexReasoningReplayItem retrieves a normalized reasoning replay item.
func GetCodexReasoningReplayItem(modelName, sessionKey string) ([]byte, bool) {
	items, ok := GetCodexReasoningReplayItems(modelName, sessionKey)
	if !ok || len(items) == 0 {
		return nil, false
	}
	return items[0], true
}

// GetCodexReasoningReplayItems retrieves normalized assistant output items.
func GetCodexReasoningReplayItems(modelName, sessionKey string) ([][]byte, bool) {
	items, ok, err := GetCodexReasoningReplayItemsRequired(context.Background(), modelName, sessionKey)
	if err == nil {
		return items, ok
	}
	return nil, false
}

// GetCodexReasoningReplayItemsRequired retrieves replay items for request-time paths.
func GetCodexReasoningReplayItemsRequired(ctx context.Context, modelName, sessionKey string) ([][]byte, bool, error) {
	return codexReasoningReplayStore.LoadRequired(ctx, codexReasoningReplayStoreScope(modelName, sessionKey))
}

// DeleteCodexReasoningReplayItem removes one replay item after upstream rejects
// it or the caller otherwise knows it is stale.
func DeleteCodexReasoningReplayItem(modelName, sessionKey string) {
	if errDelete := DeleteCodexReasoningReplayItemRequired(context.Background(), modelName, sessionKey); errDelete != nil {
		return
	}
}

// DeleteCodexReasoningReplayItemRequired removes one replay item for request-time paths.
func DeleteCodexReasoningReplayItemRequired(ctx context.Context, modelName, sessionKey string) error {
	return codexReasoningReplayStore.DeleteRequired(ctx, codexReasoningReplayStoreScope(modelName, sessionKey))
}

// ClearCodexReasoningReplayCache clears all Codex reasoning replay state.
func ClearCodexReasoningReplayCache() {
	codexReasoningReplayStore.Clear()
}

func codexReasoningReplayCacheKey(modelName, sessionKey string) string {
	modelName = strings.TrimSpace(modelName)
	sessionKey = strings.TrimSpace(sessionKey)
	if modelName == "" || sessionKey == "" {
		return ""
	}
	// The session key is the continuity boundary. Keep this independent from
	// the selected upstream Codex credential so auth failover can preserve replay.
	return strings.Join([]string{"codex-reasoning-replay", modelName, sessionKey}, "\x00")
}

func codexReasoningReplayKVKey(modelName, sessionKey string) string {
	return "cpa:codex:reasoning-replay:" + homekv.HashKeyPart(strings.TrimSpace(modelName)) + ":" + homekv.HashKeyPart(strings.TrimSpace(sessionKey))
}

func codexReasoningReplayStoreScope(modelName, sessionKey string) reasoningReplayScope {
	localKey := codexReasoningReplayCacheKey(modelName, sessionKey)
	if localKey == "" {
		return reasoningReplayScope{}
	}
	return reasoningReplayScope{
		localKey: localKey,
		kvKey:    codexReasoningReplayKVKey(modelName, sessionKey),
	}
}

func normalizeCodexReasoningReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	for _, item := range items {
		normalizedItem, ok := normalizeCodexReasoningReplayItem(item)
		if ok {
			normalized = append(normalized, normalizedItem)
		}
	}
	return normalized, len(normalized) > 0
}

func normalizeCodexReasoningReplayItem(item []byte) ([]byte, bool) {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case "reasoning":
		return normalizeCodexReasoningReplayReasoningItem(itemResult)
	case "function_call":
		return normalizeCodexReasoningReplayFunctionCallItem(itemResult)
	case "custom_tool_call":
		return normalizeCodexReasoningReplayCustomToolCallItem(itemResult)
	default:
		return nil, false
	}
}

func normalizeCodexReasoningReplayReasoningItem(itemResult gjson.Result) ([]byte, bool) {
	encryptedContentResult := itemResult.Get("encrypted_content")
	if encryptedContentResult.Type != gjson.String {
		return nil, false
	}
	encryptedContent := encryptedContentResult.String()
	if encryptedContent != strings.TrimSpace(encryptedContent) {
		return nil, false
	}
	if _, err := signature.InspectGPTReasoningSignature(encryptedContent); err != nil {
		return nil, false
	}

	normalized := []byte(`{"type":"reasoning","summary":[],"content":null}`)
	normalized, _ = sjson.SetBytes(normalized, "encrypted_content", encryptedContent)
	return normalized, true
}

func normalizeCodexReasoningReplayFunctionCallItem(itemResult gjson.Result) ([]byte, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	arguments := itemResult.Get("arguments")
	if callID == "" || name == "" || arguments.Type != gjson.String {
		return nil, false
	}

	normalized := []byte(`{"type":"function_call"}`)
	normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
	normalized, _ = sjson.SetBytes(normalized, "name", name)
	normalized, _ = sjson.SetBytes(normalized, "arguments", arguments.String())
	return normalized, true
}

func normalizeCodexReasoningReplayCustomToolCallItem(itemResult gjson.Result) ([]byte, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	input := itemResult.Get("input")
	if callID == "" || name == "" || !input.Exists() {
		return nil, false
	}

	normalized := []byte(`{"type":"custom_tool_call","status":"completed"}`)
	if status := strings.TrimSpace(itemResult.Get("status").String()); status != "" {
		normalized, _ = sjson.SetBytes(normalized, "status", status)
	}
	normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
	normalized, _ = sjson.SetBytes(normalized, "name", name)
	if input.Type == gjson.String {
		normalized, _ = sjson.SetBytes(normalized, "input", input.String())
	} else {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte(input.Raw))
	}
	return normalized, true
}

func purgeExpiredCodexReasoningReplayCache(now time.Time) {
	codexReasoningReplayStore.PurgeExpired(now)
}
