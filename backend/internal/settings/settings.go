// Package settings stores runtime-editable options as key/value rows and keeps a typed snapshot in memory.
package settings

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Pagination types.
const (
	PageAll      = "all"
	PagePages    = "pages"
	PageMore     = "more"
	PageAutoMore = "auto_more"
)

// Data modes for file bytes.
const (
	ModeDirect = "direct"
	ModeProxy  = "proxy"
)

// Settings is the full set of admin-editable options.
type Settings struct {
	// Site
	RobotsTxt      string `json:"robots_txt"`
	PaginationType string `json:"pagination_type"`
	PerPage        int    `json:"per_page"`
	// Preview
	AudioAutoplay  bool `json:"audio_autoplay"`
	VideoAutoplay  bool `json:"video_autoplay"`
	ArchivePreview bool `json:"archive_preview"`
	// System
	CacheCleanupSec       int    `json:"cache_cleanup_sec"`
	DataMode              string `json:"data_mode"`
	TranscodeConcurrency  int    `json:"transcode_concurrency"`
	ThumbCacheMB          int    `json:"thumb_cache_mb"`
	FolderSizeConcurrency int    `json:"folder_size_concurrency"`
	SearchConcurrency     int    `json:"search_concurrency"`
	ArchiveMaxMB          int    `json:"archive_max_mb"`
}

// Defaults returns the factory settings.
func Defaults() Settings {
	return Settings{
		RobotsTxt:             "User-agent: *\nDisallow: /",
		PaginationType:        PagePages,
		PerPage:               30,
		AudioAutoplay:         true,
		VideoAutoplay:         true,
		ArchivePreview:        true,
		CacheCleanupSec:       120,
		DataMode:              ModeDirect,
		TranscodeConcurrency:  2,
		ThumbCacheMB:          512,
		FolderSizeConcurrency: 3,
		SearchConcurrency:     8,
		ArchiveMaxMB:          512,
	}
}

// Validate checks ranges and enums.
func (s Settings) Validate() error {
	var errs []string
	switch s.PaginationType {
	case PageAll, PagePages, PageMore, PageAutoMore:
	default:
		errs = append(errs, "pagination_type: all, pages, more, auto_more 중 하나여야 합니다")
	}
	if s.PerPage < 5 || s.PerPage > 1000 {
		errs = append(errs, "per_page: 5~1000 사이여야 합니다")
	}
	if len(s.RobotsTxt) > 64*1024 {
		errs = append(errs, "robots_txt: 64KB를 넘을 수 없습니다")
	}
	if s.CacheCleanupSec < 10 || s.CacheCleanupSec > 86400 {
		errs = append(errs, "cache_cleanup_sec: 10~86400 사이여야 합니다")
	}
	switch s.DataMode {
	case ModeDirect, ModeProxy:
	default:
		errs = append(errs, "data_mode: direct 또는 proxy여야 합니다")
	}
	if s.TranscodeConcurrency < 0 || s.TranscodeConcurrency > 16 {
		errs = append(errs, "transcode_concurrency: 0~16 사이여야 합니다")
	}
	if s.ThumbCacheMB < 16 || s.ThumbCacheMB > 100000 {
		errs = append(errs, "thumb_cache_mb: 16~100000 사이여야 합니다")
	}
	if s.FolderSizeConcurrency < 1 || s.FolderSizeConcurrency > 32 {
		errs = append(errs, "folder_size_concurrency: 1~32 사이여야 합니다")
	}
	if s.SearchConcurrency < 1 || s.SearchConcurrency > 64 {
		errs = append(errs, "search_concurrency: 1~64 사이여야 합니다")
	}
	if s.ArchiveMaxMB < 1 || s.ArchiveMaxMB > 100000 {
		errs = append(errs, "archive_max_mb: 1~100000 사이여야 합니다")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// AllKeys lists the JSON keys of Settings, sorted. It is derived from Defaults so a new field
// can never be forgotten.
func AllKeys() []string {
	b, err := json.Marshal(Defaults())
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidateKeyValue checks one key/value pair (the value is the JSON text stored in the settings
// table) with exactly the rules Store.Update applies to the whole set. Only failures for k are
// reported: every other field keeps its default, which validates.
func ValidateKeyValue(k, v string) error {
	b, err := json.Marshal(Defaults())
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if _, ok := m[k]; !ok {
		return fmt.Errorf("%s: 알 수 없는 설정 키입니다", k)
	}
	if !json.Valid([]byte(v)) {
		return fmt.Errorf("%s: 값이 올바른 JSON이 아닙니다", k)
	}
	m[k] = json.RawMessage(v)
	nb, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("%s: %w", k, err)
	}
	n := Defaults()
	if err := json.Unmarshal(nb, &n); err != nil {
		return fmt.Errorf("%s: 값 형식이 올바르지 않습니다", k)
	}
	if err := n.Validate(); err != nil {
		for _, part := range strings.Split(err.Error(), "; ") {
			if strings.HasPrefix(part, k+": ") {
				return errors.New(part)
			}
		}
		return fmt.Errorf("%s: %w", k, err)
	}
	return nil
}

// Queryer is satisfied by *sql.DB and *sql.Tx.
type Queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// ReadAll loads every settings row through q and overlays it on the defaults. Missing keys keep
// their default; a stored set that does not validate falls back to the defaults.
func ReadAll(q Queryer) (Settings, error) {
	cur := Defaults()
	rows, err := q.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return cur, err
	}
	defer rows.Close()
	m := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return cur, err
		}
		m[k] = json.RawMessage(v)
	}
	if err := rows.Err(); err != nil {
		return cur, err
	}
	if len(m) == 0 {
		return cur, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return cur, err
	}
	loaded := Defaults()
	if err := json.Unmarshal(b, &loaded); err == nil && loaded.Validate() == nil {
		return loaded, nil
	}
	return cur, nil
}

// ErrRestoreInProgress is returned when a settings write arrives while a backup restore owns the
// store, or when the settings it is based on were read before a restore that has since completed.
var ErrRestoreInProgress = errors.New("복원 작업이 진행 중입니다. 설정을 저장할 수 없습니다. 잠시 후 다시 시도하세요")

// Store persists settings and notifies listeners on change.
type Store struct {
	db        *sql.DB
	mu        sync.RWMutex
	cur       Settings
	listeners []func(old, new Settings)

	// writeMu serializes the database write and the in-memory swap. BeginRestore holds it for the
	// whole restore, so an update either finishes before the restore starts or fails with
	// ErrRestoreInProgress instead of writing pre-restore values after the restore commits.
	writeMu sync.Mutex
	// restoring is set before BeginRestore waits for an in-flight update, so a new writer fails
	// immediately instead of blocking for the whole restore. Reads are atomic; the authoritative
	// check runs again while writeMu is held.
	restoring atomic.Bool
	// restoreVersion increments at the start and at the end of every restore. A caller that read
	// settings before or during the restore passes that revision to UpdateAtRevision, which
	// refuses the stale write.
	restoreVersion atomic.Uint64
}

// Open loads settings from the database, filling missing keys with defaults.
func Open(db *sql.DB) (*Store, error) {
	s := &Store{db: db, cur: Defaults()}
	cur, err := ReadAll(db)
	if err != nil {
		return nil, err
	}
	s.cur = cur
	return s, nil
}

// ReplaceInMemory installs n as the current settings and calls every listener once. It does not
// write to the database: the restore path has already stored the values in its own transaction.
// The caller must hold the gate taken by BeginRestore. ReplaceInMemory deliberately does not take
// writeMu, because BeginRestore already holds it and re-locking would deadlock; Update is the
// other memory writer and it fails while restoring.
func (s *Store) ReplaceInMemory(n Settings) {
	s.mu.Lock()
	old := s.cur
	s.cur = n
	ls := append([]func(old, new Settings){}, s.listeners...)
	s.mu.Unlock()
	for _, fn := range ls {
		fn(old, n)
	}
}

// BeginRestore takes the settings write gate for a restore. It waits for a write that already
// holds the gate; that write either finishes first or backs out at its post-lock restore check. A
// write that starts while the gate is held fails with ErrRestoreInProgress. Every call bumps the
// restore version, and EndRestore bumps it again once the restored settings are published, so a
// read-modify-write that began before or during the restore is refused. The caller must pair this
// with EndRestore on every path.
func (s *Store) BeginRestore() {
	// Set the flag before waiting for writeMu so a new update fails fast rather than hanging for
	// the whole restore. The post-lock check in writeAtRevision is still authoritative.
	s.restoring.Store(true)
	s.writeMu.Lock()
	s.restoreVersion.Add(1)
}

// EndRestore publishes the end of the restore and releases the gate taken by BeginRestore. It
// bumps the restore version again, under the gate and after ReplaceInMemory has installed the
// restored settings, so a revision captured during the restore is stale by the time the gate
// opens and its write is refused instead of overwriting the restored values.
func (s *Store) EndRestore() {
	s.restoreVersion.Add(1)
	s.restoring.Store(false)
	s.writeMu.Unlock()
}

// RestoreVersion returns the current restore generation. A settings read that will later be
// written back must capture this first and pass it to UpdateAtRevision.
func (s *Store) RestoreVersion() uint64 {
	return s.restoreVersion.Load()
}

// Get returns a copy of the current settings.
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// OnChange registers a listener called after each successful update.
func (s *Store) OnChange(fn func(old, new Settings)) {
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

// Update validates and persists new settings. It serializes the database write and the in-memory
// swap on one write mutex and refuses to run during a restore.
func (s *Store) Update(n Settings) error {
	return s.UpdateAtRevision(s.RestoreVersion(), n)
}

// UpdateAtRevision validates and persists n only if no restore has happened since rev was read.
// A caller that builds n from Get must capture RestoreVersion before Get and pass it here, so a
// restore that starts or completes in between rejects the stale write with ErrRestoreInProgress.
// Listeners run while writeMu is still held, so they finish in write order and BeginRestore waits
// for a pending notification before it starts replacing the runtime. A listener must not call
// Update, UpdateAtRevision or BeginRestore (that would deadlock); reading the store is fine.
func (s *Store) UpdateAtRevision(rev uint64, n Settings) error {
	if s.restoring.Load() {
		return ErrRestoreInProgress
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// A restore may have started after the fast check, or completed before this lock was taken.
	// Both cases mean the caller's revision no longer describes the store.
	if s.restoring.Load() || s.restoreVersion.Load() != rev {
		return ErrRestoreInProgress
	}
	old, ls, err := s.writeLocked(n)
	if err != nil {
		return err
	}
	// The state mutex is released, so a listener may read the store; writeMu stays held so a
	// restore cannot publish the restored settings between this write and its notification.
	for _, fn := range ls {
		fn(old, n)
	}
	return nil
}

// writeLocked performs the database write and the in-memory swap. The caller holds writeMu.
func (s *Store) writeLocked(n Settings) (Settings, []func(old, new Settings), error) {
	if err := n.Validate(); err != nil {
		return Settings{}, nil, err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return Settings{}, nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return Settings{}, nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Settings{}, nil, err
	}
	for k, v := range m {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, string(v)); err != nil {
			tx.Rollback()
			return Settings{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Settings{}, nil, err
	}
	s.mu.Lock()
	old := s.cur
	s.cur = n
	ls := append([]func(old, new Settings){}, s.listeners...)
	s.mu.Unlock()
	return old, ls, nil
}
