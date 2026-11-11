// Package cache is a sharded in-memory TTL + LRU cache with entry and byte limits.
//
// Every entry has a TTL and an estimated size. When a shard exceeds its share of the
// limits, least-recently-used entries are evicted. A background janitor removes expired
// entries on a configurable interval so memory does not grow with dead entries.
package cache

import (
	"container/list"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const shardCount = 32

type entry struct {
	key     string
	val     any
	size    int64
	expires time.Time
}

type shard struct {
	mu    sync.Mutex
	items map[string]*list.Element
	lru   *list.List
	bytes int64
}

// Cache is safe for concurrent use. Call Stop to end the janitor goroutine.
type Cache struct {
	shards     [shardCount]*shard
	maxEntries int // per shard
	maxBytes   int64
	now        func() time.Time

	hits, misses, evictions, expirations atomic.Int64

	sf       singleflight.Group
	interval chan time.Duration
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// Stats is a point-in-time snapshot.
type Stats struct {
	Entries     int   `json:"entries"`
	Bytes       int64 `json:"bytes"`
	Hits        int64 `json:"hits"`
	Misses      int64 `json:"misses"`
	Evictions   int64 `json:"evictions"`
	Expirations int64 `json:"expirations"`
	MaxEntries  int   `json:"max_entries"`
	MaxBytes    int64 `json:"max_bytes"`
}

// New creates a cache bounded by maxEntries and maxBytes overall and starts the janitor.
func New(maxEntries int, maxBytes int64, cleanup time.Duration) *Cache {
	if maxEntries < shardCount {
		maxEntries = shardCount
	}
	c := &Cache{
		maxEntries: maxEntries / shardCount,
		maxBytes:   maxBytes / shardCount,
		now:        time.Now,
		interval:   make(chan time.Duration, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	for i := range c.shards {
		c.shards[i] = &shard{items: map[string]*list.Element{}, lru: list.New()}
	}
	go c.janitor(cleanup)
	return c
}

func (c *Cache) shard(key string) *shard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return c.shards[h.Sum32()%shardCount]
}

// Get returns the value if present and not expired.
func (c *Cache) Get(key string) (any, bool) {
	s := c.shard(key)
	s.mu.Lock()
	el, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	e := el.Value.(*entry)
	if c.now().After(e.expires) {
		s.removeElement(el)
		s.mu.Unlock()
		c.expirations.Add(1)
		c.misses.Add(1)
		return nil, false
	}
	s.lru.MoveToFront(el)
	v := e.val
	s.mu.Unlock()
	c.hits.Add(1)
	return v, true
}

// Set stores a value with a TTL and an estimated size in bytes.
func (c *Cache) Set(key string, val any, size int64, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	if size < 0 {
		size = 0
	}
	size += int64(len(key)) + 64 // bookkeeping overhead estimate
	if size > c.maxBytes {
		return // never let one value monopolise a shard
	}
	s := c.shard(key)
	s.mu.Lock()
	if el, ok := s.items[key]; ok {
		s.removeElement(el)
	}
	e := &entry{key: key, val: val, size: size, expires: c.now().Add(ttl)}
	s.items[key] = s.lru.PushFront(e)
	s.bytes += size
	for (len(s.items) > c.maxEntries || s.bytes > c.maxBytes) && s.lru.Len() > 0 {
		s.removeElement(s.lru.Back())
		c.evictions.Add(1)
	}
	s.mu.Unlock()
}

// Delete removes a key.
func (c *Cache) Delete(key string) {
	s := c.shard(key)
	s.mu.Lock()
	if el, ok := s.items[key]; ok {
		s.removeElement(el)
	}
	s.mu.Unlock()
}

// DeletePrefix removes every key starting with prefix and returns the count removed.
func (c *Cache) DeletePrefix(prefix string) int {
	n := 0
	for _, s := range c.shards {
		s.mu.Lock()
		for k, el := range s.items {
			if strings.HasPrefix(k, prefix) {
				s.removeElement(el)
				n++
			}
		}
		s.mu.Unlock()
	}
	return n
}

// InvalidateAll drops every cached entry whose key starts with prefix, or every entry when
// prefix is empty, and returns how many were removed. A restore uses it to throw away the
// in-memory directory and search caches after the storage list changed.
func (c *Cache) InvalidateAll(prefix string) int {
	return c.DeletePrefix(prefix)
}

// DeleteFunc removes every key for which fn returns true.
func (c *Cache) DeleteFunc(fn func(key string) bool) int {
	n := 0
	for _, s := range c.shards {
		s.mu.Lock()
		for k, el := range s.items {
			if fn(k) {
				s.removeElement(el)
				n++
			}
		}
		s.mu.Unlock()
	}
	return n
}

// Clear removes everything.
func (c *Cache) Clear() {
	for _, s := range c.shards {
		s.mu.Lock()
		s.items = map[string]*list.Element{}
		s.lru.Init()
		s.bytes = 0
		s.mu.Unlock()
	}
}

// GetOrLoad returns the cached value or calls load once per key across concurrent callers.
// load returns the value, its estimated size, and its TTL (ttl <= 0 means "do not cache").
func (c *Cache) GetOrLoad(key string, load func() (any, int64, time.Duration, error)) (any, error) {
	if v, ok := c.Get(key); ok {
		return v, nil
	}
	v, err, _ := c.sf.Do(key, func() (any, error) {
		if v, ok := c.Get(key); ok {
			return v, nil
		}
		val, size, ttl, err := load()
		if err != nil {
			return nil, err
		}
		c.Set(key, val, size, ttl)
		return val, nil
	})
	return v, err
}

// Stats returns counters and current usage.
func (c *Cache) Stats() Stats {
	st := Stats{
		Hits:        c.hits.Load(),
		Misses:      c.misses.Load(),
		Evictions:   c.evictions.Load(),
		Expirations: c.expirations.Load(),
		MaxEntries:  c.maxEntries * shardCount,
		MaxBytes:    c.maxBytes * shardCount,
	}
	for _, s := range c.shards {
		s.mu.Lock()
		st.Entries += len(s.items)
		st.Bytes += s.bytes
		s.mu.Unlock()
	}
	return st
}

// SetCleanupInterval changes the janitor interval without restarting it.
func (c *Cache) SetCleanupInterval(d time.Duration) {
	select {
	case <-c.interval: // drop a pending, not-yet-applied value
	default:
	}
	select {
	case c.interval <- d:
	case <-c.stop:
	}
}

// Stop ends the janitor goroutine. Safe to call more than once.
func (c *Cache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
		<-c.done
	})
}

// Sweep removes expired entries now and returns the number removed.
func (c *Cache) Sweep() int {
	now := c.now()
	n := 0
	for _, s := range c.shards {
		s.mu.Lock()
		for el := s.lru.Back(); el != nil; {
			prev := el.Prev()
			if now.After(el.Value.(*entry).expires) {
				s.removeElement(el)
				n++
			}
			el = prev
		}
		s.mu.Unlock()
	}
	c.expirations.Add(int64(n))
	return n
}

func (c *Cache) janitor(d time.Duration) {
	defer close(c.done)
	if d <= 0 {
		d = 2 * time.Minute
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case nd := <-c.interval:
			if nd > 0 {
				t.Reset(nd)
			}
		case <-t.C:
			c.Sweep()
		}
	}
}

func (s *shard) removeElement(el *list.Element) {
	e := el.Value.(*entry)
	s.lru.Remove(el)
	delete(s.items, e.key)
	s.bytes -= e.size
}
