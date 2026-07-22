// Package cache provides a concurrency-safe in-memory TTL cache of boolean
// key-validation results, with a background janitor that evicts expired entries.
package cache

import (
	"sync"
	"time"
)

type entry struct {
	val       bool
	expiresAt time.Time
}

// Cache is a concurrency-safe map of string keys to boolean values with per-entry
// expiry. Keys are expected to be SHA-256 hex digests, not plaintext tokens.
type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time

	stop chan struct{}
	done chan struct{}
}

// New creates a Cache. If janitorEvery > 0, a background goroutine evicts expired
// entries on that interval. Call Close to stop it.
func New(janitorEvery time.Duration) *Cache {
	c := &Cache{
		data: make(map[string]entry),
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
func (c *Cache) Get(key string) (bool, bool) {
	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()
	if !ok || c.now().After(e.expiresAt) {
		return false, false
	}
	return e.val, true
}

// Set stores val under key with the given time-to-live.
func (c *Cache) Set(key string, val bool, ttl time.Duration) {
	c.mu.Lock()
	c.data[key] = entry{val: val, expiresAt: c.now().Add(ttl)}
	c.mu.Unlock()
}

// Len returns the number of entries currently held (including any not yet swept).
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// Close stops the janitor goroutine. Safe to call once.
func (c *Cache) Close() {
	close(c.stop)
	<-c.done
}

func (c *Cache) janitor(every time.Duration) {
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

func (c *Cache) deleteExpired() {
	now := c.now()
	c.mu.Lock()
	for k, e := range c.data {
		if now.After(e.expiresAt) {
			delete(c.data, k)
		}
	}
	c.mu.Unlock()
}
