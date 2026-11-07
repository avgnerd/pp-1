package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// newFixedClockManager builds a manager whose token endpoint blocks the given code until release
// and whose clock never advances. A fixed clock is the point: both completions write the same
// authorized_at millisecond, so the timestamp alone cannot tell the stale failure apart.
func newFixedClockManager(t *testing.T, blockedCode string, started chan<- struct{}, release <-chan struct{}, fixed time.Time) *Manager {
	t.Helper()
	_, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
			_ = r.ParseForm()
			if r.Form.Get("code") == blockedCode {
				select {
				case started <- struct{}{}:
				default:
				}
				<-release
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, fts.URL+r.URL.RequestURI(), bytes.NewReader(body))
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
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)

	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	box, _ := secret.New(make([]byte, 32))
	cfg := &config.Config{Listen: ":8080", PublicURL: "https://files.example.com",
		DropboxAPIBase: fts.URL, DropboxContentBase: fts.URL,
		DropboxAuthorize: fts.URL + "/oauth2/authorize", DropboxTokenURL: proxy.URL + "/oauth2/token"}
	m, err := NewManager(d, box, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return fixed }
	return m
}

// TestStaleCompletionCannotOverwriteSameMillisecondSuccess reproduces the order the audit found,
// with the clock fixed so the successful completion rewrites the exact authorized_at value the
// stale attempt captured. The late failure must not land on the activated row, and the test
// asserts that the two completions really wrote the same millisecond.
func TestStaleCompletionCannotOverwriteSameMillisecondSuccess(t *testing.T) {
	fixed := time.UnixMilli(1794041520104)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	m := newFixedClockManager(t, "code-bad", started, release, fixed)

	start, err := m.Create(Input{MountPath: "/s", AppKey: "app1", AppSecret: "s", AuthMode: AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	id := start.Storage.ID
	first, err := m.CompleteWithCode(context.Background(), id, "code-app1")
	if err != nil {
		t.Fatal(err)
	}
	if first.AuthorizedAt != fixed.UnixMilli() {
		t.Fatalf("first completion authorized_at = %d, want the fixed %d", first.AuthorizedAt, fixed.UnixMilli())
	}

	if _, err := m.Reauth(id, AuthCode); err != nil {
		t.Fatal(err)
	}
	staleDone := make(chan error, 1)
	go func() {
		_, err := m.CompleteWithCode(context.Background(), id, "code-bad")
		staleDone <- err
	}()
	<-started

	if _, err := m.Reauth(id, AuthCode); err != nil {
		t.Fatal(err)
	}
	second, err := m.CompleteWithCode(context.Background(), id, "code-app1")
	if err != nil {
		t.Fatal(err)
	}
	if second.AuthorizedAt != first.AuthorizedAt {
		t.Fatalf("the test did not exercise a same-millisecond write: %d then %d", first.AuthorizedAt, second.AuthorizedAt)
	}

	close(release)
	if err := <-staleDone; err == nil {
		t.Fatal("the stale completion reported success")
	}

	got, ok := m.Get(id)
	if !ok {
		t.Fatal("storage disappeared")
	}
	if got.Status != StatusActive {
		t.Fatalf("status = %q, want %q", got.Status, StatusActive)
	}
	if got.LastError != "" {
		t.Fatalf("a stale failure overwrote the active row: last_error = %q", got.LastError)
	}
	if got.AuthorizedAt != second.AuthorizedAt {
		t.Fatalf("authorized_at = %d, want %d", got.AuthorizedAt, second.AuthorizedAt)
	}
}

// TestGenuineReauthFailureStillRecordsError is the opposite direction: with the same fixed clock
// but no concurrent success, the failure must stay visible. A revision guard that hid every
// failure would pass the stale test and fail this one.
func TestGenuineReauthFailureStillRecordsError(t *testing.T) {
	m, _, _ := newManager(t)
	fixed := time.UnixMilli(1794041520104)
	m.now = func() time.Time { return fixed }

	start, err := m.Create(Input{MountPath: "/s", AppKey: "app1", AppSecret: "s", AuthMode: AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	id := start.Storage.ID
	active, err := m.CompleteWithCode(context.Background(), id, "code-app1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reauth(id, AuthCode); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompleteWithCode(context.Background(), id, "code-bad"); err == nil {
		t.Fatal("the failing completion reported success")
	}
	got, _ := m.Get(id)
	if got.Status != StatusActive {
		t.Fatalf("status = %q, want %q", got.Status, StatusActive)
	}
	if got.LastError == "" {
		t.Fatal("a failing reauth on its own row recorded no error")
	}
	if got.AuthorizedAt != active.AuthorizedAt {
		t.Fatalf("authorized_at changed on a failed completion: %d -> %d", active.AuthorizedAt, got.AuthorizedAt)
	}
}
