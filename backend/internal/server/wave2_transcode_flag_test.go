package server

import (
	"testing"
	"time"
)

// The player offers transcode when this flag is set, and opening a transcode session needs both
// ffmpeg and ffprobe. With ffmpeg alone the flag used to be true, and every HLS open answered 500
// because the probe could not report a duration.
func TestTranscodeFlagRequiresEveryMediaTool(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	e.fake.Account("app1").Put("/v.mp4", []byte("x"), time.Now())
	e.setSetting("transcode_concurrency", 4)
	e.srv.HLS.FFmpeg = "ffmpeg"
	e.srv.Prober.FFprobe = ""

	var cfg struct {
		Transcode bool `json:"transcode"`
	}
	if st := e.do("GET", "/api/config", nil, &cfg); st != 200 {
		t.Fatalf("/api/config status = %d, want 200", st)
	}
	if cfg.Transcode {
		t.Fatalf("/api/config reported transcode = true with ffmpeg set, ffprobe missing and concurrency 4")
	}
	var probe struct {
		Transcode bool `json:"transcode"`
	}
	if st := e.do("GET", "/api/media/probe?path=/m/v.mp4", nil, &probe); st != 200 {
		t.Fatalf("/api/media/probe status = %d, want 200", st)
	}
	if probe.Transcode {
		t.Fatalf("/api/media/probe reported transcode = true with ffmpeg set, ffprobe missing and concurrency 4")
	}

	// Guard against a hard-coded false: with both tools configured the same endpoints advertise
	// transcoding again.
	e.srv.Prober.FFprobe = "dvfs-test-missing-ffprobe"
	if st := e.do("GET", "/api/config", nil, &cfg); st != 200 || !cfg.Transcode {
		t.Fatalf("/api/config transcode = %v (status %d) with both tools configured, want true", cfg.Transcode, st)
	}
	if st := e.do("GET", "/api/media/probe?path=/m/v.mp4", nil, &probe); st != 200 || !probe.Transcode {
		t.Fatalf("/api/media/probe transcode = %v (status %d) with both tools configured, want true", probe.Transcode, st)
	}
}
