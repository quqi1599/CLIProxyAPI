package cache

import (
	"container/list"
	"reflect"
	"sync"
	"time"
)

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type boundedLRUEntry[K comparable, V any] struct {
	key        K
	value      V
	lastAccess time.Time
	size       int64
}

type boundedLRU[K comparable, V any] struct {
	mu             sync.Mutex
	entries        map[K]*list.Element
	lru            *list.List
	ttl            time.Duration
	maxEntries     int
	evictBatchSize int
	maxBytes       int64
	totalBytes     int64
}

func newBoundedLRU[K comparable, V any](ttl time.Duration, maxEntries, evictBatchSize int, maxBytes int64) *boundedLRU[K, V] {
	return &boundedLRU[K, V]{
		entries:        make(map[K]*list.Element),
		lru:            list.New(),
		ttl:            ttl,
		maxEntries:     maxEntries,
		evictBatchSize: evictBatchSize,
		maxBytes:       maxBytes,
	}
}

func (c *boundedLRU[K, V]) Set(key K, value V, size int64, now time.Time) bool {
	if !c.CanStore(size) {
		return false
	}
	c.mu.Lock()
	if previous, exists := c.entries[key]; exists {
		previousEntry := previous.Value.(*boundedLRUEntry[K, V])
		if now.Before(previousEntry.lastAccess) {
			now = previousEntry.lastAccess
		}
		c.removeElementLocked(previous)
	}
	entry := &boundedLRUEntry[K, V]{key: key, value: value, lastAccess: now, size: size}
	element := c.lru.PushFront(entry)
	c.entries[key] = element
	c.totalBytes += size
	c.evictLocked()
	_, retained := c.entries[key]
	c.mu.Unlock()
	return retained
}

func (c *boundedLRU[K, V]) CanStore(size int64) bool {
	return c != nil && size >= 0 && (c.maxBytes <= 0 || size <= c.maxBytes)
}

func (c *boundedLRU[K, V]) Get(key K, now time.Time) (V, bool) {
	var zero V
	if c == nil {
		return zero, false
	}
	c.mu.Lock()
	element, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return zero, false
	}
	entry := element.Value.(*boundedLRUEntry[K, V])
	if now.Before(entry.lastAccess) {
		now = entry.lastAccess
	}
	if now.Sub(entry.lastAccess) > c.ttl {
		c.removeElementLocked(element)
		c.mu.Unlock()
		return zero, false
	}
	entry.lastAccess = now
	c.lru.MoveToFront(element)
	value := entry.value
	c.mu.Unlock()
	return value, true
}

func (c *boundedLRU[K, V]) Delete(key K) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if element, ok := c.entries[key]; ok {
		c.removeElementLocked(element)
	}
	c.mu.Unlock()
}

func (c *boundedLRU[K, V]) DeleteIf(matches func(K) bool) {
	if c == nil || matches == nil {
		return
	}
	c.mu.Lock()
	for key, element := range c.entries {
		if matches(key) {
			c.removeElementLocked(element)
		}
	}
	c.mu.Unlock()
}

func (c *boundedLRU[K, V]) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[K]*list.Element)
	c.lru.Init()
	c.totalBytes = 0
	c.mu.Unlock()
}

func (c *boundedLRU[K, V]) PurgeExpired(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for element := c.lru.Back(); element != nil; {
		previous := element.Prev()
		entry := element.Value.(*boundedLRUEntry[K, V])
		if now.Sub(entry.lastAccess) > c.ttl {
			c.removeElementLocked(element)
		}
		element = previous
	}
	c.mu.Unlock()
}

func (c *boundedLRU[K, V]) Stats() (int, int64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.totalBytes
}

func (c *boundedLRU[K, V]) evictLocked() {
	entryLimitExceeded := c.maxEntries > 0 && len(c.entries) > c.maxEntries
	byteLimitExceeded := c.maxBytes > 0 && c.totalBytes > c.maxBytes
	if !entryLimitExceeded && !byteLimitExceeded {
		return
	}
	minimumEvictions := 0
	if entryLimitExceeded {
		minimumEvictions = c.evictBatchSize
		if minimumEvictions <= 0 {
			minimumEvictions = 1
		}
	}
	for removed := 0; ; removed++ {
		withinEntryLimit := c.maxEntries <= 0 || len(c.entries) <= c.maxEntries
		withinByteLimit := c.maxBytes <= 0 || c.totalBytes <= c.maxBytes
		if removed >= minimumEvictions && withinEntryLimit && withinByteLimit {
			break
		}
		element := c.lru.Back()
		if element == nil {
			break
		}
		c.removeElementLocked(element)
	}
}

func (c *boundedLRU[K, V]) removeElementLocked(element *list.Element) {
	entry := element.Value.(*boundedLRUEntry[K, V])
	delete(c.entries, entry.key)
	c.lru.Remove(element)
	c.totalBytes -= entry.size
	if c.totalBytes < 0 {
		c.totalBytes = 0
	}
}
