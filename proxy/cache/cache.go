// Package cache implements an in-memory query-result cache for the proxy.
//
// It stores pre-serialised Prometheus API response bytes keyed by the query
// parameters, so a cache hit is a direct write to the client with no
// re-evaluation and no re-marshalling. A singleflight group collapses
// concurrent identical misses into a single evaluation, which is where most of
// the value comes from under dashboard load (many panels / many viewers issuing
// the same query at once).
package cache

import (
	"container/list"
	"encoding/binary"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/sync/singleflight"
)

// Entry is a cached, fully rendered API response.
type Entry struct {
	Body        []byte
	ContentType string
}

// item is the LRU list payload.
type item struct {
	key       uint64
	entry     Entry
	expiresAt time.Time
}

// ResultCache is a size-bounded, TTL-expiring LRU of rendered responses with an
// embedded singleflight group. It is safe for concurrent use.
type ResultCache struct {
	mu      sync.Mutex
	maxSize int
	ttl     time.Duration
	ll      *list.List               // front = most-recently used
	items   map[uint64]*list.Element // key → *list.Element(item)
	sf      singleflight.Group

	hits   atomic.Uint64
	misses atomic.Uint64

	// now is overridable in tests; nil means time.Now.
	now func() time.Time
}

// New creates a ResultCache. maxSize < 1 is treated as 1.
func New(maxSize int, ttl time.Duration) *ResultCache {
	if maxSize < 1 {
		maxSize = 1
	}
	return &ResultCache{
		maxSize: maxSize,
		ttl:     ttl,
		ll:      list.New(),
		items:   make(map[uint64]*list.Element, maxSize),
	}
}

func (c *ResultCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Key derives a stable cache key from the query and its time bounds. Timestamps
// are in milliseconds; step is the resolution in milliseconds (0 for instant).
func Key(query string, startMs, endMs, stepMs int64) uint64 {
	var d xxhash.Digest
	d.Reset()
	_, _ = d.WriteString(query)
	var buf [24]byte
	binary.LittleEndian.PutUint64(buf[0:8], uint64(startMs))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(endMs))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(stepMs))
	_, _ = d.Write(buf[:])
	return d.Sum64()
}

// Get returns a live (non-expired) entry, promoting it to most-recently-used.
func (c *ResultCache) Get(key uint64) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Entry{}, false
	}
	it := el.Value.(*item)
	if c.clock().After(it.expiresAt) {
		c.removeElement(el)
		return Entry{}, false
	}
	c.ll.MoveToFront(el)
	return it.entry, true
}

// Set inserts or updates an entry, evicting the least-recently-used item when
// the cache is over capacity.
func (c *ResultCache) Set(key uint64, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.clock().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		it := el.Value.(*item)
		it.entry = e
		it.expiresAt = exp
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&item{key: key, entry: e, expiresAt: exp})
	c.items[key] = el
	for c.ll.Len() > c.maxSize {
		if back := c.ll.Back(); back != nil {
			c.removeElement(back)
		}
	}
}

// removeElement removes el from both the list and the map. Caller holds c.mu.
func (c *ResultCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.items, el.Value.(*item).key)
}

// Fetch returns a cached entry for key, or computes it via fn under
// singleflight. When store is false the computed entry is served but not
// cached (used for near-now windows whose data is still being written). The
// returned hit is true only when the entry came from the cache.
func (c *ResultCache) Fetch(key uint64, store bool, fn func() (Entry, error)) (Entry, bool, error) {
	if e, ok := c.Get(key); ok {
		c.hits.Add(1)
		return e, true, nil
	}
	c.misses.Add(1)

	v, err, _ := c.sf.Do(strconv.FormatUint(key, 10), func() (any, error) {
		// Another in-flight caller may have populated the cache while we waited.
		if e, ok := c.Get(key); ok {
			return e, nil
		}
		e, err := fn()
		if err != nil {
			return Entry{}, err
		}
		if store {
			c.Set(key, e)
		}
		return e, nil
	})
	if err != nil {
		return Entry{}, false, err
	}
	return v.(Entry), false, nil
}

// Stats returns cumulative hit and miss counts.
func (c *ResultCache) Stats() (hits, misses uint64) {
	return c.hits.Load(), c.misses.Load()
}

// Len returns the current number of cached entries.
func (c *ResultCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
