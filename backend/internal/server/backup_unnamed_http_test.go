package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/backup"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// TestBackupHTTPRestoresUnnamedStorage drives the instance's own backup through the real admin HTTP
// routes. A storage may be created without a name, so its backup must preview, apply, and restore
// the same empty name and mount without the test renaming the row first.
func TestBackupHTTPRestoresUnnamedStorage(t *testing.T) {
	e := newEnv(t)
	e.login()

	// Create the storage the way the admin form does when the optional name stays empty.
	e.connect("unnamed-app", "/unnamed")
	status, body := e.getAs(e.c, "/api/admin/storages")
	if status != http.StatusOK {
		t.Fatalf("list storages before backup: %d: %s", status, truncate(body))
	}
	var before []storage.Storage
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatalf("storages before backup: %v (%s)", err, truncate(body))
	}
	if len(before) != 1 || before[0].Name != "" || before[0].MountPath != "/unnamed" {
		t.Fatalf("seed storages = %+v, want one unnamed storage at /unnamed", before)
	}

	env, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export the instance's own backup: %d: %s", resp.StatusCode, truncate(raw))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("export content type = %q", ct)
	}
	if env.Payload == nil || len(env.Payload.Storages) != 1 {
		t.Fatalf("export payload storages = %+v", env.Payload)
	}
	if got := env.Payload.Storages[0]; got.Name != "" || got.MountPath != "/unnamed" {
		t.Fatalf("exported storage = %+v, want the unnamed /unnamed row", got)
	}

	status, body = e.postMultipart(e.c, "/api/admin/restore/preview", raw, "")
	if status != http.StatusOK {
		t.Fatalf("preview the instance's own unnamed backup: %d: %s", status, truncate(body))
	}
	var preview backup.RestorePreview
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatalf("preview body: %v (%s)", err, truncate(body))
	}
	if len(preview.Storages) != 1 || preview.Storages[0].Name != "" || preview.Storages[0].MountPath != "/unnamed" {
		t.Fatalf("preview storages = %+v, want the unnamed /unnamed row", preview.Storages)
	}

	var applied struct {
		OK             bool     `json:"ok"`
		FailedStorages []string `json:"failed_storages"`
	}
	status, body = e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, &applied)
	if status != http.StatusOK {
		t.Fatalf("apply the unnamed backup: %d: %s", status, truncate(body))
	}
	if !applied.OK || len(applied.FailedStorages) != 0 {
		t.Fatalf("apply response = %+v", applied)
	}

	// The restore revokes the pre-restore session, so sign in again before reading the result.
	if status := e.loginAs(e.c, "admin", auth.DefaultPassword); status != http.StatusOK {
		t.Fatalf("login after restore: %d", status)
	}
	status, body = e.getAs(e.c, "/api/admin/storages")
	if status != http.StatusOK {
		t.Fatalf("list storages after restore: %d: %s", status, truncate(body))
	}
	var after []storage.Storage
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatalf("storages after restore: %v (%s)", err, truncate(body))
	}
	if len(after) != 1 || after[0].Name != "" || after[0].MountPath != "/unnamed" {
		t.Fatalf("restored storages = %+v, want the unnamed /unnamed storage", after)
	}
}
