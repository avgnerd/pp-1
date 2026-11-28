package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ffmpeg keeps writing diagnostics to stderr until it exits. The reader stops at the scanner's
// token/byte limits, so without draining the pipe afterwards a failed media can leave the child
// blocked in write while Wait waits for it to exit.
func TestStartDrainsStderrBeforeWait(t *testing.T) {
	t.Setenv(wave2Helper, "1")
	t.Setenv(wave2HelperMode, "stderr-flood")

	m := NewManager(os.Args[0], filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "stderr-flood", Duration: 30, Mode: ModeTranscode,
		URL: func(context.Context) (string, error) { return "fake-input", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	init, err := s.Init(ctx)
	if err != nil {
		t.Fatalf("Init did not finish after %v: %v", time.Since(started).Round(time.Millisecond), err)
	}
	if string(init) != "wave2 init" {
		t.Fatalf("Init = %q, want the bytes the fake run wrote after its stderr", init)
	}
	seg, err := s.Segment(ctx, 0)
	if err != nil {
		t.Fatalf("Segment(0) after the fake run exited: %v", err)
	}
	if string(seg) != "wave2 segment 0" {
		t.Fatalf("Segment(0) = %q, want the fake run's segment", seg)
	}
}
