//go:build !windows

package media

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func hideWindow(cmd *exec.Cmd) {}

// unixProcTree puts the run in its own process group and kills the group. Descendants keep
// the group id after the launcher exits, so a shim death does not orphan the real encoder.
type unixProcTree struct {
	mu     sync.Mutex
	pgid   int
	killed bool
	closed bool
}

func newProcTree(cmd *exec.Cmd) (procTree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return &unixProcTree{}, nil
}

func (p *unixProcTree) Attach(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("ffmpeg process is not started")
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("read ffmpeg process group: %w", err)
	}
	if pgid != cmd.Process.Pid {
		return fmt.Errorf("ffmpeg process group %d is not its pid %d", pgid, cmd.Process.Pid)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("process tree is closed")
	}
	p.pgid = pgid
	killed := p.killed
	p.mu.Unlock()
	if killed {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return nil
}

func (p *unixProcTree) Kill() {
	p.mu.Lock()
	if p.closed || p.killed {
		p.mu.Unlock()
		return
	}
	p.killed = true
	pgid := p.pgid
	p.mu.Unlock()
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

func (p *unixProcTree) Wait(deadline time.Time) bool {
	for {
		p.mu.Lock()
		pgid, closed := p.pgid, p.closed
		p.mu.Unlock()
		if closed || pgid <= 0 {
			return true
		}
		err := syscall.Kill(-pgid, 0)
		if err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *unixProcTree) Close() {
	p.mu.Lock()
	p.closed = true
	p.pgid = 0
	p.mu.Unlock()
}
