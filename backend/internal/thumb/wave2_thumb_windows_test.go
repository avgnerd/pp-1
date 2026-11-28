//go:build windows

package thumb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// lockWithoutSharing opens path with a share mode of 0, so Windows refuses every other open,
// including the one DeleteFileW needs. That is the deterministic form of a reader still holding
// the cache file while Clear or evict tries to remove it.
func lockWithoutSharing(t *testing.T, path string) syscall.Handle {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", path, err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("CreateFile(%q) with share mode 0: %v", path, err)
	}
	return h
}

// dirHasFile checks the directory entry instead of calling os.Stat, because a metadata open on
// Windows can itself be denied while the test's deny-sharing handle is open.
func dirHasFile(t *testing.T, dir, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

// Clear must not forget a thumbnail that Windows refused to delete. Dropping the accounting entry
// while the file stays on disk makes the cache untracked, so the size limit never sees it and a
// later Clear has nothing to retry.
func TestClearKeepsLockedThumbnailIndexed(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("clear-locked-thumbnail")
	name := "clear-locked.jpg"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(nil, dir, func() int64 { return 1 << 20 })
	if err != nil {
		t.Fatal(err)
	}
	if n, total := s.Usage(); n != 1 || total != int64(len(payload)) {
		t.Fatalf("index before Clear = %d files, %d bytes; want 1 file, %d bytes", n, total, len(payload))
	}

	h := lockWithoutSharing(t, path)
	closed := false
	closeHandle := func() {
		if closed {
			return
		}
		closed = true
		if err := syscall.CloseHandle(h); err != nil {
			t.Fatalf("CloseHandle: %v", err)
		}
	}
	defer closeHandle()

	s.Clear()

	if n, total := s.Usage(); n != 1 || total != int64(len(payload)) {
		t.Fatalf("Clear dropped accounting for a file it could not remove: Usage() = %d files, %d bytes; want 1 file, %d bytes", n, total, len(payload))
	}
	if !dirHasFile(t, dir, name) {
		t.Fatalf("%s disappeared after Clear even though the open handle denied deletion", name)
	}

	closeHandle()
	s.Clear()

	if n, total := s.Usage(); n != 0 || total != 0 {
		t.Fatalf("second Clear after the handle closed: Usage() = %d files, %d bytes; want 0 files, 0 bytes", n, total)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second Clear left the thumbnail on disk: %v", err)
	}
}

// evict has the same contract: a failed removal keeps the entry and its bytes, and a later evict
// retries once the handle is gone.
func TestEvictKeepsLockedThumbnailIndexed(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("e"), 4096)
	name := "evict-locked.jpg"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(nil, dir, func() int64 { return 1 << 20 })
	if err != nil {
		t.Fatal(err)
	}

	h := lockWithoutSharing(t, path)
	closed := false
	closeHandle := func() {
		if closed {
			return
		}
		closed = true
		if err := syscall.CloseHandle(h); err != nil {
			t.Fatalf("CloseHandle: %v", err)
		}
	}
	defer closeHandle()

	// Force the next evict to reclaim the whole cache, which makes it attempt this locked file.
	s.maxSize = func() int64 { return 1 }
	s.evict()

	if n, total := s.Usage(); n != 1 || total != int64(len(payload)) {
		t.Fatalf("evict dropped accounting for a file it could not remove: Usage() = %d files, %d bytes; want 1 file, %d bytes", n, total, len(payload))
	}
	if !dirHasFile(t, dir, name) {
		t.Fatalf("%s disappeared after evict even though the open handle denied deletion", name)
	}

	closeHandle()
	s.evict()

	if n, total := s.Usage(); n != 0 || total != 0 {
		t.Fatalf("second evict after the handle closed: Usage() = %d files, %d bytes; want 0 files, 0 bytes", n, total)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second evict left the thumbnail on disk: %v", err)
	}
}
