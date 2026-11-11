package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimitAndQueue(t *testing.T) {
	l := New(3)
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire(context.Background()); err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			cur.Add(-1)
			l.Release()
		}()
	}
	wg.Wait()
	if peak.Load() > 3 {
		t.Fatalf("peak %d", peak.Load())
	}
	if a, w, _ := l.Stats(); a != 0 || w != 0 {
		t.Fatalf("leftover %d %d", a, w)
	}
}

func TestCancelWhileQueued(t *testing.T) {
	l := New(1)
	l.Acquire(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Acquire(ctx); err == nil {
		t.Fatal("expected timeout")
	}
	l.Release()
	if !l.TryAcquire() {
		t.Fatal("slot leaked after cancelled waiter")
	}
}

func TestRaiseLimitWakes(t *testing.T) {
	l := New(0)
	done := make(chan struct{})
	go func() { l.Acquire(context.Background()); close(done) }()
	time.Sleep(10 * time.Millisecond)
	l.SetLimit(1)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter not woken")
	}
}
