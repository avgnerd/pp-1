package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A run that reads the whole input exits 0 and its last segment tells the exit path where the
// media ends. Cleanup can delete that tail while the run is still alive; the deleted files are
// still part of the media, so cleanup must leave the run's high-water evidence behind or the exit
// path marks the media as ended early and every later request for those segments gets ErrPastEnd.
func TestCleanupOfLiveRunTailDoesNotCutTheMediaEnd(t *testing.T) {
	const segments = 94
	helperDir := t.TempDir()
	ready := filepath.Join(helperDir, "ready")
	release := filepath.Join(helperDir, "release")
	t.Setenv(wave2Helper, "1")
	t.Setenv(wave2HelperMode, "segments")
	t.Setenv(wave2HelperReady, ready)
	t.Setenv(wave2HelperRelease, release)

	m := NewManager(os.Args[0], filepath.Join(t.TempDir(), "hls"), 1)
	t.Cleanup(func() {
		// Never leave the fake run waiting for a release the test failed to write.
		_ = os.WriteFile(release, []byte("release"), 0o600)
		m.Close()
	})
	s, err := m.Open(Source{Key: "run-end-cleanup", Duration: float64(segments) * SegmentSeconds,
		Mode: ModeTranscode, URL: func(context.Context) (string, error) { return "fake-input", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if s.segments != segments {
		t.Fatalf("session has %d segments, want %d", s.segments, segments)
	}
	if err := s.start(0); err != nil {
		t.Fatal(err)
	}
	waitWave2File(t, ready)
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()

	// A player still on segment 0 cleans up everything more than 90 segments ahead of it while
	// the run that produced those segments is alive.
	s.startMu.Lock()
	s.cleanupOldLocked(0)
	s.startMu.Unlock()
	for n := 91; n < segments; n++ {
		if fileExists(s.segPath(n)) {
			t.Fatalf("cleanup kept seg%d, the interleaving needs the tail gone", n)
		}
	}

	// The run reaches the end of the input and exits 0.
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("fake run did not exit")
	}

	s.mu.Lock()
	endSeg := s.endSeg
	s.mu.Unlock()
	if endSeg != 0 {
		t.Fatalf("endSeg = %d after cleanup removed a live run's tail, want 0: the media has %d segments", endSeg, segments)
	}

	// The segments the run wrote are real media; put the cleaned-up tail back as the run wrote
	// it and require Segment to serve it instead of rejecting it as past the end.
	for n := 91; n < segments; n++ {
		if err := os.WriteFile(s.segPath(n), []byte(fmt.Sprintf("wave2 segment %d", n)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := s.Segment(ctx, 91)
	if err != nil {
		t.Fatalf("Segment(91) after the run ended: %v", err)
	}
	if string(got) != "wave2 segment 91" {
		t.Fatalf("Segment(91) = %q, want the run's own segment", got)
	}
}
