package vfs

import (
	"context"
	"errors"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/limiter"
)

// SizeProgress is reported while a folder size is being calculated.
type SizeProgress struct {
	Bytes   int64 `json:"bytes"`
	Files   int64 `json:"files"`
	Folders int64 `json:"folders"`
	Done    bool  `json:"done"`
	Queued  bool  `json:"queued,omitempty"`
}

// FolderSize computes the total size of a folder on demand using a recursive listing.
// The result is cached for the storage's cache TTL. lim bounds concurrent calculations
// across the server; progress is called after each page and while waiting in the queue.
func (s *Service) FolderSize(ctx context.Context, vpath string, lim *limiter.Limiter, progress func(SizeProgress)) (SizeProgress, error) {
	t, err := s.Resolve(vpath)
	if err != nil {
		return SizeProgress{}, err
	}
	if t.Virtual {
		return SizeProgress{}, errors.New("가상 폴더는 크기를 계산할 수 없습니다. 저장소 폴더를 선택하세요")
	}
	key := FolderSizeKey(t.Storage.ID, t.DbxPath)
	if v, ok := s.Cache.Get(key); ok {
		p := SizeProgress{Bytes: v.(int64), Done: true}
		return p, nil
	}
	if !lim.TryAcquire() {
		progress(SizeProgress{Queued: true})
		if err := lim.Acquire(ctx); err != nil {
			return SizeProgress{}, err
		}
	}
	defer lim.Release()
	c, err := s.St.Client(t.Storage.ID)
	if err != nil {
		return SizeProgress{}, err
	}
	var p SizeProgress
	last := time.Now()
	err = c.ListFolder(ctx, t.DbxPath, true, func(es []dropbox.Metadata) error {
		for _, e := range es {
			switch e.Tag {
			case "file":
				p.Bytes += e.Size
				p.Files++
			case "folder":
				p.Folders++
			}
		}
		if time.Since(last) > 250*time.Millisecond {
			last = time.Now()
			progress(p)
		}
		return ctx.Err()
	})
	if err != nil {
		if dropbox.HasSummary(err, "not_found") {
			return SizeProgress{}, ErrNotFound
		}
		return SizeProgress{}, err
	}
	p.Done = true
	if p.Folders > 0 && t.DbxPath != "/" {
		p.Folders-- // the recursive listing includes the folder itself
	}
	s.Cache.Set(key, p.Bytes, 16, t.Storage.CacheTTL())
	return p, nil
}
