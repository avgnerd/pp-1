package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// TestAuthorizedAtMillisecondRoundTripPreservesSecondFields walks the real export -> parse -> apply
// path. authorized_at is stored in milliseconds and must keep its fraction, while created_at and
// updated_at are stored in seconds and must keep their second values unchanged.
func TestAuthorizedAtMillisecondRoundTripPreservesSecondFields(t *testing.T) {
	f := newFixture(t)
	wantAuthorized := map[int64]int64{
		1: 1794041520104, // a millisecond fraction
		2: 1794040200000, // a whole second expressed in milliseconds
	}
	const createdAt, updatedAt = int64(1700000000), int64(1700000123)
	for id, ms := range wantAuthorized {
		f.mustExec(`UPDATE storages SET authorized_at = ?, created_at = ?, updated_at = ? WHERE id = ?`,
			ms, createdAt, updatedAt, id)
	}

	env, _, err := NewService(f.db, t.TempDir(), f.box, f.key).Export(context.Background(), ExportOptions{
		Mode: ModePassword, Password: "correct horse battery",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, payload := openFile(t, env, "correct horse battery", f.key)

	restored, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := ApplyTx(context.Background(), restored, payload, ApplyOptions{Box: f.box}); err != nil {
		t.Fatal(err)
	}

	for id, ms := range wantAuthorized {
		var gotAuthorized, gotCreated, gotUpdated int64
		if err := restored.QueryRow(`SELECT authorized_at, created_at, updated_at FROM storages WHERE id = ?`, id).
			Scan(&gotAuthorized, &gotCreated, &gotUpdated); err != nil {
			t.Fatal(err)
		}
		if gotAuthorized != ms {
			t.Fatalf("storage %d authorized_at round trip: restored %d, want %d", id, gotAuthorized, ms)
		}
		if gotCreated != createdAt || gotUpdated != updatedAt {
			t.Fatalf("storage %d second timestamps changed: created %d updated %d, want %d and %d",
				id, gotCreated, gotUpdated, createdAt, updatedAt)
		}
	}
}

// TestZeroAuthorizedAtRoundTripsAsZero keeps a pending storage pending: no authorization exports
// as the empty value and restores as 0, not as an invented timestamp.
func TestZeroAuthorizedAtRoundTripsAsZero(t *testing.T) {
	f := newFixture(t)
	f.mustExec(`UPDATE storages SET authorized_at = 0 WHERE id = 1`)
	env, _, err := NewService(f.db, t.TempDir(), f.box, f.key).Export(context.Background(), ExportOptions{
		Mode: ModePassword, Password: "correct horse battery",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, payload := openFile(t, env, "correct horse battery", f.key)
	for _, rec := range payload.Storages {
		if rec.ID == 1 && rec.AuthorizedAt != "" {
			t.Fatalf("zero authorized_at exported as %q", rec.AuthorizedAt)
		}
	}
	restored, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := ApplyTx(context.Background(), restored, payload, ApplyOptions{Box: f.box}); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := restored.QueryRow(`SELECT authorized_at FROM storages WHERE id = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("zero authorized_at restored as %d", got)
	}
}
