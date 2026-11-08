package zipstream

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/cache"
	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
)

// newTestService wires a real vfs service onto the in-memory fake Dropbox with one storage at /m.
func newTestService(t *testing.T, appKey string) (*vfs.Service, *fake.Server) {
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
	return vfs.New(sm, c), fs
}

// Selecting files whose names collide with a name the suffix rule already produced must still
// yield one unique entry per file, with each file's own bytes, exactly as unzip expects.
func TestExpandNamesStayUniqueAndKeepContent(t *testing.T) {
	svc, fs := newTestService(t, "zip-collide")
	acct := fs.Account("zip-collide")
	content := map[string]string{
		"/three/x (2).txt": "three",
		"/one/x.txt":       "one",
		"/two/x.txt":       "two",
		"/four/Report.pdf": "four",
		"/five/report.PDF": "five",
	}
	mod := time.Unix(1700000000, 0).UTC()
	for p, body := range content {
		acct.Put(p, []byte(body), mod)
	}

	paths := []string{
		"/m/three/x (2).txt",
		"/m/one/x.txt",
		"/m/two/x.txt",
		"/m/four/Report.pdf",
		"/m/five/report.PDF",
	}
	ctx := context.Background()
	files, err := Expand(ctx, svc, paths, 100)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(files) != len(paths) {
		t.Fatalf("expand returned %d entries for %d files", len(files), len(paths))
	}
	seen := map[string]string{}
	for _, f := range files {
		key := strings.ToLower(f.ZipName)
		if prev, dup := seen[key]; dup {
			t.Fatalf("entries %q and %q collide case-insensitively", prev, f.ZipName)
		}
		seen[key] = f.ZipName
	}

	var buf bytes.Buffer
	open := func(_ context.Context, _ int64, dbxPath string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content[dbxPath])), nil
	}
	if err := Write(ctx, &buf, files, open); err != nil {
		t.Fatalf("write: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if _, dup := got[f.Name]; dup {
			t.Fatalf("zip repeats entry %q, so unzip overwrites one file", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[f.Name] = string(body)
	}
	if len(got) != len(paths) {
		t.Fatalf("zip has %d entries, want %d", len(got), len(paths))
	}
	for _, f := range files {
		if got[f.ZipName] != content[f.DbxPath] {
			t.Fatalf("entry %q holds %q, want the bytes of %q", f.ZipName, got[f.ZipName], f.DbxPath)
		}
	}
}
