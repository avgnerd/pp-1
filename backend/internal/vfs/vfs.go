// Package vfs maps virtual paths onto Dropbox storages and lists directories with caching.
package vfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/cache"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// Entry types.
const (
	TypeFile    = "file"
	TypeFolder  = "folder"
	TypeMount   = "mount"   // a folder that is a storage mount point
	TypeVirtual = "virtual" // an intermediate folder created by mount paths
)

// ErrNotFound means the virtual path does not exist.
var ErrNotFound = errors.New("경로를 찾을 수 없습니다")

// Entry is one row in a listing.
type Entry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Type      string `json:"type"`
	Size      *int64 `json:"size"`
	Modified  string `json:"modified,omitempty"`
	Rev       string `json:"rev,omitempty"`
	ID        string `json:"id,omitempty"`
	StorageID int64  `json:"storage_id,omitempty"`
	Status    string `json:"status,omitempty"` // mount entries: storage status
}

// IsDir reports whether the entry can be opened as a folder.
func (e Entry) IsDir() bool { return e.Type != TypeFile }

// Target is a resolved virtual path.
type Target struct {
	VPath   string
	Virtual bool // true for "/" and intermediate folders above mounts
	Storage storage.Storage
	DbxPath string // absolute Dropbox path ("/" for the account root)
}

// Service lists and resolves paths.
type Service struct {
	St    *storage.Manager
	Cache *cache.Cache
}

// New creates a service.
func New(st *storage.Manager, c *cache.Cache) *Service { return &Service{St: st, Cache: c} }

// Resolve maps a virtual path to a storage location or a virtual folder.
func (s *Service) Resolve(vpath string) (Target, error) {
	vp, err := pathutil.Clean(vpath)
	if err != nil {
		return Target{}, err
	}
	if st, ok := s.St.ByMount(vp); ok {
		rel := pathutil.Rel(vp, st.MountPath)
		return Target{VPath: vp, Storage: st, DbxPath: pathutil.Join(st.RootPath, rel)}, nil
	}
	if vp == "/" {
		return Target{VPath: vp, Virtual: true}, nil
	}
	for _, st := range s.St.All() {
		if pathutil.HasPrefix(st.MountPath, vp) {
			return Target{VPath: vp, Virtual: true}, nil
		}
	}
	return Target{}, ErrNotFound
}

// VirtualPath converts a Dropbox path of a storage back to a virtual path. ok is false when
// the Dropbox path is outside the storage root.
func VirtualPath(st storage.Storage, dbxDisplay string) (string, bool) {
	if !pathutil.HasPrefix(dbxDisplay, st.RootPath) {
		return "", false
	}
	rel := pathutil.Rel(dbxDisplay, st.RootPath)
	return pathutil.Join(st.MountPath, rel), true
}

// Key helpers: every key of a storage starts with "s:<id>:" so a storage can be flushed at once.
func StoragePrefix(id int64) string              { return fmt.Sprintf("s:%d:", id) }
func listKey(id int64, dbx string) string        { return fmt.Sprintf("s:%d:list:%s", id, strings.ToLower(dbx)) }
func FolderSizeKey(id int64, dbx string) string  { return fmt.Sprintf("s:%d:fsize:%s", id, strings.ToLower(dbx)) }
func TempLinkKey(id int64, dbx string) string    { return fmt.Sprintf("s:%d:tl:%s", id, strings.ToLower(dbx)) }

// listDropbox returns the cached, complete listing of a Dropbox folder.
func (s *Service) listDropbox(ctx context.Context, st storage.Storage, dbx string) ([]dropbox.Metadata, error) {
	v, err := s.Cache.GetOrLoad(listKey(st.ID, dbx), func() (any, int64, time.Duration, error) {
		c, err := s.St.Client(st.ID)
		if err != nil {
			return nil, 0, 0, err
		}
		var all []dropbox.Metadata
		var size int64
		err = c.ListFolder(ctx, dbx, false, func(es []dropbox.Metadata) error {
			for _, e := range es {
				if e.Tag == "deleted" {
					continue
				}
				all = append(all, e)
				size += int64(len(e.Name)+len(e.PathDisplay)+len(e.PathLower)+len(e.ID)+len(e.Rev)+len(e.ServerModified)+len(e.ContentHash)) + 96
			}
			return nil
		})
		if err != nil {
			return nil, 0, 0, err
		}
		return all, size, st.CacheTTL(), nil
	})
	if err != nil {
		if dropbox.HasSummary(err, "not_found") {
			return nil, ErrNotFound
		}
		if dropbox.HasSummary(err, "not_folder") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return v.([]dropbox.Metadata), nil
}

// ListOptions control sorting and paging.
type ListOptions struct {
	Sort   string // name | size | modified
	Order  string // asc | desc
	Offset int
	Limit  int // 0 = all
}

// ListResult is a page of a folder.
type ListResult struct {
	Path      string          `json:"path"`
	Virtual   bool            `json:"virtual"`
	StorageID int64           `json:"storage_id,omitempty"`
	Entries   []Entry         `json:"entries"`
	Total     int             `json:"total"`
	Offset    int             `json:"offset"`
	Storage   *storage.Storage `json:"-"`
}

// List lists a virtual folder. Folder sizes are never computed here; a folder carries a size
// only when an earlier on-demand calculation is still cached.
func (s *Service) List(ctx context.Context, vpath string, opt ListOptions) (*ListResult, error) {
	t, err := s.Resolve(vpath)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if t.Virtual {
		entries = s.virtualChildren(t.VPath)
	} else {
		ms, err := s.listDropbox(ctx, t.Storage, t.DbxPath)
		if err != nil {
			return nil, err
		}
		entries = make([]Entry, 0, len(ms))
		for _, m := range ms {
			e := Entry{Name: m.Name, Path: path.Join(t.VPath, m.Name), ID: m.ID, StorageID: t.Storage.ID}
			if m.IsDir() {
				e.Type = TypeFolder
				if v, ok := s.Cache.Get(FolderSizeKey(t.Storage.ID, path.Join(t.DbxPath, m.Name))); ok {
					sz := v.(SizeProgress).Bytes
					e.Size = &sz
				}
			} else {
				e.Type = TypeFile
				sz := m.Size
				e.Size = &sz
				e.Modified = m.ServerModified
				e.Rev = m.Rev
			}
			entries = append(entries, e)
		}
	}
	SortEntries(entries, opt.Sort, opt.Order)
	res := &ListResult{Path: t.VPath, Virtual: t.Virtual, Total: len(entries)}
	if !t.Virtual {
		res.StorageID = t.Storage.ID
		st := t.Storage
		res.Storage = &st
	}
	off := opt.Offset
	if off < 0 {
		off = 0
	}
	if off > len(entries) {
		off = len(entries)
	}
	end := len(entries)
	if opt.Limit > 0 && off+opt.Limit < end {
		end = off + opt.Limit
	}
	res.Offset = off
	res.Entries = entries[off:end]
	return res, nil
}

func (s *Service) virtualChildren(vp string) []Entry {
	seen := map[string]bool{}
	var out []Entry
	for _, st := range s.St.All() {
		if !pathutil.HasPrefix(st.MountPath, vp) || strings.EqualFold(st.MountPath, vp) {
			continue
		}
		rest := strings.TrimPrefix(pathutil.Rel(st.MountPath, vp), "/")
		seg := strings.SplitN(rest, "/", 2)[0]
		key := strings.ToLower(seg)
		if seen[key] {
			continue
		}
		seen[key] = true
		child := path.Join(vp, seg)
		e := Entry{Name: seg, Path: child, Type: TypeVirtual}
		if strings.EqualFold(child, st.MountPath) {
			e.Type = TypeMount
			e.StorageID = st.ID
			e.Status = st.Status
		}
		out = append(out, e)
	}
	return out
}

// Stat returns one entry. Files use the parent's cached listing when possible.
func (s *Service) Stat(ctx context.Context, vpath string) (Entry, Target, error) {
	t, err := s.Resolve(vpath)
	if err != nil {
		return Entry{}, t, err
	}
	name := path.Base(t.VPath)
	if t.Virtual {
		return Entry{Name: name, Path: t.VPath, Type: TypeVirtual}, t, nil
	}
	if strings.EqualFold(t.VPath, t.Storage.MountPath) {
		return Entry{Name: name, Path: t.VPath, Type: TypeMount, StorageID: t.Storage.ID, Status: t.Storage.Status}, t, nil
	}
	parent := pathutil.Parent(t.DbxPath)
	if ms, err := s.listDropbox(ctx, t.Storage, parent); err == nil {
		for _, m := range ms {
			if strings.EqualFold(m.Name, path.Base(t.DbxPath)) {
				return toEntry(m, t), t, nil
			}
		}
		return Entry{}, t, ErrNotFound
	} else if !errors.Is(err, ErrNotFound) {
		return Entry{}, t, err
	}
	return Entry{}, t, ErrNotFound
}

func toEntry(m dropbox.Metadata, t Target) Entry {
	e := Entry{Name: m.Name, Path: path.Join(pathutil.Parent(t.VPath), m.Name), ID: m.ID, StorageID: t.Storage.ID}
	if m.IsDir() {
		e.Type = TypeFolder
	} else {
		e.Type = TypeFile
		sz := m.Size
		e.Size = &sz
		e.Modified = m.ServerModified
		e.Rev = m.Rev
	}
	return e
}

// SiblingFiles returns the files in the same folder as vpath, sorted, for prev/next navigation.
func (s *Service) SiblingFiles(ctx context.Context, vpath, sortBy, order string) ([]Entry, error) {
	r, err := s.List(ctx, pathutil.Parent(vpath), ListOptions{Sort: sortBy, Order: order})
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range r.Entries {
		if e.Type == TypeFile {
			out = append(out, e)
		}
	}
	return out, nil
}

// InvalidatePath drops caches affected by a change at dbx (a file or folder) in a storage:
// the listing of its parent and of itself, folder sizes of itself, its descendants and every
// ancestor, and temporary links under it.
func (s *Service) InvalidatePath(id int64, dbx string) {
	lower := strings.ToLower(dbx)
	s.Cache.Delete(listKey(id, dbx))
	s.Cache.DeletePrefix(listKey(id, lower) + "/")
	s.Cache.Delete(FolderSizeKey(id, dbx))
	s.Cache.DeletePrefix(FolderSizeKey(id, lower) + "/")
	s.Cache.Delete(TempLinkKey(id, dbx))
	s.Cache.DeletePrefix(TempLinkKey(id, lower) + "/")
	for p := pathutil.Parent(dbx); ; p = pathutil.Parent(p) {
		s.Cache.Delete(FolderSizeKey(id, p))
		if p == pathutil.Parent(dbx) {
			s.Cache.Delete(listKey(id, p))
		}
		if p == "/" {
			break
		}
	}
}

// FlushStorage drops every cache entry for a storage.
func (s *Service) FlushStorage(id int64) { s.Cache.DeletePrefix(StoragePrefix(id)) }

// SortEntries sorts in place. Folders always come first. Folders without a known size sort
// by name after sized folders when sorting by size.
func SortEntries(es []Entry, by, order string) {
	desc := order == "desc"
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.IsDir() != b.IsDir() {
			return a.IsDir()
		}
		switch by {
		case "size":
			if a.Size == nil || b.Size == nil {
				if (a.Size == nil) != (b.Size == nil) {
					return a.Size != nil
				}
				return NaturalLess(a.Name, b.Name)
			}
			if *a.Size != *b.Size {
				if desc {
					return *a.Size > *b.Size
				}
				return *a.Size < *b.Size
			}
		case "modified":
			if a.Modified != b.Modified {
				if a.Modified == "" || b.Modified == "" {
					return a.Modified != ""
				}
				if desc {
					return a.Modified > b.Modified
				}
				return a.Modified < b.Modified
			}
		default:
			if !strings.EqualFold(a.Name, b.Name) {
				if desc {
					return NaturalLess(b.Name, a.Name)
				}
				return NaturalLess(a.Name, b.Name)
			}
		}
		return NaturalLess(a.Name, b.Name)
	})
}

// NaturalLess compares case-insensitively, treating digit runs as numbers ("ep2" < "ep10").
func NaturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		ca, cb := ra[i], rb[j]
		if isDigit(ca) && isDigit(cb) {
			si := i
			for i < len(ra) && isDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && isDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
