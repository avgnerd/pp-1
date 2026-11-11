// Package search runs a query against every active storage in parallel and streams results.
package search

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/limiter"
	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
)

// Kinds.
const (
	KindAll    = "all"
	KindFolder = "folder"
	KindFile   = "file"
)

// Event is one streamed message.
type Event struct {
	Type      string      `json:"type"` // "results", "storage", "done"
	StorageID int64       `json:"storage_id,omitempty"`
	Mount     string      `json:"mount,omitempty"`
	Entries   []vfs.Entry `json:"entries,omitempty"`
	Status    string      `json:"status,omitempty"` // storage: "done" | "error"
	Count     int         `json:"count,omitempty"`
	Error     string      `json:"error,omitempty"`
	Total     int         `json:"total,omitempty"`
	Storages  int         `json:"storages,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
}

// Options for a search.
type Options struct {
	Query           string
	Kind            string
	Scope           string // optional virtual path to search under
	PerStorageLimit int
	Timeout         time.Duration
}

// Run searches every active storage (optionally limited to a virtual scope) and calls emit
// from a single goroutine. It returns when all storages finish or ctx ends.
func Run(ctx context.Context, st *storage.Manager, lim *limiter.Limiter, opt Options, emit func(Event)) error {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return errors.New("검색어를 입력하세요")
	}
	if opt.PerStorageLimit <= 0 {
		opt.PerStorageLimit = 500
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 25 * time.Second
	}
	type target struct {
		st     storage.Storage
		dbxDir string
	}
	var targets []target
	for _, s := range st.All() {
		if s.Status != storage.StatusActive {
			continue
		}
		dir := s.RootPath
		if opt.Scope != "" && opt.Scope != "/" {
			switch {
			case pathutil.HasPrefix(opt.Scope, s.MountPath):
				dir = pathutil.Join(s.RootPath, pathutil.Rel(opt.Scope, s.MountPath))
			case pathutil.HasPrefix(s.MountPath, opt.Scope):
			default:
				continue
			}
		}
		targets = append(targets, target{s, dir})
	}
	events := make(chan Event, 64)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			if err := lim.Acquire(ctx); err != nil {
				return
			}
			defer lim.Release()
			outcome, err := searchOne(ctx, st, t.st, t.dbxDir, q, opt, events)
			ev := Event{Type: "storage", StorageID: t.st.ID, Mount: t.st.MountPath, Status: "done", Count: outcome.count, Truncated: outcome.truncated}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				ev.Status = "error"
				ev.Error = err.Error()
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}(t)
	}
	go func() { wg.Wait(); close(events) }()
	emit(Event{Type: "start", Storages: len(targets)})
	total := 0
	truncated := false
	for ev := range events {
		if ev.Type == "results" {
			total += len(ev.Entries)
		}
		if ev.Type == "storage" && ev.Truncated {
			truncated = true
		}
		emit(ev)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	emit(Event{Type: "done", Total: total, Storages: len(targets), Truncated: truncated})
	return nil
}

type searchOneOutcome struct {
	count     int
	truncated bool
}

func searchOne(ctx context.Context, sm *storage.Manager, s storage.Storage, dbxDir, q string, opt Options, out chan<- Event) (searchOneOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	c, err := sm.Client(s.ID)
	if err != nil {
		return searchOneOutcome{}, err
	}
	so := dropbox.SearchOptions{MaxResults: 200, FileStatus: "active", FilenameOnly: true}
	if dbxDir != "/" {
		so.Path = dbxDir
	}
	if opt.Kind == KindFolder {
		so.FileCategories = []string{"folder"}
	}
	res, err := c.Search(ctx, q, so)
	if err != nil {
		return searchOneOutcome{}, err
	}
	n := 0
	truncated := false
	for {
		var batch []vfs.Entry
		for _, m := range res.Matches {
			md := m.Metadata.Metadata
			isDir := md.Tag == "folder"
			if (opt.Kind == KindFolder && !isDir) || (opt.Kind == KindFile && isDir) || md.Tag == "deleted" {
				continue
			}
			if !pathutil.HasPrefix(md.PathDisplay, dbxDir) {
				continue
			}
			vp, ok := vfs.VirtualPath(s, md.PathDisplay)
			if !ok {
				continue
			}
			if n >= opt.PerStorageLimit {
				truncated = true
				break
			}
			e := vfs.Entry{Name: md.Name, Path: vp, ID: md.ID, StorageID: s.ID}
			if isDir {
				e.Type = vfs.TypeFolder
			} else {
				e.Type = vfs.TypeFile
				sz := md.Size
				e.Size = &sz
				e.Modified = md.ServerModified
				e.Rev = md.Rev
			}
			batch = append(batch, e)
			n++
		}
		if len(batch) > 0 {
			select {
			case out <- Event{Type: "results", StorageID: s.ID, Mount: s.MountPath, Entries: batch}:
			case <-ctx.Done():
				return searchOneOutcome{count: n, truncated: truncated}, ctx.Err()
			}
		}
		if !res.HasMore || res.Cursor == "" || n >= opt.PerStorageLimit {
			if n >= opt.PerStorageLimit && res.HasMore {
				truncated = true
			}
			return searchOneOutcome{count: n, truncated: truncated}, nil
		}
		if res, err = c.SearchContinue(ctx, res.Cursor); err != nil {
			return searchOneOutcome{count: n, truncated: truncated}, err
		}
	}
}
