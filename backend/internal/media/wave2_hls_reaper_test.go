package media

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Open holds m.mu while it looks a session up, so a session the reaper selects under m.mu+s.mu can
// never be handed out again. The reaper used to only collect the expired sessions under the locks
// and Close them after unlocking, which left every session but the first in its list open to a
// concurrent Open on the same key.
func TestReaperDoesNotHandOutASelectedSession(t *testing.T) {
	const sessions = 12
	const idle = 2 * time.Second
	m := newManager("ffmpeg", filepath.Join(t.TempDir(), "hls"), sessions*4, idle)
	defer m.Close()

	originals := make([]*Session, 0, sessions)
	sources := make([]Source, 0, sessions)
	for i := range sessions {
		src := Source{Key: fmt.Sprintf("reap-race-%02d", i), Client: "tab", Duration: 30, Mode: ModeTranscode}
		s, err := m.Open(src)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		// A run that takes a moment to stop keeps the reaper in its teardown loop, which is
		// the window in which Open used to return a session already selected for closing.
		// The delay starts when teardown cancels the run, i.e. inside Close.
		exit := make(chan struct{})
		s.mu.Lock()
		s.cmd = &exec.Cmd{}
		s.cancel = func() {
			go func() {
				time.Sleep(150 * time.Millisecond)
				close(exit)
			}()
		}
		s.exited = exit
		s.lastAccess = time.Now().Add(-time.Hour)
		s.lastBeat = time.Now().Add(-time.Hour)
		s.mu.Unlock()
		originals = append(originals, s)
		sources = append(sources, src)
	}

	// Wait for the first selected session to be marked closed: the reaper is now inside its
	// teardown loop, so the rest of its list is the vulnerable set.
	deadline := time.Now().Add(10 * time.Second)
	teardown := false
	for time.Now().Before(deadline) && !teardown {
		for _, s := range originals {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				teardown = true
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !teardown {
		t.Fatal("reaper did not start tearing the expired sessions down")
	}

	for i, src := range sources {
		got, err := m.Open(src)
		if err != nil {
			t.Fatalf("Open(%d) during reaper teardown: %v", i, err)
		}
		if !got.Heartbeat() {
			t.Fatalf("Open(%d) returned closed session %s", i, got.ID)
		}
		for _, old := range originals {
			if got == old {
				t.Fatalf("Open(%d) returned session %s the reaper had already selected for reaping", i, got.ID)
			}
		}
	}
}
