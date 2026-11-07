package testutil

import (
	"os"
	"testing"
)

const tempDirCleanupAttempts = 8

// TempDir returns a test temporary directory and retries Windows cleanup when
// the filesystem briefly reports an empty directory as non-empty.
func TempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		var lastErr error
		for attempt := 1; attempt <= tempDirCleanupAttempts; attempt++ {
			err := os.RemoveAll(dir)
			if err == nil {
				if attempt > 1 {
					t.Logf("removed test temp dir %q after %d attempts: %v", dir, attempt, lastErr)
				}
				return
			}
			if !isDirNotEmpty(err) {
				// Leave the error to t.TempDir's original cleanup, which has its own
				// bounded retries and final failure reporting.
				return
			}
			lastErr = err
		}
		t.Errorf("remove test temp dir %q: directory remained non-empty after %d attempts: %v", dir, tempDirCleanupAttempts, lastErr)
	})
	return dir
}
