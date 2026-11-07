// Package limiter is a counting semaphore whose limit can change at runtime.
package limiter

import (
	"container/list"
	"context"
	"sync"
)

// Limiter bounds concurrent work. Waiters are served FIFO.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	active  int
	waiters list.List // of chan struct{}
}

// New creates a limiter with the given limit (minimum 1... 0 means "nothing may run").
func New(limit int) *Limiter { return &Limiter{limit: limit} }

// SetLimit changes the limit and wakes waiters if capacity grew.
func (l *Limiter) SetLimit(n int) {
	l.mu.Lock()
	l.limit = n
	l.wake()
	l.mu.Unlock()
}

// TryAcquire takes a slot without waiting.
func (l *Limiter) TryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active < l.limit && l.waiters.Len() == 0 {
		l.active++
		return true
	}
	return false
}

// Acquire waits for a slot or the context.
func (l *Limiter) Acquire(ctx context.Context) error {
	l.mu.Lock()
	if l.active < l.limit && l.waiters.Len() == 0 {
		l.active++
		l.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	el := l.waiters.PushBack(ch)
	l.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		select {
		case <-ch:
			// Granted concurrently with cancellation: give the slot back.
			l.active--
			l.wake()
		default:
			l.waiters.Remove(el)
		}
		l.mu.Unlock()
		return ctx.Err()
	}
}

// Release frees a slot.
func (l *Limiter) Release() {
	l.mu.Lock()
	l.active--
	l.wake()
	l.mu.Unlock()
}

// Stats returns active and waiting counts.
func (l *Limiter) Stats() (active, waiting, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.waiters.Len(), l.limit
}

func (l *Limiter) wake() {
	for l.active < l.limit && l.waiters.Len() > 0 {
		el := l.waiters.Front()
		l.waiters.Remove(el)
		l.active++
		close(el.Value.(chan struct{}))
	}
}
