//go:build windows

package tools

import (
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// A Job Object holds the command and every process it starts, whatever their
// parent chain looks like: Git Bash forks and execs in a way that leaves the
// real command without a stable parent, so taskkill /T misses it.
var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procCreateToolhelp32Snapshot  = kernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First             = kernel32.NewProc("Thread32First")
	procThread32Next              = kernel32.NewProc("Thread32Next")
	procOpenThread                = kernel32.NewProc("OpenThread")
	procResumeThread              = kernel32.NewProc("ResumeThread")
)

const (
	jobObjectExtendedLimitInformationClass   = 9
	jobObjectLimitKillOnJobClose             = 0x2000
	processTerminate                         = 0x0001
	processSetQuota                          = 0x0100
	createSuspended                          = 0x0004
	snapThread                               = 0x0004
	threadSuspendResume                      = 0x0002
	jobObjectBasicAccountingInformationClass = 1
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

// Tree holds the Job Object containing a command and its descendants.
type Tree struct {
	mu            sync.Mutex
	job           syscall.Handle
	contained     bool
	startWarnings []string
}

// StartTree starts cmd suspended, assigns it to a kill-on-close job, then
// resumes its initial thread. If the job is unavailable, Contained is false
// and cancellation falls back to taskkill /T and then killing the process.
// The caller must Wait on cmd and Close the tree.
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	t := &Tree{}
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
	return startTree(cmd, t)
}

func startTree(cmd *exec.Cmd, t *Tree) (*Tree, error) {
	// CommandContext's watcher can cancel as soon as Start returns. Hold the
	// lock through assignment and resume so it cannot kill an empty job and
	// then let the newly assigned process run.
	t.mu.Lock()
	defer t.mu.Unlock()
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			if err := t.Kill(); err == nil {
				return nil
			}
			if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err == nil {
				return nil
			}
			return cmd.Process.Kill()
		}
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended
	if err := cmd.Start(); err != nil {
		t.close()
		return nil, err
	}
	if t.job != 0 {
		h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
		if err == nil {
			ok, _, _ := procAssignProcessToJobObject.Call(uintptr(t.job), uintptr(h))
			t.contained = ok != 0
			_ = syscall.CloseHandle(h)
		}
		if !t.contained {
			t.close()
		}
	}
	if err := resumeInitialThread(uint32(cmd.Process.Pid), func(err error) {
		t.startWarnings = append(t.startWarnings, err.Error())
	}); err != nil {
		// Never leave a suspended process behind on a failed start.
		t.close()
		_ = cmd.Process.Kill()
		// Wait can wait for the cancellation watcher, which takes our lock.
		t.mu.Unlock()
		_ = cmd.Wait()
		t.mu.Lock()
		return nil, err
	}
	return t, nil
}

type threadEntry32 struct {
	Size, Usage, ThreadID, OwnerProcessID uint32
	BasePriority, DeltaPriority           int32
	Flags                                 uint32
}

// resumeInitialThread resumes the suspended process. A process created
// suspended has one thread, but another program (security software, a
// debugger) may have added one before this runs, and the snapshot does not say
// which is the initial thread. So every thread of the process is resumed:
// resuming a thread that was not suspended changes nothing.
func resumeInitialThread(pid uint32, warning func(error)) error {
	return resumeInitialThreadWith(pid, resumeThread, warning)
}

func resumeInitialThreadWith(pid uint32, resume func(uint32) error, warning func(error)) error {
	snapshot, _, err := procCreateToolhelp32Snapshot.Call(snapThread, 0)
	if snapshot == ^uintptr(0) {
		return fmt.Errorf("snapshot threads for process %d: %w", pid, err)
	}
	defer syscall.CloseHandle(syscall.Handle(snapshot))
	resumed := 0
	var resumeErr error
	entry := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	ok, _, err := procThread32First.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	for ok != 0 {
		if entry.OwnerProcessID == pid {
			if err := resume(entry.ThreadID); err != nil {
				// An injected thread may exit after the snapshot or deny
				// access. Keep trying the other threads of the process.
				resumeErr = err
				if warning != nil {
					warning(fmt.Errorf("resume process %d thread %d: %w", pid, entry.ThreadID, err))
				}
			} else {
				resumed++
			}
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		ok, _, err = procThread32Next.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	}
	if resumed == 0 {
		if resumeErr != nil {
			return fmt.Errorf("resume process %d: %w", pid, resumeErr)
		}
		return fmt.Errorf("find the initial thread of process %d: %w", pid, err)
	}
	return nil
}

func resumeThread(id uint32) error {
	h, _, err := procOpenThread.Call(threadSuspendResume, 0, uintptr(id))
	if h == 0 {
		return fmt.Errorf("open thread %d: %w", id, err)
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	if count, _, err := procResumeThread.Call(h); uint32(count) == ^uint32(0) {
		return fmt.Errorf("thread %d: %w", id, err)
	}
	return nil
}

// Close releases the job; with kill-on-close set, anything the command left
// running in the background ends here.
func (t *Tree) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.close()
}

func (t *Tree) close() {
	if t.job != 0 {
		_ = syscall.CloseHandle(t.job)
		t.job = 0
	}
}

// Contained reports whether job assignment succeeded.
func (t *Tree) Contained() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.contained
}

// StartWarnings reports per-thread failures even when another thread resumed.
// These explain a possible suspended initial thread without failing the start.
func (t *Tree) StartWarnings() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.startWarnings...)
}

// Kill terminates every process in the job, even after the command has exited.
// Without a job the tree cannot be killed reliably; context cancellation still
// attempts the taskkill fallback.
func (t *Tree) Kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return fmt.Errorf("process tree is not contained or its job is closed; cannot kill the tree reliably")
	}
	if ok, _, err := procTerminateJobObject.Call(uintptr(t.job), 1); ok == 0 {
		return fmt.Errorf("terminate process tree: %w", err)
	}
	return nil
}

type jobObjectBasicAccountingInformation struct {
	TotalUserTime, TotalKernelTime                                                 int64
	ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime                             int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses uint32
}

// WaitGone polls the job's active process count before Close releases its handle.
// An unavailable or closed job cannot prove that descendants have stopped.
func (t *Tree) WaitGone(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		if t.job == 0 {
			t.mu.Unlock()
			return fmt.Errorf("process tree is not contained or its job is closed; cannot verify remaining processes")
		}
		var info jobObjectBasicAccountingInformation
		ok, _, err := procQueryInformationJobObject.Call(uintptr(t.job), jobObjectBasicAccountingInformationClass, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
		t.mu.Unlock()
		if ok == 0 {
			return fmt.Errorf("query process tree: %w", err)
		}
		if info.ActiveProcesses == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process tree: %d processes still alive after %s", info.ActiveProcesses, timeout)
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}
