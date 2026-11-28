package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// octetStreamPreviewProxy forwards everything to the fake Dropbox but labels successful preview
// responses application/octet-stream, which is what the real files/get_preview returns for both
// PDF and HTML bytes.
type octetStreamPreviewProxy struct {
	target string
}

func (p octetStreamPreviewProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.target+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = r.Header.Clone()
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if strings.HasSuffix(r.URL.Path, "/files/get_preview") && resp.StatusCode == http.StatusOK {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(payload)
}

// newOctetStreamPreviewEnv builds a logged-in server whose Dropbox calls pass through a proxy
// that reports every preview as application/octet-stream.
func newOctetStreamPreviewEnv(t *testing.T) *env {
	t.Helper()
	fs, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	pts := httptest.NewServer(octetStreamPreviewProxy{target: fts.URL})
	t.Cleanup(pts.Close)
	cfg, err := config.Load(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DropboxAPIBase, cfg.DropboxContentBase = pts.URL, pts.URL
	cfg.DropboxAuthorize, cfg.DropboxTokenURL = pts.URL+"/oauth2/authorize", pts.URL+"/oauth2/token"
	cfg.FFmpegPath, cfg.FFprobePath = "", ""
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
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, fake: fs, srv: s, ts: ts,
		c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type previewResponse struct {
	status int
	header http.Header
	body   []byte
}

func getPreview(t *testing.T, e *env, vpath string) previewResponse {
	t.Helper()
	resp, err := e.c.Get(e.ts.URL + "/api/fs/preview?path=" + url.QueryEscape(vpath))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return previewResponse{status: resp.StatusCode, header: resp.Header, body: body}
}

// Dropbox labels both a PDF preview and a spreadsheet's HTML preview application/octet-stream.
// The response must be rendered from the bytes, not the upstream label, or the browser downloads
// it instead of showing it. HTML needs the sandbox CSP; PDF must not carry it.
func TestOfficePreviewUsesBodyMagicNotUpstreamContentType(t *testing.T) {
	e := newOctetStreamPreviewEnv(t)
	e.login()
	e.connect("app1", "/m")
	acct := e.fake.Account("app1")
	now := time.Now()
	acct.Put("/report.docx", []byte("docx bytes"), now)
	acct.Put("/sheet.xlsx", []byte("xlsx bytes"), now)

	const htmlCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:"

	t.Run("pdf bytes", func(t *testing.T) {
		got := getPreview(t, e, "/m/report.docx")
		if got.status != http.StatusOK {
			t.Fatalf("preview status = %d, want 200", got.status)
		}
		if ct := got.header.Get("Content-Type"); ct != "application/pdf" {
			t.Fatalf("Content-Type = %q, want application/pdf", ct)
		}
		if csp := got.header.Get("Content-Security-Policy"); csp != "" {
			t.Fatalf("PDF response carries Content-Security-Policy %q, want none", csp)
		}
		if cc := got.header.Get("Cache-Control"); cc != "private, max-age=600" {
			t.Fatalf("Cache-Control = %q, want private, max-age=600", cc)
		}
		if cd := got.header.Get("Content-Disposition"); cd != "inline" {
			t.Fatalf("Content-Disposition = %q, want inline", cd)
		}
		if want := "%PDF-1.4 fake"; string(got.body) != want {
			t.Fatalf("body = %q, want %q", got.body, want)
		}
	})

	t.Run("html bytes", func(t *testing.T) {
		got := getPreview(t, e, "/m/sheet.xlsx")
		if got.status != http.StatusOK {
			t.Fatalf("preview status = %d, want 200", got.status)
		}
		if ct := got.header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", ct)
		}
		if csp := got.header.Get("Content-Security-Policy"); csp != htmlCSP {
			t.Fatalf("Content-Security-Policy = %q, want %q", csp, htmlCSP)
		}
		if cc := got.header.Get("Cache-Control"); cc != "private, max-age=600" {
			t.Fatalf("Cache-Control = %q, want private, max-age=600", cc)
		}
		if cd := got.header.Get("Content-Disposition"); cd != "inline" {
			t.Fatalf("Content-Disposition = %q, want inline", cd)
		}
		if want := "<table><tr><td>sheet.xlsx</td></tr></table>"; string(got.body) != want {
			t.Fatalf("body = %q, want %q", got.body, want)
		}
	})
}
