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

// Store persists settings and notifies listeners on change.
type Store struct {
	db        *sql.DB
	mu        sync.RWMutex
	cur       Settings
	listeners []func(old, new Settings)
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

// Update validates and persists new settings.
func (s *Store) Update(n Settings) error {
	if err := n.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for k, v := range m {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, string(v)); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	old := s.cur
	s.cur = n
	ls := append([]func(old, new Settings){}, s.listeners...)
	s.mu.Unlock()
	for _, fn := range ls {
		fn(old, n)
	}
	return nil
}
