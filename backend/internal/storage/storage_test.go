package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
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

// TestConcurrentReloadsKeepSnapshotEqualToDatabase drives many write-then-reload cycles against
// one storage and then requires the in-memory snapshot to match the database row that was written
// last. It observes the published state, not the locking mechanism.
func TestConcurrentReloadsKeepSnapshotEqualToDatabase(t *testing.T) {
	m, _, d := newManager(t)
	start, err := m.Create(Input{Name: "v0", MountPath: "/a", AppKey: "app", AppSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	id := start.Storage.ID

	// Give every reload a nontrivial row set, so concurrent reloads actually overlap.
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 100; i++ {
		if _, err := tx.Exec(`INSERT INTO storages(name, mount_path, root_path, app_key, app_secret_enc, cache_ttl_min, status, auth_mode, redirect_uri, last_error, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("bulk-%d", i), fmt.Sprintf("/bulk/%d", i), "/", "k", []byte{}, 30, "pending", "localhost", "", "", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}

	const workers, rounds = 8, 60
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if _, err := d.Exec(`UPDATE storages SET name = ? WHERE id = ?`, fmt.Sprintf("w%d-r%d", w, r), id); err != nil {
					t.Errorf("worker %d round %d: %v", w, r, err)
					return
				}
				if err := m.reload(); err != nil {
					t.Errorf("worker %d round %d reload: %v", w, r, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	var want string
	if err := d.QueryRow(`SELECT name FROM storages WHERE id = ?`, id).Scan(&want); err != nil {
		t.Fatal(err)
	}
	got, ok := m.Get(id)
	if !ok {
		t.Fatal("storage missing from the published snapshot")
	}
	if got.Name != want {
		t.Fatalf("published name = %q, database name = %q", got.Name, want)
	}
}

// TestClientFailsFastWhileRestoreOwnsGate holds the restore gate and requires Client to refuse
// instead of handing out or publishing a client built from the row the restore is replacing.
func TestClientFailsFastWhileRestoreOwnsGate(t *testing.T) {
	m, fs, _ := newManager(t)
	_ = fs
	start, err := m.Create(Input{MountPath: "/a", AppKey: "app-client", AppSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := stateOf(t, start.AuthorizeURL)
	s, err := m.CompleteWithState(context.Background(), state, "code-app-client")
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID
	if _, err := m.Client(id); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	delete(m.clients, id)
	m.mu.Unlock()

	m.BeginRestore()
	if _, err := m.Client(id); !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("Client during restore err = %v, want ErrRestoreInProgress", err)
	}
	if cached := cachedClientFor(m, id); cached != nil {
		t.Fatal("a client was published while a restore owns the gate")
	}
	m.EndRestore()

	if _, err := m.Client(id); err != nil {
		t.Fatal(err)
	}
	if cached := cachedClientFor(m, id); cached == nil {
		t.Fatal("the client was not cached after the restore released the gate")
	}
}

// TestRestoreGateRejectsEveryStorageMutation keeps the restore gate in front of each path that
// writes the storages table, not only the create form.
func TestRestoreGateRejectsEveryStorageMutation(t *testing.T) {
	m, _, _ := newManager(t)
	m.BeginRestore()
	defer m.EndRestore()

	if _, err := m.Create(Input{MountPath: "/gated", AppKey: "k", AppSecret: "s"}); !errors.Is(err, ErrRestoreInProgress) {
		t.Errorf("Create err = %v, want ErrRestoreInProgress", err)
	}
	if _, err := m.Update(1, Input{MountPath: "/gated", CacheTTLMin: 30}); !errors.Is(err, ErrRestoreInProgress) {
		t.Errorf("Update err = %v, want ErrRestoreInProgress", err)
	}
	if err := m.Delete(context.Background(), 1); !errors.Is(err, ErrRestoreInProgress) {
		t.Errorf("Delete err = %v, want ErrRestoreInProgress", err)
	}
	if _, err := m.Reauth(1, AuthLocalhost); !errors.Is(err, ErrRestoreInProgress) {
		t.Errorf("Reauth err = %v, want ErrRestoreInProgress", err)
	}
	if _, err := m.CompleteWithState(context.Background(), "state", "code"); !errors.Is(err, ErrRestoreInProgress) {
		t.Errorf("CompleteWithState err = %v, want ErrRestoreInProgress", err)
	}
	if _, err := m.CompleteWithCode(context.Background(), 1, "code"); !errors.Is(err, ErrRestoreInProgress) {
		t.Errorf("CompleteWithCode err = %v, want ErrRestoreInProgress", err)
	}
}

func connectActiveStorage(t *testing.T, m *Manager) Storage {
	t.Helper()
	start, err := m.Create(Input{MountPath: "/a", AppKey: "app-client", AppSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := stateOf(t, start.AuthorizeURL)
	s, err := m.CompleteWithState(context.Background(), state, "code-app-client")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// cachedClientFor returns the client currently published for id, under the manager lock.
func cachedClientFor(m *Manager, id int64) *dropbox.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[id]
}

// TestClientPublicationCannotResurrectPreRestoreState forces the interleaving in which Client has
// already built a client from the pre-restore row when a restore replaces and refreshes the
// storage table. The cache afterwards must describe the restored database; a client for a pending,
// deleted or failed-init row must not be published. It observes the cache and the database row,
// not whether the gate was taken.
func TestClientPublicationCannotResurrectPreRestoreState(t *testing.T) {
	cases := []struct {
		name    string
		restore func(*sql.DB, int64) error
		status  string
	}{
		{
			name: "restored pending row",
			restore: func(d *sql.DB, id int64) error {
				_, err := d.Exec(`UPDATE storages SET status = ?, last_error = 'restored-pending' WHERE id = ?`, StatusPending, id)
				return err
			},
			status: StatusPending,
		},
		{
			name: "restored row deleted",
			restore: func(d *sql.DB, id int64) error {
				_, err := d.Exec(`DELETE FROM storages WHERE id = ?`, id)
				return err
			},
			status: "",
		},
		{
			name: "restored row whose client cannot be built",
			restore: func(d *sql.DB, id int64) error {
				_, err := d.Exec(`UPDATE storages SET status = ?, refresh_token_enc = NULL, last_error = '' WHERE id = ?`, StatusActive, id)
				return err
			},
			status: StatusError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, d := newManager(t)
			s := connectActiveStorage(t, m)

			built := make(chan struct{})
			release := make(chan struct{})
			var builtOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			m.clientBuiltHook = func() {
				builtOnce.Do(func() { close(built) })
				<-release
			}
			defer func() { m.clientBuiltHook = nil }()

			clientDone := make(chan error, 1)
			go func() {
				_, err := m.Client(s.ID)
				clientDone <- err
			}()
			<-built

			runRestore := func() error {
				m.BeginRestore()
				defer m.EndRestore()
				if err := tc.restore(d, s.ID); err != nil {
					return err
				}
				_, err := m.ResetAndInitClients()
				return err
			}

			// Before the fix Client built the client and only then tried the gate, so the gate
			// was free here and the restore could complete before the stale client was
			// published. A gate-first Client holds the read side, so the restore has to wait.
			if m.mutationMu.TryLock() {
				m.mutationMu.Unlock()
				if err := runRestore(); err != nil {
					t.Fatal(err)
				}
				unblock()
			} else {
				restoreDone := make(chan struct{})
				restoreErr := make(chan error, 1)
				go func() {
					restoreErr <- runRestore()
					close(restoreDone)
				}()
				unblock()
				<-restoreDone
				if err := <-restoreErr; err != nil {
					t.Fatal(err)
				}
			}
			if err := <-clientDone; err != nil {
				t.Fatalf("Client: %v", err)
			}

			// The database is the truth. The published snapshot and the client cache must agree
			// with the row the restore left behind.
			var dbStatus string
			dbErr := d.QueryRow(`SELECT status FROM storages WHERE id = ?`, s.ID).Scan(&dbStatus)
			if tc.status == "" {
				if !errors.Is(dbErr, sql.ErrNoRows) {
					t.Fatalf("restored database row: %v, want no row", dbErr)
				}
				if got, ok := m.Get(s.ID); ok {
					t.Fatalf("deleted storage is still published: %+v", got)
				}
			} else {
				if dbErr != nil || dbStatus != tc.status {
					t.Fatalf("database status = %q (err %v), want %q", dbStatus, dbErr, tc.status)
				}
				if got, ok := m.Get(s.ID); !ok || got.Status != tc.status {
					t.Fatalf("published storage = %+v (found %v), want status %q", got, ok, tc.status)
				}
			}
			if c := cachedClientFor(m, s.ID); c != nil {
				t.Fatalf("cache still holds a client for a storage whose restored row has no usable client (status %q)", tc.status)
			}
			if _, err := m.Client(s.ID); err == nil {
				t.Fatalf("Client returned a cached pre-restore client for a storage whose restored row has no usable client (status %q)", tc.status)
			}
		})
	}
}

// TestCallbackFromReplacedClientDoesNotChangeNewerCredentials models a completed reauth: the row
// carries a new refresh token and a new client is cached, while the old client's callback arrives
// later. The old callback must not mark the row failed or evict the replacement. The replacement's
// own callback must still work.
func TestCallbackFromReplacedClientDoesNotChangeNewerCredentials(t *testing.T) {
	m, _, d := newManager(t)
	s := connectActiveStorage(t, m)
	oldClient, err := m.Client(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := m.box.Seal([]byte("reauthorized-refresh-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE storages SET refresh_token_enc = ?, last_error = '', updated_at = ? WHERE id = ?`,
		enc, time.Now().Unix(), s.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}
	// complete() drops the old client after writing the new token; the next Client rebuilds it.
	m.mu.Lock()
	delete(m.clients, s.ID)
	m.mu.Unlock()
	newClient, err := m.Client(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newClient == oldClient {
		t.Fatal("the reauthorized storage still reuses the old client")
	}

	oldClient.OnReauthNeeded()

	var status, lastError string
	if err := d.QueryRow(`SELECT status, last_error FROM storages WHERE id = ?`, s.ID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != StatusActive || lastError != "" {
		t.Fatalf("stale callback changed the reauthorized row to status %q, last_error %q", status, lastError)
	}
	if got := cachedClientFor(m, s.ID); got != newClient {
		t.Fatalf("stale callback changed the reauthorized client cache: got %p, want %p", got, newClient)
	}

	newClient.OnReauthNeeded()
	if err := d.QueryRow(`SELECT status FROM storages WHERE id = ?`, s.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusError {
		t.Fatalf("reauthorized client callback left status %q, want %q", status, StatusError)
	}
}

// TestStaleReauthCallbackDoesNotChangeRestoredState starts a callback from a client built before
// a restore replaced the row under the same id. Once the restore completed, that callback must not
// mark the restored row failed or remove the restored client. A callback from the restored client
// must still do both.
func TestStaleReauthCallbackDoesNotChangeRestoredState(t *testing.T) {
	for _, status := range []string{StatusActive, StatusPending} {
		t.Run(status, func(t *testing.T) {
			m, _, d := newManager(t)
			s := connectActiveStorage(t, m)
			oldClient, err := m.Client(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			enc, err := m.box.Seal([]byte("restored-refresh-token"))
			if err != nil {
				t.Fatal(err)
			}

			m.BeginRestore()
			if _, err := d.Exec(`UPDATE storages SET name = ?, status = ?, refresh_token_enc = ?, last_error = '', updated_at = ? WHERE id = ?`,
				"restored", status, enc, time.Now().Unix(), s.ID); err != nil {
				m.EndRestore()
				t.Fatal(err)
			}
			if _, err := m.ResetAndInitClients(); err != nil {
				m.EndRestore()
				t.Fatal(err)
			}
			m.EndRestore()

			got, ok := m.Get(s.ID)
			if !ok || got.Name != "restored" || got.Status != status {
				t.Fatalf("restored storage not published: %+v (found %v)", got, ok)
			}
			newClient := cachedClientFor(m, s.ID)
			if status == StatusActive {
				if newClient == nil || newClient == oldClient {
					t.Fatalf("restored active storage has no new client (cached %p, old %p)", newClient, oldClient)
				}
			} else if newClient != nil {
				t.Fatalf("restored pending storage has a cached client")
			}

			oldClient.OnReauthNeeded()

			var dbStatus, lastError string
			if err := d.QueryRow(`SELECT status, last_error FROM storages WHERE id = ?`, s.ID).Scan(&dbStatus, &lastError); err != nil {
				t.Fatal(err)
			}
			if dbStatus != status || lastError != "" {
				t.Fatalf("stale callback changed the restored row to status %q, last_error %q", dbStatus, lastError)
			}
			if got, ok := m.Get(s.ID); !ok || got.Status != status || got.LastError != "" {
				t.Fatalf("stale callback changed the published storage: %+v (found %v)", got, ok)
			}
			if c := cachedClientFor(m, s.ID); c != newClient {
				t.Fatalf("stale callback changed the restored client cache: got %p, want %p", c, newClient)
			}

			if newClient != nil {
				newClient.OnReauthNeeded()
				if err := d.QueryRow(`SELECT status FROM storages WHERE id = ?`, s.ID).Scan(&dbStatus); err != nil {
					t.Fatal(err)
				}
				if dbStatus != StatusError {
					t.Fatalf("current client callback left status %q, want %q", dbStatus, StatusError)
				}
				if c := cachedClientFor(m, s.ID); c != nil {
					t.Fatalf("current client callback left a cached client")
				}
			}
		})
	}
}

// TestReauthCallbackCannotOverwriteCompletionAfterIdentityCheck forces the narrow interleaving in
// which a stale client's callback has already passed the identity/generation check when a
// concurrent reauth completion stores the new token and a replacement client is published. The
// callback must not overwrite the completed row or evict the replacement client. The callback is
// held at its first clock read, which is after the check and before the status write.
func TestReauthCallbackCannotOverwriteCompletionAfterIdentityCheck(t *testing.T) {
	m, _, d := newManager(t)
	s := connectActiveStorage(t, m)
	oldClient, err := m.Client(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var oldTokenEnc []byte
	if err := d.QueryRow(`SELECT refresh_token_enc FROM storages WHERE id = ?`, s.ID).Scan(&oldTokenEnc); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reauth(s.ID, AuthCode); err != nil {
		t.Fatal(err)
	}

	checked := make(chan struct{})
	release := make(chan struct{})
	var clockCalls atomic.Int32
	m.now = func() time.Time {
		if clockCalls.Add(1) == 1 {
			close(checked)
			<-release
		}
		return time.Now()
	}
	defer func() { m.now = time.Now }()

	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		oldClient.OnReauthNeeded()
	}()
	<-checked

	completed, err := m.CompleteWithCode(context.Background(), s.ID, "code-app-client")
	if err != nil {
		t.Fatalf("complete after the callback check: %v", err)
	}
	if completed.Status != StatusActive {
		t.Fatalf("completed status = %q, want %q", completed.Status, StatusActive)
	}
	newClient, err := m.Client(s.ID)
	if err != nil {
		t.Fatalf("client for the completed row: %v", err)
	}
	if newClient == oldClient {
		t.Fatal("completion reused the pre-completion client")
	}
	close(release)
	<-callbackDone

	var status, lastError string
	var newTokenEnc []byte
	if err := d.QueryRow(`SELECT status, last_error, refresh_token_enc FROM storages WHERE id = ?`, s.ID).
		Scan(&status, &lastError, &newTokenEnc); err != nil {
		t.Fatal(err)
	}
	if status != StatusActive || lastError != "" {
		t.Errorf("stale callback changed the completed row to status %q, last_error %q", status, lastError)
	}
	if string(newTokenEnc) == string(oldTokenEnc) {
		t.Errorf("completion did not replace the stored credential")
	}
	if got := cachedClientFor(m, s.ID); got != newClient {
		t.Errorf("stale callback changed the completed client cache: got %p, want %p", got, newClient)
	}
	if got, ok := m.Get(s.ID); !ok || got.Status != StatusActive || got.LastError != "" {
		t.Errorf("published storage disagrees with the completed row: %+v (found %v)", got, ok)
	}
}

// TestClientBuiltBeforeCompletionIsNotPublishedAfterIt forces a client build that has already
// read the pre-completion token to finish publishing after a concurrent reauth completion. The
// completed token must not be paired with a cached pre-completion client; the next Client must
// be built from the completed row.
func TestClientBuiltBeforeCompletionIsNotPublishedAfterIt(t *testing.T) {
	m, _, d := newManager(t)
	s := connectActiveStorage(t, m)
	var oldTokenEnc []byte
	if err := d.QueryRow(`SELECT refresh_token_enc FROM storages WHERE id = ?`, s.ID).Scan(&oldTokenEnc); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reauth(s.ID, AuthCode); err != nil {
		t.Fatal(err)
	}

	built := make(chan struct{})
	release := make(chan struct{})
	var builtOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	m.clientBuiltHook = func() {
		builtOnce.Do(func() { close(built) })
		<-release
	}
	defer func() { m.clientBuiltHook = nil }()

	type clientResult struct {
		client *dropbox.Client
		err    error
	}
	clientDone := make(chan clientResult, 1)
	go func() {
		c, err := m.Client(s.ID)
		clientDone <- clientResult{client: c, err: err}
	}()
	<-built

	completeDone := make(chan error, 1)
	go func() {
		_, err := m.CompleteWithCode(context.Background(), s.ID, "code-app-client")
		completeDone <- err
	}()
	completedDuringBuild := false
	select {
	case err := <-completeDone:
		if err != nil {
			t.Fatalf("complete while the client build was held: %v", err)
		}
		completedDuringBuild = true
	case <-time.After(2 * time.Second):
		// The completion is waiting for the state boundary. Release the build so it can run;
		// the order below is what must not leave a stale client in the cache.
	}
	unblock()
	got := <-clientDone
	if got.err != nil {
		t.Fatalf("client build: %v", got.err)
	}
	if !completedDuringBuild {
		if err := <-completeDone; err != nil {
			t.Fatalf("complete after the client build: %v", err)
		}
	}

	var status string
	var newTokenEnc []byte
	if err := d.QueryRow(`SELECT status, refresh_token_enc FROM storages WHERE id = ?`, s.ID).
		Scan(&status, &newTokenEnc); err != nil {
		t.Fatal(err)
	}
	if status != StatusActive {
		t.Fatalf("completed status = %q, want %q", status, StatusActive)
	}
	if string(newTokenEnc) == string(oldTokenEnc) {
		t.Fatal("completion did not replace the stored credential")
	}
	if cached := cachedClientFor(m, s.ID); cached == got.client {
		t.Errorf("a client built from the pre-completion token was published after completion")
	}
	rebuilt, err := m.Client(s.ID)
	if err != nil {
		t.Fatalf("Client after completion: %v", err)
	}
	if rebuilt == got.client {
		t.Errorf("Client returned the pre-completion client after the token was replaced")
	}
}

// TestReauthCallbackDuringRestoreDoesNotChangeStorage keeps a callback that fires while the
// restore gate is held from writing the row or the cache. An aborted restore did not replace the
// row, so the same client's later callback must still mark the error.
func TestReauthCallbackDuringRestoreDoesNotChangeStorage(t *testing.T) {
	m, _, d := newManager(t)
	s := connectActiveStorage(t, m)
	c, err := m.Client(s.ID)
	if err != nil {
		t.Fatal(err)
	}

	m.BeginRestore()
	c.OnReauthNeeded()
	var status, lastError string
	if err := d.QueryRow(`SELECT status, last_error FROM storages WHERE id = ?`, s.ID).Scan(&status, &lastError); err != nil {
		m.EndRestore()
		t.Fatal(err)
	}
	if status != StatusActive || lastError != "" {
		m.EndRestore()
		t.Fatalf("callback during restore changed status %q, last_error %q", status, lastError)
	}
	if got := cachedClientFor(m, s.ID); got != c {
		m.EndRestore()
		t.Fatalf("callback during restore changed the cached client")
	}
	m.EndRestore()

	c.OnReauthNeeded()
	if err := d.QueryRow(`SELECT status FROM storages WHERE id = ?`, s.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusError {
		t.Fatalf("callback after aborted restore left status %q, want %q", status, StatusError)
	}
	if got := cachedClientFor(m, s.ID); got != nil {
		t.Fatalf("callback after aborted restore left a cached client")
	}
}
