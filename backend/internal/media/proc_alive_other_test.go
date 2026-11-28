//go:build !windows

package media

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid is still running. Signal 0 performs the existence check
// without delivering a signal; EPERM means the process exists but belongs to another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
