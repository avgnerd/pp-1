package media

import (
	"os/exec"
	"time"
)

// procTree owns the OS mechanism that keeps one ffmpeg run's descendants together. A run is
// over only after Kill and Wait confirm that every process in the tree is gone; Close releases
// the OS handle and is the last-resort kill for anything still attached.
type procTree interface {
	Attach(*exec.Cmd) error
	Kill()
	Wait(deadline time.Time) bool
	Close()
}

const runTreeWait = 5 * time.Second
