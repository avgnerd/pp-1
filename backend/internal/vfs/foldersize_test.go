package vfs

import (
	"context"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/cache"
	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/limiter"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// newTestService wires a real service onto the in-memory fake Dropbox with one storage at /m.
func newTestService(t *testing.T, appKey string) (*Service, *fake.Server, *cache.Cache) {
	t.Helper()
	fs, fts := fake.NewServer()
	t.Cleanup(fts.Close)
	cfg, err := config.Load(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DropboxAPIBase, cfg.DropboxContentBase = fts.URL, fts.URL
	cfg.DropboxAuthorize, cfg.DropboxTokenURL = fts.URL+"/oauth2/authorize", fts.URL+"/oauth2/token"
	d, err := db.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	box, err := secret.New(cfg.Key)
	if err != nil {
		t.Fatal(err)
	}
	sm, err := storage.NewManager(d, box, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(10_000, 64<<20, time.Minute)
	t.Cleanup(c.Stop)
	start, err := sm.Create(storage.Input{MountPath: "/m", AppKey: appKey, AppSecret: "s", AuthMode: storage.AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm.CompleteWithCode(context.Background(), start.Storage.ID, "code-"+appKey); err != nil {
		t.Fatal(err)
	}
	return New(sm, c), fs, c
}

// The second read of the same folder must report the same bytes, files and folders as the first
// one, served from the cache without another Dropbox listing, and List must show that size.
func TestFolderSizeCacheKeepsCounts(t *testing.T) {
	svc, fs, _ := newTestService(t, "size-cache")
	acct := fs.Account("size-cache")
	mod := time.Unix(1700000000, 0).UTC()
	acct.Put("/big/a.txt", []byte("abc"), mod)
	acct.Put("/big/sub1/b.txt", []byte("defg"), mod)
	acct.Put("/big/sub2/c.txt", []byte("0123456789"), mod)

	ctx := context.Background()
	lim := limiter.New(3)
	progress := func(SizeProgress) {}

	first, err := svc.FolderSize(ctx, "/m/big", lim, progress)
	if err != nil {
		t.Fatalf("first folder size: %v", err)
	}
	if first.Bytes != 17 || first.Files != 3 || first.Folders != 2 || !first.Done {
		t.Fatalf("first folder size = %+v, want 17 bytes / 3 files / 2 folders", first)
	}
	if entry := listEntry(t, svc, ctx, "/m", "big"); entry.Size == nil || *entry.Size != 17 {
		t.Fatalf("List size = %v, want 17 from the folder-size cache", entry.Size)
	}

	before := listCalls(fs)
	second, err := svc.FolderSize(ctx, "/m/big", lim, progress)
	if err != nil {
		t.Fatalf("second folder size: %v", err)
	}
	if second.Bytes != first.Bytes || second.Files != first.Files || second.Folders != first.Folders || second.Done != first.Done {
		t.Fatalf("second folder size = %+v, want the cached %+v", second, first)
	}
	if calls := listCalls(fs) - before; calls != 0 {
		t.Fatalf("second read listed %d pages; the cached value should not rescan", calls)
	}
	if entry := listEntry(t, svc, ctx, "/m", "big"); entry.Size == nil || *entry.Size != 17 {
		t.Fatalf("List size after the cached read = %v, want 17", entry.Size)
	}
}

func listCalls(fs *fake.Server) int64 {
	return fs.CallCount("files/list_folder") + fs.CallCount("files/list_folder/continue")
}

func listEntry(t *testing.T, svc *Service, ctx context.Context, vpath, name string) Entry {
	t.Helper()
	res, err := svc.List(ctx, vpath, ListOptions{})
	if err != nil {
		t.Fatalf("list %s: %v", vpath, err)
	}
	for _, e := range res.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("%s has no entry %q", vpath, name)
	return Entry{}
}
