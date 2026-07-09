package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func entry(s string) Entry { return Entry{Body: []byte(s), ContentType: "application/json"} }

func TestKeyStability(t *testing.T) {
	a := Key("up", 1000, 2000, 15000)
	b := Key("up", 1000, 2000, 15000)
	if a != b {
		t.Fatalf("same inputs produced different keys: %d != %d", a, b)
	}
	// Any component change must change the key.
	for _, k := range []uint64{
		Key("up{}", 1000, 2000, 15000),
		Key("up", 1001, 2000, 15000),
		Key("up", 1000, 2001, 15000),
		Key("up", 1000, 2000, 15001),
	} {
		if k == a {
			t.Fatalf("distinct inputs collided on key %d", k)
		}
	}
}

func TestGetSetHit(t *testing.T) {
	c := New(4, time.Minute)
	k := Key("up", 0, 0, 0)
	if _, ok := c.Get(k); ok {
		t.Fatal("expected miss on empty cache")
	}
	c.Set(k, entry("v1"))
	got, ok := c.Get(k)
	if !ok || string(got.Body) != "v1" {
		t.Fatalf("expected hit v1, got ok=%v body=%q", ok, got.Body)
	}
}

func TestTTLExpiry(t *testing.T) {
	c := New(4, 30*time.Second)
	base := time.Unix(1_000_000, 0)
	cur := base
	c.now = func() time.Time { return cur }

	k := Key("up", 0, 0, 0)
	c.Set(k, entry("v1"))
	if _, ok := c.Get(k); !ok {
		t.Fatal("expected hit immediately after set")
	}
	cur = base.Add(31 * time.Second) // past TTL
	if _, ok := c.Get(k); ok {
		t.Fatal("expected miss after TTL expiry")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry should have been evicted, len=%d", c.Len())
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(2, time.Minute)
	k1, k2, k3 := Key("a", 0, 0, 0), Key("b", 0, 0, 0), Key("c", 0, 0, 0)
	c.Set(k1, entry("a"))
	c.Set(k2, entry("b"))
	// Touch k1 so k2 becomes least-recently-used.
	if _, ok := c.Get(k1); !ok {
		t.Fatal("k1 should be present")
	}
	c.Set(k3, entry("c")) // evicts k2
	if _, ok := c.Get(k2); ok {
		t.Fatal("k2 should have been evicted as LRU")
	}
	if _, ok := c.Get(k1); !ok {
		t.Fatal("k1 should still be present")
	}
	if _, ok := c.Get(k3); !ok {
		t.Fatal("k3 should be present")
	}
}

func TestFetchSingleflightDedup(t *testing.T) {
	c := New(8, time.Minute)
	k := Key("slow", 0, 0, 0)

	var produced atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{}, 1)

	const n = 20
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			e, _, err := c.Fetch(k, true, func() (Entry, error) {
				produced.Add(1)
				select {
				case started <- struct{}{}:
				default:
				}
				<-release // hold the flight open so the others pile up
				return entry("computed"), nil
			})
			if err != nil {
				t.Errorf("fetch %d: %v", idx, err)
				return
			}
			results[idx] = string(e.Body)
		}(i)
	}

	<-started                         // ensure the first flight is in fn() before releasing
	time.Sleep(20 * time.Millisecond) // let the rest coalesce
	close(release)
	wg.Wait()

	if got := produced.Load(); got != 1 {
		t.Fatalf("expected fn to run once under singleflight, ran %d times", got)
	}
	for i, r := range results {
		if r != "computed" {
			t.Fatalf("result %d = %q, want computed", i, r)
		}
	}
	// After the flight, the value is cached: next Fetch is a hit.
	_, hit, _ := c.Fetch(k, true, func() (Entry, error) {
		t.Fatal("fn should not run on a cache hit")
		return Entry{}, nil
	})
	if !hit {
		t.Fatal("expected cache hit after singleflight populated the entry")
	}
}

func TestFetchNoStore(t *testing.T) {
	c := New(8, time.Minute)
	k := Key("fresh", 0, 0, 0)
	var runs atomic.Int64
	produce := func() (Entry, error) { runs.Add(1); return entry("x"), nil }

	if _, hit, _ := c.Fetch(k, false, produce); hit {
		t.Fatal("first fetch cannot be a hit")
	}
	if _, hit, _ := c.Fetch(k, false, produce); hit {
		t.Fatal("store=false must never populate the cache")
	}
	if runs.Load() != 2 {
		t.Fatalf("expected 2 computations with store=false, got %d", runs.Load())
	}
}
