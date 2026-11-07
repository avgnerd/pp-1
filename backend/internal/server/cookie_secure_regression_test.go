package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// newSecureCookieEnv builds a server whose configuration and listener the case can shape before the
// first request, so the Secure flag is decided the way production decides it.
func newSecureCookieEnv(t *testing.T, useTLS bool, tweak func(*config.Config)) (*httptest.Server, *http.Client) {
	t.Helper()
	_, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	cfg, err := config.Load(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DropboxAPIBase, cfg.DropboxContentBase = fts.URL, fts.URL
	cfg.DropboxAuthorize, cfg.DropboxTokenURL = fts.URL+"/oauth2/authorize", fts.URL+"/oauth2/token"
	cfg.FFmpegPath, cfg.FFprobePath = "", ""
	tweak(cfg)
	d, err := db.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var ts *httptest.Server
	if useTLS {
		ts = httptest.NewTLSServer(s.Handler())
	} else {
		ts = httptest.NewServer(s.Handler())
	}
	t.Cleanup(ts.Close)
	cl := ts.Client()
	jar, _ := cookiejar.New(nil)
	cl.Jar = jar
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return ts, cl
}

func secureCookieLogin(t *testing.T, ts *httptest.Server, cl *http.Client, headers map[string]string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "1234"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, auth.CSRFValue)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

func secureCookieLogout(t *testing.T, ts *httptest.Server, cl *http.Client, headers map[string]string) *http.Cookie {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	req.Header.Set(auth.CSRFHeader, auth.CSRFValue)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatal("logout set no session cookie")
	return nil
}

// TestSecureCookieMatrix pins the four signals that decide the Secure flag and the logout cookie
// attributes: configured https, a direct TLS listener, a trusted proxy's X-Forwarded-Proto, and a
// forged header from an untrusted client, which must not count.
func TestSecureCookieMatrix(t *testing.T) {
	cases := []struct {
		name    string
		tls     bool
		tweak   func(*config.Config)
		headers map[string]string
		want    bool
	}{
		{"public_url_https", false, func(c *config.Config) { c.PublicURL = "https://files.example.com" }, nil, true},
		{"plain_http_no_public_url", false, func(c *config.Config) { c.PublicURL = "" }, nil, false},
		{"trusted_proxy_forwarded_https", false, func(c *config.Config) { c.TrustedProxies = []string{"127.0.0.1"} },
			map[string]string{"X-Forwarded-Proto": "https"}, true},
		{"untrusted_forwarded_https", false, func(c *config.Config) { c.TrustedProxies = []string{"10.9.9.9"} },
			map[string]string{"X-Forwarded-Proto": "https"}, false},
		{"tls_listener", true, func(c *config.Config) { c.PublicURL = "" }, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, cl := newSecureCookieEnv(t, tc.tls, tc.tweak)
			c := secureCookieLogin(t, ts, cl, tc.headers)
			if c.Secure != tc.want {
				t.Fatalf("session cookie Secure = %v, want %v (headers=%v)", c.Secure, tc.want, tc.headers)
			}
			if !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 {
				t.Fatalf("session cookie attributes changed: HttpOnly=%v Path=%q SameSite=%v MaxAge=%d",
					c.HttpOnly, c.Path, c.SameSite, c.MaxAge)
			}
			out := secureCookieLogout(t, ts, cl, tc.headers)
			if out.Secure != tc.want || !out.HttpOnly || out.Path != "/" || out.MaxAge >= 0 {
				t.Fatalf("logout cookie = Secure:%v HttpOnly:%v Path:%q MaxAge:%d, want the session attributes",
					out.Secure, out.HttpOnly, out.Path, out.MaxAge)
			}
		})
	}
}
