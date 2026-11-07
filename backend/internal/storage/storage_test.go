package storage

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

func newManager(t *testing.T) (*Manager, *fake.Server, *sql.DB) {
	t.Helper()
	fs, ts := fake.NewServer()
	t.Cleanup(ts.Close)
	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	box, _ := secret.New(make([]byte, 32))
	cfg := &config.Config{Listen: ":8080", PublicURL: "https://files.example.com",
		DropboxAPIBase: ts.URL, DropboxContentBase: ts.URL, DropboxAuthorize: ts.URL + "/oauth2/authorize", DropboxTokenURL: ts.URL + "/oauth2/token"}
	m, err := NewManager(d, box, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m, fs, d
}

func stateOf(t *testing.T, authURL string) (string, string) {
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state"), u.Query().Get("redirect_uri")
}

func TestCreateLocalhostFlow(t *testing.T) {
	m, _, _ := newManager(t)
	start, err := m.Create(Input{MountPath: "/path/to", AppKey: "app1", AppSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	state, redirect := stateOf(t, start.AuthorizeURL)
	if redirect != "http://localhost:8080/api/oauth/callback" || start.Storage.Status != StatusPending {
		t.Fatalf("redirect=%s status=%s", redirect, start.Storage.Status)
	}
	if !strings.Contains(start.AuthorizeURL, "token_access_type=offline") {
		t.Fatal("offline missing")
	}
	s, err := m.CompleteWithState(context.Background(), state, "code-app1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusActive || s.AccountEmail != "app1@example.com" {
		t.Fatalf("%+v", s)
	}
	// Reuse is rejected.
	if _, err := m.CompleteWithState(context.Background(), state, "code-app1"); !errors.Is(err, ErrState) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := m.Client(s.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStateExpiryAndForgery(t *testing.T) {
	m, _, _ := newManager(t)
	start, err := m.Create(Input{MountPath: "/a", AppKey: "app1", AppSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompleteWithState(context.Background(), "forged", "code-app1"); !errors.Is(err, ErrState) {
		t.Fatal("forged state accepted")
	}
	state, _ := stateOf(t, start.AuthorizeURL)
	m.now = func() time.Time { return time.Now().Add(StateTTL + time.Minute) }
	if _, err := m.CompleteWithState(context.Background(), state, "code-app1"); !errors.Is(err, ErrState) {
		t.Fatal("expired state accepted")
	}
}

func TestCodePasteFlowSendsNoRedirect(t *testing.T) {
	m, _, _ := newManager(t)
	start, err := m.Create(Input{MountPath: "/a", AppKey: "app2", AppSecret: "s", AuthMode: AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(start.AuthorizeURL, "redirect_uri") {
		t.Fatal("code mode must not send redirect_uri")
	}
	s, err := m.CompleteWithCode(context.Background(), start.Storage.ID, "  code-app2 ")
	if err != nil || s.Status != StatusActive {
		t.Fatal(err)
	}
}

func TestPublicMode(t *testing.T) {
	m, _, _ := newManager(t)
	start, err := m.Create(Input{MountPath: "/a", AppKey: "app3", AppSecret: "s", AuthMode: AuthPublic})
	if err != nil {
		t.Fatal(err)
	}
	_, redirect := stateOf(t, start.AuthorizeURL)
	if redirect != "https://files.example.com/api/oauth/callback" {
		t.Fatal(redirect)
	}
}

func TestFakeAuthorizeRoundTrip(t *testing.T) {
	m, fs, _ := newManager(t)
	_ = fs
	start, err := m.Create(Input{MountPath: "/a", AppKey: "app4", AppSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	// Follow the fake authorize redirect without connecting to localhost.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(start.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	code, state, err := ParsePastedURL(loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompleteWithState(context.Background(), state, code); err != nil {
		t.Fatal(err)
	}
}

func TestParsePastedURL(t *testing.T) {
	if _, _, err := ParsePastedURL("  http://localhost:8080/api/oauth/callback?code=ab%2Fc&state=xy  "); err != nil {
		t.Fatal(err)
	}
	c, s, _ := ParsePastedURL("http://localhost:8080/api/oauth/callback?code=ab%2Fc&state=xy")
	if c != "ab/c" || s != "xy" {
		t.Fatal(c, s)
	}
	if _, _, err := ParsePastedURL("http://localhost:8080/api/oauth/callback?error=access_denied&error_description=user+said+no"); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatal(err)
	}
	if _, _, err := ParsePastedURL("http://localhost:8080/"); err == nil {
		t.Fatal("missing code accepted")
	}
}

func TestMountConflicts(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.Create(Input{MountPath: "/path/to", AppKey: "a", AppSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/path/to", "/PATH/TO", "/path", "/path/to/x"} {
		if _, err := m.Create(Input{MountPath: p, AppKey: "b", AppSecret: "s"}); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	if _, err := m.Create(Input{MountPath: "/path/to2", AppKey: "b", AppSecret: "s"}); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentCreatesCannotClaimConflictingMounts starts two Creates at once on mounts that may
// not coexist, and requires exactly one to win. The mount_path UNIQUE index only compares bytes,
// so it cannot reject a case-only duplicate or a nested path.
func TestConcurrentCreatesCannotClaimConflictingMounts(t *testing.T) {
	m, _, d := newManager(t)
	pairs := [][2]string{
		{"/Photos", "/photos"},
		{"/Media", "/Media/Sub"},
		{"/Docs", "/DOCS/sub"},
	}
	for round, pair := range pairs {
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, len(pair))
		for i, mount := range pair {
			wg.Add(1)
			go func(i int, mount string) {
				defer wg.Done()
				<-start
				_, errs[i] = m.Create(Input{MountPath: mount, AppKey: "k", AppSecret: "s"})
			}(i, mount)
		}
		close(start)
		wg.Wait()
		won := 0
		for _, err := range errs {
			if err == nil {
				won++
			}
		}
		if won != 1 {
			t.Fatalf("round %d (%q, %q): %d of 2 conflicting creates succeeded, want exactly 1 (errors: %v)", round, pair[0], pair[1], won, errs)
		}
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM storages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != round+1 {
			t.Fatalf("round %d: %d storages in the database, want %d", round, n, round+1)
		}
	}
}

func TestDeleteRevokes(t *testing.T) {
	m, fs, _ := newManager(t)
	start, _ := m.Create(Input{MountPath: "/a", AppKey: "app5", AppSecret: "s"})
	state, _ := stateOf(t, start.AuthorizeURL)
	s, err := m.CompleteWithState(context.Background(), state, "code-app5")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if fs.CallCount("auth/token/revoke") != 1 {
		t.Fatal("revoke not called")
	}
	if len(m.All()) != 0 {
		t.Fatal("not deleted")
	}
}

func TestSecretEncrypted(t *testing.T) {
	m, _, d := newManager(t)
	if _, err := m.Create(Input{MountPath: "/a", AppKey: "k", AppSecret: "super-secret"}); err != nil {
		t.Fatal(err)
	}
	var enc []byte
	d.QueryRow(`SELECT app_secret_enc FROM storages`).Scan(&enc)
	if strings.Contains(string(enc), "super-secret") {
		t.Fatal("secret stored in plaintext")
	}
}
