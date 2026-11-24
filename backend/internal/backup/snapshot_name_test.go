package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSnapshotNameRejectsTrailingTail pins that only a name this build writes counts as a snapshot.
// PruneSnapshots deletes what it counts, so a file an admin kept next to the app's snapshots, such
// as pre-restore-20261101-030000-2_keep.json, must never be pruned.
func TestSnapshotNameRejectsTrailingTail(t *testing.T) {
	foreign := snapshotPrefix + "20261101-030000" + "-2_keep" + snapshotSuffix
	if part, ok := parseSnapshotName(foreign); ok {
		t.Fatalf("parseSnapshotName(%q) = %+v, want not a snapshot", foreign, part)
	}
	for _, name := range []string{
		snapshotPrefix + "20261101-030000" + "-2x" + snapshotSuffix,
		snapshotPrefix + "20261101-030000" + "-02" + snapshotSuffix,
		snapshotPrefix + "20261101-030000" + "-+2" + snapshotSuffix,
		snapshotPrefix + "20261101-030000" + "-2-3" + snapshotSuffix,
		snapshotPrefix + "20261101-030000" + " -2" + snapshotSuffix,
	} {
		if part, ok := parseSnapshotName(name); ok {
			t.Fatalf("parseSnapshotName(%q) = %+v, want not a snapshot", name, part)
		}
	}
	for _, name := range []string{
		snapshotPrefix + "20261101-030000" + snapshotSuffix,
		snapshotPrefix + "20261101-030000-2" + snapshotSuffix,
		snapshotPrefix + "20261101-030000-17" + snapshotSuffix,
	} {
		if _, ok := parseSnapshotName(name); !ok {
			t.Fatalf("parseSnapshotName(%q) rejected a name this build writes", name)
		}
	}
}

// TestPruneSnapshotsKeepsFilesThisBuildDidNotWrite runs the prune that a restore triggers: with more
// snapshots than the keep limit, the file the admin kept beside them must survive.
func TestPruneSnapshotsKeepsFilesThisBuildDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Six snapshots of this build, oldest first, and one kept file stamped like the oldest.
	var written []string
	for i := 0; i < 6; i++ {
		name := snapshotName(base.Add(time.Duration(i)*time.Hour), 1)
		write(name)
		written = append(written, name)
	}
	foreign := snapshotPrefix + base.Format(snapshotTimeForm) + "-2_keep" + snapshotSuffix
	write(foreign)

	removed, failed := PruneSnapshots(dir, SnapshotMaxKeep)
	if len(failed) > 0 {
		t.Fatalf("prune failed for %v", failed)
	}
	for _, name := range removed {
		if name == foreign {
			t.Fatalf("prune removed %q, a file this build did not write", foreign)
		}
	}
	if len(removed) != 1 || removed[0] != written[0] {
		t.Fatalf("removed = %v, want only the oldest snapshot %q", removed, written[0])
	}
	if _, err := os.Stat(filepath.Join(dir, foreign)); err != nil {
		t.Fatalf("kept file is gone: %v", err)
	}
	parts, err := SnapshotFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		if part.name == foreign {
			t.Fatalf("SnapshotFiles listed %q as a snapshot", foreign)
		}
	}
}
