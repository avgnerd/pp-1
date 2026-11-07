package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

func activeFolderSizeGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, block := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(block, "created by") && strings.Contains(block, ".folderSize") {
			n++
		}
	}
	return n
}

func waitFolderSizeGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if activeFolderSizeGoroutines() <= baseline {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("folderSize left %d scan goroutines after the handler returned",
		activeFolderSizeGoroutines()-baseline)
}

func TestFolderSizeSSEDone(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("size-done", "/m")
	acct := e.fake.Account("size-done")
	acct.Put("/big/a.txt", []byte("abc"), time.Now())
	acct.Put("/big/sub/b.txt", []byte("defg"), time.Now())
	before := activeFolderSizeGoroutines()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	gin.SetMode(gin.TestMode)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/fs/folder-size?path=/m/big", nil)
	e.srv.folderSize(c)
	body := w.Body.String()
	if !strings.Contains(body, "event: done") {
		t.Fatalf("folder-size stream did not end with done: %q", body)
	}
	if !strings.Contains(body, `"bytes":7`) || !strings.Contains(body, `"files":2`) {
		t.Fatalf("done payload = %q", body)
	}
	waitFolderSizeGoroutines(t, before)
}

func TestFolderSizeSSEError(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("size-error", "/m")
	before := activeFolderSizeGoroutines()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	gin.SetMode(gin.TestMode)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/fs/folder-size?path=/m/missing", nil)
	e.srv.folderSize(c)
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("folder-size stream did not report the scan error: %q", body)
	}
	if !strings.Contains(body, "찾을 수 없습니다") {
		t.Fatalf("error event lost its message: %q", body)
	}
	waitFolderSizeGoroutines(t, before)
}

// TestFolderSizeSSECancel exercises the path where the client goes away while the scan runs. The
// request context is the only signal, and both the handler and the scan goroutine must stop.
func TestFolderSizeSSECancel(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("size-cancel", "/m")
	acct := e.fake.Account("size-cancel")
	for i := 0; i < 60; i++ {
		acct.Put(fmt.Sprintf("/big/f%02d.txt", i), []byte("x"), time.Now())
	}
	e.fake.PageSize = 5
	e.fake.Latency = 20 * time.Millisecond
	before := activeFolderSizeGoroutines()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	gin.SetMode(gin.TestMode)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/fs/folder-size?path=/m/big", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.srv.folderSize(c)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("folderSize handler did not return after the client canceled")
	}
	waitFolderSizeGoroutines(t, before)
}

// folderSizeWriteFailureWriter lets the first SSE body write through, then blocks the second one
// like a client that stopped reading, and fails that write when the test releases it.
type folderSizeWriteFailureWriter struct {
	header    http.Header
	status    int
	writes    atomic.Int32
	blocked   chan struct{}
	release   chan struct{}
	blockOnce sync.Once
}

func newFolderSizeWriteFailureWriter() *folderSizeWriteFailureWriter {
	return &folderSizeWriteFailureWriter{
		header:  http.Header{},
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *folderSizeWriteFailureWriter) Header() http.Header { return w.header }

func (w *folderSizeWriteFailureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *folderSizeWriteFailureWriter) Write(p []byte) (int, error) {
	if w.writes.Add(1) == 1 {
		return len(p), nil
	}
	w.blockOnce.Do(func() { close(w.blocked) })
	<-w.release
	return 0, errors.New("folder-size client write failed")
}

func (w *folderSizeWriteFailureWriter) Flush() {}

// folderSizePageBarrier forwards Dropbox calls and closes reached after the wanted number of
// list_folder responses has been fully relayed. That is the observed signal that the scan has
// produced enough pages to fill the SSE buffer; it replaces a fixed sleep in the test.
type folderSizePageBarrier struct {
	target  string
	want    int32
	pages   atomic.Int32
	reached chan struct{}
	once    sync.Once
}

func (p *folderSizePageBarrier) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	payload, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(payload)
	if strings.HasSuffix(r.URL.Path, "/2/files/list_folder") ||
		strings.HasSuffix(r.URL.Path, "/2/files/list_folder/continue") {
		if p.pages.Add(1) == p.want {
			p.once.Do(func() { close(p.reached) })
		}
	}
}

func newFolderSizeLeakServer(t *testing.T, apiBase string) *Server {
	t.Helper()
	cfg, err := config.Load(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DropboxAPIBase, cfg.DropboxContentBase = apiBase, apiBase
	cfg.DropboxAuthorize, cfg.DropboxTokenURL = apiBase+"/oauth2/authorize", apiBase+"/oauth2/token"
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
	return s
}

// TestFolderSizeSSEWriteFailureReleasesScanAfterBufferFull closes the gap the original F4 fixture
// covered only in OS Temp. The handler is held in its second body write while the scan fills the
// 16-message buffer and blocks on its final send. When that write then fails, the handler must
// return and the scan goroutine must stop. A bounded observation replaces fixed timing: the writer
// signals the stalled write, the proxy signals the page that fills the buffer, and only goroutines
// created by folderSize are counted, so unrelated test goroutines cannot mask a leak.
func TestFolderSizeSSEWriteFailureReleasesScanAfterBufferFull(t *testing.T) {
	fs, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	// 90 files with 5 per page, plus the folder entry on the first page, is 18 list responses.
	// A 260ms latency makes the 250ms progress gate fire on every page, so the handler's two
	// consumed messages leave exactly the 16 buffered progress messages full when the scan ends.
	const (
		files     = 90
		pageSize  = 5
		wantPages = 18
	)
	proxy := &folderSizePageBarrier{target: fts.URL, want: wantPages, reached: make(chan struct{})}
	pts := httptest.NewServer(proxy)
	t.Cleanup(pts.Close)
	s := newFolderSizeLeakServer(t, pts.URL)

	start, err := s.St.Create(storage.Input{MountPath: "/m", AppKey: "f4-write", AppSecret: "s", AuthMode: storage.AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.CompleteWithCode(context.Background(), start.Storage.ID, "code-f4-write"); err != nil {
		t.Fatal(err)
	}
	acct := fs.Account("f4-write")
	for i := 0; i < files; i++ {
		acct.Put(fmt.Sprintf("/big/f%03d.txt", i), []byte("x"), time.Now())
	}
	fs.PageSize = pageSize
	fs.Latency = 260 * time.Millisecond

	before := activeFolderSizeGoroutines()
	w := newFolderSizeWriteFailureWriter()
	c, _ := gin.CreateTestContext(w)
	gin.SetMode(gin.TestMode)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/fs/folder-size?path=/m/big", nil)

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.folderSize(c)
	}()

	select {
	case <-w.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never reached the stalled second body write")
	}
	select {
	case <-proxy.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("scan did not relay the page that fills the SSE buffer")
	}

	close(w.release)
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("folderSize handler did not return after the body write failed")
	}
	waitFolderSizeGoroutines(t, before)
}
