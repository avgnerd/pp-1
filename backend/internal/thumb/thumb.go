// Package thumb fetches image thumbnails in batches of up to 25 per storage and keeps them
// in a size-bounded disk cache.
package thumb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// Sizes accepted from clients.
var Sizes = map[string]bool{"w128h128": true, "w256h256": true, "w480h320": true, "w640h480": true, "w1024h768": true}

// Thumbnailable reports whether Dropbox can make a thumbnail for the extension.
func Thumbnailable(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".tiff", ".tif", ".gif", ".webp", ".ppm", ".bmp", ".heic", ".heif":
		return true
	}
	return false
}

type request struct {
	path string
	done chan result
}

type result struct {
	data []byte
	err  error
}

type batch struct {
	reqs  []request
	timer *time.Timer
}

// Service batches requests and caches results on disk.
type Service struct {
	st      *storage.Manager
	dir     string
	window  time.Duration
	maxSize func() int64

	mu      sync.Mutex
	pending map[string]*batch // key: storageID|size

	diskMu sync.Mutex
	files  map[string]fileInfo
	total  int64
}

type fileInfo struct {
	size   int64
	access time.Time
}

// New creates the service and indexes the existing cache directory.
func New(st *storage.Manager, dir string, maxSize func() int64) (*Service, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Service{st: st, dir: dir, window: 25 * time.Millisecond, maxSize: maxSize, pending: map[string]*batch{}, files: map[string]fileInfo{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		s.files[e.Name()] = fileInfo{size: info.Size(), access: info.ModTime()}
		s.total += info.Size()
	}
	s.evict()
	return s, nil
}

func cacheName(storageID int64, dbxPath, rev, size string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", storageID, strings.ToLower(dbxPath), rev, size)))
	return hex.EncodeToString(h[:16]) + ".jpg"
}

// Get returns JPEG thumbnail bytes for a file. rev keys the cache so edits produce new thumbnails.
func (s *Service) Get(ctx context.Context, storageID int64, dbxPath, rev, size string) ([]byte, error) {
	if !Sizes[size] {
		size = "w256h256"
	}
	name := cacheName(storageID, dbxPath, rev, size)
	if b, err := os.ReadFile(filepath.Join(s.dir, name)); err == nil {
		s.diskMu.Lock()
		if fi, ok := s.files[name]; ok {
			fi.access = time.Now()
			s.files[name] = fi
		}
		s.diskMu.Unlock()
		return b, nil
	}
	ch := make(chan result, 1)
	key := fmt.Sprintf("%d|%s", storageID, size)
	s.mu.Lock()
	b := s.pending[key]
	if b == nil {
		b = &batch{}
		s.pending[key] = b
		b.timer = time.AfterFunc(s.window, func() { s.flush(key, storageID, size) })
	}
	b.reqs = append(b.reqs, request{path: dbxPath, done: ch})
	if len(b.reqs) >= dropbox.MaxThumbBatch {
		b.timer.Stop()
		delete(s.pending, key)
		go s.run(storageID, size, b.reqs)
	}
	s.mu.Unlock()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		s.store(name, r.data)
		return r.data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) flush(key string, storageID int64, size string) {
	s.mu.Lock()
	b := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if b != nil && len(b.reqs) > 0 {
		s.run(storageID, size, b.reqs)
	}
}

// ErrNoThumb means Dropbox could not make a thumbnail for this file.
var ErrNoThumb = errors.New("썸네일을 만들 수 없는 파일입니다")

func (s *Service) run(storageID int64, size string, reqs []request) {
	fail := func(err error) {
		for _, r := range reqs {
			r.done <- result{err: err}
		}
	}
	c, err := s.st.Client(storageID)
	if err != nil {
		fail(err)
		return
	}
	paths := make([]string, len(reqs))
	for i, r := range reqs {
		paths[i] = r.path
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.GetThumbnailBatch(ctx, paths, size)
	if err != nil {
		fail(err)
		return
	}
	for i, r := range reqs {
		if i >= len(res) || !res[i].OK {
			r.done <- result{err: ErrNoThumb}
			continue
		}
		r.done <- result{data: res[i].Data}
	}
}

func (s *Service) store(name string, data []byte) {
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		os.Remove(tmp)
		return
	}
	s.diskMu.Lock()
	if old, ok := s.files[name]; ok {
		s.total -= old.size
	}
	s.files[name] = fileInfo{size: int64(len(data)), access: time.Now()}
	s.total += int64(len(data))
	s.diskMu.Unlock()
	s.evict()
}

// evict removes least recently used files until the cache is under 90% of its limit.
func (s *Service) evict() {
	limit := s.maxSize()
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	if s.total <= limit {
		return
	}
	type kv struct {
		name string
		fi   fileInfo
	}
	all := make([]kv, 0, len(s.files))
	for n, fi := range s.files {
		all = append(all, kv{n, fi})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].fi.access.Before(all[j].fi.access) })
	target := limit * 9 / 10
	for _, e := range all {
		if s.total <= target {
			break
		}
		_ = os.Remove(filepath.Join(s.dir, e.name))
		s.total -= e.fi.size
		delete(s.files, e.name)
	}
}

// Usage returns the number of cached files and bytes.
func (s *Service) Usage() (int, int64) {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	return len(s.files), s.total
}

// Clear deletes every cached thumbnail.
func (s *Service) Clear() {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	for n := range s.files {
		os.Remove(filepath.Join(s.dir, n))
	}
	s.files = map[string]fileInfo{}
	s.total = 0
}
