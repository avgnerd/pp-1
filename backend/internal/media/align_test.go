package media

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// firstPTS returns the first video PTS of init+segment.
func firstPTS(t *testing.T, probe string, init, seg []byte) float64 {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "s.mp4")
	combined := append(append([]byte(nil), init...), seg...)
	if err := os.WriteFile(tmp, combined, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(probe, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", "-read_intervals", "%+#1", tmp).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(strings.Split(string(bytes.TrimSpace(out)), "\n")[0]), 64)
	return v
}

// Segment n of the playlist must start at about n*SegmentSeconds, also when the source has
// keyframes far apart and the video is copied (remux mode), and after a seek restart.
func TestRemuxLongGOPAlignment(t *testing.T) {
	probe := tool(t, "ffprobe")
	url := sample(t, "mkv", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "250", "-keyint_min", "250", "-sc_threshold", "0", "-c:a", "aac")
	m := NewManager(tool(t, "ffmpeg"), filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "k", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: ModeRemux})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	init, err := s.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check := func(n int, runStart bool) {
		t.Helper()
		p, err := s.Segment(ctx, n)
		if err != nil {
			t.Fatalf("seg%d: %v", n, err)
		}
		pts := firstPTS(t, probe, init, p)
		want := float64(n * SegmentSeconds)
		if runStart {
			// The first segment of a run starts at the keyframe at or before its slot.
			if pts > want+0.05 || pts < want-10.05 {
				t.Errorf("seg%d (run start): got %.2fs, want keyframe in [%.0f-10, %.0f]", n, pts, want, want)
			}
			return
		}
		if pts < want-0.1 || pts > want+0.1 {
			t.Errorf("seg%d: starts at %.2fs, want %.0fs", n, pts, want)
		}
	}
	check(0, true)
	for _, n := range []int{1, 2, 3, 5} {
		check(n, false)
	}
	// Seek far ahead: a new run starts mid-GOP; the following segments stay on the grid.
	s.Close()
	s, err = m.Open(Source{Key: "k2", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: ModeRemux})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Segment(ctx, 6); err != nil {
		t.Fatal(err)
	}
	init, err = s.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check(6, true)
	check(7, false)
}

// Segments left over from an earlier run must not make a fresh run look as if it had already
// passed the requested segment; that restarted ffmpeg on every request after a seek back.
func TestStaleSegmentsDoNotRestartRun(t *testing.T) {
	url := sample(t, "avi", "-c:v", "mpeg4", "-q:v", "5", "-c:a", "mp3")
	m := NewManager(tool(t, "ffmpeg"), filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "k", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: ModeTranscode, Height: 360, SrcH: 720})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	last := s.Segments() - 1
	if _, err := s.Segment(ctx, 0); err != nil {
		t.Fatal(err)
	}
	for !fileExists(s.segPath(last)) {
		if ctx.Err() != nil {
			t.Fatal("first run did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Seek back after the middle was cleaned up: only the stale tail is left on disk.
	for k := 1; k < last; k++ {
		_ = os.Remove(s.segPath(k))
	}
	for k := 1; k < last; k++ {
		if _, err := s.Segment(ctx, k); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		rs := s.runStart
		s.mu.Unlock()
		if rs != 1 {
			t.Fatalf("segment %d restarted ffmpeg (run starts at %d, want 1)", k, rs)
		}
	}
}

// A paused player gives its transcode slot back, so a forgotten tab cannot block everyone
// else; it takes a slot again on resume. Different players never share a session.
func TestPausedSessionFreesSlot(t *testing.T) {
	url := sample(t, "avi", "-c:v", "mpeg4", "-q:v", "5", "-c:a", "mp3")
	m := NewManager(tool(t, "ffmpeg"), filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	m.Idle = 300 * time.Millisecond
	src := Source{Key: "k", Client: "tab1", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: ModeTranscode, Height: 360, SrcH: 720}
	a, err := m.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := a.Segment(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if again, _ := m.Open(src); again != a {
		t.Fatal("the same player should get its session back")
	}
	// The player stays open (paused) for the rest of the test.
	beat := make(chan struct{})
	defer close(beat)
	go func() {
		for {
			select {
			case <-beat:
				return
			case <-time.After(50 * time.Millisecond):
				a.Heartbeat()
			}
		}
	}()
	for i := 0; ; i++ {
		if active, _, _ := m.Limit.Stats(); active == 0 {
			break
		}
		if i > 100 {
			t.Fatal("paused session kept its slot")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for k := 1; k < a.Segments(); k++ {
		_ = os.Remove(a.segPath(k))
	}
	src.Client = "tab2"
	b, err := m.Open(src)
	if err != nil {
		t.Fatalf("second player blocked by a paused one: %v", err)
	}
	if b == a {
		t.Fatal("two players share one session")
	}
	if _, err := b.Segment(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Segment(ctx, 5); !errors.Is(err, ErrBusy) {
		t.Fatalf("resume while the slot is taken: got %v, want ErrBusy", err)
	}
	b.Close()
	if _, err := a.Segment(ctx, 5); err != nil {
		t.Fatalf("resume after the slot was freed: %v", err)
	}
}

// Audio that outlasts the video gets no segments of its own: ffmpeg cuts on video packets.
// The playlist must end with the picture, and a segment past the real end must fail fast.
func TestAudioLongerThanVideo(t *testing.T) {
	ff := tool(t, "ffmpeg")
	probe := tool(t, "ffprobe")
	p := filepath.Join(t.TempDir(), "in.mkv")
	if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25:duration=20",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=20.3",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", p).CombinedOutput(); err != nil {
		t.Fatalf("make sample: %v %s", err, out)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, p) }))
	defer ts.Close()
	url := ts.URL + "/in.mkv"
	out, err := exec.Command(probe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", url).Output()
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parseProbe(out, "in.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Video == nil || pr.Video.End < 19.9 || pr.Video.End > 20.1 || pr.Duration < 20.25 {
		t.Fatalf("video end %+v, duration %.3f", pr.Video, pr.Duration)
	}
	for _, mode := range []string{ModeRemux, ModeTranscode} {
		for _, c := range []struct {
			end  float64
			name string
		}{{pr.Video.End, "video end"}, {pr.Duration, "file duration"}} {
			m := NewManager(ff, filepath.Join(t.TempDir(), "hls"), 1)
			s, err := m.Open(Source{Key: "k", URL: func(context.Context) (string, error) { return url, nil }, Duration: c.end, Mode: mode, Height: 180, SrcH: 180})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			for n := 0; n < s.Segments(); n++ {
				_, err := s.Segment(ctx, n)
				switch {
				case n < 5 && err != nil:
					t.Errorf("%s, %s: segment %d: %v", mode, c.name, n, err)
				case n >= 5 && !errors.Is(err, ErrPastEnd):
					t.Errorf("%s, %s: segment %d past the end: got %v, want ErrPastEnd", mode, c.name, n, err)
				}
			}
			if c.name == "video end" && s.Segments() != 5 {
				t.Errorf("%s: playlist has %d segments, want 5", mode, s.Segments())
			}
			if c.name == "file duration" {
				start := time.Now()
				if _, err := s.Segment(ctx, s.Segments()-1); !errors.Is(err, ErrPastEnd) || time.Since(start) > time.Second {
					t.Errorf("%s: repeated past-end request: %v after %v", mode, err, time.Since(start))
				}
			}
			cancel()
			m.Close()
		}
	}
}

// A paused player makes no segment requests. The session must survive that (the player sends
// heartbeats) and must not wait forever for a segment that was cleaned up.
func TestPausedSessionSurvivesWithHeartbeat(t *testing.T) {
	url := sample(t, "avi", "-c:v", "mpeg4", "-q:v", "5", "-c:a", "mp3")
	m := NewManager(tool(t, "ffmpeg"), filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	m.Idle = 300 * time.Millisecond
	s, err := m.Open(Source{Key: "k", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: ModeTranscode, Height: 360, SrcH: 720})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := s.Segment(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// "Paused": no segment requests, only heartbeats, for longer than the idle timeout.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Heartbeat() {
			t.Fatal("session closed while the player was still open")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, ok := m.Get(s.ID); !ok {
		t.Fatal("session reaped despite heartbeats")
	}
	if _, err := s.Segment(ctx, 3); err != nil {
		t.Fatalf("resume after pause: %v", err)
	}
}
