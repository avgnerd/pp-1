// Package storage manages Dropbox storages (one per account), their mounts, OAuth and API clients.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
)

// Status values.
const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusError   = "error"
)

// Auth modes.
const (
	AuthLocalhost = "localhost"
	AuthPublic    = "public"
	AuthCode      = "code"
)

// StateTTL is how long an OAuth state stays valid.
const StateTTL = 10 * time.Minute

// Storage is one connected Dropbox account.
type Storage struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	MountPath    string `json:"mount_path"`
	RootPath     string `json:"root_path"`
	AppKey       string `json:"app_key"`
	AccountID    string `json:"account_id"`
	AccountEmail string `json:"account_email"`
	AccountName  string `json:"account_name"`
	CacheTTLMin  int    `json:"cache_ttl_min"`
	Status       string `json:"status"`
	AuthMode     string `json:"auth_mode"`
	RedirectURI  string `json:"redirect_uri"`
	LastError    string `json:"last_error"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
	AuthorizedAt int64  `json:"authorized_at"`
}

// CacheTTL returns the storage cache TTL.
func (s *Storage) CacheTTL() time.Duration {
	if s.CacheTTLMin <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(s.CacheTTLMin) * time.Minute
}

// NormalizeMountPath validates and normalizes a mount path. It is the same rule Create and
// Update apply, exported so a restore can validate a backup payload before touching the DB.
func NormalizeMountPath(p string) (string, error) {
	return pathutil.NormalizeMount(p)
}

// Input is the admin form for creating or editing a storage.
type Input struct {
	Name        string `json:"name"`
	MountPath   string `json:"mount_path"`
	RootPath    string `json:"root_path"`
	AppKey      string `json:"app_key"`
	AppSecret   string `json:"app_secret"`
	CacheTTLMin int    `json:"cache_ttl_min"`
	AuthMode    string `json:"auth_mode"`
}

// ValidationError is shown to the admin next to the form.
type ValidationError struct {
	Field string `json:"field"`
	Msg   string `json:"message"`
}

func (e *ValidationError) Error() string { return e.Msg }

// Manager holds storages in memory and persists them.
type Manager struct {
	db  *sql.DB
	box *secret.Box
	cfg *config.Config
	ep  dropbox.Endpoints

	mu       sync.RWMutex
	list     []*Storage // sorted by mount path
	byID     map[int64]*Storage
	clients  map[int64]*dropbox.Client
	onChange []func(id int64)
	now      func() time.Time

	// reloadMu serializes the database read and the publication that follows it. A snapshot may
	// only overwrite the in-memory list in the same order its query ran, so a reload that started
	// earlier can never publish older rows after a newer reload has published.
	reloadMu sync.Mutex

	// mountMu serializes the mount check with the row write that follows it. The UNIQUE index on
	// mount_path compares bytes, so it cannot reject a case-only duplicate or a nested path; only
	// holding this across checkMount and the write keeps two concurrent saves from both passing.
	mountMu sync.Mutex

	// mutationMu keeps a restore apply from interleaving with a storage mutation. A restore holds
	// the write side for the transaction and the runtime refresh; each mutating method takes the
	// read side, and fails fast when a restore owns the gate.
	mutationMu sync.RWMutex

	// stateMu is the ownership boundary for one storage row's credential/status state. complete
	// takes it around the token write and the cache invalidation that belongs to it; a client
	// build takes it around the row read and cache publication; and a reauth callback rechecks its
	// client identity and generation under it before writing status. This makes "check, then
	// write" one ownership step and keeps a client built from the old token from being published
	// after the completion. Lock order: mutationMu -> stateMu -> reloadMu -> mu. No path takes
	// stateMu while holding mu, reloadMu or mountMu, and no database query runs while mu is held.
	stateMu sync.Mutex

	// clientGeneration advances whenever a restore replaces the storage rows and invalidates every
	// cached client. A client captures the generation it was built under; its reauth callback is
	// ignored once the generation moves, so a failure from a pre-restore client cannot overwrite
	// the restored row. Guarded by mu.
	clientGeneration uint64

	// clientBuiltHook, when set, runs after a client has been built but before it is published to
	// the cache. Tests use it to hold the publication across a restore or a completion; production
	// leaves it nil.
	clientBuiltHook func()
}

// NewManager loads all storages.
func NewManager(db *sql.DB, box *secret.Box, cfg *config.Config) (*Manager, error) {
	m := &Manager{db: db, box: box, cfg: cfg, now: time.Now, clients: map[int64]*dropbox.Client{},
		ep: dropbox.Endpoints{API: cfg.DropboxAPIBase, Content: cfg.DropboxContentBase, Authorize: cfg.DropboxAuthorize, Token: cfg.DropboxTokenURL}}
	return m, m.reload()
}

// Endpoints returns the Dropbox endpoints in use.
func (m *Manager) Endpoints() dropbox.Endpoints { return m.ep }

// OnChange registers a callback for storage changes (cache invalidation).
func (m *Manager) OnChange(fn func(id int64)) { m.onChange = append(m.onChange, fn) }

func (m *Manager) changed(id int64) {
	for _, fn := range m.onChange {
		fn(id)
	}
}

const cols = `id, name, mount_path, root_path, app_key, account_id, account_email, account_name, cache_ttl_min, status, auth_mode, redirect_uri, last_error, created_at, updated_at, authorized_at`

func scan(r interface{ Scan(...any) error }) (*Storage, error) {
	s := &Storage{}
	err := r.Scan(&s.ID, &s.Name, &s.MountPath, &s.RootPath, &s.AppKey, &s.AccountID, &s.AccountEmail, &s.AccountName,
		&s.CacheTTLMin, &s.Status, &s.AuthMode, &s.RedirectURI, &s.LastError, &s.CreatedAt, &s.UpdatedAt, &s.AuthorizedAt)
	return s, err
}

func (m *Manager) reload() error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	rows, err := m.db.Query(`SELECT ` + cols + ` FROM storages`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var list []*Storage
	byID := map[int64]*Storage{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return err
		}
		list = append(list, s)
		byID[s.ID] = s
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].MountPath) < strings.ToLower(list[j].MountPath) })
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = list
	m.byID = byID
	for id := range m.clients {
		if _, ok := byID[id]; !ok {
			delete(m.clients, id)
		}
	}
	return nil
}

// All returns a snapshot of all storages sorted by mount path.
func (m *Manager) All() []Storage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Storage, len(m.list))
	for i, s := range m.list {
		out[i] = *s
	}
	return out
}

// ResetAndInitClients drops every cached Dropbox client, reloads the storages from SQLite and
// builds a fresh client for each storage that has finished authorization. A storage whose
// client cannot be built is marked "error"; its name is returned in failedStorages and the
// restore that called this carries on with the remaining storages.
func (m *Manager) ResetAndInitClients() ([]string, error) {
	// Advance the generation before the restored rows are published to the in-memory snapshot, so
	// every client built from the replaced state is stale even if an in-flight request still holds
	// it.
	m.InvalidateClients()
	if err := m.reload(); err != nil {
		return nil, err
	}
	var failed []string
	for _, s := range m.All() {
		if s.Status == StatusPending {
			continue
		}
		if _, err := m.clientOwned(s.ID); err != nil {
			m.markErrorLocked(s.ID, err.Error(), m.now().Unix())
			failed = append(failed, s.Name)
		}
	}
	return failed, nil
}

// InvalidateClients drops every cached client and advances the client generation. A restore calls
// it once the restored rows are committed and again from ResetAndInitClients while rebuilding the
// runtime; the caller must already own the restore gate. A callback from a client built before
// this point is ignored, even if it arrives after the gate is released.
func (m *Manager) InvalidateClients() {
	m.mu.Lock()
	m.clients = map[int64]*dropbox.Client{}
	m.clientGeneration++
	m.mu.Unlock()
}

// Get returns a storage by id.
func (m *Manager) Get(id int64) (Storage, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byID[id]
	if !ok {
		return Storage{}, false
	}
	return *s, true
}

// ByMount finds the storage whose mount contains the virtual path.
func (m *Manager) ByMount(vpath string) (Storage, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.list {
		if pathutil.HasPrefix(vpath, s.MountPath) {
			return *s, true
		}
	}
	return Storage{}, false
}

// Client returns an API client for an active storage. It holds the mutation gate across the cache
// lookup, the row read and the cache publication, so a restore cannot replace the row between
// them. It also holds the state boundary, so a reauth completion cannot replace the token between
// the build and its publication. A restore in progress fails fast instead of returning a client
// built from the old row.
func (m *Manager) Client(id int64) (*dropbox.Client, error) {
	release, err := m.beginMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	c, err := m.buildClient(id)
	if err != nil {
		return nil, err
	}
	if m.clientBuiltHook != nil {
		m.clientBuiltHook()
	}
	return m.cacheClient(id, c), nil
}

// clientOwned builds and caches a client for a caller that already holds the mutation gate. The
// restore refresh and Delete's revoke use it; taking the gate again from Client would fail or wait
// behind a restore that cannot start until the caller finishes.
func (m *Manager) clientOwned(id int64) (*dropbox.Client, error) {
	c, err := m.buildClient(id)
	if err != nil {
		return nil, err
	}
	return m.cacheClient(id, c), nil
}

// buildClient returns a cached client or builds one without publishing it. The caller must hold
// the mutation gate, so a restore cannot replace the row between the snapshot read and the
// callback assignment. A caller that can race a reauth completion, such as Client, must also hold
// stateMu across the build and the publication; the restore refresh is serialized by the write
// side of the mutation gate instead.
func (m *Manager) buildClient(id int64) (*dropbox.Client, error) {
	m.mu.RLock()
	c, ok := m.clients[id]
	s := m.byID[id]
	generation := m.clientGeneration
	m.mu.RUnlock()
	if ok {
		return c, nil
	}
	if s == nil {
		return nil, errors.New("저장소를 찾을 수 없습니다")
	}
	if s.Status == StatusPending {
		return nil, errors.New("이 저장소는 아직 Dropbox 인증을 마치지 않았습니다")
	}
	var appSecretEnc, rtEnc []byte
	if err := m.db.QueryRow(`SELECT app_secret_enc, refresh_token_enc FROM storages WHERE id = ?`, id).Scan(&appSecretEnc, &rtEnc); err != nil {
		return nil, err
	}
	if len(rtEnc) == 0 {
		return nil, errors.New("이 저장소에는 Dropbox 토큰이 없습니다. 다시 인증하세요")
	}
	appSecret, err := m.box.Open(appSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("app secret 복호화 실패(비밀키가 바뀌었을 수 있습니다): %w", err)
	}
	rt, err := m.box.Open(rtEnc)
	if err != nil {
		return nil, fmt.Errorf("refresh token 복호화 실패: %w", err)
	}
	c = dropbox.New(m.ep, s.AppKey, string(appSecret), string(rt))
	c.OnReauthNeeded = func() { m.markErrorIfCurrent(id, generation, c, dropbox.ErrReauth.Error()) }
	return c, nil
}

// cacheClient publishes a built client and returns the one that won a concurrent insert.
func (m *Manager) cacheClient(id int64, c *dropbox.Client) *dropbox.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.clients[id]; ok {
		return existing
	}
	m.clients[id] = c
	return c
}

// markErrorIfCurrent records a reauth failure only while c is still the published client of the
// generation it was built under. The identity/generation check is repeated under the state
// boundary, the same boundary complete uses to replace the token, so a completion that lands
// after the first look cannot be overwritten by the stale callback. A client that an in-flight
// read obtained before a restore or completion can outlive it; its callback must not mark the
// newer row failed or delete the newer client from the cache.
func (m *Manager) markErrorIfCurrent(id int64, generation uint64, c *dropbox.Client, msg string) {
	release, err := m.beginMutation()
	if err != nil {
		// A restore owns the storage table. The failure belongs to the state being replaced.
		return
	}
	defer release()

	// Cheap pre-check for a client that has already been replaced. It is not authoritative:
	// complete can replace the token after this look, so the check is repeated under the state
	// boundary below.
	m.mu.RLock()
	current, ok := m.clients[id]
	firstGeneration := m.clientGeneration
	m.mu.RUnlock()
	if !ok || current != c || firstGeneration != generation {
		return
	}

	// Read the clock before taking the state boundary: a slow clock must not hold up a completion
	// or lengthen the window in which this callback owns the state.
	now := m.now().Unix()

	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	// The first look ran without the ownership complete uses for the token replacement, so it can
	// be stale by now. Recheck under that same boundary before writing the status.
	m.mu.RLock()
	current, ok = m.clients[id]
	currentGeneration := m.clientGeneration
	m.mu.RUnlock()
	if !ok || current != c || currentGeneration != generation {
		// A completion or restore replaced this client while the callback waited for the
		// boundary. The failure belongs to the replaced state, not to the row now stored.
		return
	}
	m.markErrorLocked(id, msg, now)
}

// markErrorLocked records a failure and invalidates the client cache as one state transition. The
// caller owns the state boundary: stateMu for a reauth callback, or the restore write side of
// mutationMu for ResetAndInitClients. No client build or completion can interleave with the write,
// the reload and the eviction.
func (m *Manager) markErrorLocked(id int64, msg string, now int64) {
	_, _ = m.db.Exec(`UPDATE storages SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`, StatusError, msg, now, id)
	_ = m.reload()
	m.mu.Lock()
	delete(m.clients, id)
	m.mu.Unlock()
}

func (m *Manager) redirectFor(mode string) (string, error) {
	switch mode {
	case AuthLocalhost, "":
		return m.cfg.LocalhostCallback(), nil
	case AuthPublic:
		r := m.cfg.PublicCallback()
		if r == "" {
			return "", &ValidationError{Field: "auth_mode", Msg: "config.json에 public_url이 없어 public_url 리다이렉트를 쓸 수 없습니다"}
		}
		return r, nil
	case AuthCode:
		return "", nil
	}
	return "", &ValidationError{Field: "auth_mode", Msg: "인증 방식이 올바르지 않습니다"}
}

func (m *Manager) checkMount(mount string, exceptID int64) (string, error) {
	nm, err := pathutil.NormalizeMount(mount)
	if err != nil {
		return "", &ValidationError{Field: "mount_path", Msg: err.Error()}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.list {
		if s.ID == exceptID {
			continue
		}
		if pathutil.MountsConflict(nm, s.MountPath) {
			if strings.EqualFold(nm, s.MountPath) {
				return "", &ValidationError{Field: "mount_path", Msg: fmt.Sprintf("'%s'는 이미 다른 저장소가 사용 중입니다", s.MountPath)}
			}
			return "", &ValidationError{Field: "mount_path", Msg: fmt.Sprintf("'%s' 저장소와 경로가 겹칩니다. 상위·하위 경로에는 마운트할 수 없습니다", s.MountPath)}
		}
	}
	return nm, nil
}

// CheckMount validates a mount path against existing storages (for live form feedback).
func (m *Manager) CheckMount(mount string, exceptID int64) error {
	_, err := m.checkMount(mount, exceptID)
	return err
}

// AuthStart is returned when a storage is created or re-authorized.
type AuthStart struct {
	Storage      Storage `json:"storage"`
	AuthorizeURL string  `json:"authorize_url"`
	RedirectURI  string  `json:"redirect_uri"`
}

// Create inserts a pending storage and returns the authorization URL.
func (m *Manager) Create(in Input) (*AuthStart, error) {
	release, err := m.beginMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	m.mountMu.Lock()
	defer m.mountMu.Unlock()
	mount, err := m.checkMount(in.MountPath, 0)
	if err != nil {
		return nil, err
	}
	root, err := pathutil.NormalizeRoot(in.RootPath)
	if err != nil {
		return nil, &ValidationError{Field: "root_path", Msg: err.Error()}
	}
	in.AppKey = strings.TrimSpace(in.AppKey)
	in.AppSecret = strings.TrimSpace(in.AppSecret)
	if in.AppKey == "" {
		return nil, &ValidationError{Field: "app_key", Msg: "App key를 입력하세요"}
	}
	if in.AppSecret == "" {
		return nil, &ValidationError{Field: "app_secret", Msg: "App secret을 입력하세요"}
	}
	if in.CacheTTLMin == 0 {
		in.CacheTTLMin = 30
	}
	if in.CacheTTLMin < 1 || in.CacheTTLMin > 10080 {
		return nil, &ValidationError{Field: "cache_ttl_min", Msg: "캐시 만료 시간은 1~10080분이어야 합니다"}
	}
	if in.AuthMode == "" {
		in.AuthMode = AuthLocalhost
	}
	redirect, err := m.redirectFor(in.AuthMode)
	if err != nil {
		return nil, err
	}
	enc, err := m.box.Seal([]byte(in.AppSecret))
	if err != nil {
		return nil, err
	}
	now := m.now().Unix()
	res, err := m.db.Exec(`INSERT INTO storages(name, mount_path, root_path, app_key, app_secret_enc, cache_ttl_min, status, auth_mode, redirect_uri, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, strings.TrimSpace(in.Name), mount, root, in.AppKey, enc, in.CacheTTLMin, StatusPending, in.AuthMode, redirect, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, &ValidationError{Field: "mount_path", Msg: "이미 사용 중인 마운트 경로입니다"}
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := m.reload(); err != nil {
		return nil, err
	}
	return m.startAuth(id, in.AuthMode, redirect)
}

// Reauth starts a new authorization for an existing storage.
func (m *Manager) Reauth(id int64, mode string) (*AuthStart, error) {
	release, err := m.beginMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	if _, ok := m.Get(id); !ok {
		return nil, errors.New("저장소를 찾을 수 없습니다")
	}
	if mode == "" {
		mode = AuthLocalhost
	}
	redirect, err := m.redirectFor(mode)
	if err != nil {
		return nil, err
	}
	if _, err := m.db.Exec(`UPDATE storages SET auth_mode = ?, redirect_uri = ?, updated_at = ? WHERE id = ?`, mode, redirect, m.now().Unix(), id); err != nil {
		return nil, err
	}
	if err := m.reload(); err != nil {
		return nil, err
	}
	return m.startAuth(id, mode, redirect)
}

func (m *Manager) startAuth(id int64, mode, redirect string) (*AuthStart, error) {
	s, _ := m.Get(id)
	state, err := secret.RandomToken(32)
	if err != nil {
		return nil, err
	}
	if _, err := m.db.Exec(`DELETE FROM oauth_states WHERE expires_at < ? OR storage_id = ?`, m.now().Unix(), id); err != nil {
		return nil, err
	}
	if _, err := m.db.Exec(`INSERT INTO oauth_states(state_hash, storage_id, redirect_uri, expires_at) VALUES(?,?,?,?)`,
		secret.HashToken(state), id, redirect, m.now().Add(StateTTL).Unix()); err != nil {
		return nil, err
	}
	return &AuthStart{Storage: s, AuthorizeURL: dropbox.AuthorizeURL(m.ep, s.AppKey, redirect, state), RedirectURI: redirect}, nil
}

// ErrState is returned for unknown, expired or reused OAuth states.
var ErrState = errors.New("인증 요청이 만료되었거나 이미 사용되었습니다. 저장소 화면에서 다시 인증을 시작하세요")

// ErrRestoreInProgress is returned when a storage mutation arrives while a backup restore is
// replacing the storage table and rebuilding the runtime.
var ErrRestoreInProgress = errors.New("복원 작업이 진행 중입니다. 잠시 후 다시 시도하세요")

// BeginRestore takes the storage mutation gate for a restore: in-flight mutations finish, new
// mutations fail with ErrRestoreInProgress, and the caller must call EndRestore on every path.
func (m *Manager) BeginRestore() { m.mutationMu.Lock() }

// EndRestore releases the gate taken by BeginRestore.
func (m *Manager) EndRestore() { m.mutationMu.Unlock() }

// beginMutation admits one storage mutation unless a restore owns the gate. It never waits, so a
// restore cannot deadlock behind a mutation that is itself waiting for the restore.
func (m *Manager) beginMutation() (func(), error) {
	if !m.mutationMu.TryRLock() {
		return nil, ErrRestoreInProgress
	}
	return m.mutationMu.RUnlock, nil
}

// consumeState marks a state used and returns its storage and redirect URI.
func (m *Manager) consumeState(state string) (int64, string, error) {
	if state == "" {
		return 0, "", ErrState
	}
	var id int64
	var redirect string
	err := m.db.QueryRow(`UPDATE oauth_states SET used = 1 WHERE state_hash = ? AND used = 0 AND expires_at >= ? RETURNING storage_id, redirect_uri`,
		secret.HashToken(state), m.now().Unix()).Scan(&id, &redirect)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrState
	}
	return id, redirect, err
}

// CompleteWithState finishes an authorization from a redirect (callback or pasted URL).
func (m *Manager) CompleteWithState(ctx context.Context, state, code string) (Storage, error) {
	release, err := m.beginMutation()
	if err != nil {
		return Storage{}, err
	}
	defer release()
	id, redirect, err := m.consumeState(state)
	if err != nil {
		return Storage{}, err
	}
	return m.complete(ctx, id, code, redirect)
}

// CompleteWithCode finishes a code-paste authorization for a storage. The pending state for
// the storage must still be valid; it is consumed.
func (m *Manager) CompleteWithCode(ctx context.Context, id int64, code string) (Storage, error) {
	release, err := m.beginMutation()
	if err != nil {
		return Storage{}, err
	}
	defer release()
	code = strings.TrimSpace(code)
	if code == "" {
		return Storage{}, &ValidationError{Field: "code", Msg: "Dropbox 화면에 표시된 코드를 붙여넣으세요"}
	}
	var redirect string
	err = m.db.QueryRow(`UPDATE oauth_states SET used = 1 WHERE storage_id = ? AND used = 0 AND expires_at >= ? RETURNING redirect_uri`,
		id, m.now().Unix()).Scan(&redirect)
	if errors.Is(err, sql.ErrNoRows) {
		return Storage{}, ErrState
	}
	if err != nil {
		return Storage{}, err
	}
	return m.complete(ctx, id, code, redirect)
}

// ParsePastedURL extracts code and state from a redirect URL copied out of the address bar.
// It also reports a Dropbox error (e.g. access_denied).
func ParsePastedURL(raw string) (code, state string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", &ValidationError{Field: "url", Msg: "주소창의 URL을 붙여넣으세요"}
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", &ValidationError{Field: "url", Msg: "URL 형식이 아닙니다"}
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		msg := "Dropbox가 인증을 거부했습니다: " + e
		if d := q.Get("error_description"); d != "" {
			msg += " (" + d + ")"
		}
		return "", "", &ValidationError{Field: "url", Msg: msg}
	}
	code, state = q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		return "", "", &ValidationError{Field: "url", Msg: "URL에 code와 state가 없습니다. Dropbox 승인 후 이동한 페이지의 주소 전체를 복사하세요"}
	}
	return code, state, nil
}

func (m *Manager) complete(ctx context.Context, id int64, code, redirect string) (Storage, error) {
	s, ok := m.Get(id)
	if !ok {
		return Storage{}, errors.New("저장소를 찾을 수 없습니다")
	}
	var secEnc []byte
	if err := m.db.QueryRow(`SELECT app_secret_enc FROM storages WHERE id = ?`, id).Scan(&secEnc); err != nil {
		return Storage{}, err
	}
	appSecret, err := m.box.Open(secEnc)
	if err != nil {
		return Storage{}, err
	}
	tok, err := dropbox.ExchangeCode(ctx, nil, m.ep, s.AppKey, string(appSecret), strings.TrimSpace(code), redirect)
	if err != nil {
		m.setError(id, "토큰 교환 실패: "+err.Error())
		return Storage{}, fmt.Errorf("Dropbox 토큰 교환에 실패했습니다: %w", err)
	}
	if tok.RefreshToken == "" {
		return Storage{}, errors.New("Dropbox가 refresh token을 주지 않았습니다. 앱 설정을 확인하세요")
	}
	c := dropbox.New(m.ep, s.AppKey, string(appSecret), tok.RefreshToken)
	acct, err := c.CurrentAccount(ctx)
	if err != nil {
		m.setError(id, "계정 정보 조회 실패: "+err.Error())
		return Storage{}, fmt.Errorf("계정 정보를 읽지 못했습니다(account_info.read 권한을 확인하세요): %w", err)
	}
	rtEnc, err := m.box.Seal([]byte(tok.RefreshToken))
	if err != nil {
		return Storage{}, err
	}
	// The token write and the cache invalidation are one state replacement. Client builds under
	// the same boundary, so a client that read the old token cannot be published after this write,
	// and a stale reauth callback rechecks its identity under it before touching the status.
	if err := func() error {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		if _, err := m.db.Exec(`UPDATE storages SET refresh_token_enc = ?, account_id = ?, account_email = ?, account_name = ?, status = ?, last_error = '', updated_at = ?, authorized_at = ? WHERE id = ?`,
			rtEnc, acct.AccountID, acct.Email, acct.Name.DisplayName, StatusActive, m.now().Unix(), m.now().UnixMilli(), id); err != nil {
			return err
		}
		m.mu.Lock()
		delete(m.clients, id)
		m.mu.Unlock()
		return m.reload()
	}(); err != nil {
		return Storage{}, err
	}
	m.changed(id)
	s, _ = m.Get(id)
	return s, nil
}

func (m *Manager) setError(id int64, msg string) {
	_, _ = m.db.Exec(`UPDATE storages SET last_error = ?, updated_at = ? WHERE id = ?`, msg, m.now().Unix(), id)
	_ = m.reload()
}

// Update edits name, mount, root and cache TTL.
func (m *Manager) Update(id int64, in Input) (Storage, error) {
	release, err := m.beginMutation()
	if err != nil {
		return Storage{}, err
	}
	defer release()
	if _, ok := m.Get(id); !ok {
		return Storage{}, errors.New("저장소를 찾을 수 없습니다")
	}
	m.mountMu.Lock()
	defer m.mountMu.Unlock()
	mount, err := m.checkMount(in.MountPath, id)
	if err != nil {
		return Storage{}, err
	}
	root, err := pathutil.NormalizeRoot(in.RootPath)
	if err != nil {
		return Storage{}, &ValidationError{Field: "root_path", Msg: err.Error()}
	}
	if in.CacheTTLMin < 1 || in.CacheTTLMin > 10080 {
		return Storage{}, &ValidationError{Field: "cache_ttl_min", Msg: "캐시 만료 시간은 1~10080분이어야 합니다"}
	}
	if _, err := m.db.Exec(`UPDATE storages SET name = ?, mount_path = ?, root_path = ?, cache_ttl_min = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(in.Name), mount, root, in.CacheTTLMin, m.now().Unix(), id); err != nil {
		return Storage{}, err
	}
	if err := m.reload(); err != nil {
		return Storage{}, err
	}
	m.changed(id)
	s, _ := m.Get(id)
	return s, nil
}

// Delete revokes the token (best effort) and removes the storage.
func (m *Manager) Delete(ctx context.Context, id int64) error {
	release, err := m.beginMutation()
	if err != nil {
		return err
	}
	defer release()
	s, ok := m.Get(id)
	if !ok {
		return errors.New("저장소를 찾을 수 없습니다")
	}
	if s.Status != StatusPending {
		if c, err := m.clientOwned(id); err == nil {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = c.Revoke(rctx)
			cancel()
		}
	}
	if _, err := m.db.Exec(`DELETE FROM storages WHERE id = ?`, id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.clients, id)
	m.mu.Unlock()
	if err := m.reload(); err != nil {
		return err
	}
	m.changed(id)
	return nil
}
