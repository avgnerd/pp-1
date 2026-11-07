//go:build windows

package media

import (
	"os/exec"
	"syscall"
)

// hideWindow stops ffmpeg/ffprobe from flashing a console window on Windows.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
