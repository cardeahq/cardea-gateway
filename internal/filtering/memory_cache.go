package filtering

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const sweepInterval = 30 * time.Second

var errCacheClosed = errors.New("filtering: memory cache is closed")

type cacheItem struct {
	entry  Entry
	expiry time.Time
}

// MemoryCache is the default Cache: volatile, in-process, bounded by
// capacity and per-entry TTL. Past capacity, the oldest entry by
// insertion order (not access) is evicted first — FIFO, not LRU: nothing
// reads an entry back yet, so there is no access pattern to make
// recency-aware eviction meaningful (specs/0006, Alternatives
// considered).
type MemoryCache struct {
	mu       sync.Mutex
	capacity int
	entries  map[string]cacheItem
	// queue holds keys in insertion order; it may still name a key that
	// was already evicted or expired, resolved lazily on the next
	// eviction rather than compacted eagerly.
	queue  []string
	closed bool
	stop   chan struct{}
}

// NewMemoryCache starts a background goroutine that sweeps expired
// entries every 30s, regardless of any individual entry's TTL. capacity
// must be positive. Close stops the sweep goroutine; Set and Get on a
// closed cache return an error.
func NewMemoryCache(capacity int) (*MemoryCache, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("filtering: memory cache capacity must be positive, got %d", capacity)
	}
	c := &MemoryCache{
		capacity: capacity,
		entries:  map[string]cacheItem{},
		stop:     make(chan struct{}),
	}
	go c.sweep()
	return c, nil
}

func (c *MemoryCache) sweep() {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.sweepExpired()
		}
	}
}

func (c *MemoryCache) sweepExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, it := range c.entries {
		if now.After(it.expiry) {
			delete(c.entries, k)
		}
	}
}

func (c *MemoryCache) Set(_ context.Context, key string, entry Entry, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errCacheClosed
	}
	if _, exists := c.entries[key]; !exists {
		for len(c.entries) >= c.capacity {
			c.evictOldestLocked()
		}
		c.queue = append(c.queue, key)
	}
	c.entries[key] = cacheItem{entry: entry, expiry: time.Now().Add(ttl)}
	return nil
}

// evictOldestLocked drops the oldest live entry by insertion order. c.mu
// must be held.
func (c *MemoryCache) evictOldestLocked() {
	for len(c.queue) > 0 {
		k := c.queue[0]
		c.queue = c.queue[1:]
		if _, ok := c.entries[k]; ok {
			delete(c.entries, k)
			return
		}
	}
}

func (c *MemoryCache) Get(_ context.Context, key string) (Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Entry{}, false, errCacheClosed
	}
	it, ok := c.entries[key]
	if !ok || time.Now().After(it.expiry) {
		return Entry{}, false, nil
	}
	return it.entry, true, nil
}

func (c *MemoryCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.stop)
	return nil
}
