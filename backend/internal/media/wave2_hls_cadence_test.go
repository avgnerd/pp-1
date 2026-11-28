package media

import (
	"context"
	"math"
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

// TestTranscodeSeekKeepsFourSecondCadence runs a real seek transcode and reads the muxer's own
// playlist. With the startNum offset in force_key_frames, ffmpeg n9 treats t as run-relative and
// writes the remaining media as a 10 s first segment followed by a 6 s one. The expression must
// instead cut at n_forced*4, which gives the first segment a 4 s EXTINF.
func TestTranscodeSeekKeepsFourSecondCadence(t *testing.T) {
	ff := tool(t, "ffmpeg")
	mediaPath := filepath.Join(t.TempDir(), "cadence.mp4")
	if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=40",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-g", "100", "-keyint_min", "100", "-sc_threshold", "0",
		"-c:a", "aac", "-shortest", mediaPath).CombinedOutput(); err != nil {
		t.Fatalf("make cadence sample: %v %s", err, out)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, mediaPath)
	}))
	defer ts.Close()

	m := NewManager(ff, filepath.Join(t.TempDir(), "hls"), 1)
	defer m.Close()
	s, err := m.Open(Source{Key: "cadence", Duration: 40, Mode: ModeTranscode, Height: 360, SrcH: 360,
		URL: func(context.Context) (string, error) { return ts.URL + "/cadence.mp4", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := s.Segment(ctx, 6); err != nil {
		t.Fatalf("segment 6: %v", err)
	}

	playlistPath := filepath.Join(s.dir, "live.m3u8")
	var playlist string
	for {
		b, err := os.ReadFile(playlistPath)
		if err == nil {
			playlist = string(b)
			if strings.Contains(playlist, "#EXTINF:") {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("live.m3u8 never got an EXTINF: %v (last playlist: %q)", ctx.Err(), playlist)
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Logf("live.m3u8 after segment 6:\n%s", playlist)

	dur, err := firstEXTINFDuration(playlist)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(dur-SegmentSeconds) > 0.5 {
		t.Fatalf("first EXTINF = %.3fs, want about %ds; playlist:\n%s", dur, SegmentSeconds, playlist)
	}
}

func firstEXTINFDuration(playlist string) (float64, error) {
	for _, line := range strings.Split(playlist, "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		return strconv.ParseFloat(v, 64)
	}
	return 0, os.ErrNotExist
}
