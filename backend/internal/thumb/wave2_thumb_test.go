package thumb

import (
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// A full batch and its timer can cross: the timer fires, Stop returns false, the full batch takes
// the key and runs, and a new batch is installed before the old callback gets the lock. The callback
// must leave that new batch alone.
func TestFlushTimerOnlyFlushesItsOwnBatch(t *testing.T) {
	s, err := New(nil, t.TempDir(), func() int64 { return 1 << 20 })
	if err != nil {
		t.Fatal(err)
	}
	// Client then reports ErrNotFound instead of dereferencing a nil manager, so a batch that
	// wrongly runs is visible on its done channel.
	s.st = &storage.Manager{}
	const key = "7|w256h256"

	oldDone := make(chan result, 1)
	old := &batch{reqs: []request{{path: "/old.jpg", done: oldDone}}}
	freshDone := make(chan result, 1)
	fresh := &batch{reqs: []request{{path: "/fresh.jpg", done: freshDone}}}

	s.mu.Lock()
	s.pending[key] = fresh
	s.mu.Unlock()

	s.flush(key, old, 7, "w256h256")

	s.mu.Lock()
	pending := s.pending[key]
	s.mu.Unlock()
	if pending != fresh {
		t.Fatalf("flush of the superseded batch removed the replacement: pending = %p, want %p", pending, fresh)
	}
	select {
	case r := <-oldDone:
		t.Fatalf("flush ran the superseded batch: %+v", r)
	default:
	}
	select {
	case r := <-freshDone:
		t.Fatalf("flush ran the replacement batch before its timer: %+v", r)
	default:
	}
}
