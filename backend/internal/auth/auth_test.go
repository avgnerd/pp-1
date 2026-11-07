package auth

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
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
	bcryptDefaultHash, err := bcrypt.GenerateFromPassword([]byte("restored-password"), bcrypt.DefaultCost)
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
		{"bcrypt min cost correct", string(bcryptHash), "restored-password", true},
		{"bcrypt min cost wrong", string(bcryptHash), "wrong-password", false},
		{"bcrypt min cost truncated", string(bcryptHash)[:20], "restored-password", false},
		{"bcrypt default cost correct", string(bcryptDefaultHash), "restored-password", true},
		{"bcrypt default cost wrong", string(bcryptDefaultHash), "wrong-password", false},
		{"bcrypt default cost truncated", string(bcryptDefaultHash)[:20], "restored-password", false},
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

// bcryptAtCost rewrites only the cost field of a valid bcrypt string, so validation can be tested
// across the legacy 4..31 range without ever running a high-cost KDF.
func bcryptAtCost(t *testing.T, hash string, cost int) string {
	t.Helper()
	parts := strings.SplitN(hash, "$", 4)
	if len(parts) != 4 {
		t.Fatalf("test bcrypt hash has an unexpected shape: %q", hash)
	}
	parts[2] = fmt.Sprintf("%02d", cost)
	return strings.Join(parts, "$")
}

// TestValidatePasswordHashBcryptCostBoundary pins the user-selected cap: cost 4..14 is accepted,
// cost 15..31 is rejected before any KDF runs. Only the cost field of a valid hash changes.
func TestValidatePasswordHashBcryptCostBoundary(t *testing.T) {
	base, err := bcrypt.GenerateFromPassword([]byte("restored-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, cost := range []int{4, 10, 14} {
		if err := ValidatePasswordHash(bcryptAtCost(t, string(base), cost)); err != nil {
			t.Errorf("bcrypt cost %d rejected: %v", cost, err)
		}
	}
	for _, cost := range []int{15, 31} {
		if err := ValidatePasswordHash(bcryptAtCost(t, string(base), cost)); err == nil {
			t.Errorf("bcrypt cost %d accepted, want rejection", cost)
		}
	}
}

// TestVerifyPasswordRejectsBcryptAboveCostCap requires the login path to reject cost 15..31 before
// running bcrypt, so the login range cannot diverge from restore validation.
func TestVerifyPasswordRejectsBcryptAboveCostCap(t *testing.T) {
	base, err := bcrypt.GenerateFromPassword([]byte("restored-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, cost := range []int{15, 31} {
		if VerifyPassword(bcryptAtCost(t, string(base), cost), "restored-password") {
			t.Errorf("bcrypt cost %d was accepted by login", cost)
		}
	}
}

// TestValidatePasswordHashAcceptsLegacyArgonMemoryMinimum preserves the old validator's 8 KiB
// memory floor: low memory is not a resource-exhaustion risk, and rejecting it would break a
// legacy backup that this app can still verify.
func TestValidatePasswordHashAcceptsLegacyArgonMemoryMinimum(t *testing.T) {
	hash := argon2Hash("legacy-password", []byte("12345678"), 1, 8, 1, 16)
	if err := ValidatePasswordHash(hash); err != nil {
		t.Fatalf("legacy argon2 memory minimum rejected: %v", err)
	}
	if !VerifyPassword(hash, "legacy-password") {
		t.Fatal("login rejects an argon2 hash at the legacy memory minimum")
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

// argon2Hash builds a PHC string with caller-chosen parameters, so a test can present the login
// path a hash whose parameters this build would never write.
func argon2Hash(password string, salt []byte, t, m uint32, p uint8, keyLen uint32) string {
	h := argon2.IDKey([]byte(password), salt, t, m, p, keyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", m, t, p,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h))
}

// TestLoginRejectsSessionIssuedAcrossPasswordChange covers the credential TOCTOU: Login reads the
// admin row, the password changes and revokes every session, and only then does Login write its
// session. A session minted from the pre-change credential snapshot must not survive.
func TestLoginRejectsSessionIssuedAcrossPasswordChange(t *testing.T) {
	s, _ := newService(t)
	verified := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.verifyPassword = func(encoded, pw string) bool {
		ok := VerifyPassword(encoded, pw)
		paused := false
		once.Do(func() { paused = true })
		if paused {
			close(verified)
			<-release
		}
		return ok
	}
	t.Cleanup(func() { s.verifyPassword = VerifyPassword })

	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := s.Login(DefaultUsername, DefaultPassword, "198.51.100.7", "test")
		done <- result{token: tok, err: err}
	}()
	<-verified
	if _, err := s.UpdateProfile("", DefaultPassword, DefaultUsername, "new-password-1", "198.51.100.8", "test"); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	close(release)
	r := <-done
	if r.err != nil {
		return
	}
	if s.Validate(r.token) {
		t.Fatal("a login that read the old credential minted a session after the password change revoked every session")
	}
}

// profileResult is one UpdateProfile outcome.
type profileResult struct {
	token string
	err   error
}

// startHeldUpdateProfile starts UpdateProfile with its KDF held open. The KDF wrapper runs the
// real verification or hash first, then blocks until release, so the caller exercises the real
// credential boundary while proving the KDF runs outside credMu.
func startHeldUpdateProfile(t *testing.T, s *Service, holdVerify bool) (entered, release chan struct{}, done chan profileResult) {
	t.Helper()
	entered = make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	if holdVerify {
		s.verifyPassword = func(encoded, pw string) bool {
			ok := VerifyPassword(encoded, pw)
			paused := false
			once.Do(func() { paused = true })
			if paused {
				close(entered)
				<-release
			}
			return ok
		}
	} else {
		s.hashPassword = func(pw string) (string, error) {
			h, err := HashPassword(pw)
			if err != nil {
				return "", err
			}
			paused := false
			once.Do(func() { paused = true })
			if paused {
				close(entered)
				<-release
			}
			return h, nil
		}
	}
	t.Cleanup(func() {
		s.verifyPassword = VerifyPassword
		s.hashPassword = HashPassword
	})
	done = make(chan profileResult, 1)
	go func() {
		tok, err := s.UpdateProfile("", DefaultPassword, DefaultUsername, "new-password-1", "198.51.100.9", "test")
		done <- profileResult{token: tok, err: err}
	}()
	<-entered
	return entered, release, done
}

// TestUpdateProfileDoesNotHoldCredentialLockDuringVerify runs another credential mutation while
// the current-password KDF is open. That mutation must not wait for the KDF, and the profile
// change must then fail its snapshot recheck instead of overwriting the newer credential.
func TestUpdateProfileDoesNotHoldCredentialLockDuringVerify(t *testing.T) {
	s, _ := newService(t)
	_, release, done := startHeldUpdateProfile(t, s, true)

	revoked := make(chan error, 1)
	go func() { revoked <- s.RevokeAllSessions() }()
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatalf("RevokeAllSessions: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("RevokeAllSessions waited for the password KDF; credMu is held across it")
	}
	close(release)
	r := <-done
	if !errors.Is(r.err, errCredentialsChanged) {
		t.Fatalf("UpdateProfile err = %v, want errCredentialsChanged", r.err)
	}
	if _, err := s.Login(DefaultUsername, DefaultPassword, "198.51.100.10", "test"); err != nil {
		t.Fatalf("the aborted profile change still replaced the password: %v", err)
	}
}

// TestUpdateProfileLeavesCredentialUnchangedWhenAdminWriteFails forces the admin UPDATE to fail
// after the KDF and session revocation, and requires the old credential to remain valid.
func TestUpdateProfileLeavesCredentialUnchangedWhenAdminWriteFails(t *testing.T) {
	s, d := newService(t)
	if _, err := d.Exec(`CREATE TRIGGER fail_admin_update BEFORE UPDATE ON admin BEGIN SELECT RAISE(ABORT, 'admin write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProfile("", DefaultPassword, "renamed", "new-password-1", "198.51.100.12", "test"); err == nil {
		t.Fatal("UpdateProfile reported success although the admin row was not written")
	}
	if a, err := s.Admin(); err != nil || a.Username != DefaultUsername {
		t.Fatalf("admin row after failed update = %+v, %v", a, err)
	}
	if _, err := s.Login(DefaultUsername, DefaultPassword, "198.51.100.13", "test"); err != nil {
		t.Fatalf("old password no longer logs in: %v", err)
	}
	if _, err := s.Login(DefaultUsername, "new-password-1", "198.51.100.14", "test"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("new password login err = %v, want ErrInvalidCredentials", err)
	}
}

// TestUpdateProfileLeavesOldCredentialWhenRevokeFails forces the session delete to fail. The new
// credential must not be written in that case, because it would coexist with still-live sessions.
func TestUpdateProfileLeavesOldCredentialWhenRevokeFails(t *testing.T) {
	s, d := newService(t)
	insertSession(t, d, "live", time.Now().Add(time.Hour))
	if _, err := d.Exec(`CREATE TRIGGER fail_session_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'session delete failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProfile("", DefaultPassword, DefaultUsername, "new-password-1", "198.51.100.15", "test"); err == nil {
		t.Fatal("UpdateProfile reported success although the session revoke failed")
	}
	if !s.Validate("live") {
		t.Fatal("the existing session disappeared even though the password was not changed")
	}
	if _, err := s.Login(DefaultUsername, DefaultPassword, "198.51.100.16", "test"); err != nil {
		t.Fatalf("old password no longer logs in: %v", err)
	}
}

// TestUpdateProfileAbortsWhenRestoreChangesAdminRow holds the profile KDF open, then simulates the
// restore window: BeginRestore holds the credential lock, the admin row changes, and the runtime
// refresh revokes sessions. The profile change must not overwrite the restored row.
func TestUpdateProfileAbortsWhenRestoreChangesAdminRow(t *testing.T) {
	s, d := newService(t)
	_, release, done := startHeldUpdateProfile(t, s, true)

	restored := make(chan error, 1)
	go func() {
		s.BeginRestore()
		_, err := d.Exec(`UPDATE admin SET username = 'restored-admin', updated_at = 1 WHERE id = 1`)
		if err == nil {
			err = s.RevokeAllSessionsDuringRestore()
		}
		s.EndRestore()
		restored <- err
	}()
	select {
	case err := <-restored:
		if err != nil {
			close(release)
			<-done
			t.Fatalf("restore simulation: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("BeginRestore waited for the profile KDF; credMu is held across it")
	}
	close(release)
	r := <-done
	if !errors.Is(r.err, errCredentialsChanged) {
		t.Fatalf("UpdateProfile err = %v, want errCredentialsChanged", r.err)
	}
	if a, err := s.Admin(); err != nil || a.Username != "restored-admin" {
		t.Fatalf("admin row after restore = %+v, %v", a, err)
	}
}

// TestUpdateProfileDoesNotHoldCredentialLockDuringHash is the same boundary check for the new
// password hash, which is the other expensive KDF in the profile path.
func TestUpdateProfileDoesNotHoldCredentialLockDuringHash(t *testing.T) {
	s, _ := newService(t)
	_, release, done := startHeldUpdateProfile(t, s, false)

	revoked := make(chan error, 1)
	go func() { revoked <- s.RevokeAllSessions() }()
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatalf("RevokeAllSessions: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("RevokeAllSessions waited for the new-password KDF; credMu is held across it")
	}
	close(release)
	r := <-done
	if !errors.Is(r.err, errCredentialsChanged) {
		t.Fatalf("UpdateProfile err = %v, want errCredentialsChanged", r.err)
	}
	if _, err := s.Login(DefaultUsername, DefaultPassword, "198.51.100.10", "test"); err != nil {
		t.Fatalf("the aborted profile change still replaced the password: %v", err)
	}
}

// TestPurgeExpiredDropsCachedSessionWithoutRow covers the divergence between the sessions table
// and the cache: the row is gone, so the cached copy must stop validating, not outlive the purge.
func TestPurgeExpiredDropsCachedSessionWithoutRow(t *testing.T) {
	s, d := newService(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	insertSession(t, d, "tok", now.Add(-time.Minute))
	h := secret.HashToken("tok")
	s.mu.Lock()
	s.sessions[h] = now.Add(time.Hour) // a cache copy ahead of the row the purge is about to delete
	s.lastSeen[h] = now
	s.mu.Unlock()

	s.PurgeExpired()
	if s.Validate("tok") {
		t.Fatal("a session whose row the purge deleted still validates from the cache")
	}
}

// TestValidateRejectsSessionWhoseSlideWasNotPersisted makes the expiry UPDATE affect zero rows and
// requires the slide to fail closed instead of extending the cache to an expiry the table never got.
func TestValidateRejectsSessionWhoseSlideWasNotPersisted(t *testing.T) {
	s, d := newService(t)
	insertSession(t, d, "tok", time.Now().Add(time.Hour))
	base := time.Now()
	s.now = func() time.Time { return base }
	if !s.Validate("tok") {
		t.Fatal("seeded session rejected")
	}
	if _, err := d.Exec(`CREATE TRIGGER freeze_session BEFORE UPDATE ON sessions BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(11 * time.Minute) }
	if s.Validate("tok") {
		t.Fatal("a session whose expiry slide wrote zero rows still validates")
	}
	h := secret.HashToken("tok")
	s.mu.Lock()
	_, cached := s.sessions[h]
	_, seen := s.lastSeen[h]
	s.mu.Unlock()
	if cached || seen {
		t.Fatal("the zero-row slide left the session in the cache")
	}
}

// TestValidateDropsCacheWhenSlideWriteFails forces the expiry UPDATE to return an error and
// requires the same fail-closed policy as the zero-row path. Removing the trigger must let the
// next request retry from the session row.
func TestValidateDropsCacheWhenSlideWriteFails(t *testing.T) {
	for _, action := range []string{"ABORT", "FAIL"} {
		t.Run(action, func(t *testing.T) {
			s, d := newService(t)
			insertSession(t, d, "tok", time.Now().Add(time.Hour))
			base := time.Now()
			s.now = func() time.Time { return base }
			if !s.Validate("tok") {
				t.Fatal("seeded session rejected")
			}
			s.now = func() time.Time { return base.Add(11 * time.Minute) }
			trigger := `CREATE TRIGGER fail_slide BEFORE UPDATE ON sessions BEGIN SELECT RAISE(` + action + `, 'slide failed'); END`
			if _, err := d.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if s.Validate("tok") {
				t.Fatalf("a session whose slide write failed still validates")
			}
			h := secret.HashToken("tok")
			s.mu.Lock()
			_, cached := s.sessions[h]
			_, seen := s.lastSeen[h]
			s.mu.Unlock()
			if cached || seen {
				t.Fatal("a failed slide write left the session in the cache")
			}
			if _, err := d.Exec(`DROP TRIGGER fail_slide`); err != nil {
				t.Fatal(err)
			}
			if !s.Validate("tok") {
				t.Fatal("the session row was not retried after the write failure cleared")
			}
		})
	}
}

// TestVerifyPasswordRejectsArgon2HashWithEmptyKey covers the zero-length derived key: the KDF
// panics on it, which would take the process down instead of rejecting the hash.
func TestVerifyPasswordRejectsArgon2HashWithEmptyKey(t *testing.T) {
	hash := "$argon2id$v=19$m=8192,t=1,p=1$" + base64.RawStdEncoding.EncodeToString([]byte("12345678")) + "$"
	if VerifyPassword(hash, "anything") {
		t.Fatal("an argon2id hash with an empty key accepted a password")
	}
}

// TestVerifyPasswordRejectsZeroArgon2Cost covers the parameters argon2.IDKey panics on, which
// would take the process down instead of rejecting the hash.
func TestVerifyPasswordRejectsZeroArgon2Cost(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString([]byte("12345678"))
	sum := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	for _, hash := range []string{
		"$argon2id$v=19$m=8192,t=0,p=1$" + salt + "$" + sum,
		"$argon2id$v=19$m=8192,t=1,p=0$" + salt + "$" + sum,
	} {
		if VerifyPassword(hash, "x") {
			t.Errorf("malformed hash %q was accepted", hash)
		}
	}
}

// TestVerifyPasswordEnforcesArgon2Policy requires the login path to reject a hash outside the
// parameters this build is willing to run, even when the hash itself is otherwise valid.
func TestVerifyPasswordEnforcesArgon2Policy(t *testing.T) {
	salt := []byte("12345678")
	cases := []struct{ name, hash string }{
		{"time over the login bound", argon2Hash("pw", salt, 11, 8192, 2, 16)},
		{"threads over the login bound", argon2Hash("pw", salt, 3, 8192, 17, 16)},
		{"salt below the minimum", argon2Hash("pw", []byte("abcd"), 3, 8192, 2, 16)},
		{"unsupported version", strings.Replace(argon2Hash("pw", salt, 3, 8192, 2, 16), "v=19", "v=16", 1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if VerifyPassword(c.hash, "pw") {
				t.Fatalf("hash with %s was accepted: %s", c.name, c.hash)
			}
		})
	}
}
