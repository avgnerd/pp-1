package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
)

// TestLinkURLFollowsForwardedProtoRules pins that the absolute URL handed to an external player
// reads X-Forwarded-Proto the way the session cookie does: the first value of a comma-separated
// list, compared without regard to case, and only from a configured trusted proxy.
func TestLinkURLFollowsForwardedProtoRules(t *testing.T) {
	cases := []struct {
		name    string
		trusted []string
		header  string
		want    string
	}{
		{"plain http", nil, "", "http://"},
		{"forwarded https", []string{"127.0.0.1"}, "https", "https://"},
		{"forwarded list", []string{"127.0.0.1"}, "https, http", "https://"},
		{"forwarded upper case", []string{"127.0.0.1"}, "HTTPS", "https://"},
		{"forwarded http", []string{"127.0.0.1"}, "http, https", "http://"},
		{"untrusted proxy", []string{"10.9.9.9"}, "https", "http://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.Cfg.PublicURL = ""
			e.srv.Cfg.TrustedProxies = tc.trusted
			st := e.srv.Settings.Get()
			st.DataMode = settings.ModeProxy
			if err := e.srv.Settings.Update(st); err != nil {
				t.Fatal(err)
			}
			e.login()
			e.connect("app1", "/m")
			e.fake.Account("app1").Put("/v.mp4", []byte("video"), time.Unix(1700000000, 0).UTC())

			req, err := http.NewRequest("GET", e.ts.URL+"/api/fs/link?path=/m/v.mp4", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Requested-With", "dvfs")
			if tc.header != "" {
				req.Header.Set("X-Forwarded-Proto", tc.header)
			}
			resp, err := e.c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("link status %d", resp.StatusCode)
			}
			var out struct{ URL, Kind string }
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if out.Kind != "signed" {
				t.Fatalf("link kind = %q, want signed", out.Kind)
			}
			if !strings.HasPrefix(out.URL, tc.want) {
				t.Fatalf("link URL = %q, want the %q scheme (X-Forwarded-Proto %q)", out.URL, strings.TrimSuffix(tc.want, "//"), tc.header)
			}
		})
	}
}
