//go:build windows

package update

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32Snapshot   = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW            = kernel32.NewProc("Process32FirstW")
	procProcess32NextW             = kernel32.NewProc("Process32NextW")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procReadProcessMemory          = kernel32.NewProc("ReadProcessMemory")
	procGetProcessTimes            = kernel32.NewProc("GetProcessTimes")
	ntdll                          = syscall.NewLazyDLL("ntdll.dll")
	procNtQueryInformationProcess  = ntdll.NewProc("NtQueryInformationProcess")
)

const (
	th32csSnapProcess              = 0x00000002
	processQueryInformation        = 0x0400
	processQueryLimitedInformation = 0x1000
	processVMRead                  = 0x0010
	processBasicInformationClass   = 0
	maxPath                        = 260

	// Offsets in the 64-bit PEB and RTL_USER_PROCESS_PARAMETERS.
	pebProcessParameters = 0x20
	paramsCommandLine    = 0x70

	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

type processEntry32 struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [maxPath]uint16
}

// The 64-bit PROCESS_BASIC_INFORMATION pads every field to pointer size.
type processBasicInformation struct {
	ExitStatus                   uintptr
	PebBaseAddress               uintptr
	AffinityMask                 uintptr
	BasePriority                 uintptr
	UniqueProcessID              uintptr
	InheritedFromUniqueProcessID uintptr
}

type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	_             uint32
	Buffer        uintptr
}

func systemProcesses() ([]Process, error) {
	snap, _, err := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if syscall.Handle(snap) == syscall.InvalidHandle {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer syscall.CloseHandle(syscall.Handle(snap))

	var entry processEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	ok, _, err := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		return nil, fmt.Errorf("Process32First: %w", err)
	}
	var procs []Process
	names := map[int]string{}
	for ok != 0 {
		p := Process{PID: int(entry.ProcessID), PPID: int(entry.ParentProcessID)}
		image := syscall.UTF16ToString(entry.ExeFile[:])
		names[p.PID] = image
		p.Exe = image
		if path, args, start := inspect(entry.ProcessID); path != "" {
			p.Exe, p.Args, p.Start = path, args, start
		}
		procs = append(procs, p)
		entry.Size = uint32(unsafe.Sizeof(entry))
		ok, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}
	for i := range procs {
		procs[i].ParentName = names[procs[i].PPID]
	}
	return procs, nil
}

// Restricted processes may permit only limited reads; unavailable fields stay empty.
func inspect(pid uint32) (path string, args []string, start time.Time) {
	if pid == 0 {
		return "", nil, time.Time{}
	}
	full := true
	h, _, _ := procOpenProcess.Call(processQueryInformation|processVMRead, 0, uintptr(pid))
	if h == 0 {
		full = false
		if h, _, _ = procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid)); h == 0 {
			return "", nil, time.Time{}
		}
	}
	defer syscall.CloseHandle(syscall.Handle(h))

	for _, n := range []int{syscall.MAX_PATH, syscall.MAX_LONG_PATH} {
		buf := make([]uint16, n)
		size := uint32(len(buf))
		if ok, _, _ := procQueryFullProcessImageNameW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); ok != 0 {
			path = syscall.UTF16ToString(buf[:size])
			break
		}
	}
	var created, exited, kernel, user syscall.Filetime
	if ok, _, _ := procGetProcessTimes.Call(h, uintptr(unsafe.Pointer(&created)), uintptr(unsafe.Pointer(&exited)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); ok != 0 {
		start = time.Unix(0, created.Nanoseconds())
	}
	if full {
		if line, err := commandLine(h); err == nil {
			if parts := splitCommandLine(line); len(parts) > 0 {
				args = parts[1:]
			}
		}
	}
	return path, args, start
}

func commandLine(h uintptr) (string, error) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return "", errors.New("the PEB layout here is the 64-bit one")
	}
	var pbi processBasicInformation
	var retLen uint32
	status, _, _ := procNtQueryInformationProcess.Call(h, processBasicInformationClass, uintptr(unsafe.Pointer(&pbi)), unsafe.Sizeof(pbi), uintptr(unsafe.Pointer(&retLen)))
	if status != 0 || pbi.PebBaseAddress == 0 {
		return "", fmt.Errorf("NtQueryInformationProcess: status %#x", status)
	}
	var params uintptr
	if err := readMemory(h, pbi.PebBaseAddress+pebProcessParameters, unsafe.Pointer(&params), unsafe.Sizeof(params)); err != nil {
		return "", err
	}
	if params == 0 {
		return "", errors.New("no process parameters")
	}
	var cl unicodeString
	if err := readMemory(h, params+paramsCommandLine, unsafe.Pointer(&cl), unsafe.Sizeof(cl)); err != nil {
		return "", err
	}
	if cl.Length == 0 || cl.Buffer == 0 {
		return "", nil
	}
	chars := make([]uint16, cl.Length/2)
	if err := readMemory(h, cl.Buffer, unsafe.Pointer(&chars[0]), uintptr(cl.Length)); err != nil {
		return "", err
	}
	return syscall.UTF16ToString(chars), nil
}

func readMemory(h, addr uintptr, into unsafe.Pointer, n uintptr) error {
	var read uintptr
	ok, _, err := procReadProcessMemory.Call(h, addr, uintptr(into), n, uintptr(unsafe.Pointer(&read)))
	if ok == 0 {
		return fmt.Errorf("ReadProcessMemory: %w", err)
	}
	if read != n {
		return fmt.Errorf("ReadProcessMemory: short read")
	}
	return nil
}

// Detach from the console and process group so our Ctrl+C cannot kill the replacement.
func detach(attr *syscall.SysProcAttr) {
	attr.CreationFlags |= detachedProcess | createNewProcessGroup
}
