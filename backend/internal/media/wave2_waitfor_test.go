package media

import (
	"context"
	"strings"
	"testing"
)

// exitingCtx runs its hook from inside waitFor's session lock, after waitFor has captured the
// current stderrTail and before it looks at the exited channel. That is the interleaving the exit
// goroutine produces when it records ffmpeg's last stderr lines and closes the run's exited
// channel: the tail read at the top of the wait loop is already stale when the exit is observed.
type exitingCtx struct {
	context.Context
	done   chan struct{}
	onDone func()
}

func (c *exitingCtx) Done() <-chan struct{} {
	if c.onDone != nil {
		c.onDone()
		c.onDone = nil
	}
	return c.done
}

func (c *exitingCtx) Err() error { return nil }

func TestWaitForReportsStderrRecordedAsRunExits(t *testing.T) {
	const late = "late ffmpeg stderr: Invalid data found when processing input"
	s := &Session{dir: t.TempDir(), segments: 4}
	exited := make(chan struct{})
	s.exited = exited
	s.lifeCtx = &exitingCtx{done: make(chan struct{}), onDone: func() {
		// Runs while waitFor holds s.mu, so this is the same critical section the exit
		// goroutine's tail write would take.
		s.stderrTail = late
		close(exited)
	}}

	err := s.waitFor(context.Background(), s.segPath(1))
	if err == nil {
		t.Fatal("waitFor returned nil for a run that exited without the segment")
	}
	if !strings.Contains(err.Error(), late) {
		t.Fatalf("waitFor error = %q, want the stderr recorded as the run exited", err)
	}
}
