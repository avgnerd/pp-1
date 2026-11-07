// Package auth manages the single admin account, cookie sessions, login throttling and CSRF checks.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"

	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
)

const (
	CookieName      = "dvfs_session"
	SessionTTL      = 30 * 24 * time.Hour
	DefaultUsername = "admin"
	DefaultPassword = "1234"
	CSRFHeader      = "X-Requested-With"
	CSRFValue       = "dvfs"
)

// Argon2id parameters for newly written hashes (64 MiB, 3 passes, 2 lanes). The existing low
// memory value stays: changing it would force a rehash and alter login cost for no security gain.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// Bounds accepted when verifying a stored hash. The admin row can come from a restored backup, so
// it is treated as untrusted input: without bounds one login can allocate gigabytes, burn CPU, or
// panic inside the argon2 KDF. The memory ceiling stays at the historical 256 MiB so a backup
// written by an earlier build keeps working; lowering it would reject hashes this product itself
// accepted. The argon2 memory minimum stays at the legacy validator's 8 KiB because low memory is
// not a resource risk. Bcrypt is capped at cost 14 for both login and restore validation; a cost
// 15..31 hash is rejected before any KDF runs.
const (
	maxPasswordHashLen = 512
	minArgonMemoryKiB  = 8
	maxArgonMemoryKiB  = 256 * 1024
	minArgonTime       = 1
	maxArgonTime       = 10
	minArgonThreads    = 1
	maxArgonThreads    = 16
	minArgonSaltLen    = 8
	minArgonKeyLen     = 16
	maxBcryptCost      = 14
)

// kdfConcurrency is one, not a number tuned to a heap measurement. With the 256 MiB ceiling, one
// process-wide slot makes the KDF memory bound 256 MiB by construction, however many logins
// arrive; a larger slot count would multiply that ceiling. Bcrypt cost 14 also takes the slot, so
// a burst cannot stack CPU-bound hashes around Argon2 either. This is deliberately conservative:
// a caller beyond the budget waits for a slot, and a hash rejected by the policy bounds above
// never takes one because validation finishes before the KDF starts.
const kdfConcurrency = 1

var kdfSlots = make(chan struct{}, kdfConcurrency)

// runKDF runs one password KDF inside the admission limit. The deferred release returns the slot
// on every exit path, including a panic from the KDF.
func runKDF(fn func()) {
	kdfSlots <- struct{}{}
	defer func() { <-kdfSlots }()
	fn()
}

var (
	ErrInvalidCredentials = errors.New("이름 또는 비밀번호가 올바르지 않습니다")
	ErrLocked             = errors.New("로그인 실패가 너무 많습니다. 15분 후 다시 시도하세요")
	errCredentialsChanged = errors.New("자격 증명이 다른 요청에서 변경되었습니다. 다시 시도하세요")
)

// HashPassword returns an encoded argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	var h []byte
	runKDF(func() { h = argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen) })
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h)), nil
}

// VerifyPassword checks pw against an encoded hash in constant time. A bcrypt hash is accepted so
// an instance restored from a backup written by an older build can still log in; the next password
// change rehashes it with argon2id. Bcrypt cost 15..31 and argon2 hashes outside the policy bounds
// are rejected before any KDF runs, so a hostile or damaged row cannot exhaust memory, hang a
// request, or panic the process.
func VerifyPassword(encoded, pw string) bool {
	if len(encoded) > maxPasswordHashLen {
		return false
	}
	if cost, err := bcrypt.Cost([]byte(encoded)); err == nil {
		if cost > maxBcryptCost {
			return false
		}
		ok := false
		runKDF(func() { ok = bcrypt.CompareHashAndPassword([]byte(encoded), []byte(pw)) == nil })
		return ok
	}
	p, ok := parseArgon2PHC(encoded)
	if !ok {
		return false
	}
	ok = false
	runKDF(func() {
		got := argon2.IDKey([]byte(pw), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
		ok = subtle.ConstantTimeCompare(got, p.key) == 1
	})
	return ok
}

// ValidatePasswordHash reports whether encoded is a hash this build can verify. Backup restore
// calls it before writing the admin row. Bcrypt cost 15..31 is rejected here too, so restore
// validation and login accept exactly the same range.
func ValidatePasswordHash(encoded string) error {
	if len(encoded) > maxPasswordHashLen {
		return errors.New("password hash is too long")
	}
	if cost, err := bcrypt.Cost([]byte(encoded)); err == nil {
		if cost > maxBcryptCost {
			return fmt.Errorf("bcrypt cost %d exceeds the supported maximum %d", cost, maxBcryptCost)
		}
		return nil
	}
	if _, ok := parseArgon2PHC(encoded); !ok {
		return errors.New("unsupported password hash format or parameters")
	}
	return nil
}

// argon2PHC is one parsed and policy-checked argon2id PHC string.
type argon2PHC struct {
	time    uint32
	memory  uint32
	threads uint8
	salt    []byte
	key     []byte
}

// parseArgon2PHC parses the exact PHC shape HashPassword writes and rejects every parameter the
// login and restore paths are not willing to run.
func parseArgon2PHC(encoded string) (argon2PHC, bool) {
	var p argon2PHC
	if len(encoded) > maxPasswordHashLen {
		return p, false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return p, false
	}
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return p, false
	}
	m, ok := decimalField(fields[0], "m=")
	if !ok {
		return p, false
	}
	t, ok := decimalField(fields[1], "t=")
	if !ok {
		return p, false
	}
	threads, ok := decimalField(fields[2], "p=")
	if !ok || threads > 255 {
		return p, false
	}
	if m < minArgonMemoryKiB || m > maxArgonMemoryKiB ||
		t < minArgonTime || t > maxArgonTime ||
		threads < minArgonThreads || threads > maxArgonThreads {
		return p, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < minArgonSaltLen {
		return p, false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < minArgonKeyLen {
		return p, false
	}
	p.time, p.memory, p.threads, p.salt, p.key = t, m, uint8(threads), salt, key
	return p, true
}

// decimalField parses the unsigned integer after prefix, rejecting signs, spaces and overflow.
func decimalField(s, prefix string) (uint32, bool) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// Admin is the single administrator.
type Admin struct {
	Username  string `json:"username"`
	IsDefault bool   `json:"is_default_password"`
}

// credentialSnapshot is the admin row a KDF was checked against, plus the credential revision
// observed with it. A caller verifies outside credMu and then proves this snapshot is still the
// current row while holding credMu.
type credentialSnapshot struct {
	username string
	hash     string
	rev      uint64
}

// snapshotCredentials captures the revision before reading the admin row, so a credential change
// that lands between the two makes the later recheck fail.
func (s *Service) snapshotCredentials() (credentialSnapshot, error) {
	s.credMu.Lock()
	rev := s.credRev
	s.credMu.Unlock()
	c := credentialSnapshot{rev: rev}
	err := s.db.QueryRow(`SELECT username, password_hash FROM admin WHERE id = 1`).Scan(&c.username, &c.hash)
	return c, err
}

// credentialsUnchanged reports whether c is still the current admin row and no credential
// revision has been issued since it was read. The caller must hold credMu.
func (s *Service) credentialsUnchanged(c credentialSnapshot) bool {
	if s.credRev != c.rev {
		return false
	}
	var username, hash string
	if err := s.db.QueryRow(`SELECT username, password_hash FROM admin WHERE id = 1`).Scan(&username, &hash); err != nil {
		return false
	}
	return username == c.username && hash == c.hash
}

// Service handles accounts and sessions.
type Service struct {
	db  *sql.DB
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]time.Time // token hash -> expiry (cache of valid sessions)
	lastSeen map[string]time.Time

	// credMu serializes the credential snapshot recheck and its session write with every
	// credential change. Login does not hold it during the expensive password check; it captures
	// credRev first and rechecks it under credMu, so a password change that lands mid-verify
	// either rejects the login or revokes the session it just created.
	credMu  sync.Mutex
	credRev uint64

	// verifyPassword and hashPassword are the KDF boundaries. Tests replace them to hold a KDF
	// open and prove that no credential lock is held while it runs; production uses the package
	// functions.
	verifyPassword func(encoded, pw string) bool
	hashPassword   func(pw string) (string, error)

	limiter *Limiter
}

// New ensures the admin row exists (admin/1234 on first run).
func New(db *sql.DB) (*Service, error) {
	s := &Service{db: db, now: time.Now, sessions: map[string]time.Time{}, lastSeen: map[string]time.Time{},
		verifyPassword: VerifyPassword, hashPassword: HashPassword, limiter: NewLimiter(5, 15*time.Minute)}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		h, err := HashPassword(DefaultPassword)
		if err != nil {
			return nil, err
		}
		if _, err := db.Exec(`INSERT INTO admin(id, username, password_hash, is_default, updated_at) VALUES(1, ?, ?, 1, ?)`,
			DefaultUsername, h, s.now().Unix()); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Admin returns the admin profile.
func (s *Service) Admin() (Admin, error) {
	var a Admin
	var def int
	err := s.db.QueryRow(`SELECT username, is_default FROM admin WHERE id = 1`).Scan(&a.Username, &def)
	a.IsDefault = def == 1
	return a, err
}

// Login checks credentials and returns a new session token.
func (s *Service) Login(username, password, ip, ua string) (string, error) {
	if s.limiter.Locked(ip) {
		return "", ErrLocked
	}
	snap, err := s.snapshotCredentials()
	if err != nil {
		return "", err
	}
	okName := subtle.ConstantTimeCompare([]byte(snap.username), []byte(username)) == 1
	okPass := s.verifyPassword(snap.hash, password) // always run to keep timing uniform
	if !okName || !okPass {
		if s.limiter.Fail(ip) {
			return "", ErrLocked
		}
		return "", ErrInvalidCredentials
	}
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if !s.credentialsUnchanged(snap) {
		// The credential this login verified was replaced while the KDF ran. Its password may
		// already be revoked, so do not mint a session that the revoke could not see.
		return "", ErrInvalidCredentials
	}
	s.limiter.Reset(ip)
	return s.createSession(ip, ua)
}

// createSession writes the session row and its cache entry under one lock, so a revoke that lands
// between the two cannot leave a row-less session in the cache.
func (s *Service) createSession(ip, ua string) (string, error) {
	tok, err := secret.RandomToken(32)
	if err != nil {
		return "", err
	}
	now := s.now()
	exp := now.Add(SessionTTL)
	if len(ua) > 256 {
		ua = ua[:256]
	}
	h := secret.HashToken(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`INSERT INTO sessions(token_hash, created_at, expires_at, last_seen, ip, user_agent) VALUES(?,?,?,?,?,?)`,
		h, now.Unix(), exp.Unix(), now.Unix(), ip, ua); err != nil {
		return "", err
	}
	s.sessions[h] = exp
	return tok, nil
}

// Validate reports whether the token belongs to a live session and slides its expiry.
func (s *Service) Validate(tok string) bool {
	if tok == "" {
		return false
	}
	h := secret.HashToken(tok)
	now := s.now()
	exp, ok := s.session(h)
	if !ok {
		return false
	}
	if now.After(exp) {
		s.revokeHash(h)
		return false
	}
	return s.touch(h, now)
}

// session returns a session's expiry, reading it from the database on a cache miss. The read and
// the cache write hold s.mu together with every revocation, so a revoke that lands during the read
// is not undone by the row that read saw first.
func (s *Service) session(h string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if exp, ok := s.sessions[h]; ok {
		return exp, true
	}
	var e int64
	if err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash = ?`, h).Scan(&e); err != nil {
		return time.Time{}, false
	}
	exp := time.Unix(e, 0)
	s.sessions[h] = exp
	return exp, true
}

// touch slides a session's expiry at most once every 10 minutes. The database write and the cache
// update happen under s.mu, and the row is authoritative: when the UPDATE fails or reaches no row,
// the session is dropped from the cache and the request fails closed. A transient write error is
// retried from the row on the next request, not continued from the stale cache.
func (s *Service) touch(h string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, live := s.sessions[h]; !live {
		return false
	}
	if now.Sub(s.lastSeen[h]) <= 10*time.Minute {
		return true
	}
	exp := now.Add(SessionTTL)
	res, err := s.db.Exec(`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE token_hash = ?`, now.Unix(), exp.Unix(), h)
	if err != nil {
		delete(s.sessions, h)
		delete(s.lastSeen, h)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		delete(s.sessions, h)
		delete(s.lastSeen, h)
		return false
	}
	s.lastSeen[h] = now
	s.sessions[h] = exp
	return true
}

// Logout revokes one session.
func (s *Service) Logout(tok string) {
	if tok != "" {
		s.revokeHash(secret.HashToken(tok))
	}
}

func (s *Service) revokeHash(h string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, h)
	delete(s.lastSeen, h)
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, h)
}

// UpdateProfile changes the username and/or password after verifying the current password. The
// current-password check and the new hash run outside credMu; a short final section rechecks the
// credential snapshot, revokes the old sessions and writes the new credential. When the password
// changes, every other session is revoked. Returns a fresh token for the caller.
func (s *Service) UpdateProfile(currentToken, currentPassword, newUsername, newPassword, ip, ua string) (string, error) {
	newUsername = strings.TrimSpace(newUsername)
	if newUsername == "" || len(newUsername) > 64 {
		return "", errors.New("이름은 1~64자여야 합니다")
	}
	if newPassword != "" && len(newPassword) < 8 {
		return "", errors.New("새 비밀번호는 8자 이상이어야 합니다")
	}
	snap, err := s.snapshotCredentials()
	if err != nil {
		return "", err
	}
	if !s.verifyPassword(snap.hash, currentPassword) {
		return "", errors.New("현재 비밀번호가 올바르지 않습니다")
	}
	var newHash string
	if newPassword != "" {
		newHash, err = s.hashPassword(newPassword)
		if err != nil {
			return "", err
		}
	}

	s.credMu.Lock()
	defer s.credMu.Unlock()
	if !s.credentialsUnchanged(snap) {
		return "", errCredentialsChanged
	}
	now := s.now().Unix()
	if newPassword == "" {
		if _, err := s.db.Exec(`UPDATE admin SET username = ?, updated_at = ? WHERE id = 1`, newUsername, now); err != nil {
			return "", err
		}
		s.credRev++
		return currentToken, nil
	}
	// Revoke before writing the new credential. If the revocation fails, the old password is still
	// the credential; the opposite order could leave a new password next to old live sessions.
	s.mu.Lock()
	err = s.revokeAll()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`UPDATE admin SET username = ?, password_hash = ?, is_default = 0, updated_at = ? WHERE id = 1`, newUsername, newHash, now); err != nil {
		return "", err
	}
	s.credRev++
	return s.createSession(ip, ua)
}

// RevokeAllSessions deletes every login session, in memory and in the sessions table, so a
// request that still carries an old cookie is rejected as unauthenticated. It also bumps the
// credential revision, so a login that read the credential before the revoke cannot write a new
// session after it.
func (s *Service) RevokeAllSessions() error {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	s.credRev++
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeAll()
}

// BeginRestore takes the credential lock for a restore. A KDF already running will fail its
// snapshot recheck; new logins and profile changes wait until EndRestore. The caller must pair it
// with EndRestore on every path.
func (s *Service) BeginRestore() {
	s.credMu.Lock()
	s.credRev++
}

// EndRestore releases the credential lock taken by BeginRestore.
func (s *Service) EndRestore() { s.credMu.Unlock() }

// RevokeAllSessionsDuringRestore clears every session while the restore holds the credential lock
// from BeginRestore. The caller must hold that lock; use RevokeAllSessions elsewhere.
func (s *Service) RevokeAllSessionsDuringRestore() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeAll()
}

// revokeAll deletes every session row and empties the cache. The caller holds s.mu, so a session
// created or read by a Validate that started before the delete cannot outlive it in the cache.
func (s *Service) revokeAll() error {
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	s.sessions = map[string]time.Time{}
	s.lastSeen = map[string]time.Time{}
	return nil
}

// PurgeExpired deletes expired sessions and stale limiter entries. The row delete and the cache
// eviction happen under one lock, keyed by the hashes the database actually deleted, so a row that
// disappears cannot keep validating from a cache copy that still looks live.
func (s *Service) PurgeExpired() {
	now := s.now()
	s.mu.Lock()
	s.purgeExpiredSessions(now)
	for h, e := range s.sessions {
		if now.After(e) {
			delete(s.sessions, h)
			delete(s.lastSeen, h)
		}
	}
	s.mu.Unlock()
	s.limiter.Purge()
}

// purgeExpiredSessions removes expired rows and the cache entries for exactly those rows. The
// caller holds s.mu, so a session writer cannot interleave between the read and the delete.
func (s *Service) purgeExpiredSessions(now time.Time) {
	rows, err := s.db.Query(`SELECT token_hash FROM sessions WHERE expires_at < ?`, now.Unix())
	if err != nil {
		return
	}
	var expired []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err == nil {
			expired = append(expired, h)
		}
	}
	rows.Close()
	if len(expired) == 0 {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix()); err != nil {
		return
	}
	for _, h := range expired {
		delete(s.sessions, h)
		delete(s.lastSeen, h)
	}
}

// Limiter locks an IP after max failures within window.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*attempts
	now    func() time.Time
}

type attempts struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

// NewLimiter creates a limiter.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, m: map[string]*attempts{}, now: time.Now}
}

// Locked reports whether ip is locked.
func (l *Limiter) Locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[ip]
	return a != nil && l.now().Before(a.lockedUntil)
}

// Fail records a failure and reports whether the ip is now locked.
func (l *Limiter) Fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.m[ip]
	if a == nil || now.Sub(a.first) > l.window {
		a = &attempts{first: now}
		l.m[ip] = a
	}
	a.count++
	if a.count >= l.max {
		a.lockedUntil = now.Add(l.window)
		return true
	}
	return false
}

// Reset clears an ip.
func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}

// Purge drops stale entries so the map cannot grow without bound.
func (l *Limiter) Purge() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for ip, a := range l.m {
		if now.Sub(a.first) > l.window && now.After(a.lockedUntil) {
			delete(l.m, ip)
		}
	}
}
