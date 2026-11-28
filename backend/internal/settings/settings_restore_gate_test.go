package settings

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// newStore opens a real SQLite database with the settings schema.
func newStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	// Persist every default so the database rows exist for the comparisons below.
	if err := st.Update(st.Get()); err != nil {
		t.Fatalf("persist defaults: %v", err)
	}
	return st, d
}

func storedPerPage(t *testing.T, d *sql.DB) int {
	t.Helper()
	var v int
	if err := d.QueryRow(`SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'per_page'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A writer that read the settings at revision N must not store them after a restore bumped the
// revision. This is the stale read-modify-write the settings PUT path has to refuse.
func TestUpdateAtRevisionRejectsRevisionFromBeforeRestore(t *testing.T) {
	st, d := newStore(t)
	before := st.Get()
	rev := st.RestoreVersion()

	st.BeginRestore()
	st.EndRestore()

	after := st.RestoreVersion()
	if after == rev {
		t.Fatalf("RestoreVersion did not change across a restore: %d", after)
	}
	stale := before
	stale.PerPage = 99
	err := st.UpdateAtRevision(rev, stale)
	if !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("UpdateAtRevision with the pre-restore revision err = %v, want ErrRestoreInProgress", err)
	}
	if got := st.Get().PerPage; got != before.PerPage {
		t.Fatalf("in-memory per_page = %d after refused write, want %d", got, before.PerPage)
	}
	if got := storedPerPage(t, d); got != before.PerPage {
		t.Fatalf("database per_page = %d after refused write, want %d", got, before.PerPage)
	}

	// The same value is accepted against the current revision.
	if err := st.UpdateAtRevision(after, stale); err != nil {
		t.Fatalf("UpdateAtRevision with the current revision: %v", err)
	}
	if got := st.Get().PerPage; got != 99 {
		t.Fatalf("in-memory per_page = %d after accepted write, want 99", got)
	}
	if got := storedPerPage(t, d); got != 99 {
		t.Fatalf("database per_page = %d after accepted write, want 99", got)
	}
}

// A PUT that captured the revision after BeginRestore bumped it still read the pre-restore
// snapshot before the restore published its values. EndRestore must advance the version too, so
// that revision is stale once the gate opens and the stale write is refused.
func TestUpdateAtRevisionRejectsRevisionCapturedDuringRestore(t *testing.T) {
	st, d := newStore(t)
	before := st.Get()

	st.BeginRestore()
	// The restore holds the gate from here on. A concurrent PUT captures the bumped revision and
	// reads the still-pre-restore settings, then stalls before it writes.
	duringRestore := st.RestoreVersion()
	stale := before
	stale.PerPage = 99

	restored := before
	restored.PerPage = 77
	// The restore path stores the restored rows in its own transaction before publishing them.
	if _, err := d.Exec(`UPDATE settings SET value = ? WHERE key = 'per_page'`, strconv.Itoa(restored.PerPage)); err != nil {
		st.EndRestore()
		t.Fatalf("store restored row: %v", err)
	}
	st.ReplaceInMemory(restored)
	st.EndRestore()

	err := st.UpdateAtRevision(duringRestore, stale)
	if !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("UpdateAtRevision with a revision captured during the restore err = %v, want ErrRestoreInProgress", err)
	}
	if got := st.Get().PerPage; got != restored.PerPage {
		t.Fatalf("in-memory per_page = %d after refused write, want %d", got, restored.PerPage)
	}
	if got := storedPerPage(t, d); got != restored.PerPage {
		t.Fatalf("database per_page = %d after refused write, want %d", got, restored.PerPage)
	}
}

// While a restore holds the gate, an update must return ErrRestoreInProgress instead of waiting
// for the restore to finish and then writing a value the restore already replaced.
func TestUpdateRefusedWhileRestoreIsHeld(t *testing.T) {
	st, d := newStore(t)
	before := st.Get()
	st.BeginRestore()
	defer st.EndRestore()

	done := make(chan error, 1)
	go func() {
		n := before
		n.PerPage = 99
		done <- st.Update(n)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestoreInProgress) {
			t.Fatalf("Update during restore err = %v, want ErrRestoreInProgress", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Update blocked on the restore gate instead of failing with ErrRestoreInProgress")
	}
	if got := storedPerPage(t, d); got != before.PerPage {
		t.Fatalf("database per_page = %d during restore, want %d", got, before.PerPage)
	}
}

// A restore must not publish restored settings while an earlier update's listener is still
// running: that listener applies the pre-restore runtime limits. BeginRestore waits for the
// notification because the notification runs inside the same write gate as the database write.
func TestBeginRestoreWaitsForPendingListener(t *testing.T) {
	st, _ := newStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	st.OnChange(func(old, n Settings) {
		if n.PerPage == 66 {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
	})

	n := st.Get()
	n.PerPage = 66
	updateDone := make(chan error, 1)
	go func() { updateDone <- st.Update(n) }()
	<-entered

	restoreStarted := make(chan struct{})
	restoreDone := make(chan struct{})
	go func() {
		close(restoreStarted)
		st.BeginRestore()
		close(restoreDone)
	}()
	<-restoreStarted
	// Wait until BeginRestore has raised its flag. The listener is still holding writeMu, so a
	// correct BeginRestore cannot have finished by this point.
	deadline := time.Now().Add(time.Second)
	for !st.restoring.Load() {
		if time.Now().After(deadline) {
			t.Fatal("BeginRestore did not set its restoring flag")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-restoreDone:
		close(release)
		st.EndRestore()
		t.Fatal("BeginRestore returned while its update's listener was still running")
	default:
	}

	close(release)
	if err := <-updateDone; err != nil {
		t.Fatalf("update: %v", err)
	}
	select {
	case <-restoreDone:
		st.EndRestore()
	case <-time.After(2 * time.Second):
		t.Fatal("BeginRestore did not acquire the gate after the listener finished")
	}
}

// Concurrent updates must leave the database and the in-memory snapshot on the same value. The
// store's single write mutex covers both the transaction and the memory swap.
func TestConcurrentUpdatesKeepDatabaseAndMemoryInSync(t *testing.T) {
	st, d := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := st.Get()
			n.PerPage = 100 + i
			if err := st.Update(n); err != nil {
				t.Errorf("Update(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if got, want := storedPerPage(t, d), st.Get().PerPage; got != want {
		t.Fatalf("database per_page = %d, in-memory per_page = %d; the write and the swap were not serialized", got, want)
	}
}
