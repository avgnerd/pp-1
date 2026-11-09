package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

type env struct {
	t    *testing.T
	fake *fake.Server
	srv  *Server
	ts   *httptest.Server
	c    *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fs, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	dir := testutil.TempDir(t)
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DropboxAPIBase, cfg.DropboxContentBase = fts.URL, fts.URL
	cfg.DropboxAuthorize, cfg.DropboxTokenURL = fts.URL+"/oauth2/authorize", fts.URL+"/oauth2/token"
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
	s.SPA = fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>t</title>")}, "assets/app.js": {Data: []byte("x")}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, fake: fs, srv: s, ts: ts, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *env) do(method, path string, body any, out any) int {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, r)
	req.Header.Set("X-Requested-With", "dvfs")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (e *env) login() {
	if st := e.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "1234"}, nil); st != 200 {
		e.t.Fatalf("login %d", st)
	}
}

// connect creates a storage with the localhost flow and completes it via the callback.
func (e *env) connect(appKey, mount string) int64 {
	e.t.Helper()
	var start struct {
		Storage      struct{ ID int64 } `json:"storage"`
		AuthorizeURL string             `json:"authorize_url"`
	}
	if st := e.do("POST", "/api/admin/storages", map[string]any{"mount_path": mount, "app_key": appKey, "app_secret": "s"}, &start); st != 201 {
		e.t.Fatalf("create storage %d", st)
	}
	resp, err := e.c.Get(start.AuthorizeURL)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	// The redirect targets localhost:8080; send it to the test server instead.
	cb, err := e.c.Get(e.ts.URL + "/api/oauth/callback?" + loc.RawQuery)
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := io.ReadAll(cb.Body)
	cb.Body.Close()
	if cb.StatusCode != 200 || !strings.Contains(string(body), "연결 완료") {
		e.t.Fatalf("callback %d %s", cb.StatusCode, body)
	}
	return start.Storage.ID
}

func TestAuthRequiredAndCSRF(t *testing.T) {
	e := newEnv(t)
	if st := e.do("GET", "/api/fs/list?path=/", nil, nil); st != 401 {
		t.Fatalf("unauthenticated list: %d", st)
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/auth/login", strings.NewReader(`{"username":"admin","password":"1234"}`))
	resp, _ := e.c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF header accepted: %d", resp.StatusCode)
	}
	if st := e.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "nope"}, nil); st != 401 {
		t.Fatalf("bad password: %d", st)
	}
	e.login()
	var me struct {
		Authenticated     bool `json:"authenticated"`
		IsDefaultPassword bool `json:"is_default_password"`
	}
	e.do("GET", "/api/auth/me", nil, &me)
	if !me.Authenticated || !me.IsDefaultPassword {
		t.Fatalf("me = %+v", me)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	last := 0
	for i := 0; i < 6; i++ {
		last = e.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "x"}, nil)
	}
	if last != 429 {
		t.Fatalf("expected lockout, got %d", last)
	}
	if st := e.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "1234"}, nil); st != 429 {
		t.Fatalf("locked IP logged in: %d", st)
	}
}

func TestProfileChange(t *testing.T) {
	e := newEnv(t)
	e.login()
	if st := e.do("PUT", "/api/admin/profile", map[string]string{"username": "boss", "current_password": "1234", "new_password": "longpassword"}, nil); st != 200 {
		t.Fatalf("profile %d", st)
	}
	// The current client got a fresh session; a new client must use the new credentials.
	jar, _ := cookiejar.New(nil)
	e.c = &http.Client{Jar: jar}
	if st := e.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "1234"}, nil); st != 401 {
		t.Fatalf("old credentials: %d", st)
	}
	if st := e.do("POST", "/api/auth/login", map[string]string{"username": "boss", "password": "longpassword"}, nil); st != 200 {
		t.Fatalf("new credentials: %d", st)
	}
}

func TestRobotsAndSettings(t *testing.T) {
	e := newEnv(t)
	resp, _ := http.Get(e.ts.URL + "/robots.txt")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "User-agent: *\nDisallow: /" {
		t.Fatalf("robots default %q", b)
	}
	e.login()
	var st map[string]any
	e.do("GET", "/api/admin/settings", nil, &st)
	if st["per_page"].(float64) != 30 || st["cache_cleanup_sec"].(float64) != 120 {
		t.Fatalf("defaults %+v", st)
	}
	st["robots_txt"] = "User-agent: *\nAllow: /"
	if code := e.do("PUT", "/api/admin/settings", st, nil); code != 200 {
		t.Fatalf("put settings %d", code)
	}
	resp, _ = http.Get(e.ts.URL + "/robots.txt")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "Allow: /") {
		t.Fatal("robots not updated")
	}
	st["per_page"] = 2
	if code := e.do("PUT", "/api/admin/settings", st, nil); code != 400 {
		t.Fatalf("invalid per_page accepted: %d", code)
	}
}

func TestSPAFallback(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/path/to/영상.mp4", "/admin/settings", "/search"} {
		resp, err := http.Get(e.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s -> %d %s", p, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

func TestBrowseAndOperations(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.connect("app1", "/path/to")
	a := e.fake.Account("app1")
	now := time.Now()
	a.Put("/Videos/ep10.mp4", make([]byte, 100), now)
	a.Put("/Videos/ep2.mp4", make([]byte, 300), now)
	a.Put("/Videos/sub/a.txt", []byte("hello"), now)
	a.Put("/한글 폴더/문서.txt", []byte("안녕"), now)

	var root struct {
		Entries []struct{ Name, Type, Path string }
	}
	e.do("GET", "/api/fs/list?path=/", nil, &root)
	if len(root.Entries) != 1 || root.Entries[0].Name != "path" || root.Entries[0].Type != "virtual" {
		t.Fatalf("root %+v", root)
	}
	e.do("GET", "/api/fs/list?path=/path", nil, &root)
	if len(root.Entries) != 1 || root.Entries[0].Type != "mount" {
		t.Fatalf("/path %+v", root)
	}
	var vids struct {
		Entries []struct {
			Name string
			Type string
			Size *int64
		}
		Total int
	}
	e.do("GET", "/api/fs/list?path=/path/to/Videos&sort=name", nil, &vids)
	if vids.Total != 3 || vids.Entries[0].Name != "sub" || vids.Entries[1].Name != "ep2.mp4" || vids.Entries[2].Name != "ep10.mp4" {
		t.Fatalf("natural sort/folders first: %+v", vids.Entries)
	}
	if vids.Entries[0].Size != nil {
		t.Fatal("list must not compute folder sizes")
	}
	if e.fake.CallCount("files/list_folder") != 1 {
		// one call for /Videos only; recursive listings would mean a size computation
		t.Fatalf("list_folder calls %d", e.fake.CallCount("files/list_folder"))
	}
	e.do("GET", "/api/fs/list?path=/path/to/Videos&sort=size&order=desc&limit=1&offset=1", nil, &vids)
	if len(vids.Entries) != 1 || vids.Entries[0].Name != "ep2.mp4" {
		t.Fatalf("size desc page: %+v", vids.Entries)
	}

	// Folder size on demand via SSE.
	resp, err := e.c.Get(e.ts.URL + "/api/fs/folder-size?path=" + url.QueryEscape("/path/to/Videos"))
	if err != nil {
		t.Fatal(err)
	}
	var done map[string]any
	sc := bufio.NewScanner(resp.Body)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") && event == "done" {
			json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &done)
			break
		}
	}
	resp.Body.Close()
	if done["bytes"].(float64) != 405 || done["files"].(float64) != 3 {
		t.Fatalf("folder size %+v", done)
	}
	e.do("GET", "/api/fs/list?path=/path/to", nil, &vids)
	var sized bool
	for _, en := range vids.Entries {
		if en.Name == "Videos" && en.Size != nil && *en.Size == 405 {
			sized = true
		}
	}
	if !sized {
		t.Fatal("cached folder size not shown")
	}
	// Deleting a child clears the cached size of the folder and its ancestors.
	if st := e.do("POST", "/api/fs/delete", map[string]any{"paths": []string{"/path/to/Videos/ep10.mp4"}}, nil); st != 200 {
		t.Fatalf("delete %d", st)
	}
	e.do("GET", "/api/fs/list?path=/path/to", nil, &vids)
	for _, en := range vids.Entries {
		if en.Name == "Videos" && en.Size != nil {
			t.Fatal("folder size not invalidated")
		}
	}

	// Rename, move (batch), copy.
	if st := e.do("POST", "/api/fs/rename", map[string]string{"path": "/path/to/Videos/ep2.mp4", "name": "에피소드2.mp4"}, nil); st != 200 {
		t.Fatalf("rename %d", st)
	}
	var mv struct {
		Done     int
		Failures []struct{ Path, Error string }
	}
	e.do("POST", "/api/fs/move", map[string]any{"paths": []string{"/path/to/Videos/에피소드2.mp4", "/path/to/Videos/sub"}, "dest": "/path/to/한글 폴더"}, &mv)
	if mv.Done != 2 || len(mv.Failures) != 0 {
		t.Fatalf("move %+v", mv)
	}
	e.do("GET", "/api/fs/list?path="+url.QueryEscape("/path/to/한글 폴더"), nil, &vids)
	if vids.Total != 3 {
		t.Fatalf("after move %+v", vids.Entries)
	}
	if st := e.do("POST", "/api/fs/move", map[string]any{"paths": []string{"/path/to/한글 폴더"}, "dest": "/path/to/한글 폴더/sub"}, nil); st != 400 {
		t.Fatalf("move into itself: %d", st)
	}
	if st := e.do("POST", "/api/fs/rename", map[string]string{"path": "/path/to", "name": "x"}, nil); st != 400 {
		t.Fatalf("rename mount: %d", st)
	}
	_ = id
}

func TestTraversalRejected(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	if st := e.do("GET", "/api/fs/list?path="+url.QueryEscape("/m/../../etc"), nil, nil); st != 400 {
		t.Fatalf("traversal status %d", st)
	}
}

func TestRawProxyAndRange(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	e.fake.Account("app1").Put("/f.bin", []byte("0123456789"), time.Now())
	// Direct mode redirects to the temporary link.
	resp, _ := e.c.Get(e.ts.URL + "/api/fs/raw?path=/m/f.bin")
	resp.Body.Close()
	if resp.StatusCode != 302 || !strings.Contains(resp.Header.Get("Location"), "/tl/") {
		t.Fatalf("direct: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/fs/raw?path=/m/f.bin&proxy=1&download=1", nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, _ = e.c.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "234" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("range: %d %q %s", resp.StatusCode, b, resp.Header.Get("Content-Disposition"))
	}
	req.Header.Set("Range", "bytes=50-60")
	resp, _ = e.c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 416 {
		t.Fatalf("unsatisfiable range: %d", resp.StatusCode)
	}
	// Signed URL works without the cookie; a tampered one does not.
	p := e.srv.Stream.SignedPath("/m/f.bin", time.Hour)
	resp, _ = http.Get(e.ts.URL + p)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "0123456789" {
		t.Fatalf("signed: %d", resp.StatusCode)
	}
	resp, _ = http.Get(e.ts.URL + strings.Replace(p, "/m/f.bin", "/m/other.bin", 1))
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("tampered signed URL: %d", resp.StatusCode)
	}
}

func TestZipDownload(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/a")
	e.connect("app2", "/b")
	now := time.Now()
	e.fake.Account("app1").Put("/폴더/하나.txt", []byte("one"), now)
	e.fake.Account("app1").Put("/폴더/sub/two.txt", []byte("two"), now)
	e.fake.Account("app2").Put("/x.txt", []byte("x"), now)
	e.fake.Account("app2").Put("/폴더/하나.txt", []byte("dup"), now)
	var start struct{ URL string }
	e.do("POST", "/api/fs/zip", map[string]any{"paths": []string{"/a/폴더", "/b/x.txt", "/b/폴더"}}, &start)
	resp, err := e.c.Get(e.ts.URL + start.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip: %v (%d bytes, status %d)", err, len(b), resp.StatusCode)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		c, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(c)
		if f.Method != zip.Store {
			t.Errorf("%s not stored", f.Name)
		}
	}
	want := map[string]string{"폴더/하나.txt": "one", "폴더/sub/two.txt": "two", "x.txt": "x", "폴더/하나 (2).txt": "dup"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("zip entry %q = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	// Tokens are single use.
	resp, _ = e.c.Get(e.ts.URL + start.URL)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("token reuse: %d", resp.StatusCode)
	}
}

// TestZipTokenContractUnderBusyLimiter pins all three answers of the download endpoint while the
// limiter is saturated: an unknown or expired token is 404, a valid token is 503 and survives, and
// a slot plus a valid token streams the archive and consumes the token exactly once.
func TestZipTokenContractUnderBusyLimiter(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/a")
	e.fake.Account("app1").Put("/x.txt", []byte("x"), time.Now())
	var start struct{ URL string }
	if st := e.do("POST", "/api/fs/zip", map[string]any{"paths": []string{"/a/x.txt"}}, &start); st != 200 {
		t.Fatalf("zip start %d", st)
	}
	e.srv.zipMu.Lock()
	e.srv.zipTokens["expired-token"] = zipJob{paths: []string{"/a/x.txt"}, name: "x", expires: time.Now().Add(-time.Minute)}
	e.srv.zipMu.Unlock()

	get := func(path string) int {
		t.Helper()
		resp, err := e.c.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	e.srv.ZipLim.SetLimit(0)
	if got := get("/api/fs/zip/not-a-token"); got != 404 {
		t.Fatalf("unknown token with no free slot: %d, want 404", got)
	}
	if got := get("/api/fs/zip/expired-token"); got != 404 {
		t.Fatalf("expired token with no free slot: %d, want 404", got)
	}
	if got := get(start.URL); got != 503 {
		t.Fatalf("valid token with no free slot: %d, want 503", got)
	}

	e.srv.ZipLim.SetLimit(4)
	if got := get(start.URL); got != 200 {
		t.Fatalf("retry after 503: %d, want 200 (the refused attempt consumed the token)", got)
	}
	if got := get(start.URL); got != 404 {
		t.Fatalf("token reuse: %d, want 404", got)
	}
}

func TestSearchSSE(t *testing.T) {
	e := newEnv(t)
	e.login()
	for i := 1; i <= 12; i++ {
		e.connect(fmt.Sprintf("app%d", i), fmt.Sprintf("/s%02d", i))
		e.fake.Account(fmt.Sprintf("app%d", i)).Put(fmt.Sprintf("/movies/Holiday %d.mp4", i), []byte("v"), time.Now())
		e.fake.Account(fmt.Sprintf("app%d", i)).Mkdir("/Holiday folder")
	}
	e.fake.FailAccounts.Store("app3", true)
	resp, err := e.c.Get(e.ts.URL + "/api/search?kind=file&q=holiday")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	files, errorsSeen, event := 0, 0, ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Entries []struct{ Type, Path string }
			Status  string
		}
		json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
		switch event {
		case "results":
			for _, en := range ev.Entries {
				if en.Type != "file" {
					t.Fatalf("kind=file returned %+v", en)
				}
				files++
			}
		case "storage":
			if ev.Status == "error" {
				errorsSeen++
			}
		}
		if event == "done" {
			break
		}
	}
	if files != 11 || errorsSeen != 1 {
		t.Fatalf("files=%d errors=%d", files, errorsSeen)
	}
}

func TestThumbBatching(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/p")
	for i := 0; i < 30; i++ {
		e.fake.Account("app1").Put(fmt.Sprintf("/img%02d.jpg", i), []byte("x"), time.Now())
	}
	done := make(chan int, 30)
	for i := 0; i < 30; i++ {
		go func(i int) {
			resp, err := e.c.Get(fmt.Sprintf("%s/api/fs/thumb?path=/p/img%02d.jpg&rev=1&size=w256h256", e.ts.URL, i))
			if err != nil {
				done <- 0
				return
			}
			resp.Body.Close()
			done <- resp.StatusCode
		}(i)
	}
	for i := 0; i < 30; i++ {
		if st := <-done; st != 200 {
			t.Fatalf("thumb status %d", st)
		}
	}
	if n := e.fake.CallCount("files/get_thumbnail_batch"); n < 2 || n > 6 {
		t.Fatalf("expected batched calls, got %d", n)
	}
	// Second round is served from disk.
	before := e.fake.CallCount("files/get_thumbnail_batch")
	resp, _ := e.c.Get(e.ts.URL + "/api/fs/thumb?path=/p/img00.jpg&rev=1&size=w256h256")
	resp.Body.Close()
	if e.fake.CallCount("files/get_thumbnail_batch") != before {
		t.Fatal("thumb cache miss")
	}
}

func TestMountValidationAPI(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/path/to")
	var res struct{ Error, Field string }
	if st := e.do("POST", "/api/admin/storages/check-mount", map[string]any{"mount_path": "/path"}, &res); st != 400 || res.Field != "mount_path" {
		t.Fatalf("nested mount accepted: %d %+v", st, res)
	}
	if st := e.do("POST", "/api/admin/storages/check-mount", map[string]any{"mount_path": "/path/to2"}, nil); st != 200 {
		t.Fatalf("sibling mount rejected: %d", st)
	}
}

func TestTextPreviewEUCKR(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/t")
	// "안녕" in EUC-KR
	e.fake.Account("app1").Put("/k.srt", []byte{0xbe, 0xc8, 0xb3, 0xe7}, time.Now())
	var res struct {
		Text, Encoding string
	}
	e.do("GET", "/api/fs/text?path=/t/k.srt", nil, &res)
	if res.Text != "안녕" || res.Encoding != "euc-kr" {
		t.Fatalf("%+v", res)
	}
}

func TestFreshLinkOncePerToken(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	e.fake.Account("app1").Put("/v.mp4", []byte("0123456789"), time.Now())
	get := func(q string) string {
		resp, err := e.c.Get(e.ts.URL + "/api/fs/raw?path=/m/v.mp4" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	get("")
	base := e.fake.CallCount("files/get_temporary_link")
	get("")
	if e.fake.CallCount("files/get_temporary_link") != base {
		t.Fatal("cached link not reused")
	}
	// A fresh token fetches a new link once, however many range requests use it.
	for i := 0; i < 5; i++ {
		get("&fresh=abc")
	}
	if got := e.fake.CallCount("files/get_temporary_link") - base; got != 1 {
		t.Fatalf("fresh token fetched %d links, want 1", got)
	}
	get("&fresh=def")
	if got := e.fake.CallCount("files/get_temporary_link") - base; got != 2 {
		t.Fatalf("second token fetched %d links total, want 2", got)
	}
}

func TestHLSPingUnknownSession(t *testing.T) {
	e := newEnv(t)
	e.login()
	var res struct{ Code string }
	if st := e.do("POST", "/api/media/hls/nope/ping", nil, &res); st != 404 || res.Code != "session_gone" {
		t.Fatalf("ping unknown: %d %+v", st, res)
	}
	// Segment requests for a gone session get 410 so the player reopens instead of retrying.
	if st := e.do("GET", "/api/media/hls/nope/seg3.m4s", nil, &res); st != 410 || res.Code != "session_gone" {
		t.Fatalf("segment of unknown session: %d %+v", st, res)
	}
}
