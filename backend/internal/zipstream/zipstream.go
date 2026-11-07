// Package zipstream writes a ZIP of Dropbox files straight to the response, without temp files.
//
// Entries are stored (no compression) because most large files (video, images) are already
// compressed and store keeps CPU near zero. archive/zip writes data descriptors, switches to
// ZIP64 automatically past 4 GiB, and sets the UTF-8 flag for non-ASCII names.
package zipstream

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
)

// File is one file to put in the archive.
type File struct {
	StorageID int64
	DbxPath   string
	ZipName   string
	Size      int64
	Modified  time.Time
}

// Opener opens a file's content.
type Opener func(ctx context.Context, storageID int64, dbxPath string) (io.ReadCloser, error)

// Expand turns selected virtual paths into a flat file list. Folders are listed recursively;
// mounts include the whole storage root; virtual folders include every mount under them.
// maxFiles bounds the expansion.
func Expand(ctx context.Context, svc *vfs.Service, vpaths []string, maxFiles int) ([]File, error) {
	var out []File
	used := map[string]int{}
	add := func(f File) error {
		if len(out) >= maxFiles {
			return fmt.Errorf("파일이 %d개를 넘어 한 번에 압축할 수 없습니다. 선택을 나누어 받으세요", maxFiles)
		}
		key := strings.ToLower(f.ZipName)
		if n := used[key]; n > 0 {
			ext := path.Ext(f.ZipName)
			f.ZipName = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(f.ZipName, ext), n+1, ext)
		}
		used[key]++
		out = append(out, f)
		return nil
	}
	var walkStorage func(st storage.Storage, dbx, prefix string) error
	walkStorage = func(st storage.Storage, dbx, prefix string) error {
		c, err := svc.St.Client(st.ID)
		if err != nil {
			return fmt.Errorf("%s: %w", st.MountPath, err)
		}
		base := strings.ToLower(dbx)
		if base == "/" {
			base = ""
		}
		return c.ListFolder(ctx, dbx, true, func(es []dropbox.Metadata) error {
			for _, e := range es {
				if e.Tag != "file" {
					continue
				}
				rel := e.PathDisplay
				if len(e.PathLower) >= len(base) && strings.HasPrefix(e.PathLower, base) {
					rel = e.PathDisplay[len(base):]
				}
				if err := add(File{StorageID: st.ID, DbxPath: e.PathLower, ZipName: path.Join(prefix, strings.TrimPrefix(rel, "/")),
					Size: e.Size, Modified: e.Modified()}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for _, vp := range vpaths {
		ent, t, err := svc.Stat(ctx, vp)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", vp, err)
		}
		switch ent.Type {
		case vfs.TypeFile:
			if err := add(File{StorageID: t.Storage.ID, DbxPath: t.DbxPath, ZipName: ent.Name, Size: *ent.Size, Modified: parseTime(ent.Modified)}); err != nil {
				return nil, err
			}
		case vfs.TypeFolder, vfs.TypeMount:
			if err := walkStorage(t.Storage, t.DbxPath, ent.Name); err != nil {
				return nil, err
			}
		case vfs.TypeVirtual:
			for _, st := range svc.St.All() {
				if st.Status == storage.StatusPending || !pathutil.HasPrefix(st.MountPath, t.VPath) {
					continue
				}
				prefix := path.Join(ent.Name, strings.TrimPrefix(pathutil.Rel(st.MountPath, t.VPath), "/"))
				if t.VPath == "/" {
					prefix = strings.TrimPrefix(st.MountPath, "/")
				}
				if err := walkStorage(st, st.RootPath, prefix); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("압축할 파일이 없습니다")
	}
	return out, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// Write streams files into a ZIP on w. It stops at the first error or when ctx ends.
func Write(ctx context.Context, w io.Writer, files []File, open Opener) error {
	zw := zip.NewWriter(w)
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		h := &zip.FileHeader{Name: f.ZipName, Method: zip.Store, Modified: f.Modified}
		if f.Modified.IsZero() {
			h.Modified = time.Now()
		}
		h.SetMode(0o644)
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		rc, err := open(ctx, f.StorageID, f.DbxPath)
		if err != nil {
			return fmt.Errorf("%s: %w", f.ZipName, err)
		}
		_, err = io.CopyBuffer(fw, rc, *bp)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

// TotalSize sums file sizes (used for an estimate shown to the user).
func TotalSize(files []File) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}
