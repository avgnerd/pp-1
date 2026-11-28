package search_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
	"github.com/dvfs/dropbox-vfs-manager/internal/limiter"
	"github.com/dvfs/dropbox-vfs-manager/internal/search"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
)

const searchWithOtherResponse = `{
  "matches": [
    {"metadata": {".tag": "other"}},
    {"metadata": {".tag": "metadata", "metadata": {
      ".tag": "deleted", "name": "gone.txt", "id": "id:gone", "path_display": "/gone.txt"
    }}},
    {"metadata": {".tag": "metadata", "metadata": {
      ".tag": "file", "name": "hello.txt", "id": "id:hello",
      "path_lower": "/hello.txt", "path_display": "/hello.txt",
      "size": 5, "server_modified": "2024-01-02T03:04:05Z", "rev": "rev-hello"
    }}}
  ],
  "has_more": false
}`

// Search_v2 result entries are union values. An outer "other" has no file metadata, and a deleted
// inner tag is not a live file either. Only the real file may reach the search stream.
func TestRunSkipsNonFileMatches(t *testing.T) {
	fs, fakeServer := fake.NewServer()
	t.Cleanup(fakeServer.Close)
	fakeHandler := fs.Handler()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2/files/search_v2" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, searchWithOtherResponse)
			return
		}
		fakeHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)

	cfg := &config.Config{
		Listen:             ":8080",
		DropboxAPIBase:     api.URL,
		DropboxContentBase: api.URL,
		DropboxAuthorize:   fakeServer.URL + "/oauth2/authorize",
		DropboxTokenURL:    fakeServer.URL + "/oauth2/token",
	}
	d, err := db.OpenPath(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	box, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sm, err := storage.NewManager(d, box, cfg)
	if err != nil {
		t.Fatal(err)
	}
	start, err := sm.Create(storage.Input{
		MountPath: "/m", AppKey: "search-app", AppSecret: "s", AuthMode: storage.AuthCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm.CompleteWithCode(context.Background(), start.Storage.ID, "code-search-app"); err != nil {
		t.Fatal(err)
	}

	var entries []vfs.Entry
	var storageEvent search.Event
	err = search.Run(context.Background(), sm, limiter.New(2), search.Options{
		Query: "hello", PerStorageLimit: 10, Timeout: 5 * time.Second,
	}, func(ev search.Event) {
		switch ev.Type {
		case "results":
			entries = append(entries, ev.Entries...)
		case "storage":
			storageEvent = ev
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("search emitted %d entries, want only the live file: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.Name != "hello.txt" || got.Path != "/m/hello.txt" || got.Type != vfs.TypeFile {
		t.Fatalf("entry = %+v, want name hello.txt at /m/hello.txt (file)", got)
	}
	if got.Size == nil || *got.Size != 5 {
		t.Fatalf("entry size = %v, want 5", got.Size)
	}
	if got.ID != "id:hello" || got.Rev != "rev-hello" {
		t.Fatalf("entry id/rev = %q/%q, want id:hello/rev-hello", got.ID, got.Rev)
	}
	if storageEvent.Status != "done" || storageEvent.Count != 1 {
		t.Fatalf("storage event = %+v, want done with count 1", storageEvent)
	}
}
