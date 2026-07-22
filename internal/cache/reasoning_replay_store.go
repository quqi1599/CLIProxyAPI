package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	log "github.com/sirupsen/logrus"
)

const (
	// Local totals include canonical item bytes and the scope key. Per-session
	// limits apply before and after normalization and are shared with Home KV.
	reasoningReplayCacheMaxBytes        int64 = 64 << 20
	reasoningReplayCacheMaxSessionBytes int64 = 1 << 20
	reasoningReplayCacheMaxEncodedBytes int64 = 2 << 20
	reasoningReplayCacheMaxItems              = 16384
)

var (
	errReasoningReplayKVUnavailable = errors.New("reasoning replay home kv client is unavailable")
	errReasoningReplayKVWriteFailed = errors.New("reasoning replay home kv write was not accepted")
)

type reasoningReplayKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error)
	KVDel(ctx context.Context, keys ...string) (int64, error)
	KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

type reasoningReplayScope struct {
	localKey string
	kvKey    string
}

func (s reasoningReplayScope) valid() bool {
	return s.localKey != "" && s.kvKey != ""
}

type reasoningReplayStoreConfig struct {
	ttl             time.Duration
	maxEntries      int
	evictBatchSize  int
	maxBytes        int64
	maxSessionBytes int64
	maxEncodedBytes int64
	logLabel        string
	currentKVClient func() (reasoningReplayKVClient, bool, error)
	normalizeItems  func([][]byte) ([][]byte, bool)
	now             func() time.Time
}

type reasoningReplayStore struct {
	config reasoningReplayStoreConfig
	local  *boundedLRU[string, [][]byte]
}

func newReasoningReplayStore(config reasoningReplayStoreConfig) *reasoningReplayStore {
	if config.now == nil {
		config.now = time.Now
	}
	return &reasoningReplayStore{
		config: config,
		local:  newBoundedLRU[string, [][]byte](config.ttl, config.maxEntries, config.evictBatchSize, config.maxBytes),
	}
}

func (s *reasoningReplayStore) SaveBestEffort(ctx context.Context, scope reasoningReplayScope, items [][]byte) bool {
	if s == nil || !scope.valid() || s.config.normalizeItems == nil {
		return false
	}
	if _, ok := s.itemsSizeWithinLimits(items); !ok {
		return false
	}
	normalized, ok := s.config.normalizeItems(items)
	if !ok {
		return false
	}
	payloadBytes, ok := s.itemsSizeWithinLimits(normalized)
	if !ok {
		return false
	}

	client, homeMode, errClient := s.currentKVClient()
	if homeMode {
		if errClient != nil {
			s.logBestEffortSaveError(errClient)
			return false
		}
		if isNilInterface(client) {
			s.logBestEffortSaveError(errReasoningReplayKVUnavailable)
			return false
		}
		raw, errMarshal := json.Marshal(normalized)
		if errMarshal != nil {
			s.logBestEffortSaveError(errMarshal)
			return false
		}
		written, errSet := client.KVSet(ctx, scope.kvKey, raw, homekv.KVSetOptions{EX: s.config.ttl})
		if errSet != nil {
			s.logBestEffortSaveError(errSet)
			return false
		}
		return written
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	entrySize := payloadBytes + int64(len(scope.localKey))
	return s.local.Set(scope.localKey, cloneReasoningReplayItems(normalized), entrySize, s.config.now())
}

func (s *reasoningReplayStore) LoadRequired(ctx context.Context, scope reasoningReplayScope) ([][]byte, bool, error) {
	if s == nil || !scope.valid() || s.config.normalizeItems == nil {
		return nil, false, nil
	}
	client, homeMode, errClient := s.currentKVClient()
	if homeMode {
		if errClient != nil {
			return nil, false, errClient
		}
		if isNilInterface(client) {
			return nil, false, errReasoningReplayKVUnavailable
		}
		raw, found, errGet := client.KVGet(ctx, scope.kvKey)
		if errGet != nil || !found {
			return nil, false, errGet
		}
		if s.config.maxEncodedBytes > 0 && int64(len(raw)) > s.config.maxEncodedBytes {
			return s.deleteInvalidKVEntry(ctx, client, scope.kvKey, nil)
		}
		var stored [][]byte
		if errUnmarshal := json.Unmarshal(raw, &stored); errUnmarshal != nil {
			return s.deleteInvalidKVEntry(ctx, client, scope.kvKey, errUnmarshal)
		}
		if _, ok := s.itemsSizeWithinLimits(stored); !ok {
			return s.deleteInvalidKVEntry(ctx, client, scope.kvKey, nil)
		}
		normalized, ok := s.config.normalizeItems(stored)
		if !ok {
			return s.deleteInvalidKVEntry(ctx, client, scope.kvKey, nil)
		}
		if _, ok = s.itemsSizeWithinLimits(normalized); !ok {
			return s.deleteInvalidKVEntry(ctx, client, scope.kvKey, nil)
		}
		if !slices.EqualFunc(stored, normalized, bytes.Equal) {
			canonical, errMarshal := json.Marshal(normalized)
			if errMarshal != nil {
				return nil, false, errMarshal
			}
			written, errSet := client.KVSet(ctx, scope.kvKey, canonical, homekv.KVSetOptions{EX: s.config.ttl})
			if errSet != nil {
				return nil, false, errSet
			}
			if !written {
				return nil, false, errReasoningReplayKVWriteFailed
			}
		} else if _, errExpire := client.KVExpire(ctx, scope.kvKey, s.config.ttl); errExpire != nil {
			return nil, false, errExpire
		}
		return cloneReasoningReplayItems(normalized), true, nil
	}

	cacheCleanupOnce.Do(startCacheCleanup)
	items, ok := s.local.Get(scope.localKey, s.config.now())
	if !ok {
		return nil, false, nil
	}
	return cloneReasoningReplayItems(items), true, nil
}

func (s *reasoningReplayStore) DeleteRequired(ctx context.Context, scope reasoningReplayScope) error {
	if s == nil || !scope.valid() {
		return nil
	}
	client, homeMode, errClient := s.currentKVClient()
	if homeMode {
		if errClient != nil {
			return errClient
		}
		if isNilInterface(client) {
			return errReasoningReplayKVUnavailable
		}
		_, errDelete := client.KVDel(ctx, scope.kvKey)
		return errDelete
	}
	s.local.Delete(scope.localKey)
	return nil
}

func (s *reasoningReplayStore) Clear() {
	if s == nil {
		return
	}
	s.local.Clear()
}

func (s *reasoningReplayStore) PurgeExpired(now time.Time) {
	if s == nil {
		return
	}
	s.local.PurgeExpired(now)
}

func (s *reasoningReplayStore) stats() (int, int64) {
	if s == nil {
		return 0, 0
	}
	return s.local.Stats()
}

func (s *reasoningReplayStore) currentKVClient() (reasoningReplayKVClient, bool, error) {
	if s.config.currentKVClient == nil {
		return nil, false, nil
	}
	return s.config.currentKVClient()
}

func (s *reasoningReplayStore) itemsSizeWithinLimits(items [][]byte) (int64, bool) {
	if len(items) == 0 || len(items) > reasoningReplayCacheMaxItems {
		return 0, false
	}
	var total int64
	for _, item := range items {
		itemSize := int64(len(item))
		if s.config.maxSessionBytes > 0 && itemSize > s.config.maxSessionBytes-total {
			return 0, false
		}
		total += itemSize
	}
	return total, total > 0
}

func (s *reasoningReplayStore) deleteInvalidKVEntry(ctx context.Context, client reasoningReplayKVClient, key string, cause error) ([][]byte, bool, error) {
	if _, errDelete := client.KVDel(ctx, key); errDelete != nil {
		return nil, false, errDelete
	}
	if cause != nil {
		return nil, false, cause
	}
	return nil, false, nil
}

func (s *reasoningReplayStore) logBestEffortSaveError(err error) {
	log.WithError(err).Errorf("home kv best-effort %s set failed", s.config.logLabel)
}

func cloneReasoningReplayItems(items [][]byte) [][]byte {
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, internalpayload.CloneBytes(item))
	}
	return cloned
}
