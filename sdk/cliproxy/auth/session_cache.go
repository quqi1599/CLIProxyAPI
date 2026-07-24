package auth

import (
	"sync"
	"time"
)

const sessionCacheMaxEntries = 16384

// sessionEntry stores auth binding with expiration.
type sessionEntry struct {
	authID     string
	channelKey string
	expiresAt  time.Time
}

// SessionCache provides TTL-based session to auth mapping with automatic cleanup.
type SessionCache struct {
	mu       sync.RWMutex
	entries  map[string]sessionEntry
	ttl      time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewSessionCache creates a cache with the specified TTL.
// A background goroutine periodically cleans expired entries.
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	c := &SessionCache{
		entries: make(map[string]sessionEntry),
		ttl:     ttl,
		stopCh:  make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// Get retrieves the auth ID bound to a session, if still valid.
// Does NOT refresh the TTL on access.
func (c *SessionCache) Get(sessionID string) (string, bool) {
	authID, _, ok := c.GetBinding(sessionID)
	return authID, ok && authID != ""
}

func (c *SessionCache) GetBinding(sessionID string) (string, string, bool) {
	if sessionID == "" {
		return "", "", false
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok {
		return "", "", false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", "", false
	}
	return entry.authID, entry.channelKey, entry.authID != "" || entry.channelKey != ""
}

// GetAndRefresh retrieves the auth ID bound to a session and refreshes TTL on hit.
// This extends the binding lifetime for active sessions.
func (c *SessionCache) GetAndRefresh(sessionID string) (string, bool) {
	authID, _, ok := c.GetAndRefreshBinding(sessionID)
	return authID, ok && authID != ""
}

func (c *SessionCache) GetAndRefreshBinding(sessionID string) (string, string, bool) {
	if sessionID == "" {
		return "", "", false
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[sessionID]
	if !ok {
		c.mu.Unlock()
		return "", "", false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", "", false
	}
	// Refresh TTL on successful access
	entry.expiresAt = now.Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
	return entry.authID, entry.channelKey, entry.authID != "" || entry.channelKey != ""
}

// Set binds a session to an auth ID with TTL refresh.
func (c *SessionCache) Set(sessionID, authID string) {
	c.SetBinding(sessionID, authID, "")
}

func (c *SessionCache) SetBinding(sessionID, authID, channelKey string) {
	if sessionID == "" || authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if _, exists := c.entries[sessionID]; !exists && len(c.entries) >= sessionCacheMaxEntries {
		c.cleanupExpiredLocked(now)
		if len(c.entries) >= sessionCacheMaxEntries {
			c.mu.Unlock()
			return
		}
	}
	c.entries[sessionID] = sessionEntry{
		authID:     authID,
		channelKey: channelKey,
		expiresAt:  now.Add(c.ttl),
	}
	c.mu.Unlock()
}

// Invalidate removes a specific session binding.
func (c *SessionCache) Invalidate(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	delete(c.entries, sessionID)
	c.mu.Unlock()
}

// InvalidateAuth removes a credential binding while retaining its channel
// identity when another credential can keep the session on that channel.
func (c *SessionCache) InvalidateAuth(authID string) {
	if authID == "" {
		return
	}
	c.mu.Lock()
	for sid, entry := range c.entries {
		if entry.authID == authID {
			if entry.channelKey == "" {
				delete(c.entries, sid)
				continue
			}
			entry.authID = ""
			c.entries[sid] = entry
		}
	}
	c.mu.Unlock()
}

// Stop terminates the background cleanup goroutine.
func (c *SessionCache) Stop() {
	if c != nil {
		c.stopOnce.Do(func() {
			close(c.stopCh)
		})
	}
}

func (c *SessionCache) cleanupLoop() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	c.cleanupExpiredLocked(now)
	c.mu.Unlock()
}

func (c *SessionCache) cleanupExpiredLocked(now time.Time) {
	for sid, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, sid)
		}
	}
}
