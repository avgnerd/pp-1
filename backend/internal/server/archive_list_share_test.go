package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// dropboxProxy forwards Dropbox calls to the fake and lets a test hold or fail one endpoint. It
// is how the archive tests control the exact call the handler's cache load makes, without
// sleeping and without reaching into the handler.
type dropboxProxy struct {
	target string
	mu     sync.Mutex
	holds  map[string]*proxyHold
	fails  map[string]func(w http.ResponseWriter)
}

type proxyHold struct {
	enter       chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
	calls       int
}

func newDropboxProxy(target string) *dropboxProxy {
	return &dropboxProxy{target: target, holds: map[string]*proxyHold{}, fails: map[string]func(http.ResponseWriter){}}
}

// holdFirst holds the first request for path until release is called and refuses later ones, so a
// second attempt at the same call is visible as an error instead of a second successful load.
func (p *dropboxProxy) holdFirst(path string) (entered <-chan struct{}, release func()) {
	h := &proxyHold{enter: make(chan struct{}), release: make(chan struct{})}
	p.mu.Lock()
	p.holds[path] = h
	p.mu.Unlock()
	return h.enter, func() { h.releaseOnce.Do(func() { close(h.release) }) }
}

func (p *dropboxProxy) failPath(path string, fn func(w http.ResponseWriter)) {
	p.mu.Lock()
	p.fails[path] = fn
	p.mu.Unlock()
}

func (p *dropboxProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	h := p.holds[r.URL.Path]
	f := p.fails[r.URL.Path]
	if h != nil {
		h.calls++
		if h.calls > 1 {
			h = nil
			p.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error_summary":"restarted/..","error":{}}`)
			return
		}
	}
	p.mu.Unlock()
	if h != nil {
		h.enterOnce.Do(func() { close(h.enter) })
		select {
		case <-h.release:
		case <-r.Context().Done():
			return
		}
	}
	if f != nil {
		f(w)
		return
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
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(payload)
}

// proxyArchiveEnv builds a server behind proxy with one completed storage mounted at /m.
func proxyArchiveEnv(t *testing.T, appKey string) (*Server, *fake.Server, *dropboxProxy) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fs, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	proxy := newDropboxProxy(fts.URL)
	pts := httptest.NewServer(proxy)
	t.Cleanup(pts.Close)
	s := newProxyServer(t, pts.URL)
	start, err := s.St.Create(storage.Input{MountPath: "/m", AppKey: appKey, AppSecret: "s", AuthMode: storage.AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.CompleteWithCode(context.Background(), start.Storage.ID, "code-"+appKey); err != nil {
		t.Fatal(err)
	}
	return s, fs, proxy
}

func testZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type handlerReply struct {
	status int
	body   string
}

func runArchiveList(s *Server, ctx context.Context, vpath string) <-chan handlerReply {
	replies := make(chan handlerReply, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/fs/archive?path="+url.QueryEscape(vpath), nil).WithContext(ctx)
		s.archiveList(c)
		// gin applies a status-only response (the 499 for a cancelled request) when the engine
		// finishes the request, which a directly called handler does not do for us.
		c.Writer.WriteHeaderNow()
		replies <- handlerReply{status: w.Code, body: w.Body.String()}
	}()
	return replies
}

func awaitReply(t *testing.T, replies <-chan handlerReply, what string) handlerReply {
	t.Helper()
	select {
	case r := <-replies:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never returned", what)
		return handlerReply{}
	}
}

// A cancelled first caller must not fail the archive listing for a caller already waiting on the
// same key, and the cancelled caller must not stay blocked until the shared load ends.
func TestArchiveListSurvivesCancelledFirstCaller(t *testing.T) {
	s, fs, proxy := proxyArchiveEnv(t, "arc-cancel")
	// The mount /m maps onto the account root, so /m/a.zip is /a.zip on Dropbox.
	fs.Account("arc-cancel").Put("/a.zip", testZip(t), time.Unix(1700000000, 0).UTC())

	entered, release := proxy.holdFirst("/2/files/get_temporary_link")
	defer release()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	aReplies := runArchiveList(s, ctxA, "/m/a.zip")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the archive load never asked Dropbox for its content")
	}
	bReplies := runArchiveList(s, context.Background(), "/m/a.zip")

	cancelA()
	a := awaitReply(t, aReplies, "the cancelled caller")
	if a.status != 499 {
		t.Fatalf("cancelled caller got status %d body %q, want 499", a.status, a.body)
	}

	release()
	b := awaitReply(t, bReplies, "the waiting caller")
	if b.status != 200 {
		t.Fatalf("caller waiting on the shared load got status %d body %q, want the archive listing", b.status, b.body)
	}
	var out struct {
		Listing struct {
			Entries []struct{ Name string } `json:"entries"`
		} `json:"listing"`
	}
	if err := json.Unmarshal([]byte(b.body), &out); err != nil {
		t.Fatalf("decode listing %q: %v", b.body, err)
	}
	if len(out.Listing.Entries) != 1 || out.Listing.Entries[0].Name != "hello.txt" {
		t.Fatalf("listing entries = %+v, want hello.txt", out.Listing.Entries)
	}
}

// A load error that belongs to the provider, the storage or the caller keeps its own status
// instead of being flattened into a 400 that tells the client to fix its request.
func TestArchiveListKeepsProviderErrorStatus(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(p *dropboxProxy)
		want    int
	}{
		{
			name: "revoked account is reauth",
			prepare: func(p *dropboxProxy) {
				p.failPath("/2/files/get_temporary_link", func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, `{"error_summary":"invalid_access_token/..","error":{".tag":"invalid_access_token"}}`)
				})
				p.failPath("/oauth2/token", func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token has been revoked"}`)
				})
			},
			want: http.StatusConflict,
		},
		{
			name: "rate limit stays 429",
			prepare: func(p *dropboxProxy) {
				p.failPath("/2/files/get_temporary_link", func(w http.ResponseWriter) {
					w.Header().Set("Retry-After", "120")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"error_summary":"too_many_requests/..","error":{}}`)
				})
			},
			want: http.StatusTooManyRequests,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, fs, proxy := proxyArchiveEnv(t, "arc-status")
			fs.Account("arc-status").Put("/a.zip", testZip(t), time.Unix(1700000000, 0).UTC())
			// Warm the listing and the access token so the armed failure lands on the call the
			// archive load makes, not on the handler's path lookup.
			if _, _, err := s.VFS.Stat(context.Background(), "/m/a.zip"); err != nil {
				t.Fatal(err)
			}
			tc.prepare(proxy)

			got := awaitReply(t, runArchiveList(s, context.Background(), "/m/a.zip"), "archiveList")
			if got.status != tc.want {
				t.Fatalf("status = %d body %q, want %d", got.status, got.body, tc.want)
			}
			if tc.want == http.StatusConflict && !bytes.Contains([]byte(got.body), []byte(`"code":"reauth"`)) {
				t.Fatalf("reauth response = %q, want code reauth", got.body)
			}
		})
	}
}

// A file that is not the archive its name claims is the caller's mistake, so it stays a 400.
func TestArchiveListMalformedArchiveIsBadRequest(t *testing.T) {
	s, fs, _ := proxyArchiveEnv(t, "arc-bad")
	fs.Account("arc-bad").Put("/a.zip", []byte("this is not a zip file at all"), time.Unix(1700000000, 0).UTC())

	got := awaitReply(t, runArchiveList(s, context.Background(), "/m/a.zip"), "archiveList")
	if got.status != http.StatusBadRequest {
		t.Fatalf("status = %d body %q, want 400", got.status, got.body)
	}
}
