package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertOpenPathPragmas(t *testing.T, path string) {
	t.Helper()
	d, err := OpenPath(path)
	if err != nil {
		t.Fatalf("OpenPath(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close %q: %v", path, err)
		}
	})

	var foreignKeys int
	if err := d.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1", foreignKeys)
	}

	var journalMode string
	if err := d.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("PRAGMA journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := d.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 10000 {
		t.Errorf("PRAGMA busy_timeout = %d, want 10000", busyTimeout)
	}

	var storages int
	if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'storages'`).Scan(&storages); err != nil {
		t.Fatalf("read migrated schema: %v", err)
	}
	if storages != 1 {
		t.Errorf("storages table count = %d, want 1 (migrations did not run)", storages)
	}
}

// A data directory is a filesystem path, not a URI component. '#' must not start a fragment,
// '?' must not start a query, and '%' must not start an escape sequence.
func TestOpenPathEncodesURIMetacharacters(t *testing.T) {
	cases := []struct {
		name    string
		dirName string
	}{
		{"hash", "#"},
		{"percent_escape", "%20"},
		{"hash_and_percent_escape", "#%20"},
		{"hash_question_percent", "#?%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tc.dirName)
			if err := os.Mkdir(dir, 0o700); err != nil {
				if strings.ContainsRune(tc.dirName, '?') {
					t.Skipf("filesystem refuses '?' in a directory name: %v", err)
				}
				t.Fatalf("create data directory %q: %v", dir, err)
			}
			wantPath := filepath.Join(dir, "app.db")
			assertOpenPathPragmas(t, wantPath)
			if _, err := os.Stat(wantPath); err != nil {
				t.Fatalf("database was not created at %q: %v", wantPath, err)
			}
		})
	}
}

// A memory database has no file and keeps SQLite's memory journal mode, but OpenPath must still
// recognize the DSN and apply the schema migrations.
func TestOpenPathMemory(t *testing.T) {
	d, err := OpenPath(":memory:")
	if err != nil {
		t.Fatalf("OpenPath(%q): %v", ":memory:", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close :memory:: %v", err)
		}
	})

	var storages int
	if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'storages'`).Scan(&storages); err != nil {
		t.Fatalf("read migrated schema: %v", err)
	}
	if storages != 1 {
		t.Errorf("storages table count = %d, want 1 (migrations did not run)", storages)
	}
}
