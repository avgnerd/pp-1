package thumb

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A process that dies between the temporary write and the rename leaves a .tmp file behind. It is
// not a thumbnail, and indexing it would charge the size limit for a file that can never be served.
func TestNewDropsLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	cached := []byte(strings.Repeat("c", 120))
	leftover := []byte(strings.Repeat("t", 55))
	if err := os.WriteFile(filepath.Join(dir, "0123abcd.jpg"), cached, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0123abcd.jpg.tmp"), leftover, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := New(nil, dir, func() int64 { return 1 << 20 })
	if err != nil {
		t.Fatal(err)
	}

	if n, total := s.Usage(); n != 1 || total != int64(len(cached)) {
		t.Fatalf("Usage() = %d files, %d bytes; want only the cached thumbnail: 1 file, %d bytes", n, total, len(cached))
	}
	if _, err := os.Stat(filepath.Join(dir, "0123abcd.jpg.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover temporary file was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0123abcd.jpg")); err != nil {
		t.Fatalf("cached thumbnail was removed: %v", err)
	}
}

// concurrentStoreDir is a directory for the rewrite-heavy test. Windows reports a directory as
// non-empty while a file deleted a moment ago is still closing, so cleanup retries instead of
// failing a test whose assertions already ran. The test checks the directory contents itself.
func concurrentStoreDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "thumb-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("left the temporary directory behind: %s", dir)
	})
	return dir
}

// Two callers that miss the same thumbnail each store the batch result, so stores of one key run at
// once. Every writer must leave either its whole payload or none of it, and the index must describe
// the file that is actually on disk: a reported size that disagrees with the file skews the size
// limit and lets the cache grow past it.
func TestConcurrentStoreKeepsFileAndIndexConsistent(t *testing.T) {
	dir := concurrentStoreDir(t)
	s, err := New(nil, dir, func() int64 { return 1 << 20 })
	if err != nil {
		t.Fatal(err)
	}
	const name = "9f8e7d0c.jpg"

	for round := 0; round < 12; round++ {
		payloads := make([][]byte, 8)
		for i := range payloads {
			payloads[i] = bytes.Repeat([]byte(fmt.Sprintf("%d", i)), 512*(i+1)+round)
		}
		var wg sync.WaitGroup
		for i := range payloads {
			wg.Add(1)
			go func(p []byte) {
				defer wg.Done()
				s.store(name, p)
			}(payloads[i])
		}
		wg.Wait()

		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		whole := false
		for _, p := range payloads {
			if bytes.Equal(data, p) {
				whole = true
				break
			}
		}
		if !whole {
			t.Fatalf("round %d: the file holds %d bytes, which is not any writer's payload", round, len(data))
		}
		if n, total := s.Usage(); n != 1 || total != int64(len(data)) {
			t.Fatalf("round %d: Usage() = %d files, %d bytes, but the file on disk is %d bytes", round, n, total, len(data))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}
