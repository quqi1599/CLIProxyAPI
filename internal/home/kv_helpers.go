package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

func HashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func CurrentKVClient() (*Client, bool, error) {
	client := Current()
	if client == nil {
		return nil, false, nil
	}
	if !client.Enabled() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrDisabled)
	}
	if !client.HeartbeatOK() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrNotConnected)
	}
	return client, true, nil
}

func KVGetJSONRequired(ctx context.Context, key string, out any) (bool, bool, error) {
	client, homeMode, errClient := CurrentKVClient()
	if !homeMode || errClient != nil {
		return homeMode, false, errClient
	}
	raw, found, errGet := client.KVGet(ctx, key)
	if errGet != nil || !found {
		return true, false, errGet
	}
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		return true, false, errUnmarshal
	}
	return true, true, nil
}

func KVSetJSONRequired(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return false, errMarshal
	}
	return KVSetBytesRequired(ctx, key, raw, ttl)
}

func KVSetBytesRequired(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	client, homeMode, errClient := CurrentKVClient()
	if !homeMode || errClient != nil {
		return homeMode, errClient
	}
	written, errSet := client.KVSet(ctx, key, value, kvSetOptionsForTTL(ttl))
	if errSet != nil {
		return true, errSet
	}
	if !written {
		return true, fmt.Errorf("home kv store unavailable")
	}
	return true, nil
}

func KVDelRequired(ctx context.Context, keys ...string) (bool, int64, error) {
	client, homeMode, errClient := CurrentKVClient()
	if !homeMode || errClient != nil {
		return homeMode, 0, errClient
	}
	deleted, errDel := client.KVDel(ctx, keys...)
	return true, deleted, errDel
}

func KVSetJSONBestEffort(ctx context.Context, key string, value any, ttl time.Duration) bool {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		log.Errorf("home kv best-effort set failed prefix=%s: %v", kvLogPrefix(key), errMarshal)
		return false
	}
	return KVSetBytesBestEffort(ctx, key, raw, ttl)
}

func KVSetBytesBestEffort(ctx context.Context, key string, value []byte, ttl time.Duration) bool {
	homeMode, errSet := KVSetBytesRequired(ctx, key, value, ttl)
	if !homeMode {
		return false
	}
	if errSet != nil {
		log.Errorf("home kv best-effort set failed prefix=%s: %v", kvLogPrefix(key), errSet)
		return false
	}
	return true
}

func kvSetOptionsForTTL(ttl time.Duration) KVSetOptions {
	if ttl <= 0 {
		return KVSetOptions{}
	}
	return KVSetOptions{EX: ttl}
}

func kvLogPrefix(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "unknown"
	}
	parts := strings.Split(key, ":")
	if len(parts) >= 2 {
		return parts[0] + ":" + parts[1] + ":*"
	}
	return parts[0] + ":*"
}
