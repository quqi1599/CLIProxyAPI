package cache

import (
	"context"
	"strings"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// AntigravityReasoningReplayCacheTTL limits how long encrypted reasoning replay
	// items stay in process memory.
	AntigravityReasoningReplayCacheTTL = 1 * time.Hour

	// AntigravityReasoningReplayCacheMaxEntries bounds process memory for replay
	// continuity. Oldest entries are evicted first.
	AntigravityReasoningReplayCacheMaxEntries = 10240

	// AntigravityReasoningReplayCacheEvictBatchSize leaves headroom after the cache
	// reaches capacity so high write volume does not rescan the map every turn.
	AntigravityReasoningReplayCacheEvictBatchSize = 128
)

type antigravityReasoningReplayKVClient = reasoningReplayKVClient

var currentAntigravityReasoningReplayKVClient = func() (antigravityReasoningReplayKVClient, bool, error) {
	return homekv.CurrentKVClient()
}

var antigravityReasoningReplayStore = newReasoningReplayStore(reasoningReplayStoreConfig{
	ttl:             AntigravityReasoningReplayCacheTTL,
	maxEntries:      AntigravityReasoningReplayCacheMaxEntries,
	evictBatchSize:  AntigravityReasoningReplayCacheEvictBatchSize,
	maxBytes:        reasoningReplayCacheMaxBytes,
	maxSessionBytes: reasoningReplayCacheMaxSessionBytes,
	maxEncodedBytes: reasoningReplayCacheMaxEncodedBytes,
	logLabel:        "antigravity reasoning replay",
	currentKVClient: func() (reasoningReplayKVClient, bool, error) {
		return currentAntigravityReasoningReplayKVClient()
	},
	normalizeItems: normalizeAntigravityReasoningReplayItems,
})

// CacheAntigravityReasoningReplayItem stores a final GPT/Codex reasoning item for
// stateless replay. The stored item is normalized to the minimal shape accepted
// by Responses input replay.
func CacheAntigravityReasoningReplayItem(modelName, sessionKey string, item []byte) bool {
	return CacheAntigravityReasoningReplayItems(modelName, sessionKey, [][]byte{item})
}

// CacheAntigravityReasoningReplayItems stores the final GPT/Codex assistant output
// items needed to replay a stateless next turn.
func CacheAntigravityReasoningReplayItems(modelName, sessionKey string, items [][]byte) bool {
	return CacheAntigravityReasoningReplayItemsBestEffort(context.Background(), modelName, sessionKey, items)
}

// CacheAntigravityReasoningReplayItemsBestEffort stores replay items for completed response paths.
func CacheAntigravityReasoningReplayItemsBestEffort(ctx context.Context, modelName, sessionKey string, items [][]byte) bool {
	return antigravityReasoningReplayStore.SaveBestEffort(ctx, antigravityReasoningReplayStoreScope(modelName, sessionKey), items)
}

// GetAntigravityReasoningReplayItem retrieves a normalized reasoning replay item.
func GetAntigravityReasoningReplayItem(modelName, sessionKey string) ([]byte, bool) {
	items, ok := GetAntigravityReasoningReplayItems(modelName, sessionKey)
	if !ok || len(items) == 0 {
		return nil, false
	}
	return items[0], true
}

// GetAntigravityReasoningReplayItems retrieves normalized assistant output items.
func GetAntigravityReasoningReplayItems(modelName, sessionKey string) ([][]byte, bool) {
	items, ok, err := GetAntigravityReasoningReplayItemsRequired(context.Background(), modelName, sessionKey)
	if err == nil {
		return items, ok
	}
	return nil, false
}

// GetAntigravityReasoningReplayItemsRequired retrieves replay items for request-time paths.
func GetAntigravityReasoningReplayItemsRequired(ctx context.Context, modelName, sessionKey string) ([][]byte, bool, error) {
	return antigravityReasoningReplayStore.LoadRequired(ctx, antigravityReasoningReplayStoreScope(modelName, sessionKey))
}

// DeleteAntigravityReasoningReplayItem removes one replay item after upstream rejects
// it or the caller otherwise knows it is stale.
func DeleteAntigravityReasoningReplayItem(modelName, sessionKey string) {
	if errDelete := DeleteAntigravityReasoningReplayItemRequired(context.Background(), modelName, sessionKey); errDelete != nil {
		return
	}
}

// DeleteAntigravityReasoningReplayItemRequired removes one replay item for request-time paths.
func DeleteAntigravityReasoningReplayItemRequired(ctx context.Context, modelName, sessionKey string) error {
	return antigravityReasoningReplayStore.DeleteRequired(ctx, antigravityReasoningReplayStoreScope(modelName, sessionKey))
}

// ClearAntigravityReasoningReplayCache clears all Antigravity reasoning replay state.
func ClearAntigravityReasoningReplayCache() {
	antigravityReasoningReplayStore.Clear()
}

func antigravityReasoningReplayCacheKey(modelName, sessionKey string) string {
	modelName = strings.TrimSpace(modelName)
	sessionKey = strings.TrimSpace(sessionKey)
	if modelName == "" || sessionKey == "" {
		return ""
	}
	// The session key is the continuity boundary. Keep this independent from
	// the selected upstream Codex credential so auth failover can preserve replay.
	return strings.Join([]string{"antigravity-reasoning-replay", modelName, sessionKey}, "\x00")
}

func antigravityReasoningReplayKVKey(modelName, sessionKey string) string {
	return "cpa:antigravity:reasoning-replay:" + homekv.HashKeyPart(strings.TrimSpace(modelName)) + ":" + homekv.HashKeyPart(strings.TrimSpace(sessionKey))
}

func antigravityReasoningReplayStoreScope(modelName, sessionKey string) reasoningReplayScope {
	localKey := antigravityReasoningReplayCacheKey(modelName, sessionKey)
	if localKey == "" {
		return reasoningReplayScope{}
	}
	return reasoningReplayScope{
		localKey: localKey,
		kvKey:    antigravityReasoningReplayKVKey(modelName, sessionKey),
	}
}

func normalizeAntigravityReasoningReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	for _, item := range items {
		normalizedItem, ok := normalizeAntigravityReasoningReplayItem(item)
		if ok {
			normalized = append(normalized, normalizedItem)
		}
	}
	return normalized, len(normalized) > 0
}

func normalizeAntigravityReasoningReplayItem(item []byte) ([]byte, bool) {
	itemResult := gjson.ParseBytes(item)
	switch strings.TrimSpace(itemResult.Get("type").String()) {
	case "thought_signature":
		return normalizeAntigravityThoughtSignatureReplayItem(itemResult)
	case "function_call_part":
		return normalizeAntigravityFunctionCallPartReplayItem(itemResult)
	default:
		return nil, false
	}
}

func normalizeAntigravityThoughtSignatureReplayItem(itemResult gjson.Result) ([]byte, bool) {
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if sig == "" {
		sig = strings.TrimSpace(itemResult.Get("thought_signature").String())
	}
	if sig == "" {
		return nil, false
	}
	normalized := []byte(`{"type":"thought_signature"}`)
	normalized, _ = sjson.SetBytes(normalized, "thoughtSignature", sig)
	if contentIndex := itemResult.Get("contentIndex"); contentIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "contentIndex", contentIndex.Int())
	}
	if partIndex := itemResult.Get("partIndex"); partIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "partIndex", partIndex.Int())
	}
	return normalized, true
}

func normalizeAntigravityFunctionCallPartReplayItem(itemResult gjson.Result) ([]byte, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	if name == "" || !args.Exists() {
		fc := itemResult.Get("functionCall")
		if fc.Exists() {
			if callID == "" {
				callID = strings.TrimSpace(fc.Get("id").String())
			}
			if name == "" {
				name = strings.TrimSpace(fc.Get("name").String())
			}
			if !args.Exists() {
				args = fc.Get("args")
			}
		}
	}
	if name == "" || !args.Exists() {
		return nil, false
	}
	normalized := []byte(`{"type":"function_call_part"}`)
	if callID != "" {
		normalized, _ = sjson.SetBytes(normalized, "call_id", callID)
	}
	normalized, _ = sjson.SetBytes(normalized, "name", name)
	if args.Type == gjson.String {
		normalized, _ = sjson.SetBytes(normalized, "args", args.String())
	} else {
		normalized, _ = sjson.SetRawBytes(normalized, "args", []byte(args.Raw))
	}
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if sig != "" {
		normalized, _ = sjson.SetBytes(normalized, "thoughtSignature", sig)
	}
	if contentIndex := itemResult.Get("contentIndex"); contentIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "contentIndex", contentIndex.Int())
	}
	if partIndex := itemResult.Get("partIndex"); partIndex.Type == gjson.Number {
		normalized, _ = sjson.SetBytes(normalized, "partIndex", partIndex.Int())
	}
	return normalized, true
}

func purgeExpiredAntigravityReasoningReplayCache(now time.Time) {
	antigravityReasoningReplayStore.PurgeExpired(now)
}
