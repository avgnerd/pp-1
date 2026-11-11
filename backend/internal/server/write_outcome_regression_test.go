package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

type writeFault int

const (
	writeFaultNone writeFault = iota
	writeFaultApplyThen503
	writeFaultApplyThenDrop
	writeFaultMoveElsewhereThen503
	writeFaultAllTransport
	writeFaultPermission
)

// writeFaultProxy sits in front of the fake Dropbox. It can apply a write and then discard the
// response, move the source somewhere else, fail every transport call, or reject the write with a
// permission error. Those are the outcomes the handler must tell apart.
type writeFaultProxy struct {
	target   string
	mu       sync.Mutex
	armed    bool
	endpoint string
	mode     writeFault
}

func (p *writeFaultProxy) arm(endpoint string, mode writeFault) {
	p.mu.Lock()
	p.armed, p.endpoint, p.mode = true, endpoint, mode
	p.mu.Unlock()
}

func (p *writeFaultProxy) disarm() {
	p.mu.Lock()
	p.armed = false
	p.mu.Unlock()
}

func (p *writeFaultProxy) take(path string) writeFault {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.armed {
		return writeFaultNone
	}
	if p.mode == writeFaultAllTransport {
		if strings.Contains(path, "/2/") {
			return writeFaultAllTransport
		}
		return writeFaultNone
	}
	if p.endpoint == "" || strings.HasSuffix(path, "/2/"+p.endpoint) {
		p.armed = false
		return p.mode
	}
	return writeFaultNone
}

func (p *writeFaultProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	mode := p.take(r.URL.Path)
	if mode == writeFaultPermission {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error_summary":"path/insufficient_permissions/..","error":{".tag":"path","path":{".tag":"insufficient_permissions"}}}`)
		return
	}
	if mode == writeFaultAllTransport {
		panic(http.ErrAbortHandler)
	}
	if mode == writeFaultMoveElsewhereThen503 {
		var arg map[string]any
		if err := json.Unmarshal(body, &arg); err == nil {
			arg["to_path"] = "/elsewhere.txt"
			body, _ = json.Marshal(arg)
		}
	}
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
	payload, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	switch mode {
	case writeFaultApplyThen503, writeFaultMoveElsewhereThen503:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error_summary":"internal_error/..","error":{".tag":"internal_error"}}`)
		return
	case writeFaultApplyThenDrop:
		panic(http.ErrAbortHandler)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(payload)
}

// newWriteFaultEnv builds a logged-in server whose Dropbox endpoints pass through the fault proxy.
func newWriteFaultEnv(t *testing.T) (*env, *writeFaultProxy) {
	t.Helper()
	fs, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	p := &writeFaultProxy{target: fts.URL}
	pts := httptest.NewServer(p)
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
	e := &env{t: t, fake: fs, srv: s, ts: ts,
		c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	e.login()
	start, err := s.St.Create(storage.Input{MountPath: "/m", AppKey: "app1", AppSecret: "s", AuthMode: storage.AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.CompleteWithCode(context.Background(), start.Storage.ID, "code-app1"); err != nil {
		t.Fatal(err)
	}
	return e, p
}

type writeListEntry struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

func writeList(t *testing.T, e *env, vpath string) []writeListEntry {
	t.Helper()
	var out struct {
		Entries []writeListEntry `json:"entries"`
	}
	status := e.do(http.MethodGet, "/api/fs/list?path="+url.QueryEscape(vpath), nil, &out)
	if status != http.StatusOK {
		t.Fatalf("list %s: status %d", vpath, status)
	}
	return out.Entries
}

func postWriteOp(t *testing.T, e *env, endpoint string, body any) (int, map[string]any) {
	t.Helper()
	var out map[string]any
	status := e.do(http.MethodPost, endpoint, body, &out)
	return status, out
}

func TestRenameAfterLostResponseIsConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode writeFault
	}{
		{"503_then_source_missing", writeFaultApplyThen503},
		{"dropped_then_source_missing", writeFaultApplyThenDrop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, p := newWriteFaultEnv(t)
			e.fake.Account("app1").Put("/doc.txt", []byte("hello"), time.Now())
			if got := writeList(t, e, "/m"); len(got) != 1 || got[0].Name != "doc.txt" {
				t.Fatalf("cache before the rename: %+v", got)
			}
			p.arm("files/move_v2", tc.mode)
			status, body := postWriteOp(t, e, "/api/fs/rename", map[string]string{"path": "/m/doc.txt", "name": "renamed.txt"})
			if status != http.StatusOK {
				t.Fatalf("rename answered %d %v, but Dropbox held the moved file", status, body)
			}
			got := writeList(t, e, "/m")
			if len(got) != 1 || got[0].Name != "renamed.txt" {
				t.Fatalf("listing after the rename: %+v", got)
			}
		})
	}
}

// TestSingleMoveAfterLostResponseUsesDestinationID verifies the confirmation evidence is the
// destination's stable id. The test compares it with the source id observed before the request.
func TestSingleMoveAfterLostResponseUsesDestinationID(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/m1.txt", []byte("one"), time.Now())
	e.fake.Account("app1").Mkdir("/dest")
	var sourceID string
	for _, entry := range writeList(t, e, "/m") {
		if entry.Name == "m1.txt" {
			sourceID = entry.ID
		}
	}
	if sourceID == "" {
		t.Fatal("source listing did not contain m1.txt")
	}
	p.arm("files/move_v2", writeFaultApplyThen503)
	status, body := postWriteOp(t, e, "/api/fs/move", map[string]any{"paths": []string{"/m/m1.txt"}, "dest": "/m/dest"})
	if status != http.StatusOK {
		t.Fatalf("move answered %d %v", status, body)
	}
	after := writeList(t, e, "/m/dest")
	if len(after) != 1 || after[0].Name != "m1.txt" {
		t.Fatalf("destination listing: %+v", after)
	}
	if after[0].ID != sourceID {
		t.Fatalf("destination id = %q, pre-move source id = %q", after[0].ID, sourceID)
	}
}

func TestSingleCopyAfterLostResponseIsConfirmed(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/sub/src.txt", []byte("payload"), time.Now())
	e.fake.Account("app1").Mkdir("/dest")
	_ = writeList(t, e, "/m/sub")
	_ = writeList(t, e, "/m/dest")
	p.arm("files/copy_v2", writeFaultApplyThen503)
	status, body := postWriteOp(t, e, "/api/fs/copy", map[string]any{"paths": []string{"/m/sub/src.txt"}, "dest": "/m/dest"})
	if status != http.StatusOK {
		t.Fatalf("copy answered %d %v", status, body)
	}
	got := writeList(t, e, "/m/dest")
	if len(got) != 1 || got[0].Name != "src.txt" {
		t.Fatalf("destination listing: %+v", got)
	}
}

// TestCopyToExistingSameContentIsConflict is the case the earlier fix disguised as success: the
// destination already held identical bytes. It must stay a conflict, not become a copy.
func TestCopyToExistingSameContentIsConflict(t *testing.T) {
	e, _ := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/src.txt", []byte("identical"), time.Now())
	e.fake.Account("app1").Mkdir("/dest")
	e.fake.Account("app1").Put("/dest/src.txt", []byte("identical"), time.Now())
	status, body := postWriteOp(t, e, "/api/fs/copy", map[string]any{"paths": []string{"/m/src.txt"}, "dest": "/m/dest"})
	if status == http.StatusOK {
		t.Fatalf("an existing same-content destination was reported as a successful copy: %v", body)
	}
	if unknown, _ := body["unknown"].(bool); unknown {
		t.Fatalf("a first-attempt destination conflict was reported as unknown: %v", body)
	}
	if got := writeList(t, e, "/m/dest"); len(got) != 1 || got[0].Name != "src.txt" {
		t.Fatalf("destination listing changed: %+v", got)
	}
	if got := writeList(t, e, "/m"); len(got) != 2 {
		t.Fatalf("source listing changed: %+v", got)
	}
}

// TestMoveThatLandedElsewhereStaysUnknown proves source disappearance alone is not success. The
// proxy moves the source to another path and drops the response; the requested destination never
// receives it, so the handler must say unknown, invalidate the listing, and show the real location.
func TestMoveThatLandedElsewhereStaysUnknown(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/doc.txt", []byte("hello"), time.Now())
	_ = writeList(t, e, "/m")
	p.arm("files/move_v2", writeFaultMoveElsewhereThen503)
	status, body := postWriteOp(t, e, "/api/fs/rename", map[string]string{"path": "/m/doc.txt", "name": "renamed.txt"})
	if status != http.StatusBadGateway || body["unknown"] != true {
		t.Fatalf("move to another destination answered %d %v, want 502 unknown", status, body)
	}
	got := writeList(t, e, "/m")
	if len(got) != 1 || got[0].Name != "elsewhere.txt" {
		t.Fatalf("cache was not invalidated to the real listing: %+v", got)
	}
}

// TestTransportOnlyFailureIsUnknown keeps a final transport error out of the confirmed-failure
// path even though there is no APIError to inspect.
func TestTransportOnlyFailureIsUnknown(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/doc.txt", []byte("hello"), time.Now())
	_ = writeList(t, e, "/m")
	p.arm("", writeFaultAllTransport)
	status, body := postWriteOp(t, e, "/api/fs/rename", map[string]string{"path": "/m/doc.txt", "name": "renamed.txt"})
	if status != http.StatusBadGateway || body["unknown"] != true {
		t.Fatalf("transport-only failure answered %d %v, want 502 unknown", status, body)
	}
	p.disarm()
	got := writeList(t, e, "/m")
	if len(got) != 1 || got[0].Name != "doc.txt" {
		t.Fatalf("the source disappeared after a transport-only failure: %+v", got)
	}
}

func TestMovePermissionErrorStaysConfirmedFailure(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/doc.txt", []byte("hello"), time.Now())
	p.arm("files/move_v2", writeFaultPermission)
	status, body := postWriteOp(t, e, "/api/fs/rename", map[string]string{"path": "/m/doc.txt", "name": "renamed.txt"})
	if status == http.StatusOK {
		t.Fatalf("a permission error answered 200: %v", body)
	}
	if unknown, _ := body["unknown"].(bool); unknown {
		t.Fatalf("a first-attempt permission error was reported as unknown: %v", body)
	}
	if got := writeList(t, e, "/m"); len(got) != 1 || got[0].Name != "doc.txt" {
		t.Fatalf("source listing after the rejected move: %+v", got)
	}
}

func TestBatchMoveAfterLostResponseUsesDestinationIDs(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/b1.txt", []byte("1"), time.Now())
	e.fake.Account("app1").Put("/b2.txt", []byte("22"), time.Now())
	e.fake.Account("app1").Mkdir("/dest")
	_ = writeList(t, e, "/m")
	_ = writeList(t, e, "/m/dest")
	p.arm("files/move_batch_v2", writeFaultApplyThen503)
	status, body := postWriteOp(t, e, "/api/fs/move", map[string]any{"paths": []string{"/m/b1.txt", "/m/b2.txt"}, "dest": "/m/dest"})
	if status != http.StatusOK {
		t.Fatalf("batch move answered %d %v", status, body)
	}
	if done, _ := body["done"].(float64); int(done) != 2 {
		t.Fatalf("done = %v, want 2: %v", body["done"], body)
	}
	if got := writeList(t, e, "/m/dest"); len(got) != 2 {
		t.Fatalf("destination listing: %+v", got)
	}
}

// TestBatchCopyAfterLostResponseIsUnknown keeps the batch copy honest: without pre-write
// destination state there is no proof that an existing same-content entry is the copy.
func TestBatchCopyAfterLostResponseIsUnknown(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/c1.txt", []byte("1"), time.Now())
	e.fake.Account("app1").Put("/c2.txt", []byte("2"), time.Now())
	e.fake.Account("app1").Mkdir("/dest")
	_ = writeList(t, e, "/m")
	_ = writeList(t, e, "/m/dest")
	p.arm("files/copy_batch_v2", writeFaultApplyThen503)
	status, body := postWriteOp(t, e, "/api/fs/copy", map[string]any{"paths": []string{"/m/c1.txt", "/m/c2.txt"}, "dest": "/m/dest"})
	if status != http.StatusOK {
		t.Fatalf("batch copy answered %d %v", status, body)
	}
	if done, _ := body["done"].(float64); int(done) != 0 {
		t.Fatalf("unconfirmed batch copies counted as done: %v", body)
	}
	failures, _ := body["failures"].([]any)
	if len(failures) != 2 {
		t.Fatalf("failures = %v, want two unknown entries", failures)
	}
	for _, f := range failures {
		m, _ := f.(map[string]any)
		if m["unknown"] != true {
			t.Fatalf("batch failure is not marked unknown: %v", m)
		}
	}
}

// TestBatchCopyGenuineConflictIsConfirmedFailure keeps a non-retried batch conflict definitive.
// Only a retried batch can have applied an entry before reporting its failure.
func TestBatchCopyGenuineConflictIsConfirmedFailure(t *testing.T) {
	e, _ := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/c1.txt", []byte("1"), time.Now())
	e.fake.Account("app1").Put("/c2.txt", []byte("2"), time.Now())
	e.fake.Account("app1").Mkdir("/dest")
	e.fake.Account("app1").Put("/dest/c2.txt", []byte("2"), time.Now())
	_ = writeList(t, e, "/m")
	_ = writeList(t, e, "/m/dest")
	status, body := postWriteOp(t, e, "/api/fs/copy", map[string]any{"paths": []string{"/m/c1.txt", "/m/c2.txt"}, "dest": "/m/dest"})
	if status != http.StatusOK {
		t.Fatalf("batch copy answered %d %v", status, body)
	}
	if done, _ := body["done"].(float64); int(done) != 1 {
		t.Fatalf("done = %v, want 1: %v", body["done"], body)
	}
	failures, _ := body["failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want the one real conflict", failures)
	}
	m, _ := failures[0].(map[string]any)
	if m["unknown"] == true {
		t.Fatalf("a non-retried batch conflict was reported as unknown: %v", m)
	}
	if !strings.Contains(m["error"].(string), "같은 이름") {
		t.Fatalf("conflict message lost: %v", m)
	}
}

func TestBatchMoveTransportOnlyFailureIsUnknown(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/t1.txt", []byte("1"), time.Now())
	e.fake.Account("app1").Put("/t2.txt", []byte("2"), time.Now())
	e.fake.Account("app1").Mkdir("/dest")
	_ = writeList(t, e, "/m")
	_ = writeList(t, e, "/m/dest")
	p.arm("", writeFaultAllTransport)
	status, body := postWriteOp(t, e, "/api/fs/move", map[string]any{"paths": []string{"/m/t1.txt", "/m/t2.txt"}, "dest": "/m/dest"})
	if status != http.StatusOK {
		t.Fatalf("batch move answered %d %v", status, body)
	}
	if done, _ := body["done"].(float64); int(done) != 0 {
		t.Fatalf("an unconfirmed batch counted as done: %v", body)
	}
	failures, _ := body["failures"].([]any)
	if len(failures) != 2 {
		t.Fatalf("failures = %v, want two unknown entries", failures)
	}
	for _, f := range failures {
		m, _ := f.(map[string]any)
		if m["unknown"] != true {
			t.Fatalf("transport-only batch failure is not marked unknown: %v", m)
		}
	}
}
