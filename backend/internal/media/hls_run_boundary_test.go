package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runBoundaryFile is the name the session writes its run-start boundary to. It is written as a
// literal so this test also compiles and reproduces the failure against a build that reads the
// boundary from the wall clock instead of the session filesystem.
const runBoundaryFile = ".run-start"

// TestInitAcceptsRunOutputStampedAtFilesystemTick replays a filesystem whose timestamps advance in
// coarser ticks than the wall clock: the helper writes init.mp4 and seg0.m4s, then stamps both at
// the boundary the filesystem recorded for this run, which is a timestamp at or before the wall
// clock reading the run started at. Init must still take those files as this run's output.
func TestInitAcceptsRunOutputStampedAtFilesystemTick(t *testing.T) {
	const (
		initBytes = "run init stamped at the filesystem tick"
		segBytes  = "run segment 0 stamped at the filesystem tick"
	)
	t.Setenv(initRestartHelper, "1")
	t.Setenv(initRestartMode, "coarse-tick")
	t.Setenv(initRestartInitBytes, initBytes)
	t.Setenv(initRestartSegBytes, segBytes)
	helperDir := t.TempDir()
	readyPath := filepath.Join(helperDir, "helper-ready")
	releasePath := filepath.Join(helperDir, "helper-release")
	t.Setenv(initRestartReadyPath, readyPath)
	t.Setenv(initRestartReleasePath, releasePath)

	m := NewManager(os.Args[0], filepath.Join(t.TempDir(), "hls"), 1)
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0o600)
		m.Close()
	})
	s, err := m.Open(Source{Key: "coarse-tick-init", Duration: 60, Mode: ModeTranscode,
		URL: func(context.Context) (string, error) { return "fake-input", nil }})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := s.Init(ctx)
	if err != nil {
		t.Fatalf("Init rejected the run's own output: %v", err)
	}
	if string(got) != initBytes {
		t.Fatalf("Init = %q, want %q", got, initBytes)
	}
}
