package vfs

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/limiter"
)

// folderSizeListBarrier puts a hold in front of the fake Dropbox. It forwards every call, but the
// first list_folder reply is read from the fake and then held until the test releases it. That
// makes the scan be in flight with data already read, so a test can run an invalidation while the
// scan is running without sleeping.
type folderSizeListBarrier struct {
	target  string
	held    chan struct{}
	release chan struct{}
	once    sync.Once
}

func newFolderSizeListBarrier(target string) *folderSizeListBarrier {
	return &folderSizeListBarrier{
		target:  target,
		held:    make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *folderSizeListBarrier) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	out, err := http.NewRequestWithContext(r.Context(), r.Method, b.target+r.URL.RequestURI(), bytes.NewReader(body))
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
	if strings.HasSuffix(r.URL.Path, "/2/files/list_folder") {
		b.once.Do(func() {
			close(b.held)
			<-b.release
		})
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(payload)
}

type folderSizeOutcome struct {
	progress SizeProgress
	err      error
}

// A scan that reads a folder and is then overtaken by an invalidation must still answer its own
// caller, but it must not put the value it read back into the cache: a later read has to rescan
// rather than serve the pre-invalidation size for the rest of the TTL.
func TestFolderSizeDoesNotCacheAcrossInvalidation(t *testing.T) {
	cases := []struct {
		name       string
		invalidate func(*Service, int64)
	}{
		{"InvalidatePath", func(s *Service, id int64) { s.InvalidatePath(id, "/big") }},
		{"Clear", func(s *Service, id int64) { s.Cache.Clear() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var barrier *folderSizeListBarrier
			svc, fs, _ := newTestServiceWith(t, "size-lifecycle", func(fakeURL string) string {
				barrier = newFolderSizeListBarrier(fakeURL)
				pts := httptest.NewServer(barrier)
				t.Cleanup(pts.Close)
				return pts.URL
			})
			acct := fs.Account("size-lifecycle")
			mod := time.Unix(1700000000, 0).UTC()
			acct.Put("/big/a.txt", []byte("abc"), mod)

			target, err := svc.Resolve("/m/big")
			if err != nil {
				t.Fatalf("resolve /m/big: %v", err)
			}
			key := FolderSizeKey(target.Storage.ID, target.DbxPath)

			lim := limiter.New(3)
			done := make(chan folderSizeOutcome, 1)
			go func() {
				p, err := svc.FolderSize(context.Background(), "/m/big", lim, func(SizeProgress) {})
				done <- folderSizeOutcome{progress: p, err: err}
			}()

			select {
			case <-barrier.held:
			case <-time.After(5 * time.Second):
				t.Fatal("the scan never reached the list_folder response")
			}

			before := listCalls(fs)
			tc.invalidate(svc, target.Storage.ID)
			// Data that the held response cannot contain. It is only visible to a rescan.
			acct.Put("/big/b.txt", []byte("12345"), mod)
			close(barrier.release)

			var first folderSizeOutcome
			select {
			case first = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("FolderSize did not return after the held response was released")
			}
			if first.err != nil {
				t.Fatalf("FolderSize returned %v", first.err)
			}
			if first.progress.Bytes != 3 || first.progress.Files != 1 || !first.progress.Done {
				t.Fatalf("first result = %+v, want the 3-byte snapshot its scan read", first.progress)
			}

			if v, ok := svc.Cache.Get(key); ok {
				t.Fatalf("%s cached the scan's pre-invalidation result: %+v", tc.name, v)
			}

			second, err := svc.FolderSize(context.Background(), "/m/big", lim, func(SizeProgress) {})
			if err != nil {
				t.Fatalf("second FolderSize: %v", err)
			}
			if second.Bytes != 8 || second.Files != 2 {
				t.Fatalf("second result = %+v, want a rescan of the 8-byte folder", second)
			}
			if calls := listCalls(fs) - before; calls < 1 {
				t.Fatalf("second read listed %d pages; it served the stale cached size", calls)
			}
			if v, ok := svc.Cache.Get(key); !ok {
				t.Fatal("the rescan result was not cached")
			} else if p := v.(SizeProgress); p.Bytes != 8 {
				t.Fatalf("cache holds %+v after the rescan, want the 8-byte result", p)
			}
		})
	}
}
