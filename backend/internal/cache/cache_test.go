package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestExpiry(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()
	now := time.Now()
	c.now = func() time.Time { return now }
	c.Set("a", 1, 1, time.Second)
	if v, ok := c.Get("a"); !ok || v.(int) != 1 {
		t.Fatal("expected hit")
	}
	now = now.Add(2 * time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expected expiry")
	}
}

func TestSweep(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()
	now := time.Now()
	c.now = func() time.Time { return now }
	for i := 0; i < 100; i++ {
		c.Set(fmt.Sprint(i), i, 10, time.Second)
	}
	now = now.Add(time.Minute)
	if n := c.Sweep(); n != 100 {
		t.Fatalf("swept %d", n)
	}
	if st := c.Stats(); st.Entries != 0 || st.Bytes != 0 {
		t.Fatalf("stats after sweep: %+v", st)
	}
}

func TestEntryLimit(t *testing.T) {
	c := New(64, 1<<30, time.Hour) // 2 per shard
	defer c.Stop()
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprint(i), i, 1, time.Hour)
	}
	if st := c.Stats(); st.Entries > 64 {
		t.Fatalf("entries %d exceed limit", st.Entries)
	}
}

func TestByteLimit(t *testing.T) {
	c := New(1<<20, 32*1024, time.Hour) // 1KB per shard
	defer c.Stop()
	for i := 0; i < 5000; i++ {
		c.Set(fmt.Sprint(i), i, 200, time.Hour)
	}
	if st := c.Stats(); st.Bytes > 32*1024 {
		t.Fatalf("bytes %d exceed limit", st.Bytes)
	}
	// An oversize value is refused rather than evicting the shard.
	c.Set("big", 1, 1<<20, time.Hour)
	if _, ok := c.Get("big"); ok {
		t.Fatal("oversize value cached")
	}
}

func TestLRUOrder(t *testing.T) {
	c := New(shardCount*2, 1<<30, time.Hour)
	defer c.Stop()
	// Find three keys in the same shard.
	var keys []string
	target := c.shard("k0")
	for i := 0; len(keys) < 3; i++ {
		k := fmt.Sprintf("k%d", i)
		if c.shard(k) == target {
			keys = append(keys, k)
		}
	}
	c.Set(keys[0], 0, 1, time.Hour)
	c.Set(keys[1], 1, 1, time.Hour)
	c.Get(keys[0]) // keys[0] is now most recent
	c.Set(keys[2], 2, 1, time.Hour)
	if _, ok := c.Get(keys[1]); ok {
		t.Fatal("least recently used key should be evicted")
	}
	if _, ok := c.Get(keys[0]); !ok {
		t.Fatal("recently used key evicted")
	}
}

func TestDeletePrefix(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()
	c.Set("s:1:a", 1, 1, time.Hour)
	c.Set("s:1:b", 1, 1, time.Hour)
	c.Set("s:2:a", 1, 1, time.Hour)
	if n := c.DeletePrefix("s:1:"); n != 2 {
		t.Fatalf("deleted %d", n)
	}
	if _, ok := c.Get("s:2:a"); !ok {
		t.Fatal("wrong key removed")
	}
}

func TestGetOrLoadSingleflight(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad("x", func() (any, int64, time.Duration, error) {
				calls.Add(1)
				time.Sleep(20 * time.Millisecond)
				return 42, 8, time.Hour, nil
			})
			if err != nil || v.(int) != 42 {
				t.Error("bad load")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times", calls.Load())
	}
}

func TestJanitorIntervalChange(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()
	c.Set("a", 1, 1, time.Millisecond)
	c.SetCleanupInterval(10 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Stats().Entries == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("janitor did not sweep after interval change")
}

func TestConcurrent(t *testing.T) {
	c := New(512, 1<<20, 5*time.Millisecond)
	defer c.Stop()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				k := fmt.Sprintf("%d-%d", g, i%300)
				c.Set(k, i, 16, time.Duration(i%5+1)*time.Millisecond)
				c.Get(k)
				if i%97 == 0 {
					c.DeletePrefix(fmt.Sprintf("%d-", g))
				}
			}
		}(g)
	}
	wg.Wait()
	if st := c.Stats(); st.Entries > 512 {
		t.Fatalf("limit exceeded: %d", st.Entries)
	}
}

func TestStopIdempotent(t *testing.T) {
	c := New(10, 1000, time.Hour)
	c.Stop()
	c.Stop()
}
