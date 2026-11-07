package auth

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

func newService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	return s, d
}

// insertSession writes a session row the way a login in an earlier process would have left it.
func insertSession(t *testing.T, d *sql.DB, tok string, exp time.Time) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := d.Exec(`INSERT INTO sessions(token_hash, created_at, expires_at, last_seen, ip, user_agent) VALUES(?,?,?,?,?,?)`,
		secret.HashToken(tok), now, exp.Unix(), now, "127.0.0.1", "test"); err != nil {
		t.Fatal(err)
	}
}

func sessionRowExists(t *testing.T, d *sql.DB, tok string) bool {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, secret.HashToken(tok)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// TestVerifyPasswordAcceptsBothFormats pins the compatibility contract: a bcrypt hash in the admin
// row still logs in, a wrong or damaged hash does not, and the argon2id path is unchanged.
func TestVerifyPasswordAcceptsBothFormats(t *testing.T) {
	bcryptHash, err := bcrypt.GenerateFromPassword([]byte("restored-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	argonHash, err := HashPassword("current-password")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		hash string
		pw   string
		want bool
	}{
		{"bcrypt correct", string(bcryptHash), "restored-password", true},
		{"bcrypt wrong", string(bcryptHash), "wrong-password", false},
		{"bcrypt truncated", string(bcryptHash)[:20], "restored-password", false},
		{"bcrypt invalid cost", "$2a$99$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTU", "restored-password", false},
		{"argon2id correct", argonHash, "current-password", true},
		{"argon2id wrong", argonHash, "wrong-password", false},
		{"argon2id truncated", argonHash[:len(argonHash)-8], "current-password", false},
		{"empty", "", "x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyPassword(c.hash, c.pw); got != c.want {
				t.Fatalf("VerifyPassword(%q) = %v, want %v", c.hash, got, c.want)
			}
		})
	}
}

func TestValidateRejectsLoggedOutAndExpiredSessions(t *testing.T) {
	s, d := newService(t)
	insertSession(t, d, "live", time.Now().Add(time.Hour))
	if !s.Validate("live") {
		t.Fatal("live session rejected")
	}
	s.Logout("live")
	if s.Validate("live") {
		t.Fatal("logged out session still valid")
	}

	insertSession(t, d, "stale", time.Now().Add(-time.Minute))
	if s.Validate("stale") {
		t.Fatal("expired session accepted")
	}
	if sessionRowExists(t, d, "stale") {
		t.Fatal("expired session row kept")
	}
}

// TestTouchDoesNotReviveRevokedSession covers the cache-hit slide path: after a revoke clears the
// cache entry, the slide that was already in flight must not write it back.
func TestTouchDoesNotReviveRevokedSession(t *testing.T) {
	s, d := newService(t)
	insertSession(t, d, "tok", time.Now().Add(time.Hour))
	if !s.Validate("tok") {
		t.Fatal("seeded session rejected")
	}
	s.Logout("tok")
	if s.touch(secret.HashToken("tok"), time.Now().Add(time.Minute)) {
		t.Fatal("touch reported a logged out session as live")
	}
	if s.Validate("tok") {
		t.Fatal("logged out session came back")
	}
}

// TestValidateCannotUndoLogout runs Validate and Logout concurrently. The cache is warmed and the
// clock is moved past the 10 minute slide window first, so the racing request takes the sliding
// cache-hit path. A revoked session must stay revoked once both calls return. The window this needs
// is narrower than a scheduler slice, so the run is a bounded invariant guard, not a reproduction.
func TestValidateCannotUndoLogout(t *testing.T) {
	s, d := newService(t)
	for i := 0; i < 200; i++ {
		insertSession(t, d, "tok", time.Now().Add(time.Hour))
		base := time.Now()
		s.now = func() time.Time { return base }
		if !s.Validate("tok") {
			t.Fatalf("iteration %d: seeded session rejected", i)
		}
		s.now = func() time.Time { return base.Add(11 * time.Minute) }
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			s.Validate("tok")
		}()
		go func() {
			defer wg.Done()
			<-start
			s.Logout("tok")
		}()
		close(start)
		wg.Wait()
		if s.Validate("tok") {
			t.Fatalf("iteration %d: a Validate that read the cache before Logout slid the revoked session back in", i)
		}
	}
}

// TestValidateCannotUndoRevokeAll is the same bounded stress run against the password-change path.
func TestValidateCannotUndoRevokeAll(t *testing.T) {
	s, d := newService(t)
	for i := 0; i < 200; i++ {
		insertSession(t, d, "tok", time.Now().Add(time.Hour))
		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			revoke error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			s.Validate("tok")
		}()
		go func() {
			defer wg.Done()
			<-start
			revoke = s.RevokeAllSessions()
		}()
		close(start)
		wg.Wait()
		if revoke != nil {
			t.Fatalf("iteration %d: RevokeAllSessions: %v", i, revoke)
		}
		if s.Validate("tok") {
			t.Fatalf("iteration %d: a Validate that read the row before the delete put the revoked session back", i)
		}
	}
}

// TestCreatedSessionMatchesRevokeAll races a fresh login against a revoke-all and requires the cache
// to agree with the sessions table afterwards. A token issued before the delete is gone, a token
// issued after it lives, and neither answer may come from a cache entry the table disagrees with.
// The assignment happens in a window narrower than a scheduler slice, so this is a bounded
// invariant guard for the lock boundary rather than a reproduction of the preemption case.
func TestCreatedSessionMatchesRevokeAll(t *testing.T) {
	s, d := newService(t)
	for i := 0; i < 200; i++ {
		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			tok    string
			create error
			revoke error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			tok, create = s.createSession("127.0.0.1", "test")
		}()
		go func() {
			defer wg.Done()
			<-start
			revoke = s.RevokeAllSessions()
		}()
		close(start)
		wg.Wait()
		if create != nil {
			t.Fatalf("iteration %d: createSession: %v", i, create)
		}
		if revoke != nil {
			t.Fatalf("iteration %d: RevokeAllSessions: %v", i, revoke)
		}
		row := sessionRowExists(t, d, tok)
		if got := s.Validate(tok); got != row {
			t.Fatalf("iteration %d: cache and sessions table disagree, Validate=%v and row exists=%v", i, got, row)
		}
		if err := s.RevokeAllSessions(); err != nil {
			t.Fatalf("iteration %d: cleanup revoke: %v", i, err)
		}
	}
}
