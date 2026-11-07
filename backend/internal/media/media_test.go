package media

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

func tool(t *testing.T, name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed", name)
	}
	return p
}

// sample makes a 30 s 720p H.264 file and serves it over HTTP with Range support.
func sample(t *testing.T, ext string, vcodec ...string) string {
	ff := tool(t, "ffmpeg")
	dir := t.TempDir()
	p := filepath.Join(dir, "in."+ext)
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30"}
	args = append(args, vcodec...)
	args = append(args, "-shortest", p)
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("make sample: %v %s", err, out)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, p) }))
	t.Cleanup(ts.Close)
	return ts.URL + "/in." + ext
}

func TestDetectVRName(t *testing.T) {
	cases := map[string]VRInfo{
		"trip_360.mp4":         {Projection: "360", Stereo: "mono", Source: "filename"},
		"concert_VR180_LR.mp4": {Projection: "180", Stereo: "sbs", Source: "filename"},
		"show 360 TB.mkv":      {Projection: "360", Stereo: "tb", Source: "filename"},
		"movie.1080p.mkv":      {Stereo: "mono"},
		"ep180x.mp4":           {Stereo: "mono"},
		"beach_180_sbs.mp4":    {Projection: "180", Stereo: "sbs", Source: "filename"},
		"walk.360.3dh.mp4":     {Projection: "360", Stereo: "sbs", Source: "filename"},
	}
	for name, want := range cases {
		if got := DetectVRName(name); got != want {
			t.Errorf("%s: got %+v want %+v", name, got, want)
		}
	}
}

func TestProbeCaches(t *testing.T) {
	probe := tool(t, "ffprobe")
	url := sample(t, "mkv", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac")
	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p := &Prober{DB: d, FFprobe: probe}
	pr, err := p.Probe(context.Background(), "id:1", "rev1", "clip_360.mkv", url)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Video == nil || pr.Video.CodecName != "h264" || pr.Video.Height != 720 || pr.Audio == nil || pr.Duration < 29 {
		t.Fatalf("probe %+v", pr)
	}
	if pr.VR.Projection != "360" {
		t.Fatalf("vr %+v", pr.VR)
	}
	// Cached: a bad URL still returns the stored result.
	pr2, err := p.Probe(context.Background(), "id:1", "rev1", "clip_360.mkv", "http://127.0.0.1:1/none")
	if err != nil || pr2.Video.CodecName != "h264" {
		t.Fatalf("cache miss: %v", err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM media_probe`).Scan(&n)
	if n != 1 {
		t.Fatal("not stored")
	}
	_ = sql.ErrNoRows
}

func runSession(t *testing.T, mode, ext string, vcodec ...string) {
	ff := tool(t, "ffmpeg")
	url := sample(t, ext, vcodec...)
	m := NewManager(ff, filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "k", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: mode, Height: 480, SrcH: 720})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(Source{Key: "other", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: mode}); err != ErrBusy {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	pl := s.Playlist()
	if !strings.Contains(pl, "#EXT-X-ENDLIST") || strings.Count(pl, "#EXTINF") != 8 || !strings.Contains(pl, "init.mp4") {
		t.Fatalf("playlist:\n%s", pl)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	init, err := s.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(init) == 0 {
		t.Fatal("init segment returned no bytes")
	}
	p0, err := s.Segment(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Seek far ahead: forces a restart at segment 6.
	p6, err := s.Segment(ctx, 6)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range [][]byte{p0, p6} {
		if len(b) == 0 {
			t.Fatalf("segment %d returned no bytes", i)
		}
	}
	if _, err := s.Segment(ctx, 99); !os.IsNotExist(err) {
		t.Fatalf("out of range segment: %v", err)
	}
	dir := s.dir
	s.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("session dir not removed")
	}
	if m.Count() != 0 {
		t.Fatal("session not removed")
	}
	// Slot freed.
	s2, err := m.Open(Source{Key: "other", URL: func(context.Context) (string, error) { return url, nil }, Duration: 30, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestHLSTranscode(t *testing.T) {
	runSession(t, ModeTranscode, "avi", "-c:v", "mpeg4", "-q:v", "5", "-c:a", "mp3")
}

func TestHLSRemux(t *testing.T) {
	runSession(t, ModeRemux, "mkv", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "50", "-c:a", "aac")
}

func TestIdleReaper(t *testing.T) {
	m := NewManager("ffmpeg", filepath.Join(t.TempDir(), "hls"), 2)
	defer m.Close()
	m.Idle = 0
	s, err := m.Open(Source{Key: "k", URL: func(context.Context) (string, error) { return "", nil }, Duration: 10, Mode: ModeTranscode})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastAccess = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m.Count() == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("idle session not reaped")
}
