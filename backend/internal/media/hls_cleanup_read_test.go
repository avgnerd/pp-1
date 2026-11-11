package media

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSegmentReturnsOwnedBytesBeforeCleanupEviction(t *testing.T) {
	payload := []byte("exact bytes for segment 1789")
	for _, tc := range []struct {
		name string
		n    int
	}{
		{name: "forward-1805", n: 1805},
		{name: "backward-1698", n: 1698},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{dir: t.TempDir(), segments: 2000}
			if err := os.WriteFile(s.segPath(1789), payload, 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := s.Segment(context.Background(), 1789)
			if err != nil {
				t.Fatal(err)
			}
			s.cleanupOld(tc.n)
			if !bytes.Equal(got, payload) {
				t.Fatalf("consumer bytes = %q, want %q", got, payload)
			}
			if fileExists(s.segPath(1789)) {
				t.Fatal("eviction did not remove the no-longer-selected segment file")
			}
		})
	}
}

func TestConcurrentSegmentsOwnBytesAcrossCleanupEviction(t *testing.T) {
	payload := []byte("exact concurrent bytes for segment 1789")
	for _, tc := range []struct {
		name string
		n    int
	}{
		{name: "forward-1805", n: 1805},
		{name: "backward-1698", n: 1698},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{
				dir: t.TempDir(), segments: 2000, cmd: &exec.Cmd{}, runStart: 1783,
				runAt: time.Now().Add(time.Hour),
			}
			p := s.segPath(1789)
			type result struct {
				got []byte
				err error
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results := make(chan result, 2)
			for range 2 {
				go func() {
					got, err := s.Segment(ctx, 1789)
					results <- result{got: got, err: err}
				}()
			}

			// Both public requests are now waiting for the same absent segment.
			deadline := time.Now().Add(2 * time.Second)
			for {
				s.mu.Lock()
				pins := s.pins[1789]
				s.mu.Unlock()
				if pins == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("concurrent requests reached %d pins, want 2", pins)
				}
				runtime.Gosched()
			}

			// Publish the completed file and run the eviction while both real requests
			// still hold their pins. startMu prevents either waiter from reading early.
			s.startMu.Lock()
			if err := os.WriteFile(p, payload, 0o600); err != nil {
				s.startMu.Unlock()
				t.Fatal(err)
			}
			s.cleanupOldLocked(tc.n)
			if !fileExists(p) {
				s.startMu.Unlock()
				t.Fatal("cleanup evicted the target while both requests were waiting")
			}
			s.startMu.Unlock()

			for range 2 {
				res := <-results
				if res.err != nil {
					t.Fatal(res.err)
				}
				if !bytes.Equal(res.got, payload) {
					t.Fatalf("consumer bytes = %q, want %q", res.got, payload)
				}
			}
			s.cleanupOld(tc.n)
			if fileExists(p) {
				t.Fatal("eviction did not remove the segment after both requests completed")
			}
		})
	}
}

func TestSegmentDoesNotCompleteOnEmptyIncompleteFile(t *testing.T) {
	s := &Session{dir: t.TempDir(), segments: 2, cmd: &exec.Cmd{}, runStart: 0, runAt: time.Now().Add(time.Hour)}
	if err := os.WriteFile(s.segPath(1), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := s.Segment(ctx, 1)
	if err == nil {
		t.Fatalf("empty incomplete segment returned success with %d bytes", len(got))
	}
}

func TestCloseCancelsSegmentReadBeforeItOwnsBytes(t *testing.T) {
	m := NewManager("", filepath.Join(t.TempDir(), "hls"), 1)
	m.FFmpeg = "ffmpeg"
	defer m.Close()
	s, err := m.Open(Source{Key: "close-waiting-read", Duration: 30, Mode: ModeTranscode})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cmd = &exec.Cmd{}
	s.runStart = 0
	s.mu.Unlock()

	result := make(chan error, 1)
	go func() {
		_, err := s.Segment(context.Background(), 1)
		result <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		pinned := s.pins[1] > 0
		s.mu.Unlock()
		if pinned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("segment request did not enter its pinned wait")
		}
		runtime.Gosched()
	}

	s.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("segment request owned bytes from a session closed during the wait")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("segment request did not wake after Close")
	}
}
