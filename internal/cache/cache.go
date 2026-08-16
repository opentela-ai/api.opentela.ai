// Package cache provides a concurrency-safe in-memory TTL cache, with a
// background janitor that evicts expired entries. It is generic over the
// cached value type; the request path caches API-key validation verdicts.
package cache

import (
	"sync"
	"time"
)

type entry[T any] struct {
	val       T
	expiresAt time.Time
}

// Cache is a concurrency-safe map of string keys to values of type T with
// per-entry expiry. Keys are expected to be SHA-256 hex digests, not plaintext
// tokens.
type Cache[T any] struct {
	mu   sync.RWMutex
	data map[string]entry[T]
	now  func() time.Time

	stop chan struct{}
	done chan struct{}

	closeOnce sync.Once
}

// New creates a Cache. If janitorEvery > 0, a background goroutine evicts
// expired entries on that interval. Call Close to stop it.
func New[T any](janitorEvery time.Duration) *Cache[T] {
	c := &Cache[T]{
		data: make(map[string]entry[T]),
		now:  time.Now,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if janitorEvery > 0 {
		go c.janitor(janitorEvery)
	} else {
		close(c.done)
	}
	return c
}

// Get returns the cached value and whether a live (non-expired) entry exists.
// An expired entry is treated as a miss and removed from the map.
func (c *Cache[T]) Get(key string) (T, bool) {
	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()
	if !ok {
		var zero T
		return zero, false
	}
	if c.now().After(e.expiresAt) {
		// Expired. Delete under the write lock, but only if the entry is still
		// present and still expired — a concurrent Set may have refreshed it.
		c.mu.Lock()
		if cur, present := c.data[key]; present && c.now().After(cur.expiresAt) {
			delete(c.data, key)
		}
		c.mu.Unlock()
		var zero T
		return zero, false
	}
	return e.val, true
}

// Set stores val under key with the given time-to-live.
func (c *Cache[T]) Set(key string, val T, ttl time.Duration) {
	c.mu.Lock()
	c.data[key] = entry[T]{val: val, expiresAt: c.now().Add(ttl)}
	c.mu.Unlock()
}

// Len returns the number of entries currently held (including any not yet swept).
func (c *Cache[T]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// Close stops the janitor goroutine. Safe to call more than once.
func (c *Cache[T]) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
		<-c.done
	})
}

func (c *Cache[T]) janitor(every time.Duration) {
	defer close(c.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.deleteExpired()
		}
	}
}

func (c *Cache[T]) deleteExpired() {
	now := c.now()
	c.mu.Lock()
	for k, e := range c.data {
		if now.After(e.expiresAt) {
			delete(c.data, k)
		}
	}
	c.mu.Unlock()
}
