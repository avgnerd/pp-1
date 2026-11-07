// Package auth manages the single admin account, cookie sessions, login throttling and CSRF checks.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
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

// Argon2id parameters (64 MiB, 3 passes, 2 lanes).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

var (
	ErrInvalidCredentials = errors.New("이름 또는 비밀번호가 올바르지 않습니다")
	ErrLocked             = errors.New("로그인 실패가 너무 많습니다. 15분 후 다시 시도하세요")
)

// HashPassword returns an encoded argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h)), nil
}

// VerifyPassword checks pw against an encoded hash in constant time. A bcrypt hash is accepted so
// an instance restored from a backup written by an older build can still log in; the next password
// change rehashes it with argon2id.
func VerifyPassword(encoded, pw string) bool {
	if _, err := bcrypt.Cost([]byte(encoded)); err == nil {
		return bcrypt.CompareHashAndPassword([]byte(encoded), []byte(pw)) == nil
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Admin is the single administrator.
type Admin struct {
	Username  string `json:"username"`
	IsDefault bool   `json:"is_default_password"`
}

// Service handles accounts and sessions.
type Service struct {
	db  *sql.DB
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]time.Time // token hash -> expiry (cache of valid sessions)
	lastSeen map[string]time.Time

	limiter *Limiter
}

// New ensures the admin row exists (admin/1234 on first run).
func New(db *sql.DB) (*Service, error) {
	s := &Service{db: db, now: time.Now, sessions: map[string]time.Time{}, lastSeen: map[string]time.Time{}, limiter: NewLimiter(5, 15*time.Minute)}
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
	var name, hash string
	if err := s.db.QueryRow(`SELECT username, password_hash FROM admin WHERE id = 1`).Scan(&name, &hash); err != nil {
		return "", err
	}
	okName := subtle.ConstantTimeCompare([]byte(name), []byte(username)) == 1
	okPass := VerifyPassword(hash, password) // always run to keep timing uniform
	if !okName || !okPass {
		if s.limiter.Fail(ip) {
			return "", ErrLocked
		}
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

// touch slides a session's expiry at most once every 10 minutes. A session revoked while it was
// being validated is not put back.
func (s *Service) touch(h string, now time.Time) bool {
	s.mu.Lock()
	if _, live := s.sessions[h]; !live {
		s.mu.Unlock()
		return false
	}
	if now.Sub(s.lastSeen[h]) <= 10*time.Minute {
		s.mu.Unlock()
		return true
	}
	s.lastSeen[h] = now
	s.sessions[h] = now.Add(SessionTTL)
	s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE token_hash = ?`, now.Unix(), now.Add(SessionTTL).Unix(), h)
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

// UpdateProfile changes the username and/or password after verifying the current password.
// When the password changes, every other session is revoked. Returns a fresh token for the caller.
func (s *Service) UpdateProfile(currentToken, currentPassword, newUsername, newPassword, ip, ua string) (string, error) {
	var hash string
	if err := s.db.QueryRow(`SELECT password_hash FROM admin WHERE id = 1`).Scan(&hash); err != nil {
		return "", err
	}
	if !VerifyPassword(hash, currentPassword) {
		return "", errors.New("현재 비밀번호가 올바르지 않습니다")
	}
	newUsername = strings.TrimSpace(newUsername)
	if newUsername == "" || len(newUsername) > 64 {
		return "", errors.New("이름은 1~64자여야 합니다")
	}
	now := s.now().Unix()
	if newPassword == "" {
		_, err := s.db.Exec(`UPDATE admin SET username = ?, updated_at = ? WHERE id = 1`, newUsername, now)
		return currentToken, err
	}
	if len(newPassword) < 8 {
		return "", errors.New("새 비밀번호는 8자 이상이어야 합니다")
	}
	nh, err := HashPassword(newPassword)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`UPDATE admin SET username = ?, password_hash = ?, is_default = 0, updated_at = ? WHERE id = 1`, newUsername, nh, now); err != nil {
		return "", err
	}
	s.mu.Lock()
	err = s.revokeAll()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	return s.createSession(ip, ua)
}

// RevokeAllSessions deletes every login session, in memory and in the sessions table, so a
// request that still carries an old cookie is rejected as unauthenticated.
func (s *Service) RevokeAllSessions() error {
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

// PurgeExpired deletes expired sessions and stale limiter entries.
func (s *Service) PurgeExpired() {
	now := s.now()
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	s.mu.Lock()
	for h, e := range s.sessions {
		if now.After(e) {
			delete(s.sessions, h)
			delete(s.lastSeen, h)
		}
	}
	s.mu.Unlock()
	s.limiter.Purge()
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
