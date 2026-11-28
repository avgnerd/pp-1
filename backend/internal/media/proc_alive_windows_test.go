//go:build windows

package media

import "golang.org/x/sys/windows"

// processAlive reports whether pid is still running. GetExitCodeProcess returns STILL_ACTIVE
// (259) for a process that has not exited; a PID with no open handle is gone.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
