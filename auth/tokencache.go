package auth

import (
	"context"
	"sync"
	"time"
)

// Token is a bearer token and its expiry. A zero ExpiresAt means "no expiry
// known"; such tokens are cached for MaxAge only.
type Token struct {
	Value     Secret
	ExpiresAt time.Time
}

// TokenCache caches tokens per credential identity (AUTH-05). It is safe for
// concurrent use, de-duplicates concurrent refreshes per identity and is
// bounded in size. Entries are keyed by [Identity] so tokens can never be
// served to a different credential.
type TokenCache struct {
	mu      sync.Mutex
	entries map[string]*entry
	max     int
	skew    time.Duration
	maxAge  time.Duration
	now     func() time.Time
}

type entry struct {
	mu       sync.Mutex // serialises refresh for this identity
	tok      Token
	fetched  time.Time
	lastUsed time.Time
}

// TokenCacheOption configures a TokenCache.
type TokenCacheOption func(*TokenCache)

// WithMaxEntries bounds the number of identities cached (default 256).
func WithMaxEntries(n int) TokenCacheOption { return func(c *TokenCache) { c.max = n } }

// WithExpirySkew refreshes tokens this long before expiry (default 60s).
func WithExpirySkew(d time.Duration) TokenCacheOption { return func(c *TokenCache) { c.skew = d } }

// WithMaxAge bounds tokens that report no expiry (default 5m).
func WithMaxAge(d time.Duration) TokenCacheOption { return func(c *TokenCache) { c.maxAge = d } }

// WithClock injects a clock for tests.
func WithClock(now func() time.Time) TokenCacheOption { return func(c *TokenCache) { c.now = now } }

// NewTokenCache builds a cache.
func NewTokenCache(opts ...TokenCacheOption) *TokenCache {
	c := &TokenCache{entries: map[string]*entry{}, max: 256, skew: 60 * time.Second, maxAge: 5 * time.Minute, now: time.Now}
	for _, o := range opts {
		o(c)
	}
	if c.max < 1 {
		c.max = 1
	}
	return c
}

func (c *TokenCache) fresh(e *entry) bool {
	if e.tok.Value.IsZero() {
		return false
	}
	now := c.now()
	if e.tok.ExpiresAt.IsZero() {
		return now.Sub(e.fetched) < c.maxAge
	}
	return now.Add(c.skew).Before(e.tok.ExpiresAt)
}

func (c *TokenCache) entry(id string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok {
		if len(c.entries) >= c.max {
			c.evictLocked()
		}
		e = &entry{}
		c.entries[id] = e
	}
	e.lastUsed = c.now()
	return e
}

// evictLocked drops the least recently used entry that is not mid-refresh.
func (c *TokenCache) evictLocked() {
	var oldestID string
	var oldest time.Time
	for id, e := range c.entries {
		if !e.mu.TryLock() {
			continue
		}
		stale := !c.fresh(e)
		e.mu.Unlock()
		if stale {
			delete(c.entries, id)
			return
		}
		if oldestID == "" || e.lastUsed.Before(oldest) {
			oldestID, oldest = id, e.lastUsed
		}
	}
	if oldestID != "" {
		delete(c.entries, oldestID)
	}
}

// Get returns a fresh token for identity, calling fetch at most once
// concurrently per identity. Errors are not cached.
func (c *TokenCache) Get(ctx context.Context, identity string, fetch func(context.Context) (Token, error)) (Secret, error) {
	e := c.entry(identity)
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.fresh(e) {
		return e.tok.Value, nil
	}
	tok, err := fetch(ctx)
	if err != nil {
		return "", err
	}
	e.tok, e.fetched = tok, c.now()
	return tok.Value, nil
}

// Invalidate drops a token (for example after the provider rejected it).
// It only drops the token if it still equals rejected, so a concurrently
// refreshed token is kept.
func (c *TokenCache) Invalidate(identity string, rejected Secret) {
	c.mu.Lock()
	e, ok := c.entries[identity]
	c.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	if e.tok.Value == rejected {
		e.tok = Token{}
	}
	e.mu.Unlock()
}

// Len returns the number of cached identities.
func (c *TokenCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
