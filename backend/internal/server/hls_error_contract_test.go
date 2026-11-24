package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/dvfs/dropbox-vfs-manager/internal/media"
)

// TestHLSErrorContract pins what the player branches on. A segment request past the real end of
// the media and a request for a session that is gone both answer 410, so the JSON code is the only
// thing telling them apart: past_end means the media ended and the session is still alive;
// session_gone means the player must open a new session. Retrying either one is wrong, and a
// client that reacts to the status alone reopens a session that would end at the same place.
func TestHLSErrorContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantTag  string
	}{
		{"past the end of the media", media.ErrPastEnd, http.StatusGone, "past_end"},
		{"transcode slots full", media.ErrBusy, http.StatusServiceUnavailable, "busy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/media/hls/id/seg3.m4s", nil)
			hlsFail(c, tc.err)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}
			var body struct{ Error, Code string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", w.Body.String(), err)
			}
			if body.Code != tc.wantTag {
				t.Fatalf("code = %q, want %q (body %q)", body.Code, tc.wantTag, w.Body.String())
			}
			if body.Error == "" {
				t.Fatalf("body %q carries no message for the player", w.Body.String())
			}
		})
	}

	// A session this server no longer has answers its own code, so the three outcomes stay
	// distinguishable however the client reaches them.
	e := newEnv(t)
	e.login()
	var gone struct{ Code string }
	if st := e.do("GET", "/api/media/hls/gone/seg3.m4s", nil, &gone); st != http.StatusGone || gone.Code != "session_gone" {
		t.Fatalf("segment of a gone session = %d %+v, want 410 session_gone", st, gone)
	}
	if gone.Code == "past_end" {
		t.Fatalf("gone session reported as %q", gone.Code)
	}
	var failed struct{ Code string }
	if st := e.do("GET", "/api/media/hls/gone/init.mp4", nil, &failed); st != http.StatusGone || failed.Code != "session_gone" {
		t.Fatalf("init of a gone session = %d %+v, want 410 session_gone", st, failed)
	}

	// An unknown failure keeps 500 and no code, so the client cannot mistake it for either of the
	// two 410 outcomes.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/media/hls/id/seg3.m4s", nil)
	hlsFail(c, errors.New("변환 실패"))
	var generic struct{ Error, Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &generic); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	if w.Code != http.StatusInternalServerError || generic.Code != "" {
		t.Fatalf("generic failure = %d %+v, want 500 with no code", w.Code, generic)
	}
}
