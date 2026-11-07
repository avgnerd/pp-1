package media

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestInitReturnsImmediatelyAfterActiveRunCleanup(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		dir:      dir,
		segments: 21,
		cmd:      &exec.Cmd{},
		runStart: 0,
	}
	for _, n := range []int{0, 1, 20} {
		if err := os.WriteFile(s.segPath(n), []byte("completed segment"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(s.initPath(), []byte("completed init"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := s.Segment(ctx, 20); err != nil {
		t.Fatalf("serve completed segment 20: %v", err)
	}
	if fileExists(s.segPath(1)) {
		t.Fatal("cleanup retained an unrelated old segment from the active run")
	}

	initPath, err := s.Init(ctx)
	if err != nil {
		t.Fatalf("Init after serving segment 20 reset waiting on the evicted first segment: %v", err)
	}
	if string(initPath) != "completed init" {
		t.Fatalf("Init returned %q, want completed init bytes", initPath)
	}
}
