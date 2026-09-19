//go:build windows

package tools

import (
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

// A Job Object holds the shell and every process it starts, whatever their
// parent chain looks like: Git Bash forks and execs in a way that leaves the
// real command without a stable parent, so taskkill /T misses it.
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x2000
	processTerminate                       = 0x0001
	processSetQuota                        = 0x0100
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type processTree struct {
	job syscall.Handle
}

// newProcessTree creates the job and makes cancellation terminate it. When
// the job cannot be created, cancellation falls back to taskkill /T and then
// to killing the shell alone.
func newProcessTree(cmd *exec.Cmd) *processTree {
	t := &processTree{}
	if h, _, _ := procCreateJobObjectW.Call(0, 0); h != 0 {
		var info jobObjectExtendedLimitInformation
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		ok, _, _ := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformationClass, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if ok == 0 {
			_ = syscall.CloseHandle(syscall.Handle(h))
			h = 0
		}
		t.job = syscall.Handle(h)
	}
	cmd.Cancel = func() error {
		if t.job != 0 {
			if ok, _, _ := procTerminateJobObject.Call(uintptr(t.job), 1); ok != 0 {
				return nil
			}
		}
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	return t
}

// Attach puts the started shell into the job. Its children join the job on
// creation, so a command started before this call would be missed; the shell
// is still initialising at that point, so in practice nothing is.
func (t *processTree) Attach(cmd *exec.Cmd) {
	if t.job == 0 || cmd.Process == nil {
		return
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)
	_, _, _ = procAssignProcessToJobObject.Call(uintptr(t.job), uintptr(h))
}

// Close releases the job; with kill-on-close set, anything the command left
// running in the background ends here.
func (t *processTree) Close() {
	if t.job != 0 {
		_ = syscall.CloseHandle(t.job)
		t.job = 0
	}
}
