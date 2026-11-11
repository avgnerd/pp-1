package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const (
	initRestartHelper      = "HLS_INIT_RESTART_HELPER"
	initRestartMode        = "HLS_INIT_RESTART_MODE"
	initRestartInitBytes   = "HLS_INIT_RESTART_INIT"
	initRestartSegBytes    = "HLS_INIT_RESTART_SEG"
	initRestartReadyPath   = "HLS_INIT_RESTART_READY"
	initRestartReleasePath = "HLS_INIT_RESTART_RELEASE"
)

func hlsInitRestartHelper() int {
	for _, arg := range os.Args[1:] {
		if arg == "-h" {
			return 0
		}
	}
	start := -1
	for i, arg := range os.Args[1:] {
		if arg == "-start_number" && i+2 < len(os.Args) {
			start, _ = strconv.Atoi(os.Args[i+2])
		}
	}
	switch os.Getenv(initRestartMode) {
	case "cross-start":
		switch start {
		case 0:
			time.Sleep(24 * time.Hour)
			return 2
		case 7:
			if !writeInitRestartFile("init.mp4", initRestartInitBytes) {
				return 2
			}
			if !writeInitRestartFile("seg7.m4s", initRestartSegBytes) {
				return 2
			}
			return 0
		}
	case "same-start":
		if start != 0 || !writeInitRestartFile("init.mp4", initRestartInitBytes) {
			return 2
		}
		ready, release := os.Getenv(initRestartReadyPath), os.Getenv(initRestartReleasePath)
		if ready == "" || release == "" || os.WriteFile(ready, []byte("ready"), 0o600) != nil {
			return 2
		}
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		timeout := time.NewTimer(10 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := os.Stat(release); err == nil {
					return 1
				}
			case <-timeout.C:
				return 2
			}
		}
	case "completed":
		if start != 0 || !writeInitRestartFile("init.mp4", initRestartInitBytes) {
			return 2
		}
		if !writeInitRestartFile("seg0.m4s", initRestartSegBytes) {
			return 2
		}
		return 0
	}
	return 2
}

func writeInitRestartFile(name, valueEnv string) bool {
	value, ok := os.LookupEnv(valueEnv)
	return ok && os.WriteFile(name, []byte(value), 0o600) == nil
}

func waitInitRestartCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		<-ticker.C
	}
}

func TestInitRetargetsAfterPublicSegmentRestart(t *testing.T) {
	const (
		newInit = "init bytes from run 7"
		newSeg  = "segment 7 bytes from run 7"
	)
	t.Setenv(initRestartHelper, "1")
	t.Setenv(initRestartMode, "cross-start")
	t.Setenv(initRestartInitBytes, newInit)
	t.Setenv(initRestartSegBytes, newSeg)

	m := NewManager(os.Args[0], filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "init-restart", Duration: 60, Mode: ModeTranscode,
		URL: func(context.Context) (string, error) { return "fake-input", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type initResult struct {
		got []byte
		err error
	}
	initDone := make(chan initResult, 1)
	go func() {
		got, err := s.Init(ctx)
		initDone <- initResult{got: got, err: err}
	}()

	waitInitRestartCondition(t, "run 0 Init wait", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cmd != nil && s.runStart == 0 && s.pins[0] > 0
	})
	gotSeg, err := s.Segment(ctx, 7)
	if err != nil {
		t.Fatalf("public Segment(7) after restart: %v", err)
	}
	if string(gotSeg) != newSeg {
		t.Fatalf("Segment(7) = %q, want %q", gotSeg, newSeg)
	}
	if fileExists(s.segPath(0)) {
		t.Fatal("old run unexpectedly produced segment 0")
	}

	select {
	case result := <-initDone:
		if result.err != nil {
			t.Fatalf("Init after public Segment restart: %v", result.err)
		}
		if string(result.got) != newInit {
			t.Fatalf("Init = %q, want %q", result.got, newInit)
		}
	case <-ctx.Done():
		t.Fatalf("Init did not retarget the run started by public Segment: %v", ctx.Err())
	}
}

func TestInitRejectsStaleFirstSegmentFromSameStartReplacement(t *testing.T) {
	const newInit = "fresh init from replacement run 0"
	t.Setenv(initRestartHelper, "1")
	t.Setenv(initRestartMode, "same-start")
	t.Setenv(initRestartInitBytes, newInit)
	readyPath, err := filepath.Abs(filepath.Join(t.TempDir(), "helper-ready"))
	if err != nil {
		t.Fatal(err)
	}
	releasePath, err := filepath.Abs(filepath.Join(t.TempDir(), "helper-release"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(initRestartReadyPath, readyPath)
	t.Setenv(initRestartReleasePath, releasePath)

	m := NewManager(os.Args[0], filepath.Join(t.TempDir(), "hls"), 1)
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0o600)
		m.Close()
	})
	s, err := m.Open(Source{Key: "same-start-init", Duration: 60, Mode: ModeTranscode,
		URL: func(context.Context) (string, error) { return "fake-input", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.segPath(0), s.initPath()} {
		if err := os.WriteFile(p, []byte("old generation"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.start(0); err != nil {
		t.Fatal(err)
	}
	waitInitRestartCondition(t, "fresh init from replacement run", func() bool {
		got, err := os.ReadFile(readyPath)
		return err == nil && string(got) == "ready"
	})
	if got, err := os.ReadFile(s.initPath()); err != nil || string(got) != newInit {
		t.Fatalf("replacement init = %q, %v; want exact new bytes", got, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	got, err := s.Init(ctx)
	if err == nil {
		t.Fatalf("Init returned stale same-start generation as ready: %q", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Init error = %v, want deadline while no fresh first segment exists", err)
	}
	if got, err := os.ReadFile(s.segPath(0)); err != nil || string(got) != "old generation" {
		t.Fatalf("stale segment fixture changed: %q, %v", got, err)
	}
}

func TestInitUsesCompletedRunAfterFirstSegmentCleanup(t *testing.T) {
	const (
		initBytes = "completed run init"
		segBytes  = "completed run segment 0"
	)
	t.Setenv(initRestartHelper, "1")
	t.Setenv(initRestartMode, "completed")
	t.Setenv(initRestartInitBytes, initBytes)
	t.Setenv(initRestartSegBytes, segBytes)

	m := NewManager(os.Args[0], filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "completed-init", Duration: 60, Mode: ModeTranscode,
		URL: func(context.Context) (string, error) { return "fake-input", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.start(0); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("completed helper run did not exit")
	}
	if err := os.Remove(s.segPath(0)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := s.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != initBytes {
		t.Fatalf("Init = %q, want completed run init %q", got, initBytes)
	}
}

func TestCloseWakesInitRestartWait(t *testing.T) {
	t.Setenv(initRestartHelper, "1")
	t.Setenv(initRestartMode, "cross-start")
	t.Setenv(initRestartInitBytes, "unused init")
	t.Setenv(initRestartSegBytes, "unused segment")
	m := NewManager(os.Args[0], filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "close-init", Duration: 60, Mode: ModeTranscode,
		URL: func(context.Context) (string, error) { return "fake-input", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := s.Init(ctx)
		result <- err
	}()
	waitInitRestartCondition(t, "run 0 Init wait", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cmd != nil && s.runStart == 0 && s.pins[0] > 0
	})
	s.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Init succeeded after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Init did not wake after Close")
	}
}
