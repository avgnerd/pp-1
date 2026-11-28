package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestStartAuthAfterDeletedRowReturnsNotFound covers the authorization start for a row that a
// concurrent Delete removed after Reauth's snapshot read. startAuth must report the missing row
// before it writes an oauth_states row, instead of letting the foreign key reject the insert.
func TestStartAuthAfterDeletedRowReturnsNotFound(t *testing.T) {
	m, _, d := newManager(t)
	start, err := m.Create(Input{MountPath: "/s", AppKey: "app1", AppSecret: "s", AuthMode: AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	id := start.Storage.ID
	if _, err := d.Exec(`DELETE FROM storages WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(id); ok {
		t.Fatal("the deleted storage is still published")
	}

	if _, err := m.startAuth(id, AuthCode, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("startAuth after the row was deleted = %v, want ErrNotFound", err)
	}
	var states int
	if err := d.QueryRow(`SELECT COUNT(*) FROM oauth_states WHERE storage_id = ?`, id).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != 0 {
		t.Fatalf("startAuth inserted %d oauth_states rows for a deleted storage", states)
	}
}

// TestStartAuthReportsStorageDeletedBeforeOauthStateInsert makes Delete's committed row removal
// land after startAuth's snapshot check and before its oauth_states insert, so the insert hits
// the foreign key. startAuth must ask the database for the row and report ErrNotFound instead of
// returning that FK error.
//
// startAuth first deletes the storage's existing oauth_states rows. The trigger removes the parent
// during that successful DELETE, so the removal commits; the following INSERT then fails the
// foreign key and the post-failure lookup sees no row. A BEFORE INSERT trigger cannot model this:
// SQLite rolls its DELETE back with the failed INSERT, leaving the parent for the lookup.
func TestStartAuthReportsStorageDeletedBeforeOauthStateInsert(t *testing.T) {
	m, _, d := newManager(t)
	start, err := m.Create(Input{MountPath: "/s", AppKey: "app1", AppSecret: "s", AuthMode: AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	id := start.Storage.ID
	if _, ok := m.Get(id); !ok {
		t.Fatal("Create did not publish the storage before startAuth")
	}
	var before int
	if err := d.QueryRow(`SELECT COUNT(*) FROM oauth_states WHERE storage_id = ?`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("Create left no oauth_states row for startAuth's cleanup DELETE to remove")
	}
	// Create's startAuth inserted the oauth_states row this cleanup DELETE will remove.
	if _, err := d.Exec(`CREATE TRIGGER delete_storage_on_state_delete AFTER DELETE ON oauth_states
		BEGIN DELETE FROM storages WHERE id = OLD.storage_id; END`); err != nil {
		t.Fatal(err)
	}

	if _, err := m.startAuth(id, AuthCode, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("startAuth when the storage row was deleted before the insert = %v, want ErrNotFound", err)
	}
	var states int
	if err := d.QueryRow(`SELECT COUNT(*) FROM oauth_states WHERE storage_id = ?`, id).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != 0 {
		t.Fatalf("startAuth left %d oauth_states rows for a deleted storage", states)
	}
}

// TestUpdateReportsRowDeletedAfterWrite reproduces the interleaving the trigger makes
// deterministic: the UPDATE changes one row and the trigger deletes that row, so the
// row-affected check passes and only the final snapshot read can observe the deletion.
func TestUpdateReportsRowDeletedAfterWrite(t *testing.T) {
	m, _, d := newManager(t)
	s := connectActiveStorage(t, m)
	installDeleteAfterUpdateTrigger(t, d)

	got, err := m.Update(s.ID, Input{Name: "gone", MountPath: "/gone", RootPath: "/", CacheTTLMin: 30})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update after the row disappeared = (%+v, %v), want ErrNotFound", got, err)
	}
	if _, ok := m.Get(s.ID); ok {
		t.Fatal("Update left the deleted storage in the published snapshot")
	}
}

// TestCompleteReportsRowDeletedAfterCredentialWrite is the same interleaving for an authorization
// completion: the credential write succeeds, the trigger removes the row, and the final snapshot
// read finds nothing. The completion must report ErrNotFound, not return a zero Storage with nil.
func TestCompleteReportsRowDeletedAfterCredentialWrite(t *testing.T) {
	m, _, d := newManager(t)
	start, err := m.Create(Input{MountPath: "/s", AppKey: "app1", AppSecret: "s", AuthMode: AuthCode})
	if err != nil {
		t.Fatal(err)
	}
	id := start.Storage.ID
	installDeleteAfterUpdateTrigger(t, d)

	got, err := m.CompleteWithCode(context.Background(), id, "code-app1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CompleteWithCode after the row disappeared = (%+v, %v), want ErrNotFound", got, err)
	}
	if _, ok := m.Get(id); ok {
		t.Fatal("the deleted storage is still published")
	}
}

// installDeleteAfterUpdateTrigger deletes the updated row from a trigger. SQLite still reports
// one row affected by the UPDATE, so the code proceeds past its row-affected check exactly as it
// would when a concurrent Delete wins the race after the write.
func installDeleteAfterUpdateTrigger(t *testing.T, d *sql.DB) {
	t.Helper()
	if _, err := d.Exec(`CREATE TRIGGER delete_storage_after_update AFTER UPDATE ON storages
		BEGIN DELETE FROM storages WHERE id = NEW.id; END`); err != nil {
		t.Fatal(err)
	}
}
