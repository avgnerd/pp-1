package vfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
)

// listGate forwards Dropbox calls to the fake and holds the first directory listing until the
// test releases it. A second listing is refused: only a load that lost its first caller restarts,
// so the refusal separates a shared load from a restarted one without guessing at timing.
type listGate struct {
	target   string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	attempts atomic.Int32
}

func newListGate(target string) *listGate {
	return &listGate{target: target, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *listGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/2/files/list_folder" {
		if g.attempts.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error_summary": "restarted/..", "error": map[string]any{}})
			return
		}
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, g.target+r.URL.RequestURI(), bytes.NewReader(body))
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

func newGatedListService(t *testing.T, appKey string) (*Service, *fake.Server, *listGate) {
	t.Helper()
	var gate *listGate
	svc, fs, _ := newTestServiceWith(t, appKey, func(fakeURL string) string {
		gate = newListGate(fakeURL)
		pts := httptest.NewServer(gate)
		t.Cleanup(pts.Close)
		return pts.URL
	})
	return svc, fs, gate
}

type listCall struct {
	res *ListResult
	err error
}

// A cancelled first caller must neither fail the listing for a caller already waiting on the same
// key nor keep the cancelled caller until the listing ends.
func TestListSharedLoadSurvivesFirstCallerCancel(t *testing.T) {
	svc, fs, gate := newGatedListService(t, "share-cancel")
	acct := fs.Account("share-cancel")
	acct.Put("/big/a.txt", []byte("abc"), time.Unix(1700000000, 0).UTC())

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	aDone := make(chan listCall, 1)
	go func() {
		res, err := svc.List(ctxA, "/m/big", ListOptions{})
		aDone <- listCall{res: res, err: err}
	}()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the listing never reached the provider")
	}

	bDone := make(chan listCall, 1)
	go func() {
		res, err := svc.List(context.Background(), "/m/big", ListOptions{})
		bDone <- listCall{res: res, err: err}
	}()

	cancelA()
	select {
	case got := <-aDone:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancelled caller returned %v, want context.Canceled", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled caller stayed blocked on the shared load")
	}

	close(gate.release)
	select {
	case got := <-bDone:
		if got.err != nil {
			t.Fatalf("caller waiting on the shared load failed after the first caller cancelled: %v", got.err)
		}
		if len(got.res.Entries) != 1 || got.res.Entries[0].Name != "a.txt" {
			t.Fatalf("shared listing = %+v, want the one file a.txt", got.res.Entries)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the caller waiting on the shared load never finished")
	}
}
