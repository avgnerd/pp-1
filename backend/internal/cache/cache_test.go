package cache

import (
	"context"
	"errors"
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
			v, err := c.GetOrLoad(context.Background(), "x", time.Hour, func(context.Context) (any, int64, time.Duration, error) {
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

// A caller leaving must not end the shared load for the callers waiting on the same key, and the
// caller that left must not stay blocked until the load ends.
func TestGetOrLoadDetachesSharedLoadFromCaller(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()

	loadStarted := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context) (any, int64, time.Duration, error) {
		if calls.Add(1) > 1 {
			return nil, 0, 0, errors.New("the shared load restarted instead of being shared")
		}
		close(loadStarted)
		<-release
		// The load runs on its own context: the first caller has already cancelled by now.
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, fmt.Errorf("shared load ran on the caller's context: %w", err)
		}
		return 42, 8, time.Hour, nil
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	type outcome struct {
		val any
		err error
	}
	aOut := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxA, "x", time.Minute, load)
		aOut <- err
	}()
	select {
	case <-loadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the load never started")
	}

	bOut := make(chan outcome, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "x", time.Minute, load)
		bOut <- outcome{val: v, err: err}
	}()

	cancelA()
	select {
	case err := <-aOut:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled caller stayed blocked on the shared load")
	}

	close(release)
	select {
	case got := <-bOut:
		if got.err != nil {
			t.Fatalf("caller waiting on the shared load failed: %v", got.err)
		}
		if got.val.(int) != 42 {
			t.Fatalf("shared value = %v, want 42", got.val)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller waiting on the shared load never finished")
	}
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times, want one shared load", calls.Load())
	}
}

// The shared load carries its own deadline, so it cannot run forever once its callers have gone.
func TestGetOrLoadBoundsTheDetachedLoad(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()

	loadCtxErr := make(chan error, 1)
	v, err := c.GetOrLoad(context.Background(), "y", 50*time.Millisecond, func(ctx context.Context) (any, int64, time.Duration, error) {
		<-ctx.Done()
		loadCtxErr <- ctx.Err()
		return nil, 0, 0, ctx.Err()
	})
	if v != nil {
		t.Fatalf("value = %v, want none", v)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller got %v, want the load's deadline", err)
	}
	if got := <-loadCtxErr; !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("load context ended with %v, want its own deadline", got)
	}
}

// A failed load is not cached, so a later caller retries instead of inheriting the failure.
func TestGetOrLoadDoesNotCacheErrors(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()

	var calls atomic.Int32
	load := func(context.Context) (any, int64, time.Duration, error) {
		if calls.Add(1) == 1 {
			return nil, 0, 0, errors.New("transient")
		}
		return 7, 8, time.Hour, nil
	}
	if _, err := c.GetOrLoad(context.Background(), "z", time.Minute, load); err == nil {
		t.Fatal("first load should fail")
	}
	v, err := c.GetOrLoad(context.Background(), "z", time.Minute, load)
	if err != nil || v.(int) != 7 {
		t.Fatalf("retry = %v, %v; want the retried value 7", v, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("loader called %d times, want a retry", calls.Load())
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

// A removal says the data behind the key changed. A load that was already running read the old
// data, so it may answer the callers waiting on it but must not put that value back for the rest of
// its TTL.
func TestGetOrLoadDoesNotPublishAcrossRemoval(t *testing.T) {
	cases := []struct {
		name   string
		remove func(*Cache)
	}{
		{"Delete", func(c *Cache) { c.Delete("dir:/x") }},
		{"DeletePrefix", func(c *Cache) { c.DeletePrefix("dir:") }},
		{"InvalidateAll", func(c *Cache) { c.InvalidateAll("") }},
		{"DeleteFunc", func(c *Cache) { c.DeleteFunc(func(key string) bool { return key == "dir:/x" }) }},
		{"Clear", func(c *Cache) { c.Clear() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(1000, 1<<20, time.Hour)
			defer c.Stop()

			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			load := func(context.Context) (any, int64, time.Duration, error) {
				if calls.Add(1) > 1 {
					return "fresh", 5, time.Hour, nil
				}
				close(started)
				<-release
				return "stale", 5, time.Hour, nil
			}

			type outcome struct {
				val any
				err error
			}
			first := make(chan outcome, 1)
			go func() {
				v, err := c.GetOrLoad(context.Background(), "dir:/x", time.Minute, load)
				first <- outcome{val: v, err: err}
			}()
			<-started

			tc.remove(c)
			close(release)

			select {
			case got := <-first:
				if got.err != nil {
					t.Fatalf("the caller that started the load failed: %v", got.err)
				}
				if got.val.(string) != "stale" {
					t.Fatalf("the caller got %v, want the value its load read", got.val)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the load did not finish")
			}
			if v, ok := c.Get("dir:/x"); ok {
				t.Fatalf("the removal was undone: cached %v", v)
			}
			v, err := c.GetOrLoad(context.Background(), "dir:/x", time.Minute, load)
			if err != nil {
				t.Fatalf("later load: %v", err)
			}
			if v.(string) != "fresh" || calls.Load() != 2 {
				t.Fatalf("later caller got %v after %d loads, want a fresh load", v, calls.Load())
			}
		})
	}
}

// A caller that arrives after a removal must start its own load. Joining the load in flight when
// the removal ran would hand it the pre-removal value.
func TestGetOrLoadStartsFreshLoadAfterRemoval(t *testing.T) {
	c := New(1000, 1<<20, time.Hour)
	defer c.Stop()

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (any, int64, time.Duration, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return "stale", 5, time.Hour, nil
		}
		return "fresh", 5, time.Hour, nil
	}

	firstDone := make(chan any, 1)
	go func() {
		v, _ := c.GetOrLoad(context.Background(), "dir:/x", time.Minute, load)
		firstDone <- v
	}()
	<-started
	c.Delete("dir:/x")

	second := make(chan any, 1)
	go func() {
		v, _ := c.GetOrLoad(context.Background(), "dir:/x", time.Minute, load)
		second <- v
	}()
	select {
	case got := <-second:
		if got != "fresh" {
			t.Fatalf("caller after the removal got %v, want its own load", got)
		}
		if calls.Load() != 2 {
			t.Fatalf("loader called %d times, want a new load after the removal", calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a caller that arrived after the removal joined the load that started before it")
	}
	close(release)
	if got := <-firstDone; got != "stale" {
		t.Fatalf("first caller got %v, want the value its load read", got)
	}
}

func TestStopIdempotent(t *testing.T) {
	c := New(10, 1000, time.Hour)
	c.Stop()
	c.Stop()
}
