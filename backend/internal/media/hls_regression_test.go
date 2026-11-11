package media

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("HLS_LOOKUP_HELPER") == "1" {
		f, err := os.OpenFile(os.Getenv("HLS_LOOKUP_HELPER_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strings.Join(os.Args, "\x00") + "\n")
			_ = f.Close()
		}
		os.Exit(1)
	}
	if os.Getenv("HLS_INIT_RESTART_HELPER") == "1" {
		os.Exit(hlsInitRestartHelper())
	}
	os.Exit(m.Run())
}

func TestConcurrentOpenSamePlayerIsAtomic(t *testing.T) {
	root := t.TempDir()
	for attempt := 0; attempt < 100; attempt++ {
		m := NewManager("", filepath.Join(root, "attempt", "hls"), 2)
		m.FFmpeg = "ffmpeg"
		src := Source{Key: "same-file-rev", Client: "same-player", Duration: 30, Mode: ModeTranscode}
		gate := make(chan struct{})
		var got [2]*Session
		var errs [2]error
		var wg sync.WaitGroup
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				got[i], errs[i] = m.Open(src)
			}()
		}
		close(gate)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			m.Close()
			t.Fatalf("attempt %d: Open errors: %v, %v", attempt, errs[0], errs[1])
		}
		if got[0] != got[1] {
			m.Close()
			t.Fatalf("attempt %d: simultaneous opens returned different sessions", attempt)
		}
		if n := m.Count(); n != 1 {
			m.Close()
			t.Fatalf("attempt %d: %d sessions after simultaneous opens, want 1", attempt, n)
		}
		if active, _, _ := m.Limit.Stats(); active != 1 {
			m.Close()
			t.Fatalf("attempt %d: %d active slots after simultaneous opens, want 1", attempt, active)
		}
		m.Close()
		if active, _, _ := m.Limit.Stats(); active != 0 {
			t.Fatalf("attempt %d: %d active slots after manager close", attempt, active)
		}
	}
}

func TestDifferentPlayersKeepIndependentSessions(t *testing.T) {
	m := NewManager("", filepath.Join(t.TempDir(), "hls"), 2)
	m.FFmpeg = "ffmpeg"
	defer m.Close()
	src := Source{Key: "same-file-rev", Client: "tab1", Duration: 30, Mode: ModeTranscode}
	a, err := m.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	src.Client = "tab2"
	b, err := m.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("different players shared one session")
	}
	if n := m.Count(); n != 2 {
		t.Fatalf("manager has %d sessions, want 2", n)
	}
	a.Close()
	if _, ok := m.Get(b.ID); !ok {
		t.Fatal("closing one player removed another player's session")
	}
}

func makeIrregularGOPSample(t *testing.T) string {
	t.Helper()
	ff := tool(t, "ffmpeg")
	p := filepath.Join(t.TempDir(), "irregular-gop.mkv")
	if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "250",
		"-keyint_min", "250", "-sc_threshold", "0", "-force_key_frames", "22",
		"-c:a", "aac", "-shortest", p).CombinedOutput(); err != nil {
		t.Fatalf("make irregular-GOP sample: %v %s", err, out)
	}
	return p
}

func TestRemuxSeekLookupTransientFailurePreservesTimeline(t *testing.T) {
	probe := tool(t, "ffprobe")
	p := makeIrregularGOPSample(t)

	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusServiceUnavailable)
			return
		}
		http.ServeFile(w, r, p)
	}))
	defer ts.Close()

	m := NewManager(tool(t, "ffmpeg"), filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "seek-transient", Duration: 30, Mode: ModeRemux,
		URL: func(context.Context) (string, error) { return ts.URL + "/irregular-gop.mkv", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p6, err := s.Segment(ctx, 6)
	if err != nil {
		t.Fatal(err)
	}
	p7, err := s.Segment(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	init, err := s.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pts6 := firstPTS(t, probe, init, p6)
	pts7 := firstPTS(t, probe, init, p7)
	t.Logf("http_requests=%d segment6_first_pts=%.3f segment7_first_pts=%.3f", requests.Load(), pts6, pts7)
	if pts6 < 23.8 || pts6 > 24.2 || pts7 < 27.8 || pts7 > 28.2 {
		t.Fatalf("seek timeline drifted: segment 6/7 start at %.3f/%.3f s, want 24.000/28.000", pts6, pts7)
	}
}

func TestRemuxSeekLookupFailureDoesNotStartRun(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ffmpeg-calls")
	t.Setenv("HLS_LOOKUP_HELPER", "1")
	t.Setenv("HLS_LOOKUP_HELPER_LOG", logPath)

	m := NewManager(tool(t, "ffmpeg"), filepath.Join(t.TempDir(), "hls"), 1)
	m.FFmpeg = os.Args[0]
	defer m.Close()
	s, err := m.Open(Source{Key: "seek-unavailable", Duration: 30, Mode: ModeRemux,
		URL: func(context.Context) (string, error) { return "http://example.invalid/media.mkv", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Segment(ctx, 6); err == nil {
		t.Fatal("seek started a copied run without knowing its keyframe")
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 3 {
		t.Fatalf("ffmpeg was invoked %d times, want exactly three keyframe lookups and no run: %q", len(calls), calls)
	}
	for i, args := range calls {
		if !strings.Contains(args, "\x00-frames:v\x001\x00-f\x00framecrc\x00-") {
			t.Fatalf("ffmpeg call %d was not a keyframe lookup: %q", i+1, args)
		}
	}
	if fileExists(s.segPath(6)) {
		t.Fatal("failed keyframe lookup still produced segment 6")
	}
	if active, _, _ := m.Limit.Stats(); active != 0 {
		t.Fatalf("failed seek left %d slots active", active)
	}
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil {
		t.Fatal("failed keyframe lookup left ffmpeg running")
	}
}

func TestCloseSerializesInFlightStart(t *testing.T) {
	m := NewManager("", filepath.Join(t.TempDir(), "hls"), 1)
	m.FFmpeg = "ffmpeg"
	defer m.Close()

	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	src := Source{Key: "close-race", Duration: 30, Mode: ModeTranscode,
		URL: func(ctx context.Context) (string, error) {
			close(entered)
			select {
			case <-ctx.Done():
				close(canceled)
				<-release
				return "", ctx.Err()
			case <-release:
				return "", errors.New("start callback released without cancellation")
			}
		}}
	s, err := m.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := s.Segment(context.Background(), 0)
		result <- err
	}()
	<-entered
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()

	var canceledInFlight bool
	select {
	case <-canceled:
		canceledInFlight = true
	case <-time.After(2 * time.Second):
	}
	close(release)
	<-closed
	if err := <-result; err == nil {
		t.Fatal("in-flight segment request succeeded after Close")
	}
	if !canceledInFlight {
		t.Fatal("Close returned without canceling the in-flight start")
	}
	if active, _, _ := m.Limit.Stats(); active != 0 {
		t.Fatalf("slot leaked after Close: %d active", active)
	}
	if n := m.Count(); n != 0 {
		t.Fatalf("session remained registered after Close: %d", n)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("session directory remained after Close: %v", err)
	}
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil {
		t.Fatal("ffmpeg process remained owned by the closed session")
	}
}
