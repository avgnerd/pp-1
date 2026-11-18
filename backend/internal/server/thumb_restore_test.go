package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/backup"
)

// A restore keeps the storage ids from the backup, so an id can name a different Dropbox account
// afterwards. The disk thumbnail cache keys entries by storage id and path; a request without rev
// has no content identity, so it must not be answered from the pre-restore account's cache.
func TestThumbnailWithoutRevIsNotServedFromAnotherAccountAfterRestore(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/p")
	e.fake.Account("app1").Put("/a.jpg", []byte("old"), time.Now())

	thumbURL := e.ts.URL + "/api/fs/thumb?path=/p/a.jpg&size=w256h256"
	resp, err := e.c.Get(thumbURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail warm-up status = %d, want 200", resp.StatusCode)
	}

	// Connect a second account under another mount, then restore its row onto the first storage id
	// and mount: the account behind storage 1 changes, the cache key does not.
	e.connect("app2", "/q")
	env, raw, exp := e.exportBackup(e.c, map[string]any{"confirm_unencrypted": true})
	if exp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", exp.StatusCode, truncate(raw))
	}
	restored := make([]backup.StorageRecord, 0, 1)
	for _, rec := range env.Payload.Storages {
		if rec.MountPath != "/q" {
			continue
		}
		rec.ID = 1
		rec.Name = "Restored"
		rec.MountPath = "/p"
		restored = append(restored, rec)
	}
	if len(restored) != 1 {
		t.Fatalf("export has no storage at /q: %+v", env.Payload.Storages)
	}
	env.Payload.Storages = restored
	file, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	e.previewAndApply(e.c, file, "")
	e.login()

	resp, err = e.c.Get(thumbURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("thumbnail without rev after restore = %d, want 404: the restored account has no such file", resp.StatusCode)
	}
}
