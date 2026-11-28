//go:build windows

package media

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hideWindow stops ffmpeg/ffprobe from flashing a console window on Windows.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// winProcTree ties one ffmpeg run to a job object. The job is created before the process and
// the process is created suspended, so it cannot spawn a child before it is assigned. Closing
// the last job handle with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE kills whatever is left in it.
type winProcTree struct {
	mu     sync.Mutex
	job    windows.Handle
	cmd    *exec.Cmd
	killed bool
	closed bool
}

func newProcTree(cmd *exec.Cmd) (procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("set kill-on-close: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Assigning a running process could miss a child it spawned in between. Keep the process
	// suspended until it is in the job, then resume its threads.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	return &winProcTree{job: job, cmd: cmd}, nil
}

func (p *winProcTree) Attach(cmd *exec.Cmd) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("process tree is closed")
	}
	if p.killed {
		return fmt.Errorf("process tree was terminated before attach")
	}
	if cmd.Process == nil {
		return fmt.Errorf("ffmpeg process is not started")
	}
	var attachErr error
	err := cmd.Process.WithHandle(func(h uintptr) {
		if err := windows.AssignProcessToJobObject(p.job, windows.Handle(h)); err != nil {
			attachErr = fmt.Errorf("assign ffmpeg to job: %w", err)
			return
		}
		if err := resumeProcess(windows.Handle(h)); err != nil {
			attachErr = fmt.Errorf("resume ffmpeg: %w", err)
		}
	})
	if err != nil {
		return fmt.Errorf("use ffmpeg process handle: %w", err)
	}
	return attachErr
}

func (p *winProcTree) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.killed {
		return
	}
	p.killed = true
	if p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *winProcTree) Wait(deadline time.Time) bool {
	for {
		p.mu.Lock()
		if p.closed || p.job == 0 {
			p.mu.Unlock()
			return true
		}
		var info jobObjectBasicAccountingInformation
		err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
		active := info.ActiveProcesses
		p.mu.Unlock()
		if err == nil && active == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *winProcTree) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	job := p.job
	p.job = 0
	p.mu.Unlock()
	if job != 0 {
		// Closing the last handle is the backstop: kill-on-close takes down any survivor.
		_ = windows.CloseHandle(job)
	}
}

// jobObjectBasicAccountingInformation is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

func resumeProcess(h windows.Handle) error {
	if err := ntResumeProcess.Find(); err != nil {
		return err
	}
	status, _, _ := ntResumeProcess.Call(uintptr(h))
	if status != 0 {
		return fmt.Errorf("NtResumeProcess returned NTSTATUS 0x%x", uint32(status))
	}
	return nil
}
