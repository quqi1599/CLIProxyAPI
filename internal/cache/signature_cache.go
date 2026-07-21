package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	signaturevalidation "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	log "github.com/sirupsen/logrus"
)

const (
	// SignatureCacheTTL is how long signatures are valid
	SignatureCacheTTL = 3 * time.Hour

	// SignatureTextHashLen is the length of the hash key (16 hex chars = 64-bit key space)
	SignatureTextHashLen = 16

	// MinValidSignatureLen is the minimum length for a signature to be considered valid
	MinValidSignatureLen = 50

	// CacheCleanupInterval controls how often stale entries are purged
	CacheCleanupInterval = 10 * time.Minute

	// Local limits bound retained signatures independently of periodic cleanup.
	signatureCacheMaxEntries           = 10240
	signatureCacheEvictBatchSize       = 1
	signatureCacheMaxBytes       int64 = 32 << 20
	signatureCacheMaxValueBytes        = signaturevalidation.MaxClaudeThinkingSignatureLen
)

var errSignatureKVUnavailable = errors.New("signature home kv client is unavailable")

type signatureLocalKey struct {
	group    string
	textHash string
}

var (
	signatureLocalCache = newBoundedLRU[signatureLocalKey, string](
		SignatureCacheTTL,
		signatureCacheMaxEntries,
		signatureCacheEvictBatchSize,
		signatureCacheMaxBytes,
	)
	signatureCacheNow = time.Now
)

// cacheCleanupOnce ensures the background cleanup goroutine starts only once
var cacheCleanupOnce sync.Once

type signatureKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error)
	KVDel(ctx context.Context, keys ...string) (int64, error)
	KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

var currentSignatureKVClient = func() (signatureKVClient, bool, error) {
	return homekv.CurrentKVClient()
}

// hashText creates a stable, Unicode-safe key from text content
func hashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])[:SignatureTextHashLen]
}

func newSignatureLocalKey(modelName, text string) signatureLocalKey {
	return signatureLocalKey{
		group:    GetModelGroup(modelName),
		textHash: hashText(text),
	}
}

func signatureLocalEntrySize(key signatureLocalKey, signature string) int64 {
	return int64(len(key.group) + 1 + len(key.textHash) + len(signature))
}

func signatureCacheMiss(group string) string {
	if group == "gemini" {
		return "skip_thought_signature_validator"
	}
	return ""
}

// startCacheCleanup launches a background goroutine that periodically
// removes caches where all entries have expired.
func startCacheCleanup() {
	go func() {
		ticker := time.NewTicker(CacheCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredCaches()
		}
	}()
}

// purgeExpiredCaches removes expired entries from local caches.
func purgeExpiredCaches() {
	now := time.Now()
	signatureLocalCache.PurgeExpired(now)
	purgeExpiredCodexReasoningReplayCache(now)
	purgeExpiredAntigravityReasoningReplayCache(now)
}

// CacheSignature stores a thinking signature for a given model group and text.
// Used for Claude models that require signed thinking blocks in multi-turn conversations.
func CacheSignature(modelName, text, signature string) {
	CacheSignatureBestEffort(context.Background(), modelName, text, signature)
}

// CacheSignatureBestEffort stores a thinking signature for completed response paths.
func CacheSignatureBestEffort(ctx context.Context, modelName, text, signature string) bool {
	if text == "" || signature == "" {
		return false
	}
	if len(signature) < MinValidSignatureLen || len(signature) > signatureCacheMaxValueBytes {
		return false
	}

	if client, homeMode, errClient := currentSignatureKVClient(); homeMode {
		if errClient != nil {
			log.Errorf("home kv best-effort signature set failed prefix=cpa:signature:*: %v", errClient)
			return false
		}
		if isNilInterface(client) {
			log.Errorf("home kv best-effort signature set failed prefix=cpa:signature:*: %v", errSignatureKVUnavailable)
			return false
		}
		written, errSet := client.KVSet(ctx, signatureKVKey(modelName, text), []byte(signature), homekv.KVSetOptions{EX: SignatureCacheTTL})
		if errSet != nil {
			log.Errorf("home kv best-effort signature set failed prefix=cpa:signature:*: %v", errSet)
			return false
		}
		return written
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	key := newSignatureLocalKey(modelName, text)
	entrySize := signatureLocalEntrySize(key, signature)
	if !signatureLocalCache.CanStore(entrySize) {
		return false
	}
	return signatureLocalCache.Set(key, strings.Clone(signature), entrySize, signatureCacheNow())
}

// GetCachedSignature retrieves a cached signature for a given model group and text.
// Returns empty string if not found or expired.
func GetCachedSignature(modelName, text string) string {
	signature, errSignature := GetCachedSignatureRequired(context.Background(), modelName, text)
	if errSignature != nil {
		return ""
	}
	return signature
}

// GetCachedSignatureRequired retrieves a cached signature for request-time paths.
func GetCachedSignatureRequired(ctx context.Context, modelName, text string) (string, error) {
	groupKey := GetModelGroup(modelName)

	if text == "" {
		return signatureCacheMiss(groupKey), nil
	}

	if client, homeMode, errClient := currentSignatureKVClient(); homeMode {
		if errClient != nil {
			return "", errClient
		}
		if isNilInterface(client) {
			return "", errSignatureKVUnavailable
		}
		key := signatureKVKey(modelName, text)
		raw, found, errGet := client.KVGet(ctx, key)
		if errGet != nil {
			return "", errGet
		}
		if !found {
			return signatureCacheMiss(groupKey), nil
		}
		var signature string
		valid := len(raw) <= signatureCacheMaxValueBytes
		if valid {
			signature = string(raw)
			valid = HasValidSignature(modelName, signature)
		}
		if !valid {
			if _, errDelete := client.KVDel(ctx, key); errDelete != nil {
				return "", errDelete
			}
			return signatureCacheMiss(groupKey), nil
		}
		if _, errExpire := client.KVExpire(ctx, key, SignatureCacheTTL); errExpire != nil {
			return "", errExpire
		}
		return signature, nil
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	signature, ok := signatureLocalCache.Get(newSignatureLocalKey(modelName, text), signatureCacheNow())
	if !ok {
		return signatureCacheMiss(groupKey), nil
	}
	return strings.Clone(signature), nil
}

// ClearSignatureCache clears signature cache for a specific model group or all groups.
func ClearSignatureCache(modelName string) {
	if modelName == "" {
		signatureLocalCache.Clear()
		return
	}
	groupKey := GetModelGroup(modelName)
	signatureLocalCache.DeleteIf(func(key signatureLocalKey) bool {
		return key.group == groupKey
	})
}

// DeleteCachedSignatureRequired removes one exact cached signature.
func DeleteCachedSignatureRequired(ctx context.Context, modelName, text string) error {
	if text == "" {
		return nil
	}
	if client, homeMode, errClient := currentSignatureKVClient(); homeMode {
		if errClient != nil {
			return errClient
		}
		if isNilInterface(client) {
			return errSignatureKVUnavailable
		}
		_, errDel := client.KVDel(ctx, signatureKVKey(modelName, text))
		return errDel
	}
	signatureLocalCache.Delete(newSignatureLocalKey(modelName, text))
	return nil
}

// HasValidSignature checks if a signature is valid (non-empty and long enough)
func HasValidSignature(modelName, signature string) bool {
	return (signature != "" && len(signature) >= MinValidSignatureLen) || (signature == "skip_thought_signature_validator" && GetModelGroup(modelName) == "gemini")
}

func GetModelGroup(modelName string) string {
	if strings.Contains(modelName, "gpt") {
		return "gpt"
	} else if strings.Contains(modelName, "claude") {
		return "claude"
	} else if strings.Contains(modelName, "gemini") {
		return "gemini"
	}
	return modelName
}

func signatureKVKey(modelName, text string) string {
	return fmt.Sprintf("cpa:signature:%s:%s", GetModelGroup(modelName), homekv.HashKeyPart(text))
}

var signatureCacheEnabled atomic.Bool
var signatureBypassStrictMode atomic.Bool

func init() {
	signatureCacheEnabled.Store(true)
	signatureBypassStrictMode.Store(false)
}

// SetSignatureCacheEnabled switches Antigravity signature handling between cache mode and bypass mode.
func SetSignatureCacheEnabled(enabled bool) {
	previous := signatureCacheEnabled.Swap(enabled)
	if previous == enabled {
		return
	}
	if !enabled {
		log.Info("antigravity signature cache DISABLED - bypass mode active, cached signatures will not be used for request translation")
	}
}

// SignatureCacheEnabled returns whether signature cache validation is enabled.
func SignatureCacheEnabled() bool {
	return signatureCacheEnabled.Load()
}

// SetSignatureBypassStrictMode controls whether bypass mode uses strict protobuf-tree validation.
func SetSignatureBypassStrictMode(strict bool) {
	previous := signatureBypassStrictMode.Swap(strict)
	if previous == strict {
		return
	}
	if strict {
		log.Debug("antigravity bypass signature validation: strict mode (protobuf tree)")
	} else {
		log.Debug("antigravity bypass signature validation: basic mode (R/E + 0x12)")
	}
}

// SignatureBypassStrictMode returns whether bypass mode uses strict protobuf-tree validation.
func SignatureBypassStrictMode() bool {
	return signatureBypassStrictMode.Load()
}
