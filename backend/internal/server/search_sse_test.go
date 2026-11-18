package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// failingSSEWriter accepts the response headers and then fails every body write, the way a client
// that stopped reading between events does.
type failingSSEWriter struct {
	header http.Header
	status int
}

func newFailingSSEWriter() *failingSSEWriter {
	return &failingSSEWriter{header: http.Header{}}
}

func (w *failingSSEWriter) Header() http.Header { return w.header }

func (w *failingSSEWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *failingSSEWriter) Write([]byte) (int, error) {
	return 0, errors.New("search client write failed")
}

func (w *failingSSEWriter) Flush() {}

func activeSearchGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, block := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(block, "created by") {
			continue
		}
		if strings.Contains(block, ".search") || strings.Contains(block, "internal/search.Run") {
			n++
		}
	}
	return n
}

// TestSearchSSEWriteFailureReleasesWorkers closes the gap the handler had: when the event write
// fails, the handler returns while the request context is still live, and the search goroutine is
// left blocked on a channel nobody reads, holding its SearchLim slot. The search is paged so the
// stream buffers fill deterministically, and the assertion polls the limiter instead of sleeping
// for a fixed time.
func TestSearchSSEWriteFailureReleasesWorkers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := newEnv(t)
	e.login()
	e.connect("search-leak", "/m")
	acct := e.fake.Account("search-leak")
	const hits = 300
	for i := 0; i < hits; i++ {
		acct.Put(fmt.Sprintf("/hits/hit-%03d.txt", i), []byte("x"), time.Unix(1700000000, 0).UTC())
	}
	e.fake.SearchPageSize = 2
	e.srv.SearchLim.SetLimit(1)

	beforeGoroutines := activeSearchGoroutines()
	w := newFailingSSEWriter()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/search?q=hit&kind=file", nil)

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		e.srv.search(c)
	}()
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the search handler did not return after its event write failed")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		active, waiting, _ := e.srv.SearchLim.Stats()
		if active == 0 && waiting == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("search kept %d active and %d waiting limiter slots after the handler returned", active, waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The limiter is released before the goroutines finish unwinding, so this waits too rather
	// than sampling once and calling any later exit a leak.
	goroutineDeadline := time.Now().Add(10 * time.Second)
	for activeSearchGoroutines() > beforeGoroutines {
		if time.Now().After(goroutineDeadline) {
			t.Fatalf("search left %d goroutines after the handler returned", activeSearchGoroutines()-beforeGoroutines)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A scope with a trailing slash names the same folder as one without, so both must search the same
// mounts instead of matching no mount at all.
func TestSearchScopeTrailingSlashIsNormalized(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("scope-a", "/team/s0")
	e.connect("scope-b", "/team/s1")
	e.fake.Account("scope-a").Put("/a/scope-hit.txt", []byte("x"), time.Unix(1700000000, 0).UTC())
	e.fake.Account("scope-b").Put("/b/scope-hit.txt", []byte("x"), time.Unix(1700000000, 0).UTC())

	plain := observeSearchStream(t, e, url.Values{"q": {"scope-hit"}, "scope": {"/team"}})
	if plain.total != 2 || plain.storages != 2 {
		t.Fatalf("scope /team found total=%d storages=%d, want 2/2", plain.total, plain.storages)
	}
	slashed := observeSearchStream(t, e, url.Values{"q": {"scope-hit"}, "scope": {"/team/"}})
	if slashed.total != plain.total || slashed.storages != plain.storages {
		t.Fatalf("scope /team/ found total=%d storages=%d, want the same as /team: %d/%d",
			slashed.total, slashed.storages, plain.total, plain.storages)
	}
}

// A scope carrying a parent segment is not a path inside a mount; it is refused like every other
// handler refuses one, instead of being searched as if it were one.
func TestSearchScopeRejectsParentSegment(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("scope-a", "/team/s0")
	e.fake.Account("scope-a").Put("/a/scope-hit.txt", []byte("x"), time.Unix(1700000000, 0).UTC())

	for _, scope := range []string{"/team/s0/../s1", "/team/../../etc"} {
		resp, err := e.c.Get(e.ts.URL + "/api/search?" + url.Values{"q": {"scope-hit"}, "scope": {scope}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("scope %q got status %d, want 400", scope, resp.StatusCode)
		}
	}
}
