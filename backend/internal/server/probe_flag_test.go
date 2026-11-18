package server

import (
	"net/http"
	"testing"
	"time"
)

// A failed probe must report the same transcode availability as a successful one. With transcoding
// disabled by the concurrency setting, the failure branch used to advertise it anyway.
func TestProbeFailureBranchReportsSameTranscodeFlag(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	e.fake.Account("app1").Put("/v.mp4", []byte("x"), time.Now())
	e.setSetting("transcode_concurrency", 0)
	e.srv.HLS.FFmpeg = "ffmpeg"
	e.srv.Prober.FFprobe = "dvfs-test-missing-ffprobe"

	var res struct {
		ProbeError string `json:"probe_error"`
		Transcode  bool   `json:"transcode"`
	}
	status := e.do("GET", "/api/media/probe?path=/m/v.mp4", nil, &res)
	if status != http.StatusOK {
		t.Fatalf("probe status = %d, want 200", status)
	}
	if res.ProbeError == "" {
		t.Fatalf("probe did not fail: %+v", res)
	}
	if res.Transcode {
		t.Fatalf("transcode = true after a probe failure with transcode_concurrency 0")
	}
}
