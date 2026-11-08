package backup

import (
	"context"
	"testing"
)

// A storage may be created without a name: storage.Create stores the empty string and the admin
// form labels the field optional. A backup the instance produced therefore has to preview and
// apply again, so the round trip must accept the same values the live rows hold.
func TestBackupOfUnnamedStorageRestores(t *testing.T) {
	src := newFixture(t)
	src.mustExec(`DELETE FROM storages`)
	src.seedStorage(1, "", "/unnamed", "active", "app-key-1", "app-secret-1", "refresh-1")

	env := src.export(ModeNone, "", true)
	_, payload := openFile(t, env, "", src.key)
	if err := ValidatePayload(payload); err != nil {
		t.Fatalf("the instance's own backup must validate: %v", err)
	}

	target := newFixtureWithKey(t, rawKey(21))
	if err := ApplyTx(context.Background(), target.db, payload, ApplyOptions{Box: target.box}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var name, mount string
	if err := target.db.QueryRow(`SELECT name, mount_path FROM storages WHERE id = 1`).Scan(&name, &mount); err != nil {
		t.Fatalf("read restored storage: %v", err)
	}
	if name != "" || mount != "/unnamed" {
		t.Fatalf("restored storage name %q mount %q, want the unnamed /unnamed storage", name, mount)
	}
}
